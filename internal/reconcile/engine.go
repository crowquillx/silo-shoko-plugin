// Package reconcile provides the durable, bounded crawl used by both the
// short scheduled-task lane and Silo's longer scan-source polling lane.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

const (
	shokoPageSize      = 1000
	defaultConcurrency = 8
)

// stateSaveInterval is how often a crawl in progress is written to disk. The
// state holds every file read so far, so writing it after each small batch of
// series made a large library's crawl mostly disk writes. A crawl that is
// stopped loses at most this much work.
var stateSaveInterval = 5 * time.Second

type Engine struct {
	client           *shoko.Client
	reconciler       *vfs.Reconciler
	managedFolderMap map[int]string
	folderIDs        []int
	fingerprint      string
	concurrency      int

	// viewMu guards view, the snapshot file as last read with its plan, and is
	// held while the file is read so concurrent callers share one read.
	viewMu    sync.Mutex
	view      *View
	viewStamp fileStamp
	// applyMu guards applyState, the snapshot Apply last saved, kept so the
	// next Apply need not read the file again.
	applyMu    sync.Mutex
	applyState *crawlState
	applyStamp fileStamp
}

// Config contains the already validated runtime objects. The engine never
// writes to Shoko; it only performs GET requests and writes the configured VFS.
type Config struct {
	Client           *shoko.Client
	Reconciler       *vfs.Reconciler
	ManagedFolderMap map[int]string
	Concurrency      int
}

type Outcome struct {
	Complete        bool
	Phase           string
	FilesFetched    int
	SeriesFetched   int
	EpisodesFetched int
	Plan            topology.Plan
	Result          vfs.Result
}

func New(cfg Config) (*Engine, error) {
	if cfg.Client == nil || cfg.Reconciler == nil {
		return nil, errors.New("reconcile: client and reconciler are required")
	}
	if len(cfg.ManagedFolderMap) == 0 {
		return nil, errors.New("reconcile: managed-folder map is required")
	}
	folderIDs := make([]int, 0, len(cfg.ManagedFolderMap))
	for id := range cfg.ManagedFolderMap {
		if id < 1 {
			return nil, fmt.Errorf("reconcile: invalid managed-folder ID %d", id)
		}
		folderIDs = append(folderIDs, id)
	}
	sort.Ints(folderIDs)
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}
	fingerprintData, err := json.Marshal(struct {
		Root    string         `json:"root"`
		Mapping map[int]string `json:"mapping"`
	}{Root: cfg.Reconciler.Root(), Mapping: cfg.ManagedFolderMap})
	if err != nil {
		return nil, fmt.Errorf("reconcile: fingerprint config: %w", err)
	}
	digest := sha256.Sum256(fingerprintData)
	return &Engine{
		client:           cfg.Client,
		reconciler:       cfg.Reconciler,
		managedFolderMap: cloneMap(cfg.ManagedFolderMap),
		folderIDs:        folderIDs,
		fingerprint:      hex.EncodeToString(digest[:]),
		concurrency:      concurrency,
	}, nil
}

// Pending reports whether a crawl has been started but not yet committed.
// It is deliberately read under the same persistent lock as reconciliation.
func (e *Engine) Pending(ctx context.Context) (bool, error) {
	if e == nil || e.reconciler == nil {
		return false, errors.New("reconcile: engine is not configured")
	}
	var pending bool
	err := e.reconciler.WithExclusive(ctx, func(locked *vfs.Reconciler) error {
		_, err := os.Lstat(vfs.CrawlStatePath(locked.Root()))
		switch {
		case err == nil:
			pending = true
			return nil
		case os.IsNotExist(err):
			return nil
		default:
			return fmt.Errorf("reconcile: inspect pending state: %w", err)
		}
	})
	return pending, err
}

