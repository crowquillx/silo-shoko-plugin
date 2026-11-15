package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
)

func newMaintenanceReconciler(t *testing.T) (*Reconciler, string, string) {
	t.Helper()
	root, source := t.TempDir(), t.TempDir()
	media := filepath.Join(source, "episode.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, root, media
}

func entryAt(key, logical, target string) topology.Plan {
	return topology.Plan{Entries: []topology.Entry{{StableKey: key, LogicalPath: logical, TargetPath: target}}}
}

// A renamed show leaves its old folders empty. They stay for the reconcile
// that emptied them, so Silo can scan them and drop what was there, and go
// with the next full reconcile.
func TestFullReconcileRemovesFoldersLeftEmpty(t *testing.T) {
	reconciler, root, media := newMaintenanceReconciler(t)
	ctx := context.Background()
	if _, err := reconciler.Reconcile(ctx, entryAt("file:1/episode:1", "Old/Season 01/E.mkv", media), false); err != nil {
		t.Fatal(err)
	}
	// A file the plugin did not put there keeps its folder.
	kept := filepath.Join(root, "Kept", "Season 01")
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	renamed := entryAt("file:1/episode:1", "New/Season 01/E.mkv", media)
	if _, err := reconciler.Reconcile(ctx, renamed, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Old", "Season 01")); err != nil {
		t.Fatalf("the folder was removed by the reconcile that emptied it: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, renamed, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Old")); !os.IsNotExist(err) {
		t.Fatalf("empty show folder left after a later full reconcile: %v", err)
	}
	for _, path := range []string{filepath.Join(root, "New", "Season 01", "E.mkv"), filepath.Join(kept, "notes.txt")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s is gone: %v", path, err)
		}
	}
}

// A link that moves is reported as moved, and when its old path cannot be
// removed the manifest keeps that path, so a later reconcile removes it.
func TestMovedLinkKeepsAnOldPathItCouldNotRemove(t *testing.T) {
	reconciler, root, media := newMaintenanceReconciler(t)
	ctx := context.Background()
	if _, err := reconciler.Reconcile(ctx, entryAt("file:1/episode:1", "Show/Season 01/E.mkv", media), false); err != nil {
		t.Fatal(err)
	}
	// Something else replaces the link, so the plugin refuses to remove it.
	oldLink := filepath.Join(root, "Show", "Season 01", "E.mkv")
	if err := os.Remove(oldLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", oldLink); err != nil {
		t.Fatal(err)
	}
	moved := entryAt("file:1/episode:1", "Show/Season 02/E.mkv", media)
	result, err := reconciler.Reconcile(ctx, moved, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Reason, ErrNotOwned.Error()) {
		t.Fatalf("failures = %#v", result.Failures)
	}
	sawMove := false
	for _, action := range result.Actions {
		if action.Kind == "update" && action.Reason == ReasonMoved && action.LogicalPath == "Show/Season 02/E.mkv" {
			sawMove = true
		}
	}
	if !sawMove {
		t.Fatalf("no moved update in %#v", result.Actions)
	}
	if entries := manifestEntries(t, root); len(entries) != 2 {
		t.Fatalf("manifest forgot the old path: %#v", entries)
	}

	// Once the link is the plugin's again, the next reconcile removes it.
	if err := os.Remove(oldLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(media, oldLink); err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.Reconcile(ctx, moved, false); err != nil || len(result.Failures) != 0 {
		t.Fatalf("second reconcile = %#v, %v", result.Failures, err)
	}
	if _, err := os.Lstat(oldLink); !os.IsNotExist(err) {
		t.Fatalf("old link still there: %v", err)
	}
	if entries := manifestEntries(t, root); len(entries) != 1 {
		t.Fatalf("manifest after cleanup: %#v", entries)
	}
}

func manifestEntries(t *testing.T, root string) map[string]ManifestItem {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".shokoanime-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Entries
}

// Old journal entries are trimmed once the journal is large. A host whose
// marker is older than what is left is told so, and one that kept up is not.
func TestJournalTrimsOldEntries(t *testing.T) {
	trimAbove, keep := journalTrimAbove, journalKeep
	journalTrimAbove, journalKeep = 3, time.Millisecond
	t.Cleanup(func() { journalTrimAbove, journalKeep = trimAbove, keep })

	reconciler, root, media := newMaintenanceReconciler(t)
	ctx := context.Background()
	plan := topology.Plan{}
	add := func(names ...string) {
		for _, name := range names {
			plan.Entries = append(plan.Entries, topology.Entry{StableKey: "key:" + name, LogicalPath: "Show/Season 01/" + name, TargetPath: media})
		}
		if _, err := reconciler.Reconcile(ctx, plan, false); err != nil {
			t.Fatal(err)
		}
	}
	add("a.mkv", "b.mkv")
	time.Sleep(20 * time.Millisecond)
	add("c.mkv", "d.mkv", "e.mkv")

	if _, _, err := reconciler.PollChanges(ctx, "0"); !errors.Is(err, ErrUnusableMarker) {
		t.Fatalf("poll from before the trimmed entries = %v, want ErrUnusableMarker", err)
	}
	paths, marker, err := reconciler.PollChanges(ctx, "2")
	if err != nil || len(paths) != 3 || marker != "5" {
		t.Fatalf("poll from the last trimmed entry = %v, %q, %v", paths, marker, err)
	}
	// Sequence numbers carry on after a trim, also for a reconciler that has
	// to read the journal from disk.
	restarted, err := NewReconciler(Config{Root: root, AllowedSourceRoots: []string{filepath.Dir(media)}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.CurrentMarker(ctx); err != nil || got != "5" {
		t.Fatalf("marker after restart = %q, %v", got, err)
	}
	reconciler = restarted
	add("f.mkv")
	if paths, marker, err := reconciler.PollChanges(ctx, "5"); err != nil || len(paths) != 1 || marker != "6" {
		t.Fatalf("poll after another change = %v, %q, %v", paths, marker, err)
	}
}
