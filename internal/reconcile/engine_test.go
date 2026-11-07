package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

func TestStepResumesFromPersistentStateAndPollReplaysAfterRestart(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	sourcePath := filepath.Join(sourceRoot, "release", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "fixture-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v3/ManagedFolder" {
			// Ensure the first bounded step leaves state on disk rather than
			// winning the race to complete the tiny fixture.
			time.Sleep(20 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"Fixture","path":"/source"}]`)
		case "/api/v3/ManagedFolder/1/File":
			fmt.Fprintf(w, `{"total":1,"list":[{"id":9,"locations":[{"managedFolderId":1,"relativePath":"release/episode.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":7},"episodeIds":[{"id":42}]}]}]}`)
		case "/api/v3/Series/7":
			fmt.Fprint(w, `{"ids":{"id":7},"name":"Resume Show"}`)
		case "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":42,"parentSeries":7},"name":"Resume episode","aniDB":{"type":"Episode","episodeNumber":1}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	newEngine := func() *Engine {
		reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{sourceRoot}})
		if err != nil {
			t.Fatal(err)
		}
		engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}

	first := newEngine()
	outcome, err := first.Step(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("bounded first step: %v", err)
	}
	if outcome.Complete {
		t.Fatal("bounded first step unexpectedly completed")
	}
	if _, err := os.Stat(vfs.CrawlStatePath(outputRoot)); err != nil {
		t.Fatalf("persistent crawl state: %v", err)
	}

	second := newEngine()
	pending, err := second.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("restart lost pending crawl state")
	}
	completed, err := second.Step(context.Background(), 0)
	if err != nil {
		t.Fatalf("resumed step: %v", err)
	}
	if !completed.Complete || len(completed.Result.Actions) != 1 {
		t.Fatalf("resumed outcome = %#v", completed)
	}
	if _, err := os.Lstat(filepath.Join(outputRoot, "Resume Show", "Season 01", "Resume Show - S01E01 [Shoko Series=7] [Shoko Episode=42] [Shoko File=9].mkv")); err != nil {
		t.Fatalf("generated link: %v", err)
	}

	reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	paths, marker, err := reconciler.PollChanges(context.Background(), "0")
	if err != nil {
		t.Fatal(err)
	}
	if marker == "0" || len(paths) != 1 {
		t.Fatalf("journal replay = paths %v, marker %q", paths, marker)
	}
	if _, err := os.Stat(vfs.CrawlStatePath(outputRoot)); !os.IsNotExist(err) {
		t.Fatalf("completed crawl state still exists: %v", err)
	}
}

// Episodes the series listing hides are fetched one by one in batches, and every
// batch must run, not just the first.
func TestStepFetchesEveryEpisodeMissingFromSeriesListing(t *testing.T) {
	const fileCount = 25
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch {
		case path == "/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"a","path":"/x"}]`)
		case path == "/ManagedFolder/1/File":
			var files strings.Builder
			for i := 1; i <= fileCount; i++ {
				if i > 1 {
					files.WriteByte(',')
				}
				fmt.Fprintf(&files, `{"id":%d,"locations":[{"managedFolderId":1,"relativePath":"f%d.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":1},"episodeIds":[{"id":%d}]}]}`, i, i, i+100)
			}
			fmt.Fprintf(w, `{"total":%d,"list":[%s]}`, fileCount, files.String())
		case path == "/Series/1":
			fmt.Fprint(w, `{"ids":{"id":1,"parentGroup":1},"name":"S","aniDB":{"type":"TV","airDate":"2020-01-01"}}`)
		case path == "/Series/1/Episode":
			fmt.Fprint(w, `{"total":0,"list":[]}`)
		case path == "/Group/1":
			fmt.Fprint(w, `{"ids":{"id":1},"name":"S"}`)
		case path == "/Group/1/Series":
			fmt.Fprint(w, `[{"ids":{"id":1,"parentGroup":1},"name":"S","aniDB":{"type":"TV","airDate":"2020-01-01"}}]`)
		case strings.HasPrefix(path, "/Episode/"):
			var id int
			fmt.Sscanf(path, "/Episode/%d", &id)
			fmt.Fprintf(w, `{"ids":{"id":%d,"parentSeries":1},"name":"e","aniDB":{"type":"Episode","episodeNumber":%d}}`, id, id-100)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: source}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := engine.Step(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Complete || len(outcome.Plan.Entries) != fileCount || len(outcome.Plan.Diagnostics) != 0 {
		t.Fatalf("complete = %v, entries = %d, diagnostics = %#v", outcome.Complete, len(outcome.Plan.Entries), outcome.Plan.Diagnostics)
	}
}

// A failed commit must not leave a snapshot that every later poll re-commits.
func TestStepDropsSnapshotAfterFailedCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch path {
		case "/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"a","path":"/x"}]`)
		case "/ManagedFolder/1/File":
			fmt.Fprint(w, `{"total":1,"list":[{"id":1,"locations":[{"managedFolderId":1,"relativePath":"f.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":1},"episodeIds":[{"id":101}]}]}]}`)
		case "/Series/1":
			fmt.Fprint(w, `{"ids":{"id":1},"name":"S","aniDB":{"type":"TV","airDate":"2020-01-01"}}`)
		case "/Series/1/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":101,"parentSeries":1},"aniDB":{"type":"Episode","episodeNumber":1}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	source, root := t.TempDir(), filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the show's folder belongs breaks the only link.
	if err := os.WriteFile(filepath.Join(root, "S (2020)"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: source}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Step(context.Background(), 0); err == nil {
		t.Fatal("expected the commit to fail")
	}
	pending, err := engine.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("failed snapshot is still pending")
	}
}

// Shoko's listing order decides season numbers, so the crawl must not re-sort it.
func TestStepKeepsShokoGroupOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch path {
		case "/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"a","path":"/x"}]`)
		case "/ManagedFolder/1/File":
			fmt.Fprint(w, `{"total":1,"list":[{"id":1,"locations":[{"managedFolderId":1,"relativePath":"f.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":2},"episodeIds":[{"id":101}]}]}]}`)
		case "/Series/2":
			fmt.Fprint(w, `{"ids":{"id":2,"parentGroup":1},"name":"Later ID","aniDB":{"type":"Movie"}}`)
		case "/Series/2/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":101,"parentSeries":2},"aniDB":{"type":"Episode","episodeNumber":1}}]}`)
		case "/Group/1":
			fmt.Fprint(w, `{"ids":{"id":1},"name":"G"}`)
		case "/Group/1/Series":
			fmt.Fprint(w, `[{"ids":{"id":9,"parentGroup":1},"name":"First in Shoko","aniDB":{"type":"TV"}},{"ids":{"id":2,"parentGroup":1},"name":"Later ID","aniDB":{"type":"Movie"}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: source}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := engine.Step(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Plan.Entries) != 1 || outcome.Plan.Entries[0].SeasonNumber != 2 {
		t.Fatalf("entries = %#v, want the movie in season 2", outcome.Plan.Entries)
	}
}
