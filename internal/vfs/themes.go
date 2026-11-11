package vfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
)

// Theme songs are kept in sync between each paired media folder and its VFS
// folder. Silo only plays a theme that is a regular file, either theme.<ext> in
// a show or season folder or any audio file in a real theme-music folder, so
// themes are hardlinked into the VFS rather than symlinked, and copied when the
// two folders are on different filesystems.
//
//   - A theme in the media folder appears in the VFS folder.
//   - A theme written into the VFS folder, for example by Theme Songs, is added
//     to the media folder. The VFS file stays as written.
//   - A theme deleted from the media folder is deleted from the VFS, but only
//     while the media folder itself is reachable.
//
// Nothing in a media folder is ever overwritten or deleted.

const (
	themeStateVersion = 1
	themeStateName    = ".shokoanime-themes.json"
	themeTempPrefix   = ".shokoanime-theme-"
	vfsThemeFolder    = "theme-music"
	maxThemeBytes     = 256 << 20
)

// themeAudioExtensions are the audio types Silo accepts as themes.
var themeAudioExtensions = map[string]struct{}{
	".mp3": {}, ".m4a": {}, ".m4b": {}, ".flac": {}, ".ogg": {}, ".opus": {}, ".wav": {}, ".aac": {},
}

// sourceThemeFolders are the folder names theme songs are commonly kept in,
// most preferred first. New themes written back go into the first one that
// exists, or theme-music.
var sourceThemeFolders = []string{"theme-music", "theme-songs", "themesongs", "theme songs", "theme music", "themes"}

// numberedTheme matches theme1.mp3, theme-2.mp3 and theme (3).mp3, which Silo
// only reads from a theme-music folder.
var numberedTheme = regexp.MustCompile(`(?i)^theme[ ._-]*\(?\d{1,3}\)?$`)

type themeRecord struct {
	VFSDir         string `json:"vfs_dir"`
	SourcePath     string `json:"source_path"`
	Origin         string `json:"origin"` // "source" or "vfs"
	SourceSize     int64  `json:"source_size"`
	SourceModified int64  `json:"source_modified"`
	VFSSize        int64  `json:"vfs_size"`
	VFSModified    int64  `json:"vfs_modified"`
}

type themeState struct {
	Version int                    `json:"version"`
	Pairs   []topology.ThemePair   `json:"pairs"`
	Files   map[string]themeRecord `json:"files"`
}

// ThemeResult summarises one theme sync.
type ThemeResult struct {
	Linked      int       `json:"linked"`
	WrittenBack int       `json:"written_back"`
	Removed     int       `json:"removed"`
	Failures    []Failure `json:"failures,omitempty"`
}

// SyncThemes syncs the theme songs of the pairs saved by the last reconcile.
func (r *Reconciler) SyncThemes(ctx context.Context) (ThemeResult, error) {
	var result ThemeResult
	if _, err := os.Lstat(r.config.Root); os.IsNotExist(err) {
		return result, nil
	}
	err := r.WithExclusive(ctx, func(locked *Reconciler) error {
		var err error
		result, err = locked.SyncThemesLocked(ctx, nil)
		return err
	})
	return result, err
}

// SyncThemesLocked syncs theme songs while the caller holds WithExclusive. A
// non-nil pairs replaces the saved pairs, as a reconcile does.
func (r *Reconciler) SyncThemesLocked(ctx context.Context, pairs *[]topology.ThemePair) (ThemeResult, error) {
	return r.SyncThemesLockedIn(ctx, pairs, nil)
}

