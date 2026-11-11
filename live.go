package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/live"
	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/silocatalog"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// liveUpdatesEnabled lets tests keep Configure from connecting to Shoko's
// event hub.
var liveUpdatesEnabled = true

const (
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

	mu          sync.Mutex
	libraries   []string
	librariesAt time.Time
	// shows caches the Silo show ID for each Shoko marker.
	shows map[string]string
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// startLiveUpdates starts following Shoko for state and returns how to stop.
func (s *runtimeServer) startLiveUpdates(state configuredState) context.CancelFunc {
	if !liveUpdatesEnabled || !state.cfg.LiveUpdates {
		return nil
	}
	updates := &liveUpdates{
		runtime:    s,
		engine:     state.engine,
		reconciler: state.reconciler,
		client:     state.client,
		shows:      make(map[string]string),
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
	}, updates)
	go watcher.Run(ctx)
	if updates.queue != nil {
		go updates.queue.Run(ctx)
		go updates.watchShowPosters(ctx)
	}
	return cancel
}

// Connected catches up on changes made while the plugin was not listening,
// which Shoko does not replay, by starting a full crawl. A commit made just
// before the plugin started listening is recent enough to skip that.
func (l *liveUpdates) Connected(ctx context.Context, reconnect bool) {
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
	logf("shokoanime: crawling Shoko to catch up on changes made while not listening")
	l.runtime.startBackgroundCrawl(l.engine)
}

// Apply updates the VFS for a batch of changes, then asks Silo to refresh the
// shows whose metadata or artwork changed.
func (l *liveUpdates) Apply(ctx context.Context, batch live.Batch) {
	switch {
	case batch.Recrawl:
		// A crawl that is running may have read Shoko before the change.
		if l.waitForCrawl(ctx) && l.vfsBuilt(ctx) {
			logf("shokoanime: crawling Shoko, since it recreated every group")
			l.runtime.startBackgroundCrawl(l.engine)
		}
	case !batch.VFS.Empty():
		l.applyVFS(ctx, batch.VFS)
	}
	if l.queue != nil && !batch.Refresh.Empty() {
		l.refreshChanged(ctx, batch.Refresh)
	}
}

func (l *liveUpdates) applyVFS(ctx context.Context, changes reconcile.Changes) {
	for attempt := 0; attempt < 3; attempt++ {
		// A crawl may have read Shoko before these changes, so they are
		// applied after it commits.
		if !l.waitForCrawl(ctx) {
			return
		}
		outcome, err := l.engine.Apply(ctx, changes)
		switch {
		case err == nil:
			logReconcileFailures(outcome.Result)
			if count := changedLinks(outcome.Result); count > 0 {
				logf("shokoanime: applied Shoko changes (%d files, %d series, %d groups): %d links changed", len(changes.Files), len(changes.Series), len(changes.Groups), count)
			}
			return
		case errors.Is(err, reconcile.ErrCrawlPending):
			l.runtime.startBackgroundCrawl(l.engine)
		case errors.Is(err, reconcile.ErrNoSnapshot):
			// Built by a version without snapshots, or settings changed: a
			// full crawl covers these changes and leaves a snapshot.
			if l.vfsBuilt(ctx) && !l.runtime.backgroundCrawlRunning() {
				logf("shokoanime: crawling Shoko once to apply its changes, since there is no snapshot to update yet")
				l.runtime.startBackgroundCrawl(l.engine)
			}
			return
		default:
			if ctx.Err() == nil {
				logf("shokoanime: could not apply Shoko changes, the next reconcile will: %v", err)
			}
			return
		}
	}
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
	snapshot, err := l.engine.SnapshotData(ctx)
	if err != nil {
		// Without a snapshot there is no telling which shows are in the VFS.
		return
	}
	inVFS := make(map[int]bool)
	for _, file := range snapshot.Files {
		for _, reference := range file.SeriesIDs {
			if reference.SeriesID.ID != nil {
				inVFS[*reference.SeriesID.ID] = true
			}
		}
	}
	seriesIDs := slices.Clone(refresh.Series)
	for _, id := range refresh.Episodes {
		if episode, ok := snapshot.Episodes[id]; ok && episode.IDs.ParentSeries > 0 {
			seriesIDs = append(seriesIDs, episode.IDs.ParentSeries)
		}
	}
	markers := make(map[string]struct{})
	for _, id := range seriesIDs {
		series, ok := snapshot.Series[id]
		if !ok || !inVFS[id] {
			continue
		}
		markers[showMarker(snapshot, id, series)] = struct{}{}
	}
	for _, groupID := range refresh.Groups {
		if _, ok := snapshot.Groups[groupID]; ok && slices.ContainsFunc(snapshot.GroupSeries[groupID], func(id int) bool { return inVFS[id] }) {
			markers[fmt.Sprintf("[Shoko Group=%d]", groupID)] = struct{}{}
		}
	}
	for _, marker := range slices.Sorted(maps.Keys(markers)) {
		l.queue.Add(marker)
	}
}

// showMarker is the marker every file name of the series' show carries. A
// series in no group is its own show.
func showMarker(snapshot shoko.Snapshot, seriesID int, series shoko.Series) string {
	groupIDs := make([]int, 0, 1)
	for groupID, members := range snapshot.GroupSeries {
		if slices.Contains(members, seriesID) {
			if _, ok := snapshot.Groups[groupID]; ok {
				groupIDs = append(groupIDs, groupID)
			}
		}
	}
	if len(groupIDs) > 0 {
		return fmt.Sprintf("[Shoko Group=%d]", slices.Min(groupIDs))
	}
	return fmt.Sprintf("[Shoko Series=%d]", series.IDs.ID)
}

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
	for _, libraryID := range libraries {
		contentID, found, err := l.admin.ShowContentID(ctx, libraryID, marker)
		if err != nil {
			return "", err
		}
		if found {
			l.mu.Lock()
			l.shows[marker] = contentID
			l.mu.Unlock()
			return contentID, nil
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

// watchShowPosters asks Silo to refresh a show when the poster chosen for it
// with the Season Posters Shoko plugin changes.
func (l *liveUpdates) watchShowPosters(ctx context.Context) {
	var last map[int]string
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
				snapshot, err := l.engine.SnapshotData(ctx)
				if err == nil {
					for _, groupID := range changed {
						if _, ok := snapshot.Groups[groupID]; ok {
							l.queue.Add(fmt.Sprintf("[Shoko Group=%d]", groupID))
						}
					}
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

// changedGroups returns the groups whose poster was added, changed or removed.
func changedGroups(before, after map[int]string) []int {
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
