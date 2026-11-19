package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/live"
	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/silocatalog"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// liveUpdatesEnabled lets tests keep Configure from connecting to Shoko's
// event hub.
var liveUpdatesEnabled = true

const (
	// maxApplyFailures is how many times in a row a batch of VFS changes may
	// fail to apply before a full crawl takes over from retrying it.
	maxApplyFailures = 6
	// sweepInterval is how often the VFS is checked against Shoko's file list
	// and groups, for changes Shoko sent no event for. Stable servers send
	// none when a series moves between groups or a group is renamed.
	sweepInterval = time.Hour
	// catchUpAfter is how old the last commit may be when the plugin starts
	// listening before a full crawl catches up on changes it did not hear.
	catchUpAfter = 10 * time.Minute
	// posterCheckInterval is how often the Season Posters list is compared
	// with the last one. That plugin sends no event when a show poster
	// changes.
	posterCheckInterval = 5 * time.Minute
	// libraryCacheTTL is how long the list of Silo libraries that scan the
	// VFS is trusted.
	libraryCacheTTL = 10 * time.Minute
)

// liveStartDelay is how long a new plugin process waits before following
// Shoko. Silo starts a short-lived process with the submitted settings to test
// a connection; it is gone before then, so it never connects or crawls.
var liveStartDelay = 30 * time.Second

// liveUpdates follows Shoko's events for one set of settings: it applies file
// and series changes to the VFS, and asks Silo to refresh shows whose
// metadata or artwork changed.
type liveUpdates struct {
	runtime    *runtimeServer
	engine     *reconcile.Engine
	reconciler *vfs.Reconciler
	client     *shoko.Client
	// admin is nil when no Silo admin API key is set, and queue with it.
	admin *silocatalog.Admin
	queue *refreshQueue
	// status is reported in the reconcile task's output.
	status *liveStatus

	mu          sync.Mutex
	libraries   []string
	librariesAt time.Time
	// shows caches the Silo show ID for each Shoko marker, and folders what
	// identifies the show for a marker queued for a refresh.
	shows   map[string]string
	folders map[string]showFolder
	// described caches describeShows for the view it was made from.
	described     vfsShows
	describedView *reconcile.View
	// applyFailures counts the VFS batches in a row that failed to apply. Only
	// the watcher's dispatch reads and writes it.
	applyFailures int
}

// showFolder is how a show is found in Silo: the VFS folder Silo has for it,
// and titles to search the catalog for.
type showFolder struct {
	path   string
	titles []string
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// startLiveUpdates starts following Shoko for state and returns how to stop.
func (s *runtimeServer) startLiveUpdates(state configuredState) (*liveUpdates, context.CancelFunc) {
	if !liveUpdatesEnabled || !state.cfg.LiveUpdates {
		return nil, nil
	}
	status := &liveStatus{startedAt: time.Now()}
	updates := &liveUpdates{
		runtime:    s,
		engine:     state.engine,
		reconciler: state.reconciler,
		client:     state.client,
		shows:      make(map[string]string),
		folders:    make(map[string]showFolder),
		status:     status,
	}
	if state.cfg.SiloBaseURL != "" && state.cfg.SiloAPIKey != "" {
		updates.admin = silocatalog.NewAdmin(silocatalog.AdminConfig{BaseURL: state.cfg.SiloBaseURL, APIKey: state.cfg.SiloAPIKey}, nil)
		updates.queue = newRefreshQueue(updates.refreshShow)
	}
	ctx, cancel := context.WithCancel(context.Background())
	watcher := live.New(live.Config{
		BaseURL: state.cfg.BaseURL,
		APIKey:  state.cfg.APIKey,
		Logf:    logf,
		OnEvent: func(target string) {
			status.update(func(s *liveStatus) {
				s.events++
				s.lastEvent, s.lastEventAt = target, time.Now()
			})
		},
		OnDisconnect: func(err error) {
			status.update(func(s *liveStatus) {
				s.connected = false
				s.disconnectedAt = time.Now()
				s.lastError = err.Error()
			})
		},
	}, updates)
	go func() {
		if !sleep(ctx, liveStartDelay) {
			return
		}
		if updates.queue != nil {
			go updates.queue.Run(ctx)
			go updates.watchShowPosters(ctx)
		}
		go updates.sweepRegularly(ctx)
		go updates.writeStatusFiles(ctx)
		watcher.Run(ctx)
	}()
	return updates, cancel
}

// Connected catches up on changes made while the plugin was not listening,
// which Shoko does not replay: with a sweep after a dropped connection, and a
// full crawl for a process that has just started. A commit made just before
// the plugin started listening is recent enough to skip that.
func (l *liveUpdates) Connected(ctx context.Context, reconnect bool) {
	l.status.update(func(s *liveStatus) {
		s.connected = true
		s.connectedAt = time.Now()
		s.connections++
	})
	info, err := l.engine.Snapshot(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logf("shokoanime: could not read the VFS snapshot: %v", err)
		}
		return
	}
	switch {
	case info.Exists && !reconnect && time.Since(info.CommittedAt) < catchUpAfter:
		return
	case !info.Exists && !l.vfsBuilt(ctx):
		// The VFS has never been built; the reconcile task builds it first.
		return
	}
	if l.runtime.backgroundCrawlRunning() {
		return
	}
	// A dropped connection is usually short, and a sweep finds what changed
	// meanwhile for a fraction of a crawl's cost. A process that has just
	// started may have missed far more, so it crawls.
	if reconnect && info.Exists && l.sweep(ctx) {
		return
	}
	if ctx.Err() != nil || l.runtime.backgroundCrawlRunning() {
		return
	}
	logf("shokoanime: crawling Shoko to catch up on changes made while not listening")
	l.runtime.startBackgroundCrawl(l.engine)
}

