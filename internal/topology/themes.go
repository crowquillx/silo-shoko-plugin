package topology

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ThemePair links a VFS folder, relative to the VFS root, to the media folder
// whose theme songs it mirrors.
type ThemePair struct {
	VFSDir    string `json:"vfs_dir"`
	SourceDir string `json:"source_dir"`
}

var seasonFolderName = regexp.MustCompile(`(?i)^(?:season[ ._-]*\d{1,4}|specials?)$`)

// ThemePairs finds the media folders that clearly belong to a VFS show or
// season, so their theme songs can be shared. A folder qualifies only when every
// Shoko file under it belongs to the same show (or, for a season, the same
// series), and never when it is a managed-folder root, so a shared download
// folder or a flat library is never touched.
//
//   - A show's folder is the deepest folder holding all of the show's files. A
//     single "Season N" folder there means the show folder is its parent.
//   - A season's folder is the one folder holding all of its series' files,
//     when that is not also the show's folder.
func ThemePairs(entries []Entry) []ThemePair {
	type series struct {
		vfsDir string
		dirs   map[string]struct{}
	}
	type show struct {
		vfsDir string
		root   string
		dirs   map[string]struct{}
		series map[int]*series
	}
	shows := make(map[string]*show)
	dirSeries := make(map[string]map[int]struct{})
	treeShows := make(map[string]map[string]struct{})
	for _, entry := range entries {
		if entry.SourceRoot == "" || entry.TargetPath == "" {
			continue
		}
		parts := strings.Split(filepath.ToSlash(entry.LogicalPath), "/")
		if len(parts) < 3 {
			continue
		}
		dir := filepath.Dir(filepath.Clean(entry.TargetPath))
		key := parts[0]
		current := shows[key]
		if current == nil {
			current = &show{vfsDir: parts[0], root: entry.SourceRoot, dirs: map[string]struct{}{}, series: map[int]*series{}}
			shows[key] = current
		}
		if current.root != entry.SourceRoot {
			current.root = "" // spread over managed folders: no common media folder
		}
		current.dirs[dir] = struct{}{}
		if entry.SeasonNumber > 0 {
			member := current.series[entry.ShokoSeriesID]
			if member == nil {
				member = &series{vfsDir: filepath.ToSlash(filepath.Join(parts[0], parts[1])), dirs: map[string]struct{}{}}
				current.series[entry.ShokoSeriesID] = member
			}
			member.dirs[dir] = struct{}{}
		}
		if dirSeries[dir] == nil {
			dirSeries[dir] = map[int]struct{}{}
		}
		dirSeries[dir][entry.ShokoSeriesID] = struct{}{}
		for ancestor := dir; strictlyUnder(ancestor, entry.SourceRoot); ancestor = filepath.Dir(ancestor) {
			if treeShows[ancestor] == nil {
				treeShows[ancestor] = map[string]struct{}{}
			}
			treeShows[ancestor][key] = struct{}{}
		}
	}

	ownedBy := func(dir, key, root string) bool {
		owners := treeShows[dir]
		_, owned := owners[key]
		return root != "" && strictlyUnder(dir, root) && len(owners) == 1 && owned
	}
	var pairs []ThemePair
	for key, current := range shows {
		showDir := commonDir(current.dirs)
		if !ownedBy(showDir, key, current.root) {
			showDir = ""
		}
		if showDir != "" && len(current.dirs) == 1 && seasonFolderName.MatchString(filepath.Base(showDir)) && ownedBy(filepath.Dir(showDir), key, current.root) {
			showDir = filepath.Dir(showDir)
		}
		if showDir != "" {
			pairs = append(pairs, ThemePair{VFSDir: current.vfsDir, SourceDir: showDir})
		}
		for id, member := range current.series {
			if len(member.dirs) != 1 {
				continue
			}
			var dir string
			for only := range member.dirs {
				dir = only
			}
			if _, alone := dirSeries[dir][id]; !alone || len(dirSeries[dir]) != 1 || dir == showDir || !ownedBy(dir, key, current.root) {
				continue
			}
			pairs = append(pairs, ThemePair{VFSDir: member.vfsDir, SourceDir: dir})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].VFSDir < pairs[j].VFSDir })
	return pairs
}

func commonDir(dirs map[string]struct{}) string {
	var common string
	for dir := range dirs {
		if common == "" {
			common = dir
			continue
		}
		for common != dir && !strictlyUnder(dir, common) {
			parent := filepath.Dir(common)
			if parent == common {
				return ""
			}
			common = parent
		}
	}
	return common
}

func strictlyUnder(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
