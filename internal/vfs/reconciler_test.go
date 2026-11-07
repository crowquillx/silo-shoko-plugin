package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func link(key, logical, target string) topology.Entry {
	return topology.Entry{StableKey: key, LogicalPath: logical, TargetPath: target, SourcePath: target, Kind: topology.KindEpisode}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newReconciler(t *testing.T, root string, sources ...string) *Reconciler {
	t.Helper()
	reconciler, err := NewReconciler(Config{Root: root, AllowedSourceRoots: sources})
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

func resolvesTo(t *testing.T, link, want string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != wanted {
		t.Fatalf("%s resolves to %s, want %s", link, got, wanted)
	}
}

func TestReconcileRecoversAfterPathMapCorrection(t *testing.T) {
	base := t.TempDir()
	root, oldSource, newSource := filepath.Join(base, "vfs"), filepath.Join(base, "old"), filepath.Join(base, "new")
	touch(t, filepath.Join(oldSource, "a.mkv"))
	touch(t, filepath.Join(oldSource, "gone.mkv"))
	touch(t, filepath.Join(newSource, "a.mkv"))
	ctx := context.Background()

	first := topology.Plan{Entries: []topology.Entry{
		link("a", "S/Season 01/a.mkv", filepath.Join(oldSource, "a.mkv")),
		link("gone", "S/Season 01/gone.mkv", filepath.Join(oldSource, "gone.mkv")),
	}}
	if _, err := newReconciler(t, root, oldSource).Reconcile(ctx, first, false); err != nil {
		t.Fatal(err)
	}

	second := topology.Plan{Entries: []topology.Entry{link("a", "S/Season 01/a.mkv", filepath.Join(newSource, "a.mkv"))}}
	result, err := newReconciler(t, root, newSource).Reconcile(ctx, second, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %#v", result.Failures)
	}
	resolvesTo(t, filepath.Join(root, "S/Season 01/a.mkv"), filepath.Join(newSource, "a.mkv"))
	if _, err := os.Lstat(filepath.Join(root, "S/Season 01/gone.mkv")); !os.IsNotExist(err) {
		t.Fatalf("obsolete link still present: %v", err)
	}
}

func TestReconcileLinksEverythingElseWhenOneNameIsTooLong(t *testing.T) {
	base := t.TempDir()
	root, source := filepath.Join(base, "vfs"), filepath.Join(base, "media")
	touch(t, filepath.Join(source, "a.mkv"))
	touch(t, filepath.Join(source, "b.mkv"))
	reconciler := newReconciler(t, root, source)
	ctx := context.Background()

	longName := "S/Season 01/" + strings.Repeat("x", 300) + ".mkv"
	result, err := reconciler.Reconcile(ctx, topology.Plan{Entries: []topology.Entry{
		link("a", "S/Season 01/a.mkv", filepath.Join(source, "a.mkv")),
		link("b", longName, filepath.Join(source, "b.mkv")),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 || result.Failures[0].LogicalPath != longName {
		t.Fatalf("failures = %#v", result.Failures)
	}
	resolvesTo(t, filepath.Join(root, "S/Season 01/a.mkv"), filepath.Join(source, "a.mkv"))

	// The journal reports only the link that was really written.
	paths, _, err := reconciler.PollChanges(ctx, "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(root, "S/Season 01/a.mkv") {
		t.Fatalf("journal paths = %v", paths)
	}

	// Once the name is valid the next run finishes without a conflict.
	result, err = reconciler.Reconcile(ctx, topology.Plan{Entries: []topology.Entry{
		link("a", "S/Season 01/a.mkv", filepath.Join(source, "a.mkv")),
		link("b", "S/Season 01/b.mkv", filepath.Join(source, "b.mkv")),
	}}, false)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("retry: err = %v, failures = %#v", err, result.Failures)
	}
	resolvesTo(t, filepath.Join(root, "S/Season 01/b.mkv"), filepath.Join(source, "b.mkv"))
}

// A previous plugin version, or a process killed mid-run, can leave correct links
// that were never recorded in the manifest.
func TestReconcileAdoptsLinksLeftByAnInterruptedRun(t *testing.T) {
	base := t.TempDir()
	root, source := filepath.Join(base, "vfs"), filepath.Join(base, "media")
	target := filepath.Join(source, "a.mkv")
	touch(t, target)
	linkPath := filepath.Join(root, "S/Season 01/a.mkv")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatal(err)
	}
	result, err := newReconciler(t, root, source).Reconcile(context.Background(), topology.Plan{Entries: []topology.Entry{link("a", "S/Season 01/a.mkv", target)}}, false)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("err = %v, failures = %#v", err, result.Failures)
	}
}

func TestReconcileNeverOverwritesFilesItDoesNotOwn(t *testing.T) {
	base := t.TempDir()
	root, source := filepath.Join(base, "vfs"), filepath.Join(base, "media")
	target := filepath.Join(source, "a.mkv")
	touch(t, target)
	touch(t, filepath.Join(source, "good.mkv"))
	elsewhere := filepath.Join(base, "elsewhere.mkv")
	touch(t, elsewhere)

	regular := filepath.Join(root, "S/regular.mkv")
	touch(t, regular)
	foreign := filepath.Join(root, "S/foreign.mkv")
	if err := os.Symlink(elsewhere, foreign); err != nil {
		t.Fatal(err)
	}

	result, err := newReconciler(t, root, source).Reconcile(context.Background(), topology.Plan{Entries: []topology.Entry{
		link("regular", "S/regular.mkv", target),
		link("foreign", "S/foreign.mkv", target),
		link("good", "S/good.mkv", filepath.Join(source, "good.mkv")),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("failures = %#v", result.Failures)
	}
	if info, err := os.Lstat(regular); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("regular file was replaced: %v", err)
	}
	resolvesTo(t, foreign, elsewhere)
	resolvesTo(t, filepath.Join(root, "S/good.mkv"), filepath.Join(source, "good.mkv"))
}

func TestReconcileAcceptsSourceRootReachedThroughSymlink(t *testing.T) {
	base := t.TempDir()
	root, pool, media := filepath.Join(base, "vfs"), filepath.Join(base, "pool"), filepath.Join(base, "media")
	touch(t, filepath.Join(pool, "a.mkv"))
	if err := os.Symlink(pool, media); err != nil {
		t.Fatal(err)
	}
	result, err := newReconciler(t, root, media).Reconcile(context.Background(), topology.Plan{Entries: []topology.Entry{link("a", "S/a.mkv", filepath.Join(media, "a.mkv"))}}, false)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("err = %v, failures = %#v", err, result.Failures)
	}
	resolvesTo(t, filepath.Join(root, "S/a.mkv"), filepath.Join(pool, "a.mkv"))
}

func TestReconcileSkipsOnlyTheFileThatEscapesItsRoot(t *testing.T) {
	base := t.TempDir()
	root, source, other := filepath.Join(base, "vfs"), filepath.Join(base, "media"), filepath.Join(base, "other")
	touch(t, filepath.Join(source, "good.mkv"))
	touch(t, filepath.Join(other, "secret.mkv"))
	if err := os.Symlink(filepath.Join(other, "secret.mkv"), filepath.Join(source, "linked.mkv")); err != nil {
		t.Fatal(err)
	}
	result, err := newReconciler(t, root, source).Reconcile(context.Background(), topology.Plan{Entries: []topology.Entry{
		link("good", "S/good.mkv", filepath.Join(source, "good.mkv")),
		link("linked", "S/linked.mkv", filepath.Join(source, "linked.mkv")),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 || result.Failures[0].LogicalPath != "S/linked.mkv" {
		t.Fatalf("failures = %#v", result.Failures)
	}
	resolvesTo(t, filepath.Join(root, "S/good.mkv"), filepath.Join(source, "good.mkv"))
	if _, err := os.Lstat(filepath.Join(root, "S/linked.mkv")); !os.IsNotExist(err) {
		t.Fatalf("escaping target was linked: %v", err)
	}
}

func TestReconcileFailsVisiblyWhenNothingCanBeWritten(t *testing.T) {
	base := t.TempDir()
	root, source := filepath.Join(base, "vfs"), filepath.Join(base, "media")
	touch(t, filepath.Join(source, "a.mkv"))
	touch(t, filepath.Join(root, "S"))
	_, err := newReconciler(t, root, source).Reconcile(context.Background(), topology.Plan{Entries: []topology.Entry{link("a", "S/Season 01/a.mkv", filepath.Join(source, "a.mkv"))}}, false)
	if err == nil {
		t.Fatal("expected an error when no link can be written")
	}
}

func TestWithExclusiveHonorsDeadlineWhileAnotherCallHoldsIt(t *testing.T) {
	base := t.TempDir()
	reconciler := newReconciler(t, filepath.Join(base, "vfs"), filepath.Join(base, "media"))
	if err := reconciler.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	release, held := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- reconciler.WithExclusive(context.Background(), func(*Reconciler) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := reconciler.WithExclusive(ctx, func(*Reconciler) error { return nil })
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
