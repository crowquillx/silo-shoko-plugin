package main

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// The fixture payloads are intentionally redacted and small. Keeping them as
// files makes API-shape changes visible in review instead of hiding a large
// JSON literal in the test.
//
//go:embed testdata/shoko/*.json
var shokoFixtures embed.FS

func TestEndToEndFixtureReconcileAndScanJournal(t *testing.T) {
	const apiKey = "fixture-api-key"
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	for _, relative := range []string{
		"redacted-release/fixture-pack.mkv",
		"redacted-release/fixture-episode-03.mkv",
	} {
		path := filepath.Join(sourceRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("redacted media fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != apiKey {
			http.Error(w, "bad api key", http.StatusUnauthorized)
			return
		}
		var fixture string
		switch r.URL.Path {
		case "/api/v3/ManagedFolder":
			fixture = "managed_folders.json"
		case "/api/v3/ManagedFolder/1/File":
			fixture = "managed_folder_1_files.json"
		case "/api/v3/Series/7":
			fixture = "series_7.json"
		case "/api/v3/Series/7/Episode":
			fixture = "series_7_episodes.json"
		case "/api/v3/Episode/42":
			fixture = "episode_42.json"
		default:
			http.NotFound(w, r)
			return
		}
		data, err := shokoFixtures.ReadFile("testdata/shoko/" + fixture)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	configValue, err := structpb.NewStruct(map[string]any{
		"base_url":           server.URL,
		"api_key":            apiKey,
		"vfs_root":           outputRoot,
		"managed_folder_map": fmt.Sprintf(`{"1":%q}`, sourceRoot),
	})
	if err != nil {
		t.Fatal(err)
	}

	runtime := &runtimeServer{}
	if _, err := runtime.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: configValue}},
	}); err != nil {
		t.Fatalf("configure runtime: %v", err)
	}
	scheduled := &scheduledTaskServer{runtime: runtime}
	scanner := &scanSourceServer{runtime: runtime}

	dryRunInput, err := structpb.NewStruct(map[string]any{"dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduled.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{
		TaskKey: "reconcile",
		Input:   dryRunInput,
	}); err != nil {
		t.Fatalf("dry-run reconcile: %v", err)
	}
	initial, err := scanner.PollChanges(context.Background(), &pluginv1.PollChangesRequest{CapabilityId: "shoko"})
	if err != nil {
		t.Fatalf("initial PollChanges: %v", err)
	}
	if len(initial.GetSourcePaths()) != 0 || initial.GetNextMarker() != "0" {
		t.Fatalf("dry-run journal state = paths %v, marker %q", initial.GetSourcePaths(), initial.GetNextMarker())
	}

	realRun, err := scheduled.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
	if err != nil {
		t.Fatalf("real reconcile: %v", err)
	}
	if got := realRun.GetOutput().AsMap()["action_count"]; got != float64(3) {
		t.Fatalf("real reconcile action_count = %#v, want 3", got)
	}

	linkOne := filepath.Join(outputRoot, "Fixture Show", "Season 01", "Fixture Show - S01E01 [Shoko Series=7] [Shoko Episode=42] [Shoko File=100].mkv")
	linkTwo := filepath.Join(outputRoot, "Fixture Show", "Season 01", "Fixture Show - S01E02 [Shoko Series=7] [Shoko Episode=43] [Shoko File=100].mkv")
	linkThree := filepath.Join(outputRoot, "Fixture Show", "Season 01", "Fixture Show - S01E03 [Shoko Series=7] [Shoko Episode=44] [Shoko File=101].mkv")
	for _, link := range []string{linkOne, linkTwo, linkThree} {
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat %q: %v", link, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%q is %s, want symlink", link, info.Mode())
		}
	}
	packTarget := filepath.Join(sourceRoot, "redacted-release", "fixture-pack.mkv")
	for _, link := range []string{linkOne, linkTwo} {
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatalf("resolve %q: %v", link, err)
		}
		if resolved != packTarget {
			t.Fatalf("%q resolves to %q, want %q", link, resolved, packTarget)
		}
	}

	metadata, err := (&metadataServer{runtime: runtime}).GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType: "episode",
		FilePath: linkOne,
	})
	if err != nil {
		t.Fatalf("metadata from generated path: %v", err)
	}
	if metadata.GetItem().GetTitle() != "Fixture episode one" || metadata.GetItem().GetProviderIds().AsMap()["shoko_episode"] != "42" {
		t.Fatalf("metadata from generated path = %#v", metadata.GetItem())
	}

	changeResponse, err := scanner.PollChanges(context.Background(), &pluginv1.PollChangesRequest{
		CapabilityId: "shoko",
		Marker:       initial.GetNextMarker(),
	})
	if err != nil {
		t.Fatalf("PollChanges after reconcile: %v", err)
	}
	marker := changeResponse.GetNextMarker()
	changes := changeResponse.GetSourcePaths()
	sort.Strings(changes)
	wantPaths := []string{linkOne, linkTwo, linkThree}
	sort.Strings(wantPaths)
	if len(changes) != len(wantPaths) {
		t.Fatalf("changed paths = %v, want %v", changes, wantPaths)
	}
	for i := range wantPaths {
		if changes[i] != wantPaths[i] {
			t.Fatalf("changed paths = %v, want %v", changes, wantPaths)
		}
	}
	if marker == "" || marker == initial.GetNextMarker() {
		t.Fatalf("marker after reconcile = %q, initial = %q", marker, initial.GetNextMarker())
	}

	// A fresh runtime/reconciler instance replays the same committed journal
	// window, proving that the marker is backed by VFS state rather than process
	// memory.
	restarted := &runtimeServer{}
	if _, err := restarted.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: configValue}},
	}); err != nil {
		t.Fatalf("configure restarted runtime: %v", err)
	}
	replayedResponse, err := (&scanSourceServer{runtime: restarted}).PollChanges(context.Background(), &pluginv1.PollChangesRequest{
		CapabilityId: "shoko",
		Marker:       initial.GetNextMarker(),
	})
	if err != nil {
		t.Fatalf("replay PollChanges: %v", err)
	}
	if replayedResponse.GetNextMarker() != marker || len(replayedResponse.GetSourcePaths()) != len(wantPaths) {
		t.Fatalf("replay = paths %v, marker %q; want paths %v, marker %q", replayedResponse.GetSourcePaths(), replayedResponse.GetNextMarker(), wantPaths, marker)
	}
}
