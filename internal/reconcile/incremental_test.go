package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// fakeShoko serves a small library that tests change between calls.
type fakeShoko struct {
	mu sync.Mutex
	// files maps a file ID to its relative path and episode.
	files map[int]fakeFile
	// seriesGroup maps a series ID to its group.
	seriesGroup map[int]int
	// episodes maps an episode ID to its series and number.
	episodes map[int][2]int
	requests map[string]int
	// memberOrder lists series in the order groups list them; series not in
	// it follow by ID.
	memberOrder []int
	// groupNames overrides a group's name, which is Group <id> by default.
	groupNames map[int]string
	// beforeFile runs, once, before the first single-file lookup is
	// answered.
	beforeFile func()
}

type fakeFile struct {
	path    string
	series  int
	episode int
}

func (f *fakeShoko) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/v3/File/") {
		f.mu.Lock()
		hook := f.beforeFile
		f.beforeFile = nil
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	f.requests[path]++
	parts := strings.Split(strings.Trim(path, "/"), "/")
	id := 0
	if len(parts) > 1 {
		id, _ = strconv.Atoi(parts[1])
	}
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	switch {
	case path == "/ManagedFolder":
		write([]map[string]any{{"id": 1, "name": "a", "path": "/x"}})
	case path == "/ManagedFolder/1/File":
		list := make([]any, 0)
		for _, fileID := range sortedKeys(f.files) {
			list = append(list, f.fileJSON(fileID))
		}
		write(map[string]any{"total": len(list), "list": list})
	case parts[0] == "File" && len(parts) == 2:
		if _, ok := f.files[id]; !ok {
			http.NotFound(w, r)
			return
		}
		write(f.fileJSON(id))
	case parts[0] == "Series" && len(parts) == 2:
		group, ok := f.seriesGroup[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(f.seriesJSON(id, group))
	case parts[0] == "Series" && len(parts) == 3 && parts[2] == "Episode":
		list := make([]any, 0)
		for _, episodeID := range sortedKeys(f.episodes) {
			if f.episodes[episodeID][0] == id {
				list = append(list, f.episodeJSON(episodeID))
			}
		}
		write(map[string]any{"total": len(list), "list": list})
	case parts[0] == "Episode" && len(parts) == 2:
		if _, ok := f.episodes[id]; !ok {
			http.NotFound(w, r)
			return
		}
		write(f.episodeJSON(id))
	case parts[0] == "Group" && len(parts) >= 2:
		members := make([]any, 0)
		order := append([]int(nil), f.memberOrder...)
		for _, seriesID := range sortedKeys(f.seriesGroup) {
			if !containsID(order, seriesID) {
				order = append(order, seriesID)
			}
		}
		for _, seriesID := range order {
			if group, ok := f.seriesGroup[seriesID]; ok && group == id {
				members = append(members, f.seriesJSON(seriesID, id))
			}
		}
		if len(members) == 0 {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 3 && parts[2] == "Series" {
			write(members)
			return
		}
		name := f.groupNames[id]
		if name == "" {
			name = fmt.Sprintf("Group %d", id)
		}
		write(map[string]any{"IDs": map[string]any{"ID": id}, "Name": name})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeShoko) fileJSON(id int) any {
	file := f.files[id]
	return map[string]any{
		"id":        id,
		"locations": []any{map[string]any{"managedFolderId": 1, "relativePath": file.path, "isAccessible": true}},
		"seriesIds": []any{map[string]any{"seriesId": map[string]any{"id": file.series}, "episodeIds": []any{map[string]any{"id": file.episode}}}},
	}
}

func (f *fakeShoko) seriesJSON(id, group int) any {
	return map[string]any{
		"ids":   map[string]any{"id": id, "parentGroup": group},
		"name":  fmt.Sprintf("Series %d", id),
		"aniDB": map[string]any{"type": "TV", "airDate": "2020-01-01"},
	}
}

func (f *fakeShoko) episodeJSON(id int) any {
	episode := f.episodes[id]
	return map[string]any{
		"ids":   map[string]any{"id": id, "parentSeries": episode[0]},
		"name":  fmt.Sprintf("Episode %d", id),
		"aniDB": map[string]any{"type": "Normal", "episodeNumber": episode[1]},
	}
}

func (f *fakeShoko) update(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeShoko) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func vfsLinks(t *testing.T, root string) []string {
	t.Helper()
	links := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			relative, _ := filepath.Rel(root, path)
			links = append(links, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(links)
	return links
}

func TestApplyUpdatesOnlyWhatShokoReportedChanged(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	for _, name := range []string{"a.mkv", "b.mkv"} {
		if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceRoot, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeShoko{
		files:       map[int]fakeFile{9: {path: "a.mkv", series: 7, episode: 42}},
		seriesGroup: map[int]int{7: 3},
		episodes:    map[int][2]int{42: {7, 1}},
		requests:    make(map[string]int),
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
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
	ctx := context.Background()

	if _, err := engine.Apply(ctx, Changes{Files: []int{9}}); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("Apply before any crawl = %v, want ErrNoSnapshot", err)
	}
	if outcome, err := engine.Step(ctx, 0); err != nil || !outcome.Complete {
		t.Fatalf("full crawl = %#v, %v", outcome, err)
	}
	if info, err := engine.Snapshot(ctx); err != nil || !info.Exists {
		t.Fatalf("snapshot after crawl = %#v, %v", info, err)
	}
	if _, err := os.Stat(vfs.CrawlStatePath(outputRoot)); !os.IsNotExist(err) {
		t.Fatalf("crawl state left after commit: %v", err)
	}
	want := []string{"Group 3 (2020)/Season 01/Series 7 - S01E01 [Shoko Group=3] [Shoko Series=7] [Shoko Episode=42] [Shoko File=9].mkv"}
	if got := vfsLinks(t, outputRoot); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("links after crawl =\n%s", strings.Join(got, "\n"))
	}

	// A new file is matched to a new episode.
	fake.update(func() {
		fake.files[10] = fakeFile{path: "b.mkv", series: 7, episode: 43}
		fake.episodes[43] = [2]int{7, 2}
	})
	outcome, err := engine.Apply(ctx, Changes{Files: []int{10}})
	if err != nil {
		t.Fatal(err)
	}
	if changed := changedLinks(outcome); changed != 1 {
		t.Fatalf("changed links for a new file = %d: %#v", changed, outcome.Result.Actions)
	}
	if fake.count("/ManagedFolder/1/File") != 1 {
		t.Fatalf("Apply listed the managed folder again")
	}
	if got := vfsLinks(t, outputRoot); len(got) != 2 {
		t.Fatalf("links after new file = %v", got)
	}
	paths, _, err := reconciler.PollChanges(ctx, "1")
	if err != nil || len(paths) != 1 || !strings.Contains(paths[0], "[Shoko File=10]") {
		t.Fatalf("journal after new file = %v, %v", paths, err)
	}

	// The series moves to another group, which renames its show folder.
	fake.update(func() { fake.seriesGroup[7] = 4 })
	if _, err := engine.Apply(ctx, Changes{Series: []int{7}}); err != nil {
		t.Fatal(err)
	}
	for _, link := range vfsLinks(t, outputRoot) {
		if !strings.HasPrefix(link, "Group 4 (2020)/") || !strings.Contains(link, "[Shoko Group=4]") {
			t.Fatalf("link not moved to the new group: %s", link)
		}
	}

	// A renamed group renames its show folder.
	fake.update(func() { fake.groupNames = map[int]string{4: "Renamed"} })
	if _, err := engine.Apply(ctx, Changes{Groups: []int{4}}); err != nil {
		t.Fatal(err)
	}
	for _, link := range vfsLinks(t, outputRoot) {
		if !strings.HasPrefix(link, "Renamed (2020)/") {
			t.Fatalf("link not moved to the renamed group's folder: %s", link)
		}
	}

	// A file Shoko no longer has is removed.
	fake.update(func() { delete(fake.files, 9) })
	if _, err := engine.Apply(ctx, Changes{Files: []int{9}}); err != nil {
		t.Fatal(err)
	}
	got := vfsLinks(t, outputRoot)
	if len(got) != 1 || !strings.Contains(got[0], "[Shoko File=10]") {
		t.Fatalf("links after delete = %v", got)
	}

	// The snapshot survives a restart.
	restarted, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restarted.SnapshotData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || snapshot.Files[0].ID != 10 {
		t.Fatalf("snapshot files after restart = %#v", snapshot.Files)
	}

	// A crawl that has started but not committed is applied to first.
	if err := os.WriteFile(vfs.CrawlStatePath(outputRoot), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, Changes{Files: []int{10}}); !errors.Is(err, ErrCrawlPending) {
		t.Fatalf("Apply during a crawl = %v, want ErrCrawlPending", err)
	}
}

func changedLinks(outcome Outcome) int {
	changed := 0
	for _, action := range outcome.Result.Actions {
		if action.Kind != "unchanged" {
			changed++
		}
	}
	return changed
}

func TestApplyWithOtherSettingsNeedsACrawl(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	otherRoot := filepath.Join(t.TempDir(), "other")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeShoko{
		files:       map[int]fakeFile{9: {path: "a.mkv", series: 7, episode: 42}},
		seriesGroup: map[int]int{7: 3},
		episodes:    map[int][2]int{42: {7, 1}},
		requests:    make(map[string]int),
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	client, _ := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	newEngine := func(root string) *Engine {
		reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{root}})
		if err != nil {
			t.Fatal(err)
		}
		engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: root}})
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}
	if _, err := newEngine(sourceRoot).Step(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := newEngine(otherRoot).Apply(context.Background(), Changes{Files: []int{9}}); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("Apply after a mapping change = %v, want ErrNoSnapshot", err)
	}
}

