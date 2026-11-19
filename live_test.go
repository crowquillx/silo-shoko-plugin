package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/live"
	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/silocatalog"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

func TestMain(m *testing.M) {
	// Configure would otherwise connect every test's fake Shoko to the
	// event hub.
	liveUpdatesEnabled = false
	os.Exit(m.Run())
}

// liveFixture is a one-show library in a fake Shoko, a fake Silo that knows
// the show, and the live updates for them.
type liveFixture struct {
	updates *liveUpdates
	runtime *runtimeServer
	root    string

	mu        sync.Mutex
	refreshed []string
	lookups   []string
	posters   string
	// noRoots makes Silo list no roots, as for a show an autoscan added.
	noRoots bool
	// group is the Shoko group that holds series 7. Changing it regroups the
	// series, as Shoko does when it merges groups.
	group int
	// patched records the episode IDs Silo was asked to clear, by episode.
	patched []string
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	fixture := &liveFixture{posters: `{"shows":[]}`, group: 3}
	sourceRoot := filepath.Join(t.TempDir(), "source")
	fixture.root = filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	shokoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"a","path":"/x"}]`)
		case "/api/v3/ManagedFolder/1/File":
			fmt.Fprint(w, `{"total":1,"list":[{"id":9,"locations":[{"managedFolderId":1,"relativePath":"a.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":7},"episodeIds":[{"id":42}]}]}]}`)
		case "/api/v3/Series/7":
			fixture.mu.Lock()
			group := fixture.group
			fixture.mu.Unlock()
			fmt.Fprintf(w, `{"ids":{"id":7,"parentGroup":%d},"name":"Show","aniDB":{"type":"TV","airDate":"2020-01-01"}}`, group)
		case "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":42,"parentSeries":7},"name":"E1","aniDB":{"type":"Normal","episodeNumber":1}}]}`)
		case "/api/v3/Group/3", "/api/v3/Group/5":
			fixture.mu.Lock()
			group := fixture.group
			fixture.mu.Unlock()
			if r.URL.Path != fmt.Sprintf("/api/v3/Group/%d", group) {
				http.NotFound(w, r)
				return
			}
			name := map[int]string{3: "Show", 5: "Franchise"}[group]
			fmt.Fprintf(w, `{"IDs":{"ID":%d},"Name":%q}`, group, name)
		case "/api/v3/Group/3/Series", "/api/v3/Group/5/Series":
			fixture.mu.Lock()
			group := fixture.group
			fixture.mu.Unlock()
			if r.URL.Path != fmt.Sprintf("/api/v3/Group/%d/Series", group) {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"ids":{"id":7,"parentGroup":%d},"name":"Show","aniDB":{"type":"TV","airDate":"2020-01-01"}}]`, group)
		case "/api/plugin/season-posters/show-posters":
			fixture.mu.Lock()
			fmt.Fprint(w, fixture.posters)
			fixture.mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(shokoServer.Close)
	siloServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/api/v2/catalog") && r.Header.Get("X-Profile-Id") != "admin-profile" {
			// Silo requires a profile on its catalog endpoints.
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		switch {
		case r.URL.Path == "/api/v2/profiles":
			fmt.Fprint(w, `{"items":[{"id":"admin-profile"}]}`)
		case r.URL.Path == "/api/v2/libraries":
			fmt.Fprintf(w, `{"items":[{"id":"1","paths":[%q]},{"id":"2","paths":["/elsewhere"]}]}`, fixture.root)
		case r.URL.Path == "/api/v2/libraries/roots":
			query := r.URL.Query().Get("q")
			fixture.lookups = append(fixture.lookups, r.URL.Query().Get("library_id")+" "+query)
			if query == "[Shoko Group=3]" && !fixture.noRoots {
				fmt.Fprintf(w, `{"items":[{"root_path":%q,"sample_file_path":"x [Shoko Group=3] [Shoko Series=7].mkv","content_id":"silo-show"}]}`, filepath.Join(fixture.root, "Show (2020)"))
				return
			}
			fmt.Fprint(w, `{"items":[]}`)
		case r.URL.Path == "/api/v2/catalog":
			fixture.lookups = append(fixture.lookups, "catalog "+r.URL.Query().Get("q"))
			if r.URL.Query().Get("q") == "Show" && r.URL.Query().Get("type") == "series" {
				fmt.Fprint(w, `{"items":[{"content_id":"decoy","type":"series"},{"content_id":"silo-show","type":"series"}]}`)
				return
			}
			fmt.Fprint(w, `{"items":[]}`)
		case r.URL.Path == "/api/v2/catalog/items/decoy":
			fmt.Fprint(w, `{"content_id":"decoy","folder_paths":["/elsewhere/Show (2020)"]}`)
		case r.URL.Path == "/api/v2/catalog/items/silo-show":
			fmt.Fprintf(w, `{"content_id":"silo-show","folder_paths":[%q]}`, filepath.Join(fixture.root, "Show (2020)"))
		case r.URL.Path == "/api/v2/catalog/series/silo-show/seasons":
			fmt.Fprint(w, `{"items":[{"season_number":1}]}`)
		case r.URL.Path == "/api/v2/catalog/series/silo-show/seasons/1/episodes":
			fmt.Fprint(w, `{"items":[{"content_id":"silo-e1","tmdb_id":"1001","tvdb_id":"2001"},{"content_id":"silo-e2"}]}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/metadata"):
			fixture.patched = append(fixture.patched, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/items/"), "/metadata"))
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/refresh-metadata"):
			fixture.refreshed = append(fixture.refreshed, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/items/"), "/refresh-metadata"))
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(siloServer.Close)

	client, err := shoko.NewClient(shoko.Config{BaseURL: shokoServer.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: fixture.root, AllowedSourceRoots: []string{sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := reconcile.New(reconcile.Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = &runtimeServer{engine: engine}
	fixture.updates = &liveUpdates{
		runtime:    fixture.runtime,
		engine:     engine,
		reconciler: reconciler,
		client:     client,
		admin:      silocatalog.NewAdmin(silocatalog.AdminConfig{BaseURL: siloServer.URL, APIKey: "sa_admin"}, nil),
		shows:      make(map[string]string),
		folders:    make(map[string]showFolder),
	}
	fixture.updates.queue = newRefreshQueue(fixture.updates.refreshShow)
	fixture.updates.queue.settle, fixture.updates.queue.maxWait = 0, 0
	fixture.updates.queue.minInterval, fixture.updates.queue.spacing = 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		fixture.updates.queue.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return fixture
}

// waitForRefreshes waits until Silo was asked for count refreshes.
func (f *liveFixture) waitForRefreshes(t *testing.T, count int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		refreshed := slices.Clone(f.refreshed)
		f.mu.Unlock()
		if len(refreshed) >= count {
			return refreshed
		}
		if time.Now().After(deadline) {
			t.Fatalf("refreshed = %v, want %d refreshes", refreshed, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *liveFixture) waitForCrawl(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.runtime.backgroundCrawlRunning() {
		if time.Now().After(deadline) {
			t.Fatal("background crawl did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveUpdatesRefreshTheShowOfAChangedEpisode(t *testing.T) {
	fixture := newLiveFixture(t)
	ctx := context.Background()

	// Nothing is built yet, so connecting starts no crawl and there is no
	// show to refresh.
	fixture.updates.Connected(ctx, false)
	if fixture.runtime.backgroundCrawlRunning() {
		t.Fatal("connecting before the VFS was built started a crawl")
	}
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}

	fixture.updates.Apply(ctx, live.Batch{Refresh: live.Refresh{Episodes: []int{42}, Series: []int{7, 404}}})
	fixture.waitForRefreshes(t, 1)
	fixture.updates.Apply(ctx, live.Batch{Refresh: live.Refresh{Groups: []int{3, 99}}})
	refreshed := fixture.waitForRefreshes(t, 2)
	time.Sleep(50 * time.Millisecond)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !reflect.DeepEqual(fixture.refreshed, []string{"silo-show", "silo-show"}) || len(refreshed) != 2 {
		t.Fatalf("refreshed = %v", fixture.refreshed)
	}
	// The second refresh used the cached show ID, and only the library
	// that scans the VFS was searched.
	if !reflect.DeepEqual(fixture.lookups, []string{"1 [Shoko Group=3]"}) {
		t.Fatalf("lookups = %v", fixture.lookups)
	}
}

// A show an autoscan added is not among Silo's roots; it is found in the
// catalog by title and told apart by its folder.
func TestLiveUpdatesFindAShowSiloHasNoRootFor(t *testing.T) {
	fixture := newLiveFixture(t)
	fixture.noRoots = true
	ctx := context.Background()
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fixture.updates.Apply(ctx, live.Batch{Refresh: live.Refresh{Series: []int{7}}})
	refreshed := fixture.waitForRefreshes(t, 1)
	if !reflect.DeepEqual(refreshed, []string{"silo-show"}) {
		t.Fatalf("refreshed = %v", refreshed)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !reflect.DeepEqual(fixture.lookups, []string{"1 [Shoko Group=3]", "catalog Show"}) {
		t.Fatalf("lookups = %v", fixture.lookups)
	}
}

func TestLiveUpdatesCatchUpWithACrawl(t *testing.T) {
	fixture := newLiveFixture(t)
	ctx := context.Background()
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}

	// Just committed: the first connection needs no crawl.
	fixture.updates.Connected(ctx, false)
	if fixture.runtime.backgroundCrawlRunning() {
		t.Fatal("first connection right after a commit started a crawl")
	}
	// After a dropped connection, events may have been missed.
	fixture.updates.Connected(ctx, true)
	fixture.waitForCrawl(t)
	if err := fixture.runtime.takeBackgroundCrawlError(); err != nil {
		t.Fatal(err)
	}

	// A VFS built before snapshots existed is crawled once to make one.
	if err := os.Remove(vfs.SnapshotPath(fixture.root)); err != nil {
		t.Fatal(err)
	}
	fixture.updates.Apply(ctx, live.Batch{VFS: reconcile.Changes{Files: []int{9}}})
	fixture.waitForCrawl(t)
	if info, err := fixture.updates.engine.Snapshot(ctx); err != nil || !info.Exists {
		t.Fatalf("snapshot after catching up = %#v, %v", info, err)
	}
}

func TestLiveUpdatesRecrawlWhenShokoRecreatesGroups(t *testing.T) {
	fixture := newLiveFixture(t)
	ctx := context.Background()
	// Before the VFS is built there is nothing to recrawl.
	fixture.updates.Apply(ctx, live.Batch{Recrawl: true})
	if fixture.runtime.backgroundCrawlRunning() {
		t.Fatal("recrawl before the VFS was built")
	}
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.updates.engine.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixture.updates.Apply(ctx, live.Batch{Recrawl: true, VFS: reconcile.Changes{Files: []int{9}}})
	fixture.waitForCrawl(t)
	after, err := fixture.updates.engine.Snapshot(ctx)
	if err != nil || !after.CommittedAt.After(before.CommittedAt) {
		t.Fatalf("snapshot after recrawl = %#v (before %#v), %v", after, before, err)
	}
}

// When Shoko moves a series into another group, the old group's show folder
// leaves the VFS and the series appears in another show. The Silo show for the
// old group gives up its episodes' external IDs, so the show that now holds the
// files can take them.
func TestLiveUpdatesReleaseTheIDsOfAShowWhoseSeriesMoved(t *testing.T) {
	fixture := newLiveFixture(t)
	fixture.updates.status = &liveStatus{startedAt: time.Now()}
	ctx := context.Background()
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}

	// A change that keeps every show releases nothing.
	fixture.updates.Apply(ctx, live.Batch{VFS: reconcile.Changes{Series: []int{7}}})
	fixture.mu.Lock()
	if len(fixture.patched) != 0 {
		t.Fatalf("patched = %v after a change that removed no show", fixture.patched)
	}
	fixture.group = 5
	fixture.mu.Unlock()

	fixture.updates.Apply(ctx, live.Batch{VFS: reconcile.Changes{Series: []int{7}, Groups: []int{3, 5}}})
	shows, err := fixture.updates.describeShows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if shows.bySeries[7] != "[Shoko Group=5]" {
		t.Fatalf("series 7 is in %q after the regroup", shows.bySeries[7])
	}
	fixture.mu.Lock()
	patched := slices.Clone(fixture.patched)
	fixture.mu.Unlock()
	// Only the episode that had IDs is edited.
	if !reflect.DeepEqual(patched, []string{"silo-e1"}) {
		t.Fatalf("patched = %v", patched)
	}
	report := fixture.updates.status.report(0)
	if report["retired_shows"] != 1 || report["released_episodes"] != 1 || report["last_retire_show"] != "[Shoko Group=3]" {
		t.Fatalf("status = retired %v, released %v, show %v", report["retired_shows"], report["released_episodes"], report["last_retire_show"])
	}
}

func TestRegroupedShowsNamesOnlyShowsWhoseSeriesMoved(t *testing.T) {
	folder := func(path string) showFolder { return showFolder{path: path} }
	before := vfsShows{
		bySeries: map[int]string{1: "[Shoko Group=1]", 2: "[Shoko Group=2]", 3: "[Shoko Group=3]", 4: "[Shoko Group=4]"},
		folders: map[string]showFolder{
			"[Shoko Group=1]": folder("/vfs/A (2001)"),
			"[Shoko Group=2]": folder("/vfs/B (2002)"),
			"[Shoko Group=3]": folder("/vfs/C (2003)"),
			"[Shoko Group=4]": folder("/vfs/D (2004)"),
		},
	}
	after := vfsShows{
		// Series 1 moved into group 2. Group 3's only file was replaced, so its
		// series is gone for now. Group 4 was renumbered but kept its folder.
		bySeries: map[int]string{1: "[Shoko Group=2]", 2: "[Shoko Group=2]", 4: "[Shoko Group=9]"},
		folders: map[string]showFolder{
			"[Shoko Group=2]": folder("/vfs/B (2002)"),
			"[Shoko Group=9]": folder("/vfs/D (2004)"),
		},
	}
	if got := regroupedShows(before, after); !reflect.DeepEqual(got, []string{"[Shoko Group=1]"}) {
		t.Fatalf("regroupedShows = %v", got)
	}
}

func TestChangedGroups(t *testing.T) {
	before := map[int]string{1: "a", 2: "b", 3: "c"}
	after := map[int]string{1: "a", 2: "x", 4: "d"}
	if got := changedGroups(before, after); !reflect.DeepEqual(got, []int{2, 3, 4}) {
		t.Fatalf("changedGroups = %v", got)
	}
}

// A show whose specials poster alone changed is refreshed too.
func TestChangedGroupsNoticesSpecialsPosters(t *testing.T) {
	before := map[int]shoko.ShowPoster{1: {Show: "a"}, 2: {Show: "b", Specials: "s"}}
	after := map[int]shoko.ShowPoster{1: {Show: "a", Specials: "t"}, 2: {Show: "b", Specials: "s"}}
	if got := changedGroups(before, after); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("changedGroups = %v", got)
	}
}

func TestMovedSeriesNamesOnlySeriesWhoseLinksMoved(t *testing.T) {
	outcome := reconcile.Outcome{
		Plan: topology.Plan{Entries: []topology.Entry{
			{LogicalPath: "Show/Season 02/a.mkv", ShokoSeriesID: 7},
			{LogicalPath: "Show/Season 02/b.mkv", ShokoSeriesID: 7},
			{LogicalPath: "Show/Season 01/c.mkv", ShokoSeriesID: 6},
			{LogicalPath: "Other/Season 01/d.mkv", ShokoSeriesID: 5},
		}},
		Result: vfs.Result{Actions: []vfs.Action{
			{Kind: "remove", LogicalPath: "Show/Season 01/a.mkv", Reason: vfs.ReasonMoved},
			{Kind: "update", LogicalPath: "Show/Season 02/a.mkv", Reason: vfs.ReasonMoved},
			{Kind: "update", LogicalPath: "Show/Season 02/b.mkv", Reason: vfs.ReasonMoved},
			// A new file and a file whose target changed move nothing.
			{Kind: "create", LogicalPath: "Show/Season 01/c.mkv"},
			{Kind: "update", LogicalPath: "Other/Season 01/d.mkv"},
		}},
	}
	if got := movedSeries(outcome); !reflect.DeepEqual(got, []int{7}) {
		t.Fatalf("movedSeries = %v, want [7]", got)
	}
}

// Changes that cannot be applied come back to be tried again, and after
// enough failures a full crawl takes over.
func TestLiveUpdatesRetryChangesThatCouldNotBeApplied(t *testing.T) {
	fixture := newLiveFixture(t)
	ctx := context.Background()
	if _, err := fixture.updates.engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// An unreadable snapshot fails every apply.
	if err := os.WriteFile(vfs.SnapshotPath(fixture.root), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	batch := live.Batch{VFS: reconcile.Changes{Files: []int{9}}}
	for attempt := 1; attempt < maxApplyFailures; attempt++ {
		retry := fixture.updates.Apply(ctx, batch)
		if !reflect.DeepEqual(retry, batch) {
			t.Fatalf("failed apply %d returned %#v to retry, want the batch", attempt, retry)
		}
		if fixture.runtime.backgroundCrawlRunning() {
			t.Fatalf("failed apply %d started a crawl", attempt)
		}
	}
	if retry := fixture.updates.Apply(ctx, batch); !retry.Empty() {
		t.Fatalf("the last failed apply still returned %#v to retry", retry)
	}
	fixture.waitForCrawl(t)
	if err := fixture.runtime.takeBackgroundCrawlError(); err != nil {
		t.Fatal(err)
	}
	// The crawl left a good snapshot, so the next batch applies.
	if retry := fixture.updates.Apply(ctx, batch); !retry.Empty() {
		t.Fatalf("apply after the crawl returned %#v to retry", retry)
	}
}

func TestInBuiltOrderKeepsTheSeasonsTheVFSHas(t *testing.T) {
	series := func(id int, name string) shoko.Series {
		return shoko.Series{IDs: shoko.IDs{ID: id}, Name: name}
	}
	// Shoko already lists a new first season and no longer has series 8; the
	// VFS was built with 7 then 8.
	current := []shoko.Series{series(6, "new"), series(7, "seven now")}
	built := []shoko.Series{series(7, "seven then"), series(8, "eight then")}
	got := inBuiltOrder(current, built)
	want := []shoko.Series{series(7, "seven now"), series(8, "eight then")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inBuiltOrder = %#v, want %#v", got, want)
	}
}
