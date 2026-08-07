package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
