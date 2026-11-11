package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// ErrNoSnapshot means there is no committed crawl for the current settings to
// apply changes to, so only a full crawl can bring the VFS up to date.
var ErrNoSnapshot = errors.New("reconcile: no snapshot of the last crawl")

// ErrCrawlPending means a full crawl has started but not committed. Changes
// are applied after it commits, since it may already have read the old data.
var ErrCrawlPending = errors.New("reconcile: a full crawl is in progress")

// Changes names what Shoko reported changed since the last commit.
type Changes struct {
	// Files are re-read with their episode links. A file Shoko no longer has,
	// or that is no longer in a mapped managed folder, is dropped.
	Files []int
	// Series are re-read with their episodes and their group.
	Series []int
	// Groups are re-read with their member series, for a renamed group or
	// one whose members changed.
	Groups []int
}

// Empty reports whether there is nothing to apply.
func (c Changes) Empty() bool {
	return len(c.Files) == 0 && len(c.Series) == 0 && len(c.Groups) == 0
}

// Merge adds other's IDs to c, without duplicates.
func (c *Changes) Merge(other Changes) {
	for _, id := range other.Files {
		c.Files = appendUnique(c.Files, id)
	}
	for _, id := range other.Series {
		c.Series = appendUnique(c.Series, id)
	}
	for _, id := range other.Groups {
		c.Groups = appendUnique(c.Groups, id)
	}
}

// SnapshotInfo describes the snapshot of the last committed crawl.
type SnapshotInfo struct {
	Exists bool
	// CommittedAt is when the snapshot was last committed to the VFS.
	CommittedAt time.Time
}

// Snapshot reports whether a snapshot of a committed crawl exists for the
// current settings, and when it was last written.
func (e *Engine) Snapshot(ctx context.Context) (SnapshotInfo, error) {
	if e == nil || e.reconciler == nil {
		return SnapshotInfo{}, errors.New("reconcile: engine is not configured")
	}
	var info SnapshotInfo
	err := e.reconciler.WithExclusive(ctx, func(locked *vfs.Reconciler) error {
		state, err := loadState(vfs.SnapshotPath(locked.Root()))
		if errors.Is(err, errStaleState) {
			return nil
		}
		if err != nil || state == nil || state.Fingerprint != e.fingerprint || state.Phase != phaseReady {
			return err
		}
		stat, err := os.Stat(vfs.SnapshotPath(locked.Root()))
		if err != nil {
			return fmt.Errorf("reconcile: inspect snapshot: %w", err)
		}
		info = SnapshotInfo{Exists: true, CommittedAt: stat.ModTime()}
		return nil
	})
	return info, err
}

// SnapshotData returns the Shoko data behind the last commit. It fails with
// ErrNoSnapshot when there is none for the current settings.
func (e *Engine) SnapshotData(ctx context.Context) (shoko.Snapshot, error) {
	if e == nil || e.reconciler == nil {
		return shoko.Snapshot{}, errors.New("reconcile: engine is not configured")
	}
	var snapshot shoko.Snapshot
	err := e.reconciler.WithExclusive(ctx, func(locked *vfs.Reconciler) error {
		state, err := e.loadSnapshot(locked)
		if err != nil {
			return err
		}
		snapshot = stateSnapshot(state)
		return nil
	})
	return snapshot, err
}

func (e *Engine) loadSnapshot(locked *vfs.Reconciler) (*crawlState, error) {
	state, err := loadState(vfs.SnapshotPath(locked.Root()))
	if errors.Is(err, errStaleState) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	if state == nil || state.Fingerprint != e.fingerprint || state.Phase != phaseReady {
		return nil, ErrNoSnapshot
	}
	return state, nil
}