// SyncThemesLockedIn is SyncThemesLocked limited to the pairs whose VFS
// folder is in only, such as the show and season folders whose links just
// changed. Every pair is still saved, and themes of pairs that are gone are
// still removed. Reading the media folder of every show costs too much on a
// slow share for each small update; a nil only syncs every pair.
func (r *Reconciler) SyncThemesLockedIn(ctx context.Context, pairs *[]topology.ThemePair, only map[string]bool) (ThemeResult, error) {
	var result ThemeResult
	state, err := r.readThemeState()
	if err != nil {
		return result, err
	}
	if pairs != nil {
		state.Pairs = append([]topology.ThemePair(nil), (*pairs)...)
	}
	current := make(map[string]struct{}, len(state.Pairs))
	var changed []string
	for _, pair := range state.Pairs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		current[pair.VFSDir] = struct{}{}
		if only != nil && !only[pair.VFSDir] {
			continue
		}
		changed = append(changed, r.syncThemePair(pair, state, &result)...)
	}
	// A pair can disappear when a show folder is renamed. Themes this plugin
	// placed there go with it; files written into the VFS stay.
	for key, record := range state.Files {
		if _, kept := current[record.VFSDir]; kept {
			continue
		}
		delete(state.Files, key)
		if record.Origin != "source" {
			continue
		}
		vfsPath, err := r.logicalPath(key)
		if err != nil {
			continue
		}
		if info, err := os.Lstat(vfsPath); err == nil && info.Mode().IsRegular() && sameStat(info, record.VFSSize, record.VFSModified) {
			if os.Remove(vfsPath) == nil {
				result.Removed++
				changed = append(changed, vfsPath)
			}
		}
	}
	if err := r.writeThemeState(state); err != nil {
		return result, err
	}
	return result, r.appendJournal(changed)
}

func (r *Reconciler) syncThemePair(pair topology.ThemePair, state *themeState, result *ThemeResult) []string {
	fail := func(key, target string, err error) {
		result.Failures = append(result.Failures, Failure{LogicalPath: key, TargetPath: target, Reason: err.Error()})
	}
	vfsDir, err := r.logicalPath(pair.VFSDir)
	if err != nil || checkDirectory(vfsDir) != nil {
		return nil // the VFS folder does not exist yet
	}
	sourceDir, err := r.themeSourceDir(pair.SourceDir)
	if err != nil {
		fail(pair.VFSDir, pair.SourceDir, err)
		return nil
	}
	if info, err := os.Stat(sourceDir); err != nil || !info.IsDir() {
		return nil // offline or moved: leave everything as it is
	}
	sources, duplicates := sourceThemes(sourceDir)
	for _, duplicate := range duplicates {
		fail(path.Join(pair.VFSDir, duplicate.name), duplicate.path, errors.New("another theme with the same name is already shared"))
	}
	inVFS := vfsThemes(vfsDir)

	var changed []string
	for _, name := range sortedKeys(sources) {
		sourcePath := sources[name]
		key := path.Join(pair.VFSDir, name)
		vfsPath := filepath.Join(vfsDir, filepath.FromSlash(name))
		sourceInfo, err := os.Lstat(sourcePath)
		if err != nil || !sourceInfo.Mode().IsRegular() {
			continue
		}
		record, recorded := state.Files[key]
		vfsInfo, present := inVFS[name]
		delete(inVFS, name)
		switch {
		case !present:
			if err := r.placeTheme(sourcePath, vfsDir, name, false); err != nil {
				fail(key, sourcePath, err)
				continue
			}
			origin := "source"
			if recorded {
				origin = record.Origin
			}
			r.recordTheme(state, key, pair.VFSDir, sourcePath, origin, vfsPath)
			result.Linked++
			changed = append(changed, vfsPath)
		case recorded && sameStat(sourceInfo, record.SourceSize, record.SourceModified) && sameStat(vfsInfo, record.VFSSize, record.VFSModified):
			// In sync.
		case recorded && sameStat(vfsInfo, record.VFSSize, record.VFSModified):
			// Replaced in the media folder: replace the VFS copy.
			if err := r.placeTheme(sourcePath, vfsDir, name, true); err != nil {
				fail(key, sourcePath, err)
				continue
			}
			r.recordTheme(state, key, pair.VFSDir, sourcePath, record.Origin, vfsPath)
			result.Linked++
			changed = append(changed, vfsPath)
		default:
			if !sameContent(sourcePath, vfsPath) {
				fail(key, sourcePath, errors.New("the VFS folder has a different theme with the same name"))
				continue
			}
			origin := "source"
			if recorded {
				origin = record.Origin
			}
			r.recordTheme(state, key, pair.VFSDir, sourcePath, origin, vfsPath)
		}
	}

	for _, name := range sortedKeys(inVFS) {
		vfsInfo := inVFS[name]
		key := path.Join(pair.VFSDir, name)
		vfsPath := filepath.Join(vfsDir, filepath.FromSlash(name))
		if record, recorded := state.Files[key]; recorded {
			// Removed from the media folder, which is reachable: remove the copy,
			// unless the VFS file changed since, which makes it new.
			delete(state.Files, key)
			if sameStat(vfsInfo, record.VFSSize, record.VFSModified) {
				if err := os.Remove(vfsPath); err != nil {
					fail(key, vfsPath, err)
					continue
				}
				result.Removed++
				changed = append(changed, vfsPath)
				continue
			}
		}
		destination, err := writeBackPath(sourceDir, name)
		if err != nil {
			fail(key, sourceDir, err)
			continue
		}
		if _, err := os.Lstat(destination); err == nil {
			if !sameContent(vfsPath, destination) {
				fail(key, destination, errors.New("the media folder has a different theme with the same name"))
				continue
			}
		} else if err := copyOrLink(vfsPath, destination, false); err != nil {
			fail(key, destination, err)
			continue
		} else {
			result.WrittenBack++
		}
		r.recordTheme(state, key, pair.VFSDir, destination, "vfs", vfsPath)
	}
	return changed
}