// sweep checks the VFS against Shoko's file list and groups, and reports
// whether it could. It may run alongside Apply: whichever commits second
// reads Shoko again on top of the first.
func (l *liveUpdates) sweep(ctx context.Context) bool {
	if !l.waitForCrawl(ctx) {
		return false
	}
	started := time.Now()
	outcome, err := l.engine.Sweep(ctx)
	l.status.update(func(s *liveStatus) {
		s.lastSweepAt, s.lastSweepTook = time.Now(), time.Since(started)
		s.lastSweepLinks = changedLinks(outcome.Result)
		s.lastSweepError = ""
		if err != nil {
			s.lastSweepError = err.Error()
		} else {
			s.sweeps++
		}
	})
	if err != nil {
		// Without a snapshot, or with a crawl pending, a crawl does the work.
		if ctx.Err() == nil && !errors.Is(err, reconcile.ErrNoSnapshot) && !errors.Is(err, reconcile.ErrCrawlPending) {
			logf("shokoanime: could not check the VFS against Shoko: %v", err)
		}
		return false
	}
	logReconcileFailures(outcome.Result)
	if count := changedLinks(outcome.Result); count > 0 {
		logf("shokoanime: checked the VFS against Shoko: %d links changed", count)
	}
	if moved := movedSeries(outcome); l.queue != nil && len(moved) > 0 {
		l.refreshChanged(ctx, live.Refresh{Series: moved})
	}
	return true
}

// sweepRegularly sweeps every sweepInterval while the VFS exists.
func (l *liveUpdates) sweepRegularly(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !l.runtime.backgroundCrawlRunning() {
			l.sweep(ctx)
		}
	}
}

// Apply updates the VFS for a batch of changes, then asks Silo to refresh the
// shows whose metadata or artwork changed. It returns the VFS changes that
// could not be applied, to be tried again.
func (l *liveUpdates) Apply(ctx context.Context, batch live.Batch) live.Batch {
	var retry live.Batch
	l.status.update(func(s *liveStatus) {
		s.batches++
		s.lastBatchAt = time.Now()
	})
	switch {
	case batch.Recrawl:
		// A crawl that is running may have read Shoko before the change.
		if l.waitForCrawl(ctx) && l.vfsBuilt(ctx) {
			logf("shokoanime: crawling Shoko, since it recreated every group")
			l.runtime.startBackgroundCrawl(l.engine)
		}
	case !batch.VFS.Empty():
		// The shows in the VFS before the change, to tell which ones it removes.
		before, describedBefore := l.showsBefore(ctx)
		moved, applied := l.applyVFS(ctx, batch.VFS)
		switch {
		case applied:
			l.applyFailures = 0
			if describedBefore {
				l.retireRemovedShows(ctx, before)
			}
			// Silo matched these files under their old season and episode
			// numbers, so it has to read the show again.
			batch.Refresh.Series = append(slices.Clone(batch.Refresh.Series), moved...)
		case ctx.Err() != nil:
		case l.applyFailures+1 >= maxApplyFailures:
			l.applyFailures = 0
			if l.vfsBuilt(ctx) {
				logf("shokoanime: crawling Shoko, since its changes still could not be applied")
				l.runtime.startBackgroundCrawl(l.engine)
			}
		default:
			l.applyFailures++
			retry.VFS = batch.VFS
		}
	}
	if l.queue != nil && !batch.Refresh.Empty() {
		l.refreshChanged(ctx, batch.Refresh)
	}
	return retry
}

