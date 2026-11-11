package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
)

type themeFixture struct {
	t          *testing.T
	root       string
	media      string
	show       string
	vfsShow    string
	reconciler *Reconciler
}

func newThemeFixture(t *testing.T) *themeFixture {
	t.Helper()
	base := t.TempDir()
	f := &themeFixture{t: t, root: filepath.Join(base, "vfs"), media: filepath.Join(base, "media")}
	f.show = filepath.Join(f.media, "Show")
	f.vfsShow = filepath.Join(f.root, "Show (2020)")
	write(t, filepath.Join(f.show, "01.mkv"), "video")
	f.reconciler = newReconciler(t, f.root, f.media)
	plan := topology.Plan{Entries: []topology.Entry{link("e", "Show (2020)/Season 01/e.mkv", filepath.Join(f.show, "01.mkv"))}}
	if _, err := f.reconciler.Reconcile(context.Background(), plan, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *themeFixture) sync(pairs ...topology.ThemePair) ThemeResult {
	f.t.Helper()
	var result ThemeResult
	err := f.reconciler.WithExclusive(context.Background(), func(locked *Reconciler) error {
		var err error
		if pairs == nil {
			result, err = locked.SyncThemesLocked(context.Background(), nil)
		} else {
			result, err = locked.SyncThemesLocked(context.Background(), &pairs)
		}
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *themeFixture) pair() topology.ThemePair {
	return topology.ThemePair{VFSDir: "Show (2020)", SourceDir: f.show}
}

func regularFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file, so Silo would ignore it", path)
	}
	return info
}

func TestMediaFolderThemesAppearInTheVFS(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "root theme")
	write(t, filepath.Join(f.show, "theme2.mp3"), "numbered theme")
	write(t, filepath.Join(f.show, "Theme-Songs", "op.flac"), "opening")
	write(t, filepath.Join(f.show, "backdrops", "video.mp4"), "not audio")
	write(t, filepath.Join(f.show, "notes.mp3"), "not a theme")

	result := f.sync(f.pair())
	if result.Linked != 3 || len(result.Failures) != 0 {
		t.Fatalf("result = %#v", result)
	}
	for vfsName, source := range map[string]string{
		"theme.mp3":              "theme.mp3",
		"theme-music/theme2.mp3": "theme2.mp3",
		"theme-music/op.flac":    "Theme-Songs/op.flac",
	} {
		vfsInfo := regularFile(t, filepath.Join(f.vfsShow, vfsName))
		sourceInfo, _ := os.Stat(filepath.Join(f.show, source))
		if !os.SameFile(vfsInfo, sourceInfo) {
			t.Errorf("%s is not hardlinked to %s", vfsName, source)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.vfsShow, "theme-music", "notes.mp3")); !os.IsNotExist(err) {
		t.Fatal("a non-theme audio file was shared")
	}
	paths, _, err := f.reconciler.PollChanges(context.Background(), "1")
	if err != nil || len(paths) != 3 {
		t.Fatalf("journal = %v, %v", paths, err)
	}
	if again := f.sync(); again.Linked != 0 || again.WrittenBack != 0 || again.Removed != 0 {
		t.Fatalf("second sync changed something: %#v", again)
	}
}

func TestThemesWrittenIntoTheVFSAreWrittenBack(t *testing.T) {
	f := newThemeFixture(t)
	f.sync(f.pair())
	written := filepath.Join(f.vfsShow, "theme-music", "animethemes-op1.mp3")
	write(t, written, "downloaded")
	before := regularFile(t, written)

	result := f.sync()
	if result.WrittenBack != 1 || len(result.Failures) != 0 {
		t.Fatalf("result = %#v", result)
	}
	back := filepath.Join(f.show, "theme-music", "animethemes-op1.mp3")
	if read(t, back) != "downloaded" {
		t.Fatal("theme was not written back")
	}
	after := regularFile(t, written)
	if !os.SameFile(before, after) || after.ModTime() != before.ModTime() {
		t.Fatal("the VFS file Theme Songs wrote was changed")
	}
	if again := f.sync(); again.WrittenBack != 0 || again.Linked != 0 || again.Removed != 0 {
		t.Fatalf("second sync changed something: %#v", again)
	}
}

