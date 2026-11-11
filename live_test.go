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
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	fixture := &liveFixture{posters: `{"shows":[]}`}
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
			fmt.Fprint(w, `{"ids":{"id":7,"parentGroup":3},"name":"Show","aniDB":{"type":"TV","airDate":"2020-01-01"}}`)
		case "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":42,"parentSeries":7},"name":"E1","aniDB":{"type":"Normal","episodeNumber":1}}]}`)
		case "/api/v3/Group/3":
			fmt.Fprint(w, `{"IDs":{"ID":3},"Name":"Show"}`)
		case "/api/v3/Group/3/Series":
			fmt.Fprint(w, `[{"ids":{"id":7,"parentGroup":3},"name":"Show","aniDB":{"type":"TV","airDate":"2020-01-01"}}]`)
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
		switch {
		case r.URL.Path == "/api/v2/libraries":
			fmt.Fprintf(w, `{"items":[{"id":"1","paths":[%q]},{"id":"2","paths":["/elsewhere"]}]}`, fixture.root)
		case r.URL.Path == "/api/v2/libraries/roots":
			query := r.URL.Query().Get("q")
			fixture.lookups = append(fixture.lookups, r.URL.Query().Get("library_id")+" "+query)
			if query == "[Shoko Group=3]" {
				fmt.Fprintf(w, `{"items":[{"root_path":%q,"sample_file_path":"x [Shoko Group=3] [Shoko Series=7].mkv","content_id":"silo-show"}]}`, filepath.Join(fixture.root, "Show (2020)"))
				return
			}
			fmt.Fprint(w, `{"items":[]}`)
		case strings.HasSuffix(r.URL.Path, "/refresh-metadata"):
			fixture.refreshed = append(fixture.refreshed, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/items/"), "/refresh-metadata"))
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

func TestChangedGroups(t *testing.T) {
	before := map[int]string{1: "a", 2: "b", 3: "c"}
	after := map[int]string{1: "a", 2: "x", 4: "d"}
	if got := changedGroups(before, after); !reflect.DeepEqual(got, []int{2, 3, 4}) {
		t.Fatalf("changedGroups = %v", got)
	}
}