// showsBefore describes the VFS before a change is applied, when Silo
// refreshes are on. It reports false when there is nothing to compare with.
func (l *liveUpdates) showsBefore(ctx context.Context) (vfsShows, bool) {
	if l.admin == nil {
		return vfsShows{}, false
	}
	shows, err := l.describeShows(ctx)
	if err != nil {
		return vfsShows{}, false
	}
	return shows, true
}

// retireRemovedShows releases the episode IDs of every Silo show the last
// change retired by moving its series into other shows.
//
// When Shoko moves a series into another group, the old group's folder leaves
// the VFS and the series' files appear under the group that now holds them.
// Silo makes a new show for those files or adds them to the existing one, but
// the old show keeps its episodes' TMDB, TVDB and IMDb IDs until Silo deletes
// it, which takes at least its missing-file grace period and does not always
// happen. Silo stores each of those IDs on one episode only, so the episodes of
// the show that now holds the files cannot take them and keep "Episode N"
// placeholders. Releasing the IDs right away lets the refresh that follows the
// change fill those episodes in.
//
// The old show is looked up now, before Silo scans the change, while Silo still
// lists it under its folder. Nothing is deleted: the old show, its titles and
// any watch state stay until Silo removes the show itself.
func (l *liveUpdates) retireRemovedShows(ctx context.Context, before vfsShows) {
	after, err := l.describeShows(ctx)
	if err != nil {
		return
	}
	for _, marker := range regroupedShows(before, after) {
		l.mu.Lock()
		l.folders[marker] = before.folders[marker]
		l.mu.Unlock()
		contentID, err := l.showContentID(ctx, marker)
		if err == nil && contentID == "" {
			// Silo never scanned the show, or already removed it.
			continue
		}
		released := 0
		if err == nil {
			released, _, err = l.admin.ReleaseEpisodeIDs(ctx, contentID)
		}
		l.mu.Lock()
		delete(l.shows, marker)
		delete(l.folders, marker)
		l.mu.Unlock()
		l.status.update(func(s *liveStatus) {
			s.lastRetireAt, s.lastRetireShow, s.lastRetireError = time.Now(), marker, ""
			if err != nil {
				s.lastRetireError = err.Error()
				return
			}
			s.retiredShows++
			s.releasedIDs += released
		})
		switch {
		case err != nil && ctx.Err() == nil:
			logf("shokoanime: could not release the episode IDs of the Silo show for %s, whose series moved to another show: %v", marker, err)
		case err == nil:
			logf("shokoanime: released the IDs of %d episodes of the Silo show for %s, whose series moved to another show", released, marker)
		}
	}
}

// regroupedShows returns, in order, the shows a change retired by moving their
// series into other shows: the show's marker is gone, no show is left in its
// folder, and at least one of its series is now in another show. A show that
// only lost its files, as when its last file is replaced or deleted, is not
// one of them; Silo keeps or removes that show by itself.
func regroupedShows(before, after vfsShows) []string {
	folders := make(map[string]struct{}, len(after.folders))
	for _, folder := range after.folders {
		folders[folder.path] = struct{}{}
	}
	moved := make(map[string]bool)
	for series, marker := range before.bySeries {
		if now, ok := after.bySeries[series]; ok && now != marker {
			moved[marker] = true
		}
	}
	var retired []string
	for marker, folder := range before.folders {
		if _, ok := after.folders[marker]; ok {
			continue
		}
		if _, ok := folders[folder.path]; ok {
			// Another show took over the folder, and Silo keeps its show for it.
			continue
		}
		if moved[marker] {
			retired = append(retired, marker)
		}
	}
	slices.Sort(retired)
	return retired
}