func TestWriteBackFollowsTheMediaFolderConvention(t *testing.T) {
	f := newThemeFixture(t)
	if err := os.MkdirAll(filepath.Join(f.show, "theme-songs"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.sync(f.pair())
	write(t, filepath.Join(f.vfsShow, "theme-music", "new.mp3"), "new")
	write(t, filepath.Join(f.vfsShow, "theme.ogg"), "root")
	f.sync()
	if read(t, filepath.Join(f.show, "theme-songs", "new.mp3")) != "new" || read(t, filepath.Join(f.show, "theme.ogg")) != "root" {
		t.Fatal("themes were not written where the media folder keeps them")
	}
	if _, err := os.Lstat(filepath.Join(f.show, "theme-music")); !os.IsNotExist(err) {
		t.Fatal("a second theme folder was created")
	}
}

func TestThemeSyncNeverOverwritesMediaFolderFiles(t *testing.T) {
	f := newThemeFixture(t)
	f.sync(f.pair())
	write(t, filepath.Join(f.show, "theme-music", "same.mp3"), "the user's file")
	write(t, filepath.Join(f.vfsShow, "theme-music", "same.mp3"), "a different download")
	result := f.sync()
	if len(result.Failures) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if read(t, filepath.Join(f.show, "theme-music", "same.mp3")) != "the user's file" || read(t, filepath.Join(f.vfsShow, "theme-music", "same.mp3")) != "a different download" {
		t.Fatal("a theme was overwritten")
	}
}

func TestThemeDeletedFromMediaFolderLeavesTheVFS(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "theme")
	f.sync(f.pair())
	if err := os.Remove(filepath.Join(f.show, "theme.mp3")); err != nil {
		t.Fatal(err)
	}
	if result := f.sync(); result.Removed != 1 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Lstat(filepath.Join(f.vfsShow, "theme.mp3")); !os.IsNotExist(err) {
		t.Fatal("deleted theme is still in the VFS")
	}
}

// An unmounted share looks like a folder whose themes all vanished. Nothing
// may be deleted while the media folder is unreachable.
func TestUnreachableMediaFolderRemovesNothing(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "theme")
	f.sync(f.pair())
	if err := os.Rename(f.show, f.show+".offline"); err != nil {
		t.Fatal(err)
	}
	if result := f.sync(); result.Removed != 0 {
		t.Fatalf("result = %#v", result)
	}
	regularFile(t, filepath.Join(f.vfsShow, "theme.mp3"))
}

func TestReplacedMediaFolderThemeIsUpdated(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "old")
	f.sync(f.pair())
	replacement := filepath.Join(f.media, "replacement.mp3")
	write(t, replacement, "new and longer")
	if err := os.Rename(replacement, filepath.Join(f.show, "theme.mp3")); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if read(t, filepath.Join(f.vfsShow, "theme.mp3")) != "new and longer" {
		t.Fatal("VFS still has the old theme")
	}
}

// After a show folder is renamed, copies this plugin placed in the old folder
// go, and files written into the VFS stay.
func TestRemovedPairCleansUpOnlyItsOwnCopies(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "from media")
	f.sync(f.pair())
	written := filepath.Join(f.vfsShow, "theme-music", "downloaded.mp3")
	write(t, written, "downloaded")
	f.sync()

	empty := []topology.ThemePair{}
	if err := f.reconciler.WithExclusive(context.Background(), func(locked *Reconciler) error {
		_, err := locked.SyncThemesLocked(context.Background(), &empty)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.vfsShow, "theme.mp3")); !os.IsNotExist(err) {
		t.Fatal("this plugin's copy was left behind")
	}
	regularFile(t, written)
}

func TestThemesAreCopiedAcrossFilesystems(t *testing.T) {
	previous := linkFile
	linkFile = func(string, string) error { return errors.New("invalid cross-device link") }
	defer func() { linkFile = previous }()

	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "theme")
	f.sync(f.pair())
	vfsInfo := regularFile(t, filepath.Join(f.vfsShow, "theme.mp3"))
	sourceInfo, _ := os.Stat(filepath.Join(f.show, "theme.mp3"))
	if os.SameFile(vfsInfo, sourceInfo) || read(t, filepath.Join(f.vfsShow, "theme.mp3")) != "theme" {
		t.Fatal("theme was not copied")
	}
	if again := f.sync(); again.Linked != 0 || again.WrittenBack != 0 {
		t.Fatalf("copy is not recognised as in sync: %#v", again)
	}
	write(t, filepath.Join(f.vfsShow, "theme-music", "new.mp3"), "new")
	if result := f.sync(); result.WrittenBack != 1 || read(t, filepath.Join(f.show, "theme-music", "new.mp3")) != "new" {
		t.Fatalf("copy write-back failed: %#v", result)
	}
}