// themeSourceDir confines a theme folder to the allowed source roots, also
// after resolving symlinks.
func (r *Reconciler) themeSourceDir(dir string) (string, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return "", ErrOutsideRoot
	}
	for _, root := range r.config.AllowedSourceRoots {
		if !strictUnder(dir, root) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return dir, nil // missing: the caller skips it
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			resolvedRoot = root
		}
		if !strictUnder(resolved, resolvedRoot) {
			return "", ErrOutsideRoot
		}
		return dir, nil
	}
	return "", ErrOutsideRoot
}

type duplicateTheme struct{ name, path string }

// sourceThemes lists a media folder's themes by the name they get in the VFS
// folder: theme.<ext> stays at the top and everything else goes in theme-music.
func sourceThemes(dir string) (map[string]string, []duplicateTheme) {
	themes := make(map[string]string)
	var duplicates []duplicateTheme
	add := func(name, filePath string) {
		if _, taken := themes[name]; taken {
			duplicates = append(duplicates, duplicateTheme{name, filePath})
			return
		}
		themes[name] = filePath
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return themes, nil
	}
	folders := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			folders[strings.ToLower(name)] = name
			continue
		}
		if !entry.Type().IsRegular() || !isThemeAudio(name) {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		switch {
		case strings.EqualFold(stem, "theme"):
			add("theme"+strings.ToLower(filepath.Ext(name)), filepath.Join(dir, name))
		case numberedTheme.MatchString(stem):
			add(vfsThemeFolder+"/"+name, filepath.Join(dir, name))
		}
	}
	for _, folder := range sourceThemeFolders {
		actual, ok := folders[folder]
		if !ok {
			continue
		}
		inside, err := os.ReadDir(filepath.Join(dir, actual))
		if err != nil {
			continue
		}
		for _, entry := range inside {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || !entry.Type().IsRegular() || !isThemeAudio(name) {
				continue
			}
			add(vfsThemeFolder+"/"+name, filepath.Join(dir, actual, name))
		}
	}
	return themes, duplicates
}

// vfsThemes lists the theme files in a VFS folder where Silo reads them.
func vfsThemes(dir string) map[string]fs.FileInfo {
	themes := make(map[string]fs.FileInfo)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return themes
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() && strings.EqualFold(name, vfsThemeFolder) {
			inside, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			for _, file := range inside {
				if strings.HasPrefix(file.Name(), ".") || !file.Type().IsRegular() || !isThemeAudio(file.Name()) {
					continue
				}
				if info, err := file.Info(); err == nil {
					themes[vfsThemeFolder+"/"+file.Name()] = info
				}
			}
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if entry.Type().IsRegular() && isThemeAudio(name) && strings.EqualFold(stem, "theme") {
			if info, err := entry.Info(); err == nil {
				themes["theme"+strings.ToLower(filepath.Ext(name))] = info
			}
		}
	}
	return themes
}

func isThemeAudio(name string) bool {
	_, ok := themeAudioExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

// writeBackPath is where a theme written into the VFS goes in the media
// folder, following the folder the media already uses.
func writeBackPath(sourceDir, name string) (string, error) {
	if !strings.Contains(name, "/") {
		return filepath.Join(sourceDir, name), nil
	}
	file := path.Base(name)
	entries, _ := os.ReadDir(sourceDir)
	existing := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			existing[strings.ToLower(entry.Name())] = entry.Name()
		}
	}
	for _, folder := range sourceThemeFolders {
		if actual, ok := existing[folder]; ok {
			return filepath.Join(sourceDir, actual, file), nil
		}
	}
	folder := filepath.Join(sourceDir, vfsThemeFolder)
	if err := os.Mkdir(folder, 0o777); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("vfs: create theme folder: %w", err)
	}
	if info, err := os.Lstat(folder); err != nil || !info.IsDir() {
		return "", fmt.Errorf("vfs: %s is not a folder", folder)
	}
	return filepath.Join(folder, file), nil
}