// applyVFS applies changes to the VFS. It returns the series whose links
// moved to another path, and whether the changes are dealt with: applied, or
// covered by a full crawl.
func (l *liveUpdates) applyVFS(ctx context.Context, changes reconcile.Changes) (moved []int, done bool) {
	for attempt := 0; attempt < 3; attempt++ {
		// A crawl may have read Shoko before these changes, so they are
		// applied after it commits.
		if !l.waitForCrawl(ctx) {
			return nil, false
		}
		started := time.Now()
		outcome, err := l.engine.Apply(ctx, changes)
		l.status.update(func(s *liveStatus) {
			s.applies++
			s.lastApplyAt, s.lastApplyTook = time.Now(), time.Since(started)
			s.lastApplyLinks = changedLinks(outcome.Result)
			s.lastApplyError = ""
			if err != nil {
				s.lastApplyError = err.Error()
			}
		})
		switch {
		case err == nil:
			logReconcileFailures(outcome.Result)
			if count := changedLinks(outcome.Result); count > 0 {
				logf("shokoanime: applied Shoko changes (%d files, %d series, %d groups): %d links changed", len(changes.Files), len(changes.Series), len(changes.Groups), count)
			}
			return movedSeries(outcome), true
		case errors.Is(err, reconcile.ErrCrawlPending):
			l.runtime.startBackgroundCrawl(l.engine)
		case errors.Is(err, reconcile.ErrNoSnapshot):
			// Built by a version without snapshots, or settings changed: a
			// full crawl leaves a snapshot.
			if !l.vfsBuilt(ctx) {
				// The reconcile task builds the VFS first, from current data.
				return nil, true
			}
			if !l.runtime.backgroundCrawlRunning() {
				// This crawl starts after the changes, so it covers them.
				logf("shokoanime: crawling Shoko once to apply its changes, since there is no snapshot to update yet")
				l.runtime.startBackgroundCrawl(l.engine)
				return nil, true
			}
			// A crawl that is already running may have read Shoko before the
			// changes: apply them to the snapshot it leaves.
		default:
			if ctx.Err() == nil {
				logf("shokoanime: could not apply Shoko changes, trying again shortly: %v", err)
			}
			return nil, false
		}
	}
	return nil, false
}

