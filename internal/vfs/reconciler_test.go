package vfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
)

func TestReconcileCreatesAndRemovesOwnedLeafLink(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	media := filepath.Join(source, "episode.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	plan := topology.Plan{Entries: []topology.Entry{{StableKey: "file:9/episode:42", LogicalPath: "Show/Season 01/Episode.mkv", TargetPath: media}}}
	if _, err := reconciler.Reconcile(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "Show", "Season 01", "Episode.mkv")
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link info = %v, %v", info, err)
	}
	if _, err := reconciler.Reconcile(context.Background(), topology.Plan{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link still exists: %v", err)
	}
}

func TestReconcileDryRunDoesNotCreateOutputRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-root")
	source := t.TempDir()
	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	plan := topology.Plan{Entries: []topology.Entry{{StableKey: "file:1/episode:1", LogicalPath: "Show/Episode.mkv", TargetPath: filepath.Join(source, "episode.mkv")}}}
	result, err := reconciler.Reconcile(context.Background(), plan, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || len(result.Actions) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("dry run created root: %v", err)
	}
	if _, err := os.Stat(ChangeJournalPath(root)); !os.IsNotExist(err) {
		t.Fatalf("dry run created change journal: %v", err)
	}
}

func TestChangeJournalReplaysAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vfs")
	source := t.TempDir()
	media := filepath.Join(source, "episode.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := topology.Plan{Entries: []topology.Entry{{
		StableKey:   "file:9/episode:42",
		LogicalPath: "Show/Season 01/Episode.mkv",
		TargetPath:  media,
	}}}

	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), plan, true); err != nil {
		t.Fatal(err)
	}
	initialPaths, markerBefore, err := reconciler.PollChanges(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(initialPaths) != 0 || markerBefore != "0" {
		t.Fatalf("initial PollChanges = paths %v, marker %q", initialPaths, markerBefore)
	}
	if _, err := reconciler.Reconcile(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	paths, marker, err := reconciler.PollChanges(context.Background(), markerBefore)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(root, "Show", "Season 01", "Episode.mkv") {
		t.Fatalf("paths = %v", paths)
	}
	if marker != "1" {
		t.Fatalf("marker = %q, want 1", marker)
	}

	// A newly-created reconciler must read the same journal and replay from the
	// old cursor; the journal is not process memory.
	restarted, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	replayed, next, err := restarted.PollChanges(context.Background(), "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0] != paths[0] || next != marker {
		t.Fatalf("replayed = %v, marker = %q", replayed, next)
	}
	again, next, err := restarted.PollChanges(context.Background(), marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 || next != marker {
		t.Fatalf("replay after cursor = %v, marker = %q", again, next)
	}
}

func TestReconcileMovesChangedLogicalPathAndJournalsBothPaths(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	media := filepath.Join(source, "episode.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	first := topology.Plan{Entries: []topology.Entry{{StableKey: "file:9/episode:42", LogicalPath: "Old/Episode.mkv", TargetPath: media}}}
	second := topology.Plan{Entries: []topology.Entry{{StableKey: "file:9/episode:42", LogicalPath: "New/Episode.mkv", TargetPath: media}}}
	if _, err := reconciler.Reconcile(context.Background(), first, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), second, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Old", "Episode.mkv")); !os.IsNotExist(err) {
		t.Fatalf("old link still exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "New", "Episode.mkv")); err != nil {
		t.Fatalf("new link missing: %v", err)
	}
	paths, _, err := reconciler.PollChanges(context.Background(), "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != filepath.Join(root, "Old", "Episode.mkv") || paths[1] != filepath.Join(root, "New", "Episode.mkv") {
		t.Fatalf("journal paths = %v", paths)
	}
}

func TestPollChangesRejectsInvalidMarker(t *testing.T) {
	reconciler, err := NewReconciler(Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconciler.PollChanges(context.Background(), "not-a-marker"); err == nil {
		t.Fatal("invalid marker accepted")
	}
}

func TestNewReconcilerRejectsOverlappingRoots(t *testing.T) {
	if _, err := NewReconciler(Config{Root: "/media/anime/vfs", AllowedSourceRoots: []string{"/media/anime"}}); err == nil {
		t.Fatal("overlapping roots accepted")
	}
}