func TestApplyReadsAgainWhenAnotherCommitLandsMeanwhile(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		if err := os.WriteFile(filepath.Join(sourceRoot, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeShoko{
		files:       map[int]fakeFile{9: {path: "a.mkv", series: 7, episode: 42}},
		seriesGroup: map[int]int{7: 3},
		episodes:    map[int][2]int{42: {7, 1}, 43: {7, 2}, 44: {7, 3}},
		requests:    make(map[string]int),
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	client, _ := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fake.update(func() {
		fake.files[10] = fakeFile{path: "b.mkv", series: 7, episode: 43}
		fake.files[11] = fakeFile{path: "c.mkv", series: 7, episode: 44}
		// While file 10 is read, a full crawl commits file 11.
		fake.beforeFile = func() {
			if _, err := engine.Step(ctx, 0); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := engine.Apply(ctx, Changes{Files: []int{10}}); err != nil {
		t.Fatal(err)
	}
	if got := vfsLinks(t, outputRoot); len(got) != 3 {
		t.Fatalf("Apply dropped the crawl's commit: links = %v", got)
	}
	snapshot, err := engine.SnapshotData(ctx)
	if err != nil || len(snapshot.Files) != 3 {
		t.Fatalf("snapshot files = %d, %v", len(snapshot.Files), err)
	}
}

// Shoko's order of a group's series decides their season numbers, so a
// group read again must keep it, not sort it.
func TestApplyKeepsShokosSeasonOrder(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &fakeShoko{
		files: map[int]fakeFile{
			9:  {path: "a.mkv", series: 7, episode: 42},
			10: {path: "b.mkv", series: 8, episode: 43},
		},
		seriesGroup: map[int]int{7: 3, 8: 3},
		episodes:    map[int][2]int{42: {7, 1}, 43: {8, 1}},
		// Series 8 is the group's first season.
		memberOrder: []int{8, 7},
		requests:    make(map[string]int),
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	client, _ := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: outputRoot, AllowedSourceRoots: []string{sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Client: client, Reconciler: reconciler, ManagedFolderMap: map[int]string{1: sourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := engine.Step(ctx, 0); err != nil {
		t.Fatal(err)
	}
	before := vfsLinks(t, outputRoot)
	if len(before) != 2 || !strings.Contains(before[0], "Season 01/Series 8") {
		t.Fatalf("links after crawl = %v", before)
	}
	for _, changes := range []Changes{{Series: []int{7}}, {Groups: []int{3}}, {Files: []int{9, 10}}} {
		outcome, err := engine.Apply(ctx, changes)
		if err != nil {
			t.Fatal(err)
		}
		if changed := changedLinks(outcome); changed != 0 {
			t.Fatalf("Apply(%+v) changed %d links: %#v", changes, changed, outcome.Result.Actions)
		}
	}
	if after := vfsLinks(t, outputRoot); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("links after Apply = %v, want %v", after, before)
	}
}
