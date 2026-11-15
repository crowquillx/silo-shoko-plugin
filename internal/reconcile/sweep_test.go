package reconcile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

func newSweepEngine(t *testing.T, fake *fakeShoko) (*Engine, string) {
	t.Helper()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fake.requests = make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	return engine, outputRoot
}

// A sweep finds what changed in Shoko without being told: new and deleted
// files, a series that moved to another group, and a renamed group.
func TestSweepFindsChangesNoEventReported(t *testing.T) {
	fake := &fakeShoko{
		files: map[int]fakeFile{
			9:  {path: "a.mkv", series: 7, episode: 42},
			10: {path: "b.mkv", series: 8, episode: 43},
		},
		seriesGroup: map[int]int{7: 3, 8: 3},
		episodes:    map[int][2]int{42: {7, 1}, 43: {8, 1}},
	}
	engine, outputRoot := newSweepEngine(t, fake)
	ctx := context.Background()
	if _, err := engine.Sweep(ctx); err != ErrNoSnapshot {
		t.Fatalf("Sweep before any crawl = %v, want ErrNoSnapshot", err)
	}
	if _, err := engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}

	// Nothing changed: no link changes, and no series is read again one by one.
	seriesReads := fake.count("/Series/7") + fake.count("/Series/8")
	outcome, err := engine.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed := changedLinks(outcome); changed != 0 {
		t.Fatalf("a sweep with nothing to find changed %d links: %#v", changed, outcome.Result.Actions)
	}
	if again := fake.count("/Series/7") + fake.count("/Series/8"); again != seriesReads {
		t.Fatalf("a sweep with nothing to find read series %d times", again-seriesReads)
	}

	fake.update(func() {
		// A new file for a new episode, and a deleted file.
		fake.files[11] = fakeFile{path: "c.mkv", series: 7, episode: 44}
		fake.episodes[44] = [2]int{7, 2}
		delete(fake.files, 10)
		// A new series joins the group ahead of series 7, which makes
		// series 7 the group's third season.
		fake.files[12] = fakeFile{path: "d.mkv", series: 6, episode: 45}
		fake.episodes[45] = [2]int{6, 1}
		fake.seriesGroup[6] = 3
		fake.groupNames = map[int]string{3: "Renamed"}
	})
	if _, err := engine.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Renamed (2020)/Season 01/Series 6 - S01E01 [Shoko Group=3] [Shoko Series=6] [Shoko Episode=45] [Shoko File=12].mkv",
		"Renamed (2020)/Season 02/Series 7 - S02E01 [Shoko Group=3] [Shoko Series=7] [Shoko Episode=42] [Shoko File=9].mkv",
		"Renamed (2020)/Season 02/Series 7 - S02E02 [Shoko Group=3] [Shoko Series=7] [Shoko Episode=44] [Shoko File=11].mkv",
	}
	if got := vfsLinks(t, outputRoot); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("links after sweep =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The series moves to a group of its own; nothing about its files changes.
	fake.update(func() { fake.seriesGroup[7] = 4 })
	if _, err := engine.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	moved := 0
	for _, link := range vfsLinks(t, outputRoot) {
		if strings.HasPrefix(link, "Group 4 (2020)/Season 01/") {
			moved++
		}
	}
	if moved != 2 {
		t.Fatalf("links after the series moved = %v", vfsLinks(t, outputRoot))
	}
}

// A file list read while Shoko imports can skip a file. The file is still in
// Shoko, so its link must stay.
func TestSweepKeepsAFileTheListSkipped(t *testing.T) {
	fake := &fakeShoko{
		files: map[int]fakeFile{
			9:  {path: "a.mkv", series: 7, episode: 42},
			10: {path: "b.mkv", series: 7, episode: 43},
		},
		seriesGroup: map[int]int{7: 3},
		episodes:    map[int][2]int{42: {7, 1}, 43: {7, 2}},
	}
	engine, outputRoot := newSweepEngine(t, fake)
	ctx := context.Background()
	if _, err := engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fake.update(func() { fake.unlisted = map[int]bool{10: true} })
	outcome, err := engine.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed := changedLinks(outcome); changed != 0 || len(vfsLinks(t, outputRoot)) != 2 {
		t.Fatalf("sweep changed %d links, leaving %v", changed, vfsLinks(t, outputRoot))
	}
	// A later sweep that lists the file again finds nothing to do either.
	fake.update(func() { fake.unlisted = nil })
	if outcome, err := engine.Sweep(ctx); err != nil || changedLinks(outcome) != 0 {
		t.Fatalf("sweep after the list recovered = %d changed, %v", changedLinks(outcome), err)
	}
}

// A server may answer with smaller pages than asked for. The total it reports
// decides when the list is complete, not a short page.
func TestCrawlReadsEveryPageOfACappedFileList(t *testing.T) {
	fake := &fakeShoko{
		files: map[int]fakeFile{
			9:  {path: "a.mkv", series: 7, episode: 42},
			10: {path: "b.mkv", series: 7, episode: 43},
			11: {path: "c.mkv", series: 7, episode: 44},
		},
		seriesGroup: map[int]int{7: 3},
		episodes:    map[int][2]int{42: {7, 1}, 43: {7, 2}, 44: {7, 3}},
		pageCap:     2,
	}
	engine, outputRoot := newSweepEngine(t, fake)
	ctx := context.Background()
	if _, err := engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := vfsLinks(t, outputRoot); len(got) != 3 {
		t.Fatalf("links after a crawl of capped pages = %v", got)
	}
	if outcome, err := engine.Sweep(ctx); err != nil || changedLinks(outcome) != 0 {
		t.Fatalf("sweep of capped pages = %d changed, %v", changedLinks(outcome), err)
	}
}