// Step advances a crawl until it commits a reconciliation or the supplied
// budget expires. A budget expiry is a successful incomplete outcome: state
// remains on disk for the next scheduled task or PollChanges call. A zero
// budget means run until completion (the bootstrap command uses this mode).
func (e *Engine) Step(ctx context.Context, budget time.Duration) (Outcome, error) {
	if e == nil || e.client == nil || e.reconciler == nil {
		return Outcome{}, errors.New("reconcile: engine is not configured")
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	// Creating the root before entering WithExclusive closes the only race in
	// which two fresh plugin processes would both observe a missing directory
	// and therefore have no lock file to acquire. The state writes themselves
	// remain fully serialized by WithExclusive.
	if err := e.reconciler.EnsureRoot(); err != nil {
		return Outcome{}, err
	}
	stepCtx := ctx
	var cancel context.CancelFunc
	if budget > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	var outcome Outcome
	err := e.reconciler.WithExclusive(stepCtx, func(locked *vfs.Reconciler) error {
		if err := locked.EnsureRootLocked(); err != nil {
			return err
		}
		state, err := loadState(vfs.CrawlStatePath(locked.Root()))
		if errors.Is(err, errStaleState) {
			state = nil
			err = nil
		}
		if err != nil {
			return err
		}
		if state == nil || state.Fingerprint != e.fingerprint {
			state = e.newState()
			if err := saveState(vfs.CrawlStatePath(locked.Root()), state); err != nil {
				return err
			}
		}
		statePath := vfs.CrawlStatePath(locked.Root())
		lastSave, dirty := time.Now(), false
		persist := func() error {
			if !dirty {
				return nil
			}
			if err := saveState(statePath, state); err != nil {
				return err
			}
			lastSave, dirty = time.Now(), false
			return nil
		}
		for {
			if stepCtx.Err() != nil {
				outcome = e.progress(state)
				return persist()
			}
			var fetch func(context.Context, *crawlState) error
			switch state.Phase {
			case phaseFolders:
				fetch = e.fetchFolders
			case phaseFiles:
				fetch = e.fetchFilePage
			case phaseSeries:
				fetch = e.fetchSeries
			case phaseGroups:
				fetch = e.fetchGroups
			case phaseEpisodes:
				fetch = e.fetchEpisodes
			case phaseReady:
				// Do not begin a large filesystem commit at the tail end of a
				// scheduled-task budget. The commit does not stop at the budget,
				// so the background crawl, which has no deadline, picks it up.
				if deadline, ok := stepCtx.Deadline(); ok && time.Until(deadline) < 1500*time.Millisecond {
					outcome = e.progress(state)
					return nil
				}
				// The commit runs to the end on the caller's context, not the
				// step's: it can take minutes on a slow share, and work
				// after the links, such as the theme sync, stops at a
				// deadline, which would leave it undone every time.
				plan, result, err := e.commit(ctx, locked, state, false)
				if err != nil {
					if isBudgetExpiry(stepCtx, ctx, err) {
						outcome = e.progress(state)
						return nil
					}
					// Drop the snapshot so the next run re-crawls current data
					// instead of re-committing the same failed plan on every poll.
					if clearErr := clearState(vfs.CrawlStatePath(locked.Root())); clearErr != nil {
						return errors.Join(err, clearErr)
					}
					return err
				}
				// The finished crawl becomes the snapshot that later changes
				// from Shoko are applied to. Its file is already on disk in its
				// final phase, so moving it is enough.
				if err := os.Rename(vfs.CrawlStatePath(locked.Root()), vfs.SnapshotPath(locked.Root())); err != nil {
					return fmt.Errorf("reconcile: keep crawl snapshot: %w", err)
				}
				// The file was last written before the commit, which can take
				// minutes; its time is when the snapshot was committed.
				now := time.Now()
				if err := os.Chtimes(vfs.SnapshotPath(locked.Root()), now, now); err != nil {
					return fmt.Errorf("reconcile: date crawl snapshot: %w", err)
				}
				e.rememberView(locked, state, plan)
				outcome = e.progress(state)
				outcome.Complete = true
				outcome.Plan = plan
				outcome.Result = result
				return nil
			default:
				return fmt.Errorf("reconcile: unsupported crawl phase %q", state.Phase)
			}
			before := state.Phase
			err := fetch(stepCtx, state)
			// A failed batch still records the lookups in it that succeeded.
			dirty = true
			if err != nil {
				saveErr := persist()
				if isBudgetExpiry(stepCtx, ctx, err) {
					outcome = e.progress(state)
					return saveErr
				}
				return err
			}
			// The commit moves the state file, so it is always written on
			// reaching a new phase.
			if state.Phase != before || time.Since(lastSave) >= stateSaveInterval {
				if err := persist(); err != nil {
					return err
				}
			}
		}
	})
	if err != nil {
		if isBudgetExpiry(stepCtx, ctx, err) {
			return outcome, nil
		}
		return outcome, err
	}
	return outcome, nil
}

func (e *Engine) newState() *crawlState {
	nextPages := make(map[int]int, len(e.folderIDs))
	for _, id := range e.folderIDs {
		nextPages[id] = 1
	}
	return &crawlState{
		Version:        stateVersion,
		Fingerprint:    e.fingerprint,
		Phase:          phaseFolders,
		FolderIDs:      append([]int(nil), e.folderIDs...),
		ManagedFolders: make(map[int]shoko.ManagedFolder),
		NextFilePage:   nextPages,
		Files:          make([]shoko.File, 0),
		SeriesDone:     make(map[int]bool),
		Series:         make(map[int]shoko.Series),
		GroupDone:      make(map[int]bool),
		Groups:         make(map[int]shoko.Group),
		GroupSeries:    make(map[int][]int),
		EpisodeDone:    make(map[int]bool),
		Episodes:       make(map[int]shoko.Episode),
	}
}

func (e *Engine) fetchFolders(ctx context.Context, state *crawlState) error {
	folders, err := e.client.ManagedFolders(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int]shoko.ManagedFolder, len(folders))
	for _, folder := range folders {
		byID[folder.ID] = folder
	}
	for _, id := range e.folderIDs {
		folder, ok := byID[id]
		if !ok {
			return fmt.Errorf("reconcile: configured managed folder %d was not returned by Shoko", id)
		}
		state.ManagedFolders[id] = folder
	}
	state.Phase = phaseFiles
	state.FolderIndex = 0
	return nil
}