func TestThemeSyncStaysInsideSourceRoots(t *testing.T) {
	f := newThemeFixture(t)
	outside := filepath.Join(filepath.Dir(f.media), "elsewhere")
	write(t, filepath.Join(outside, "theme.mp3"), "theme")
	result := f.sync(topology.ThemePair{VFSDir: "Show (2020)", SourceDir: outside})
	if len(result.Failures) != 1 || result.Linked != 0 {
		t.Fatalf("result = %#v", result)
	}
	write(t, filepath.Join(f.vfsShow, "theme-music", "x.mp3"), "x")
	f.sync()
	if _, err := os.Lstat(filepath.Join(outside, "theme-music")); !os.IsNotExist(err) {
		t.Fatal("wrote outside the source roots")
	}
}

func TestSymlinkedThemesAreNotShared(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.media, "real.mp3"), "real")
	if err := os.Symlink(filepath.Join(f.media, "real.mp3"), filepath.Join(f.show, "theme.mp3")); err != nil {
		t.Fatal(err)
	}
	if result := f.sync(f.pair()); result.Linked != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestThemeSyncLimitedToFoldersSyncsOnlyThose(t *testing.T) {
	f := newThemeFixture(t)
	write(t, filepath.Join(f.show, "theme.mp3"), "song")
	pairs := []topology.ThemePair{f.pair()}
	syncIn := func(only map[string]bool) ThemeResult {
		var result ThemeResult
		err := f.reconciler.WithExclusive(context.Background(), func(locked *Reconciler) error {
			var err error
			result, err = locked.SyncThemesLockedIn(context.Background(), &pairs, only)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := syncIn(map[string]bool{"Other (2021)": true}); result.Linked != 0 {
		t.Fatalf("sync of another folder linked %d themes", result.Linked)
	}
	if _, err := os.Lstat(filepath.Join(f.vfsShow, "theme.mp3")); !os.IsNotExist(err) {
		t.Fatalf("theme appeared without its folder being synced: %v", err)
	}
	if result := syncIn(map[string]bool{"Show (2020)": true}); result.Linked != 1 {
		t.Fatalf("sync of the show folder linked %d themes", result.Linked)
	}
	if read(t, filepath.Join(f.vfsShow, "theme.mp3")) != "song" {
		t.Fatal("theme not shared")
	}
}

func TestChangedThemeSyncWritesBackWhatWasWrittenIntoTheVFS(t *testing.T) {
	f := newThemeFixture(t)
	f.sync(f.pair())
	changed := func() ThemeResult {
		t.Helper()
		result, err := f.reconciler.SyncChangedThemes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	// Nothing changed in the VFS since the full sync.
	if result := changed(); result.WrittenBack != 0 || result.Linked != 0 {
		t.Fatalf("sync of unchanged folders = %#v", result)
	}

	// A theme added on the media side waits for the next full sync, which
	// is what keeps polls from reading every media folder...
	write(t, filepath.Join(f.show, "theme.mp3"), "song")
	if result := changed(); result.Linked != 0 {
		t.Fatalf("changed-folder sync read a media folder: %#v", result)
	}
	if result := f.sync(); result.Linked != 1 {
		t.Fatalf("full sync = %#v", result)
	}

	// ...while one written into the VFS, as Theme Songs does, is written
	// back, inside a theme-music folder that is new and then one that exists.
	for _, name := range []string{"animethemes-op1.mp3", "animethemes-ed1.mp3"} {
		write(t, filepath.Join(f.vfsShow, "theme-music", name), name)
		if result := changed(); result.WrittenBack != 1 || len(result.Failures) != 0 {
			t.Fatalf("changed-folder sync after writing %s = %#v", name, result)
		}
		if read(t, filepath.Join(f.show, "theme-music", name)) != name {
			t.Fatalf("%s was not written back", name)
		}
	}
	if result := changed(); result.WrittenBack != 0 || result.Linked != 0 {
		t.Fatalf("sync after write-back changed something: %#v", result)
	}
}