// Apply re-reads only what changed from Shoko, updates the snapshot of the
// last crawl, and reconciles the VFS from it. The reconcile compares the whole
// plan with the links on disk, so only links whose path or target changed are
// written and journaled. It fails with ErrNoSnapshot or ErrCrawlPending when a
// full crawl has to run first.
//
// Shoko is read without holding the VFS lock, so scan-source polls are not
// held up by a large batch. If another commit lands meanwhile, the changes
// are read again on top of it.
func (e *Engine) Apply(ctx context.Context, changes Changes) (Outcome, error) {
	if e == nil || e.client == nil || e.reconciler == nil {
		return Outcome{}, errors.New("reconcile: engine is not configured")
	}
	if err := e.reconciler.EnsureRoot(); err != nil {
		return Outcome{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		var state *crawlState
		err := e.reconciler.WithExclusive(ctx, func(locked *vfs.Reconciler) error {
			var err error
			state, err = e.loadApplicable(locked)
			return err
		})
		if err != nil {
			return Outcome{}, err
		}
		base := state.UpdatedAt
		if err := e.refresh(ctx, state, changes); err != nil {
			return Outcome{}, err
		}
		var outcome Outcome
		stale := false
		err = e.reconciler.WithExclusive(ctx, func(locked *vfs.Reconciler) error {
			current, err := e.loadApplicable(locked)
			if err != nil {
				return err
			}
			if !current.UpdatedAt.Equal(base) {
				stale = true
				return nil
			}
			plan, result, err := e.commit(ctx, locked, state, true)
			if err != nil {
				return err
			}
			if err := saveState(vfs.SnapshotPath(locked.Root()), state); err != nil {
				return err
			}
			outcome = e.progress(state)
			outcome.Complete = true
			outcome.Plan = plan
			outcome.Result = result
			return nil
		})
		if err != nil || !stale {
			return outcome, err
		}
	}
	return Outcome{}, errors.New("reconcile: the snapshot kept changing while changes were applied")
}

// loadApplicable loads the snapshot that changes can be applied to.
func (e *Engine) loadApplicable(locked *vfs.Reconciler) (*crawlState, error) {
	if err := locked.EnsureRootLocked(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(vfs.CrawlStatePath(locked.Root())); err == nil {
		return nil, ErrCrawlPending
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reconcile: inspect pending state: %w", err)
	}
	return e.loadSnapshot(locked)
}

// refresh re-reads the changed files and series, then everything they depend
// on that the snapshot does not have or that may have moved: the groups the
// series left and joined, and episodes a file links to that are not known yet.
func (e *Engine) refresh(ctx context.Context, state *crawlState, changes Changes) error {
	seriesToFetch := make(map[int]struct{})
	for _, id := range changes.Series {
		if id > 0 {
			seriesToFetch[id] = struct{}{}
		}
	}

	files, err := fetchAll(ctx, e.concurrency, positiveIDs(changes.Files), e.client.File)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		byID := make(map[int]shoko.File, len(files))
		for id, file := range files {
			byID[id] = file.value
		}
		kept := state.Files[:0]
		for _, file := range state.Files {
			if _, changed := byID[file.ID]; changed {
				// The old links tell which series lose an episode.
				for id := range fileSeriesIDs(file) {
					seriesToFetch[id] = struct{}{}
				}
				continue
			}
			kept = append(kept, file)
		}
		state.Files = kept
		for _, id := range sortedKeys(files) {
			item := files[id]
			if item.missing || !e.inMappedFolder(item.value) {
				continue
			}
			state.Files = append(state.Files, item.value)
			for seriesID := range fileSeriesIDs(item.value) {
				seriesToFetch[seriesID] = struct{}{}
			}
		}
	}

	// Episodes a file links to that the snapshot lacks are fetched one by one,
	// and can name a series that is new to the snapshot.
	fetchedSeries := make(map[int]bool)
	for round := 0; round < 3; round++ {
		groupsToFetch, err := e.refreshSeries(ctx, state, seriesToFetch, fetchedSeries)
		if err != nil {
			return err
		}
		if round == 0 {
			for _, id := range positiveIDs(changes.Groups) {
				groupsToFetch[id] = struct{}{}
			}
		}
		if err := e.refreshGroups(ctx, state, groupsToFetch); err != nil {
			return err
		}
		seriesToFetch = make(map[int]struct{})
		missing := missingEpisodes(state)
		if len(missing) == 0 {
			break
		}
		episodes, err := fetchAll(ctx, e.concurrency, missing, e.client.Episode)
		if err != nil {
			return err
		}
		for _, id := range sortedKeys(episodes) {
			item := episodes[id]
			if item.missing {
				continue
			}
			state.Episodes[id] = item.value
			state.EpisodeDone[id] = true
			parent := item.value.IDs.ParentSeries
			if _, known := state.Series[parent]; parent > 0 && !known && !fetchedSeries[parent] {
				seriesToFetch[parent] = struct{}{}
			}
		}
		if len(seriesToFetch) == 0 {
			break
		}
	}
	state.SeriesIDs = sortedSet(seriesSetOf(state))
	state.GroupIDs = sortedSet(groupSetOf(state))
	return nil
}

// refreshSeries re-reads each series with its episodes and returns the groups
// to re-read: the series' old and new parent groups.
func (e *Engine) refreshSeries(ctx context.Context, state *crawlState, ids map[int]struct{}, fetched map[int]bool) (map[int]struct{}, error) {
	groups := make(map[int]struct{})
	pending := make([]int, 0, len(ids))
	for id := range ids {
		if !fetched[id] {
			pending = append(pending, id)
		}
	}
	sort.Ints(pending)
	type seriesData struct {
		series   shoko.Series
		episodes []shoko.Episode
	}
	results, err := fetchAll(ctx, e.concurrency, pending, func(ctx context.Context, id int) (seriesData, error) {
		series, err := e.client.Series(ctx, id)
		if err != nil {
			return seriesData{}, err
		}
		episodes, err := e.client.Episodes(ctx, id)
		return seriesData{series: series, episodes: episodes}, err
	})
	if err != nil {
		return nil, err
	}
	for _, id := range sortedKeys(results) {
		item := results[id]
		fetched[id] = true
		if old, ok := state.Series[id]; ok && old.IDs.ParentGroup > 0 {
			groups[old.IDs.ParentGroup] = struct{}{}
		}
		for groupID, members := range state.GroupSeries {
			if containsID(members, id) {
				groups[groupID] = struct{}{}
			}
		}
		if item.missing {
			delete(state.Series, id)
			delete(state.SeriesDone, id)
			continue
		}
		state.Series[id] = item.value.series
		state.SeriesDone[id] = true
		for _, episode := range item.value.episodes {
			if episode.IDs.ID > 0 {
				state.Episodes[episode.IDs.ID] = episode
				state.EpisodeDone[episode.IDs.ID] = true
			}
		}
		if parent := item.value.series.IDs.ParentGroup; parent > 0 {
			groups[parent] = struct{}{}
		}
	}
	return groups, nil
}

func (e *Engine) refreshGroups(ctx context.Context, state *crawlState, ids map[int]struct{}) error {
	pending := make([]int, 0, len(ids))
	for id := range ids {
		pending = append(pending, id)
	}
	sort.Ints(pending)
	type groupData struct {
		group  shoko.Group
		series []shoko.Series
	}
	results, err := fetchAll(ctx, e.concurrency, pending, func(ctx context.Context, id int) (groupData, error) {
		group, err := e.client.Group(ctx, id)
		if err != nil {
			return groupData{}, err
		}
		series, err := e.client.GroupSeries(ctx, id)
		return groupData{group: group, series: series}, err
	})
	if err != nil {
		return err
	}
	for _, id := range sortedKeys(results) {
		item := results[id]
		if item.missing {
			delete(state.Groups, id)
			delete(state.GroupSeries, id)
			delete(state.GroupDone, id)
			continue
		}
		state.Groups[id] = item.value.group
		state.GroupDone[id] = true
		// Keep Shoko's order: it decides the group's season numbers, so
		// appendUnique, which sorts, must not be used here.
		members := make([]int, 0, len(item.value.series))
		for _, series := range item.value.series {
			if series.IDs.ID <= 0 {
				continue
			}
			state.Series[series.IDs.ID] = series
			if !containsID(members, series.IDs.ID) {
				members = append(members, series.IDs.ID)
			}
		}
		state.GroupSeries[id] = members
	}
	return nil
}

func (e *Engine) inMappedFolder(file shoko.File) bool {
	for _, location := range file.Locations {
		if _, ok := e.managedFolderMap[location.ManagedFolderID]; ok {
			return true
		}
	}
	return false
}

func fileSeriesIDs(file shoko.File) map[int]struct{} {
	ids := make(map[int]struct{})
	for _, reference := range file.SeriesIDs {
		if reference.SeriesID.ID != nil && *reference.SeriesID.ID > 0 {
			ids[*reference.SeriesID.ID] = struct{}{}
		}
	}
	return ids
}

func missingEpisodes(state *crawlState) []int {
	missing := make(map[int]struct{})
	for _, file := range state.Files {
		for _, reference := range file.SeriesIDs {
			for _, episode := range reference.EpisodeIDs {
				if episode.ID == nil || *episode.ID <= 0 {
					continue
				}
				if _, ok := state.Episodes[*episode.ID]; !ok {
					missing[*episode.ID] = struct{}{}
				}
			}
		}
	}
	return sortedSet(missing)
}

func seriesSetOf(state *crawlState) map[int]struct{} {
	set := make(map[int]struct{}, len(state.Series))
	for id := range state.Series {
		set[id] = struct{}{}
	}
	return set
}

func groupSetOf(state *crawlState) map[int]struct{} {
	set := make(map[int]struct{}, len(state.Groups))
	for id := range state.Groups {
		set[id] = struct{}{}
	}
	return set
}

func stateSnapshot(state *crawlState) shoko.Snapshot {
	return shoko.Snapshot{
		ManagedFolders: cloneFolders(state.ManagedFolders),
		Files:          append([]shoko.File(nil), state.Files...),
		Series:         cloneSeries(state.Series),
		Groups:         cloneGroups(state.Groups),
		GroupSeries:    cloneGroupSeries(state.GroupSeries),
		Episodes:       cloneEpisodes(state.Episodes),
	}
}

func positiveIDs(ids []int) []int {
	set := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			set[id] = struct{}{}
		}
	}
	return sortedSet(set)
}

func sortedKeys[T any](values map[int]T) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

// fetched is one ID lookup. missing means Shoko answered 404.
type fetched[T any] struct {
	value   T
	missing bool
}

// fetchAll looks up ids with at most limit requests in flight. A 404 is a
// result, not an error; any other error fails the whole call.
func fetchAll[T any](ctx context.Context, limit int, ids []int, get func(context.Context, int) (T, error)) (map[int]fetched[T], error) {
	results := make(map[int]fetched[T], len(ids))
	if len(ids) == 0 {
		return results, nil
	}
	if limit < 1 {
		limit = 1
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	slots := make(chan struct{}, limit)
	for _, id := range ids {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() { <-slots }()
			value, err := get(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case shoko.IsNotFound(err):
				results[id] = fetched[T]{missing: true}
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			default:
				results[id] = fetched[T]{value: value}
			}
		}(id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// Plan returns the VFS plan for a snapshot, as a commit would build it.
func (e *Engine) Plan(snapshot shoko.Snapshot) (topology.Plan, error) {
	return topology.Build(snapshot, topology.Policy{
		Mode:             topology.ModeAniDB,
		VFSRoot:          e.reconciler.Root(),
		ManagedFolderMap: e.managedFolderMap,
	})
}