// movedSeries returns the series that have a link an apply moved to another
// path, as a renumbered season or a renamed show does.
func movedSeries(outcome reconcile.Outcome) []int {
	paths := make(map[string]struct{})
	for _, action := range outcome.Result.Actions {
		if action.Kind == "update" && action.Reason == vfs.ReasonMoved {
			paths[action.LogicalPath] = struct{}{}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	series := make(map[int]struct{})
	for _, entry := range outcome.Plan.Entries {
		if _, moved := paths[entry.LogicalPath]; moved {
			series[entry.ShokoSeriesID] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(series))
}

// changedLinks counts the links a reconcile created, replaced or removed.
func changedLinks(result vfs.Result) int {
	changed := 0
	for _, action := range result.Actions {
		if action.Kind != "unchanged" {
			changed++
		}
	}
	return changed
}

// waitForCrawl waits until no background crawl is running, and reports false
// if ctx ended first.
func (l *liveUpdates) waitForCrawl(ctx context.Context) bool {
	for l.runtime.backgroundCrawlRunning() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return ctx.Err() == nil
}

// vfsBuilt reports whether a reconcile has ever written the VFS.
func (l *liveUpdates) vfsBuilt(ctx context.Context) bool {
	marker, err := l.reconciler.CurrentMarker(ctx)
	return err == nil && marker != "0"
}

// refreshChanged queues a Silo refresh of each show in the VFS that holds a
// changed group, series or episode. Silo refreshes a show's seasons, episodes
// and artwork with it.
func (l *liveUpdates) refreshChanged(ctx context.Context, refresh live.Refresh) {
	shows, err := l.describeShows(ctx)
	if err != nil {
		// Without a snapshot there is no telling which shows are in the VFS.
		return
	}
	markers := make(map[string]struct{})
	seriesIDs := slices.Clone(refresh.Series)
	for _, id := range refresh.Episodes {
		if parent, ok := shows.episodeSeries[id]; ok {
			seriesIDs = append(seriesIDs, parent)
		}
	}
	for _, id := range seriesIDs {
		if marker, ok := shows.bySeries[id]; ok {
			markers[marker] = struct{}{}
		}
	}
	for _, id := range refresh.Groups {
		if marker, ok := shows.byGroup[id]; ok {
			markers[marker] = struct{}{}
		}
	}
	l.queueRefreshes(shows, slices.Sorted(maps.Keys(markers)))
}

func (l *liveUpdates) queueRefreshes(shows vfsShows, markers []string) {
	l.mu.Lock()
	for _, marker := range markers {
		l.folders[marker] = shows.folders[marker]
	}
	l.mu.Unlock()
	for _, marker := range markers {
		l.queue.Add(marker)
	}
}

// vfsShows describes the shows in the VFS: the marker every file of a show
// carries, by the series and groups in it, and each show's folder.
type vfsShows struct {
	bySeries      map[int]string
	byGroup       map[int]string
	episodeSeries map[int]int
	folders       map[string]showFolder
}

func (l *liveUpdates) describeShows(ctx context.Context) (vfsShows, error) {
	view, err := l.engine.View()
	if err != nil {
		return vfsShows{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.describedView != view {
		l.described, l.describedView = describeShows(l.reconciler.Root(), view.Snapshot, view.Plan), view
	}
	return l.described, nil
}

func describeShows(root string, snapshot shoko.Snapshot, plan topology.Plan) vfsShows {
	shows := vfsShows{
		bySeries:      make(map[int]string),
		byGroup:       make(map[int]string),
		episodeSeries: make(map[int]int),
		folders:       make(map[string]showFolder),
	}
	for id, episode := range snapshot.Episodes {
		if episode.IDs.ParentSeries > 0 {
			shows.episodeSeries[id] = episode.IDs.ParentSeries
		}
	}
	for _, entry := range plan.Entries {
		marker := fmt.Sprintf("[Shoko Series=%d]", entry.ShokoSeriesID)
		if entry.ShokoGroupID > 0 {
			marker = fmt.Sprintf("[Shoko Group=%d]", entry.ShokoGroupID)
			shows.byGroup[entry.ShokoGroupID] = marker
		}
		shows.bySeries[entry.ShokoSeriesID] = marker
		if _, ok := shows.folders[marker]; ok {
			continue
		}
		segment := strings.SplitN(filepath.ToSlash(entry.LogicalPath), "/", 2)[0]
		titles := make([]string, 0, 3)
		if group, ok := snapshot.Groups[entry.ShokoGroupID]; ok && group.Name != "" {
			titles = append(titles, group.Name)
		}
		if series, ok := snapshot.Series[entry.ShokoSeriesID]; ok && series.Name != "" && !slices.Contains(titles, series.Name) {
			titles = append(titles, series.Name)
		}
		if bare := yearSuffix.ReplaceAllString(segment, ""); !slices.Contains(titles, bare) {
			titles = append(titles, bare)
		}
		shows.folders[marker] = showFolder{path: filepath.Join(root, segment), titles: titles}
	}
	return shows
}

// yearSuffix is the " (2020)" a show folder name ends with.
var yearSuffix = regexp.MustCompile(` \(\d{4}\)$`)

func (l *liveUpdates) refreshShow(ctx context.Context, marker string) {
	for attempt := 0; attempt < 2; attempt++ {
		contentID, err := l.showContentID(ctx, marker)
		if err != nil {
			if ctx.Err() == nil {
				logf("shokoanime: could not find the Silo show for %s: %v", marker, err)
			}
			return
		}
		if contentID == "" {
			// Silo has not scanned this show yet; its scan fetches metadata.
			return
		}
		found, err := l.admin.Refresh(ctx, contentID)
		l.status.update(func(s *liveStatus) {
			s.lastRefreshAt, s.lastRefreshShow, s.lastRefreshError = time.Now(), marker, ""
			if err != nil {
				s.lastRefreshError = err.Error()
			} else if found {
				s.refreshes++
			}
		})
		if err != nil {
			if ctx.Err() == nil {
				logf("shokoanime: could not ask Silo to refresh %s: %v", marker, err)
			}
			return
		}
		if found {
			logf("shokoanime: asked Silo to refresh the show for %s", marker)
			return
		}
		// The show was rebuilt under a new ID; look it up again.
		l.mu.Lock()
		delete(l.shows, marker)
		l.mu.Unlock()
	}
}

func (l *liveUpdates) showContentID(ctx context.Context, marker string) (string, error) {
	l.mu.Lock()
	if contentID, ok := l.shows[marker]; ok {
		l.mu.Unlock()
		return contentID, nil
	}
	l.mu.Unlock()
	libraries, err := l.siloLibraries(ctx)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	folder := l.folders[marker]
	l.mu.Unlock()
	remember := func(contentID string) (string, error) {
		l.mu.Lock()
		l.shows[marker] = contentID
		l.mu.Unlock()
		return contentID, nil
	}
	for _, libraryID := range libraries {
		contentID, found, err := l.admin.ShowContentID(ctx, libraryID, marker)
		if err != nil {
			return "", err
		}
		if found {
			return remember(contentID)
		}
		if folder.path == "" {
			continue
		}
		// Shows an autoscan added are not among the library's roots.
		contentID, found, err = l.admin.ShowByFolder(ctx, libraryID, folder.path, folder.titles)
		if err != nil {
			return "", err
		}
		if found {
			return remember(contentID)
		}
	}
	return "", nil
}

func (l *liveUpdates) siloLibraries(ctx context.Context) ([]string, error) {
	l.mu.Lock()
	if l.libraries != nil && time.Since(l.librariesAt) < libraryCacheTTL {
		libraries := l.libraries
		l.mu.Unlock()
		return libraries, nil
	}
	l.mu.Unlock()
	all, err := l.admin.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	libraries := silocatalog.LibrariesUnder(all, l.reconciler.Root())
	if len(libraries) == 0 {
		logf("shokoanime: no Silo library scans the VFS output root %s, so there is nothing to refresh", l.reconciler.Root())
	}
	l.mu.Lock()
	l.libraries, l.librariesAt = libraries, time.Now()
	l.mu.Unlock()
	return libraries, nil
}

// watchShowPosters asks Silo to refresh a show when the show or specials
// poster chosen for it with the Season Posters Shoko plugin changes.
func (l *liveUpdates) watchShowPosters(ctx context.Context) {
	var last map[int]shoko.ShowPoster
	ticker := time.NewTicker(posterCheckInterval)
	defer ticker.Stop()
	for {
		lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		posters, err := l.client.ShowPosters(lookupCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logf("shokoanime: could not check Season Posters for changes: %v", err)
		case last == nil:
			last = posters
		default:
			changed := changedGroups(last, posters)
			last = posters
			if len(changed) > 0 {
				// Show metadata reads the cached list, which would
				// otherwise hand Silo's refresh the old poster.
				l.runtime.setShowPosters(l.client, posters)
				if shows, err := l.describeShows(ctx); err == nil {
					markers := make([]string, 0, len(changed))
					for _, groupID := range changed {
						if marker, ok := shows.byGroup[groupID]; ok {
							markers = append(markers, marker)
						}
					}
					l.queueRefreshes(shows, markers)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// changedGroups returns the groups whose show or specials poster was added,
// changed or removed.
func changedGroups[T comparable](before, after map[int]T) []int {
	changed := make([]int, 0)
	for groupID, imageID := range after {
		if before[groupID] != imageID {
			changed = append(changed, groupID)
		}
	}
	for groupID := range before {
		if _, ok := after[groupID]; !ok {
			changed = append(changed, groupID)
		}
	}
	slices.Sort(changed)
	return changed
}

// report returns the live-update status for the reconcile task's output.
func (l *liveUpdates) report() map[string]any {
	if l == nil {
		return map[string]any{"enabled": false}
	}
	pending := 0
	if l.queue != nil {
		pending = l.queue.Pending()
	}
	report := l.status.report(pending)
	report["silo_refreshes"] = l.admin != nil
	return report
}