func (e *Engine) fetchFilePage(ctx context.Context, state *crawlState) error {
	if state.FolderIndex >= len(state.FolderIDs) {
		e.prepareGraph(state)
		return nil
	}
	folderID := state.FolderIDs[state.FolderIndex]
	page := state.NextFilePage[folderID]
	if page < 1 {
		page = 1
	}
	files, total, err := e.client.FilesForManagedFolderPage(ctx, folderID, page)
	if err != nil {
		return err
	}
	state.Files = append(state.Files, files...)
	if state.FolderFiles == nil {
		state.FolderFiles = make(map[int]int)
	}
	state.FolderFiles[folderID] += len(files)
	// Shoko's total decides when a folder is done. A short page only ends it
	// when there is no total, since a server may cap pages below the size
	// asked for.
	done := len(files) == 0
	if total > 0 {
		done = done || state.FolderFiles[folderID] >= total
	} else {
		done = done || len(files) < shokoPageSize
	}
	if done {
		state.FolderIndex++
		state.NextFilePage[folderID] = 0
	} else {
		state.NextFilePage[folderID] = page + 1
	}
	if state.FolderIndex >= len(state.FolderIDs) {
		e.prepareGraph(state)
	}
	return nil
}

func (e *Engine) prepareGraph(state *crawlState) {
	seriesSet := make(map[int]struct{})
	episodeSet := make(map[int]struct{})
	for _, file := range state.Files {
		for _, reference := range file.SeriesIDs {
			if reference.SeriesID.ID != nil && *reference.SeriesID.ID > 0 {
				seriesSet[*reference.SeriesID.ID] = struct{}{}
			}
			for _, episodeReference := range reference.EpisodeIDs {
				if episodeReference.ID != nil && *episodeReference.ID > 0 {
					episodeSet[*episodeReference.ID] = struct{}{}
				}
			}
		}
	}
	state.SeriesIDs = sortedSet(seriesSet)
	state.EpisodeIDs = sortedSet(episodeSet)
	state.SeriesDone = make(map[int]bool, len(state.SeriesIDs))
	state.EpisodeDone = make(map[int]bool, len(state.EpisodeIDs))
	state.Phase = phaseSeries
}

type seriesFetch struct {
	id       int
	series   shoko.Series
	episodes []shoko.Episode
	err      error
}