// placeTheme puts a media-folder theme into the VFS folder.
func (r *Reconciler) placeTheme(sourcePath, vfsDir, name string, replace bool) error {
	destination := filepath.Join(vfsDir, filepath.FromSlash(name))
	if err := ensureParents(r.config.Root, filepath.Dir(destination)); err != nil {
		return err
	}
	return copyOrLink(sourcePath, destination, replace)
}

// linkFile is os.Link, replaceable in tests to exercise the copy fallback.
var linkFile = os.Link

// copyOrLink hardlinks source to destination, or copies it when they are on
// different filesystems. Without replace an existing destination is never
// overwritten.
func copyOrLink(source, destination string, replace bool) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("vfs: theme is not a regular file")
	}
	if info.Size() > maxThemeBytes {
		return errors.New("vfs: theme file is too large")
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(destination), themeTempPrefix+hex.EncodeToString(random))
	if err := linkFile(source, temp); err != nil {
		if err := copyFile(source, temp); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	defer os.Remove(temp)
	if replace {
		return os.Rename(temp, destination)
	}
	if err := os.Link(temp, destination); err != nil {
		if os.IsExist(err) {
			return errors.New("vfs: a different theme with the same name already exists")
		}
		return err
	}
	return nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxThemeBytes+1)); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (r *Reconciler) recordTheme(state *themeState, key, vfsDir, sourcePath, origin, vfsPath string) {
	record := themeRecord{VFSDir: vfsDir, SourcePath: sourcePath, Origin: origin}
	if info, err := os.Lstat(sourcePath); err == nil {
		record.SourceSize, record.SourceModified = info.Size(), info.ModTime().UnixNano()
	}
	if info, err := os.Lstat(vfsPath); err == nil {
		record.VFSSize, record.VFSModified = info.Size(), info.ModTime().UnixNano()
	}
	state.Files[key] = record
}

func sameStat(info fs.FileInfo, size, modified int64) bool {
	return info.Size() == size && info.ModTime().UnixNano() == modified
}

// sameContent reports whether two files hold the same bytes.
func sameContent(a, b string) bool {
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	if aErr != nil || bErr != nil || aInfo.Size() != bInfo.Size() {
		return false
	}
	if os.SameFile(aInfo, bInfo) {
		return true
	}
	aSum, aErr := fileHash(a)
	bSum, bErr := fileHash(b)
	return aErr == nil && bErr == nil && aSum == bSum
}

func fileHash(file string) (string, error) {
	in, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer in.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(in, maxThemeBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (r *Reconciler) themeStatePath() string {
	return filepath.Join(r.config.Root, themeStateName)
}

func (r *Reconciler) readThemeState() (*themeState, error) {
	state := &themeState{Version: themeStateVersion, Files: make(map[string]themeRecord)}
	statePath := r.themeStatePath()
	info, err := os.Lstat(statePath)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vfs: inspect theme state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("vfs: theme state is not a regular file")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("vfs: read theme state: %w", err)
	}
	if err := json.Unmarshal(data, state); err != nil || state.Version != themeStateVersion {
		// Unreadable state only costs re-adoption: matching files are recognised
		// again by content.
		return &themeState{Version: themeStateVersion, Files: make(map[string]themeRecord)}, nil
	}
	if state.Files == nil {
		state.Files = make(map[string]themeRecord)
	}
	return state, nil
}

func (r *Reconciler) writeThemeState(state *themeState) error {
	state.Version = themeStateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("vfs: encode theme state: %w", err)
	}
	tmpFile, err := os.CreateTemp(r.config.Root, ".shokoanime-themes-*")
	if err != nil {
		return fmt.Errorf("vfs: create theme state temp file: %w", err)
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if _, err := tmpFile.Write(append(data, '\n')); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("vfs: write theme state: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("vfs: close theme state: %w", err)
	}
	if err := os.Rename(tmp, r.themeStatePath()); err != nil {
		return fmt.Errorf("vfs: install theme state: %w", err)
	}
	return nil
}