func (e *Engine) fetchSeries(ctx context.Context, state *crawlState) error {
	pending := make([]int, 0)
	for _, id := range state.SeriesIDs {
		if !state.SeriesDone[id] {
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		e.prepareGroups(state)
		state.Phase = phaseGroups
		return nil
	}
	results := e.fetchSeriesBatch(ctx, pending)
	var firstErr error
	for _, item := range results {
		if item.err != nil {
			if firstErr == nil && !errors.Is(item.err, context.Canceled) && !errors.Is(item.err, context.DeadlineExceeded) {
				firstErr = item.err
			}
			continue
		}
		state.Series[item.id] = item.series
		state.SeriesDone[item.id] = true
		for _, episode := range item.episodes {
			if episode.IDs.ID > 0 {
				state.Episodes[episode.IDs.ID] = episode
				state.EpisodeDone[episode.IDs.ID] = true
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type groupFetch struct {
	id     int
	group  shoko.Group
	series []shoko.Series
	err    error
}

func (e *Engine) prepareGroups(state *crawlState) {
	groupSet := make(map[int]struct{}, len(state.GroupIDs))
	for _, id := range state.GroupIDs {
		if id > 0 {
			groupSet[id] = struct{}{}
		}
	}
	for _, seriesID := range state.SeriesIDs {
		series, ok := state.Series[seriesID]
		if !ok || series.IDs.ParentGroup <= 0 {
			continue
		}
		groupSet[series.IDs.ParentGroup] = struct{}{}
	}
	state.GroupIDs = sortedSet(groupSet)
	if state.GroupDone == nil {
		state.GroupDone = make(map[int]bool, len(state.GroupIDs))
	}
	for _, id := range state.GroupIDs {
		if _, ok := state.GroupDone[id]; !ok {
			state.GroupDone[id] = false
		}
	}
}

func (e *Engine) fetchGroups(ctx context.Context, state *crawlState) error {
	pending := make([]int, 0)
	for _, id := range state.GroupIDs {
		if !state.GroupDone[id] {
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		for _, id := range state.EpisodeIDs {
			if _, ok := state.Episodes[id]; ok {
				state.EpisodeDone[id] = true
			}
		}
		state.Phase = phaseEpisodes
		return nil
	}
	results := e.fetchGroupBatch(ctx, pending)
	var firstErr error
	for _, item := range results {
		if item.err != nil {
			if firstErr == nil && !errors.Is(item.err, context.Canceled) && !errors.Is(item.err, context.DeadlineExceeded) {
				firstErr = item.err
			}
			continue
		}
		state.Groups[item.id] = item.group
		// Keep Shoko's order: it decides the group's season numbers.
		memberIDs := make([]int, 0, len(item.series))
		for _, series := range item.series {
			if series.IDs.ID <= 0 {
				continue
			}
			state.Series[series.IDs.ID] = series
			if !containsID(memberIDs, series.IDs.ID) {
				memberIDs = append(memberIDs, series.IDs.ID)
			}
		}
		state.GroupSeries[item.id] = memberIDs
		state.GroupDone[item.id] = true
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (e *Engine) fetchGroupBatch(ctx context.Context, ids []int) []groupFetch {
	if len(ids) > e.concurrency {
		ids = ids[:e.concurrency]
	}
	results := make(chan groupFetch, len(ids))
	for _, id := range ids {
		go func(id int) {
			group, err := e.client.Group(ctx, id)
			if err == nil {
				var series []shoko.Series
				series, err = e.client.GroupSeries(ctx, id)
				results <- groupFetch{id: id, group: group, series: series, err: err}
				return
			}
			results <- groupFetch{id: id, err: err}
		}(id)
	}
	resultList := make([]groupFetch, 0, len(ids))
	for range ids {
		resultList = append(resultList, <-results)
	}
	sort.Slice(resultList, func(i, j int) bool { return resultList[i].id < resultList[j].id })
	return resultList
}

func (e *Engine) fetchSeriesBatch(ctx context.Context, ids []int) []seriesFetch {
	if len(ids) > e.concurrency {
		ids = ids[:e.concurrency]
	}
	results := make(chan seriesFetch, len(ids))
	for _, id := range ids {
		go func(id int) {
			series, err := e.client.Series(ctx, id)
			if err == nil {
				var episodes []shoko.Episode
				episodes, err = e.client.Episodes(ctx, id)
				results <- seriesFetch{id: id, series: series, episodes: episodes, err: err}
				return
			}
			results <- seriesFetch{id: id, err: err}
		}(id)
	}
	resultList := make([]seriesFetch, 0, len(ids))
	for range ids {
		resultList = append(resultList, <-results)
	}
	sort.Slice(resultList, func(i, j int) bool { return resultList[i].id < resultList[j].id })
	return resultList
}

type episodeFetch struct {
	id      int
	episode shoko.Episode
	err     error
}

func (e *Engine) fetchEpisodes(ctx context.Context, state *crawlState) error {
	pending := make([]int, 0)
	for _, id := range state.EpisodeIDs {
		if !state.EpisodeDone[id] {
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		state.Phase = phaseReady
		return nil
	}
	if len(pending) > e.concurrency {
		pending = pending[:e.concurrency]
	}
	results := make(chan episodeFetch, len(pending))
	for _, id := range pending {
		go func(id int) {
			episode, err := e.client.Episode(ctx, id)
			results <- episodeFetch{id: id, episode: episode, err: err}
		}(id)
	}
	resultList := make([]episodeFetch, 0, len(pending))
	for range pending {
		resultList = append(resultList, <-results)
	}
	sort.Slice(resultList, func(i, j int) bool { return resultList[i].id < resultList[j].id })
	var firstErr error
	for _, item := range resultList {
		if item.err != nil {
			if firstErr == nil && !errors.Is(item.err, context.Canceled) && !errors.Is(item.err, context.DeadlineExceeded) {
				firstErr = item.err
			}
			continue
		}
		state.Episodes[item.id] = item.episode
		state.EpisodeDone[item.id] = true
		parent := item.episode.IDs.ParentSeries
		if parent > 0 && !containsID(state.SeriesIDs, parent) {
			state.SeriesIDs = appendUnique(state.SeriesIDs, parent)
			state.SeriesDone[parent] = false
			state.Phase = phaseSeries
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// Stay in this phase while episodes remain; the empty-pending check above
	// advances to phaseReady once every batch has been fetched.
	return ctx.Err()
}

// commit plans the VFS from state and reconciles it. A full commit checks
// every link and syncs every theme folder; an incremental one, for changes
// Shoko reported, checks only links that change and syncs themes only in the
// show and season folders they are in.
func (e *Engine) commit(ctx context.Context, locked *vfs.Reconciler, state *crawlState, incremental bool) (topology.Plan, vfs.Result, error) {
	snapshot := shoko.Snapshot{
		ManagedFolders: cloneFolders(state.ManagedFolders),
		Files:          append([]shoko.File(nil), state.Files...),
		Series:         cloneSeries(state.Series),
		Groups:         cloneGroups(state.Groups),
		GroupSeries:    cloneGroupSeries(state.GroupSeries),
		Episodes:       cloneEpisodes(state.Episodes),
	}
	plan, err := topology.Build(snapshot, topology.Policy{
		Mode:             topology.ModeAniDB,
		VFSRoot:          locked.Root(),
		ManagedFolderMap: e.managedFolderMap,
	})
	if err != nil {
		return topology.Plan{}, vfs.Result{}, err
	}
	result, err := locked.ReconcileLockedWith(ctx, plan, false, vfs.ReconcileOptions{TrustUnchanged: incremental})
	if err != nil {
		return plan, result, err
	}
	if afterReconcileHook != nil {
		afterReconcileHook()
	}
	var only map[string]bool
	if incremental {
		only = changedFolders(result)
	}
	// Theme songs follow the new links. A theme problem never fails the
	// reconcile itself; it is reported with the result.
	themes, err := locked.SyncThemesLockedIn(ctx, &plan.ThemePairs, only)
	if err != nil {
		themes.Failures = append(themes.Failures, vfs.Failure{Reason: "theme sync: " + err.Error()})
	}
	result.Themes = &themes
	return plan, result, nil
}

// afterReconcileHook lets tests act between a commit's reconcile and its theme
// sync.
var afterReconcileHook func()

// changedFolders returns the show and season folders of the links a
// reconcile created, updated or removed, as theme pairs name them.
func changedFolders(result vfs.Result) map[string]bool {
	folders := make(map[string]bool)
	for _, action := range result.Actions {
		if action.Kind == "unchanged" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(action.LogicalPath))
		for dir != "." && dir != "/" && dir != "" {
			folders[dir] = true
			dir = path.Dir(dir)
		}
	}
	return folders
}

func (e *Engine) progress(state *crawlState) Outcome {
	return Outcome{
		Complete:        false,
		Phase:           string(state.Phase),
		FilesFetched:    len(state.Files),
		SeriesFetched:   countDone(state.SeriesDone),
		EpisodesFetched: countDone(state.EpisodeDone),
	}
}

func isBudgetExpiry(stepCtx, parent context.Context, err error) bool {
	if err == nil || stepCtx.Err() == nil || parent.Err() != nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func sortedSet(values map[int]struct{}) []int {
	result := make([]int, 0, len(values))
	for id := range values {
		result = append(result, id)
	}
	sort.Ints(result)
	return result
}

func appendUnique(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	sort.Ints(values)
	return values
}
func containsID(values []int, value int) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func countDone(values map[int]bool) int {
	count := 0
	for _, done := range values {
		if done {
			count++
		}
	}
	return count
}

func cloneMap(values map[int]string) map[int]string {
	result := make(map[int]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneFolders(values map[int]shoko.ManagedFolder) map[int]shoko.ManagedFolder {
	result := make(map[int]shoko.ManagedFolder, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneSeries(values map[int]shoko.Series) map[int]shoko.Series {
	result := make(map[int]shoko.Series, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func cloneGroups(values map[int]shoko.Group) map[int]shoko.Group {
	result := make(map[int]shoko.Group, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneGroupSeries(values map[int][]int) map[int][]int {
	result := make(map[int][]int, len(values))
	for key, value := range values {
		result[key] = append([]int(nil), value...)
	}
	return result
}

func cloneEpisodes(values map[int]shoko.Episode) map[int]shoko.Episode {
	result := make(map[int]shoko.Episode, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
