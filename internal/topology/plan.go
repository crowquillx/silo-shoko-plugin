// Package topology turns a Shoko graph into a deterministic logical library.
// It has no filesystem or Silo SDK dependency, which lets the same plan later
// be rendered by a native catalog source instead of symlinks.
package topology

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

type Mode string

// AniDBAnimeMarker carries the series' AniDB anime ID in each link name, so
// other plugins, such as Theme Songs, can identify the anime of a season from
// its files. It is a contract: keep the format stable.
const AniDBAnimeMarker = "[AniDB Anime=%d]"

const ModeAniDB Mode = "anidb"

type Policy struct {
	Mode              Mode
	VFSRoot           string
	ManagedFolderMap  map[int]string
	IncludeIgnored    bool
	IncludeVariations bool
}

type Kind string

const (
	KindEpisode Kind = "episode"
)

type Entry struct {
	StableKey   string `json:"stable_key"`
	LogicalPath string `json:"logical_path"`
	TargetPath  string `json:"target_path"`
	SourcePath  string `json:"source_path"`
	// SourceRoot is the mapped managed-folder root the target is under.
	SourceRoot     string `json:"source_root,omitempty"`
	Kind           Kind   `json:"kind"`
	ShokoFileID    int    `json:"shoko_file_id"`
	ShokoGroupID   int    `json:"shoko_group_id,omitempty"`
	ShokoSeriesID  int    `json:"shoko_series_id"`
	ShokoEpisodeID int    `json:"shoko_episode_id"`
	SeasonNumber   int    `json:"season_number,omitempty"`
	EpisodeNumber  int    `json:"episode_number,omitempty"`
}

type Diagnostic struct {
	FileID   int    `json:"file_id,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type Plan struct {
	Entries     []Entry      `json:"entries"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// ThemePairs links VFS show and season folders to the media folders their
	// theme songs are kept in sync with.
	ThemePairs []ThemePair `json:"theme_pairs,omitempty"`
}

func (p Policy) Validate() error {
	if p.Mode == "" {
		p.Mode = ModeAniDB
	}
	if p.Mode != ModeAniDB {
		return fmt.Errorf("topology: unsupported mode %q", p.Mode)
	}
	if p.VFSRoot == "" || !filepath.IsAbs(p.VFSRoot) {
		return errors.New("topology: vfs root must be absolute")
	}
	if filepath.Clean(p.VFSRoot) == string(filepath.Separator) {
		return errors.New("topology: refusing filesystem root")
	}
	if len(p.ManagedFolderMap) == 0 {
		return errors.New("topology: managed-folder map is empty")
	}
	return nil
}

// Build creates one logical leaf entry per Shoko file/episode binding. A
// single physical file may consequently appear at multiple episode paths,
// matching the behavior Silo's scanner already supports for leaf symlinks.
func Build(snapshot shoko.Snapshot, policy Policy) (Plan, error) {
	if err := policy.Validate(); err != nil {
		return Plan{}, err
	}
	plan := Plan{Entries: make([]Entry, 0)}
	seenKeys := make(map[string]struct{})
	groupLayouts := newSnapshotGroupLayouts(snapshot)
	seriesGroups := newSeriesGroups(snapshot)
	// A variation is another version of an episode, so it is left out where
	// another file already gives that episode a link. But Shoko users also
	// mark the parts of an episode split across files as variations, to clear
	// them from Shoko's release review, and those parts are the episode's only
	// files. So variations are added last, and only for episodes that still
	// have no link.
	covered := make(map[int]struct{})
	for _, variations := range []bool{false, true} {
		for _, file := range snapshot.Files {
			if file.IsIgnored && !policy.IncludeIgnored {
				continue
			}
			if file.IsVariation != variations {
				continue
			}
			plan.addFile(snapshot, policy, file, covered, seenKeys, groupLayouts, seriesGroups)
		}
	}
	sort.Slice(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].LogicalPath == plan.Entries[j].LogicalPath {
			return plan.Entries[i].StableKey < plan.Entries[j].StableKey
		}
		return plan.Entries[i].LogicalPath < plan.Entries[j].LogicalPath
	})
	plan.ThemePairs = ThemePairs(plan.Entries)
	return plan, nil
}

// addFile adds an entry for each episode the file is linked to. Unless the
// policy includes variations, a variation is skipped for the episodes in
// covered: those a file that is not a variation already has a link for.
func (plan *Plan) addFile(snapshot shoko.Snapshot, policy Policy, file shoko.File, covered map[int]struct{}, seenKeys map[string]struct{}, groupLayouts map[int]GroupLayout, seriesGroups map[int]int) {
	skipCovered := file.IsVariation && !policy.IncludeVariations
	location, ok, diagnostic := chooseLocation(file, policy.ManagedFolderMap)
	if !ok {
		if diagnostic.Message != "" {
			plan.Diagnostics = append(plan.Diagnostics, diagnostic)
		}
		return
	}
	relative, safe := safeRelativePath(location.RelativePath)
	if !safe {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{
			FileID:   file.ID,
			Message:  fmt.Sprintf("managed-folder relative path %q escapes its source root", location.RelativePath),
			Severity: "error",
		})
		return
	}
	sourceRoot := policy.ManagedFolderMap[location.ManagedFolderID]
	for _, reference := range file.SeriesIDs {
		seriesID := 0
		if reference.SeriesID.ID != nil {
			seriesID = *reference.SeriesID.ID
		}
		for _, episodeReference := range reference.EpisodeIDs {
			episode, episodeID, ok := resolveEpisode(snapshot, seriesID, episodeReference)
			if !ok {
				plan.Diagnostics = append(plan.Diagnostics, Diagnostic{
					FileID:   file.ID,
					Message:  fmt.Sprintf("could not resolve episode xref (shoko id %d, AniDB id %d)", pointerValue(episodeReference.ID), episodeReference.AniDB),
					Severity: "warning",
				})
				continue
			}
			if _, done := covered[episodeID]; done && skipCovered {
				continue
			}
			if seriesID == 0 {
				seriesID = episode.IDs.ParentSeries
			}
			series, seriesOK := snapshot.Series[seriesID]
			if !seriesOK {
				plan.Diagnostics = append(plan.Diagnostics, Diagnostic{
					FileID:   file.ID,
					Message:  fmt.Sprintf("episode %d references missing series %d", episodeID, seriesID),
					Severity: "warning",
				})
				continue
			}
			groupID := seriesGroups[seriesID]
			group := snapshot.Groups[groupID]
			entry := makeEntry(sourceRoot, relative, file, series, seriesID, episode, episodeID, group, groupID, groupLayouts[groupID], snapshot)
			if _, seen := seenKeys[entry.StableKey]; seen {
				continue
			}
			seenKeys[entry.StableKey] = struct{}{}
			plan.Entries = append(plan.Entries, entry)
			if !file.IsVariation {
				covered[episodeID] = struct{}{}
			}
		}
	}
}

// newSeriesGroups maps each series to the group it is a season of: the group
// the series itself names as its parent, when that group lists it, and
// otherwise the lowest-numbered known group that lists it. A series can be in
// two lists while a move between groups is only half read.
func newSeriesGroups(snapshot shoko.Snapshot) map[int]int {
	groups := make(map[int]int, len(snapshot.Series))
	for groupID, memberIDs := range snapshot.GroupSeries {
		if _, known := snapshot.Groups[groupID]; !known {
			continue
		}
		for _, seriesID := range memberIDs {
			current, taken := groups[seriesID]
			if taken && current == snapshot.Series[seriesID].IDs.ParentGroup {
				continue
			}
			if !taken || groupID < current || groupID == snapshot.Series[seriesID].IDs.ParentGroup {
				groups[seriesID] = groupID
			}
		}
	}
	return groups
}

func completeAirDate(series shoko.Series) (time.Time, bool) {
	if series.AniDB == nil {
		return time.Time{}, false
	}
	date, err := time.Parse("2006-01-02", series.AniDB.AirDate)
	return date, err == nil
}

// chooseLocation picks the location of a file that its links lead to: one in a
// mapped managed folder, preferably accessible. Among those it prefers the
// file itself to a symbolic link to it, whose target is a path as Shoko sees
// it and may not resolve where Silo reads the VFS, then a location of a group
// daily Shoko keeps rather than counts as a duplicate, then the primary of its
// group, and otherwise Shoko's order. A file with one location, or from a
// stable server, which sends none of these, gets its first accessible one.
func chooseLocation(file shoko.File, roots map[int]string) (shoko.Location, bool, Diagnostic) {
	var best, fallback *shoko.Location
	for i := range file.Locations {
		location := &file.Locations[i]
		root, mapped := roots[location.ManagedFolderID]
		if !mapped || strings.TrimSpace(root) == "" {
			continue
		}
		if location.IsAccessible {
			if best == nil || locationRank(*location) > locationRank(*best) {
				best = location
			}
		} else if fallback == nil || locationRank(*location) > locationRank(*fallback) {
			fallback = location
		}
	}
	if best != nil {
		return *best, true, Diagnostic{}
	}
	if fallback == nil {
		return shoko.Location{}, false, Diagnostic{
			FileID:   file.ID,
			Message:  "file has no location in a configured managed-folder map",
			Severity: "warning",
		}
	}
	return *fallback, true, Diagnostic{
		FileID:   file.ID,
		Message:  "using an inaccessible Shoko location as a planned target",
		Severity: "warning",
	}
}

// locationRank orders a file's locations for chooseLocation; higher is
// better.
func locationRank(location shoko.Location) int {
	rank := 0
	if !location.IsSymbolicLink {
		rank += 4
	}
	if location.IsPreferredGroup {
		rank += 2
	}
	if location.PrimaryID != 0 && location.PrimaryID == location.ID {
		rank++
	}
	return rank
}

func resolveEpisode(snapshot shoko.Snapshot, seriesID int, reference shoko.EpisodeCrossReferenceIDs) (shoko.Episode, int, bool) {
	if reference.ID != nil {
		if episode, ok := snapshot.Episodes[*reference.ID]; ok {
			return episode, *reference.ID, true
		}
	}
	for id, episode := range snapshot.Episodes {
		if seriesID != 0 && episode.IDs.ParentSeries != seriesID {
			continue
		}
		if episode.AniDB != nil && reference.AniDB != 0 && episode.AniDB.ID == reference.AniDB {
			return episode, id, true
		}
		if episode.IDs.AniDB != 0 && episode.IDs.AniDB == reference.AniDB {
			return episode, id, true
		}
	}
	return shoko.Episode{}, 0, false
}

func makeEntry(sourceRoot, relative string, file shoko.File, series shoko.Series, seriesID int, episode shoko.Episode, episodeID int, group shoko.Group, groupID int, groupLayout GroupLayout, snapshot shoko.Snapshot) Entry {
	relative = filepath.FromSlash(relative)
	target := filepath.Join(sourceRoot, relative)
	ext := filepath.Ext(relative)
	seriesSegment := rootSegment(cleanSegment(series.Name), series.AniDB.AirYear())
	if groupID != 0 {
		seriesSegment = groupRootSegment(snapshot, group, groupID, series)
	}
	groupMarker := ""
	if groupID != 0 {
		groupMarker = fmt.Sprintf(" [Shoko Group=%d]", groupID)
	}
	season, number := EpisodeNumbers(episode)
	if groupID != 0 {
		season, number = groupLayout.Position(series, episode)
	}
	anidbMarker := ""
	if series.IDs.AniDB > 0 {
		anidbMarker = fmt.Sprintf(" "+AniDBAnimeMarker, series.IDs.AniDB)
	}
	suffix := fmt.Sprintf(" - S%02dE%02d%s [Shoko Series=%d]%s [Shoko Episode=%d] [Shoko File=%d]%s", season, number, groupMarker, seriesID, anidbMarker, episodeID, file.ID, ext)
	name := truncateBytes(fileTitle(series.Name), maxNameBytes-len(suffix)) + suffix
	return Entry{
		StableKey:      stableKey(file.ID, episodeID),
		LogicalPath:    filepath.Join(seriesSegment, fmt.Sprintf("Season %02d", season), cleanSegment(name)),
		TargetPath:     target,
		SourcePath:     target,
		SourceRoot:     filepath.Clean(sourceRoot),
		Kind:           KindEpisode,
		ShokoFileID:    file.ID,
		ShokoGroupID:   groupID,
		ShokoSeriesID:  seriesID,
		ShokoEpisodeID: episodeID,
		SeasonNumber:   season,
		EpisodeNumber:  number,
	}
}

func groupRootSegment(snapshot shoko.Snapshot, group shoko.Group, groupID int, current shoko.Series) string {
	return rootSegment(cleanSegment(group.Name), groupYear(snapshot, group, groupID, current))
}

func groupYear(snapshot shoko.Snapshot, group shoko.Group, groupID int, current shoko.Series) int {
	if group.IDs.MainSeries != 0 {
		if main, ok := snapshot.Series[group.IDs.MainSeries]; ok {
			if year := main.AniDB.AirYear(); year != 0 {
				return year
			}
		}
	}
	earliest := time.Time{}
	hasEarliest := false
	seen := make(map[int]struct{}, len(snapshot.GroupSeries[groupID]))
	for _, memberID := range snapshot.GroupSeries[groupID] {
		if _, duplicate := seen[memberID]; duplicate {
			continue
		}
		seen[memberID] = struct{}{}
		member, ok := snapshot.Series[memberID]
		if !ok {
			continue
		}
		date, complete := completeAirDate(member)
		if complete && (!hasEarliest || date.Before(earliest)) {
			earliest, hasEarliest = date, true
		}
	}
	if hasEarliest {
		return earliest.Year()
	}
	return current.AniDB.AirYear()
}

func rootSegment(title string, year int) string {
	if title == "" {
		title = "Unknown Series"
	}
	if year == 0 {
		return truncateBytes(title, maxNameBytes)
	}
	yearToken := fmt.Sprintf("(%d)", year)
	if strings.Contains(title, yearToken) {
		return truncateBytes(title, maxNameBytes)
	}
	return truncateBytes(title, maxNameBytes-len(yearToken)-1) + " " + yearToken
}

// maxNameBytes is the file name limit of common Linux and macOS filesystems.
const maxNameBytes = 255

// truncateBytes shortens value to at most limit bytes without splitting a
// character. The Shoko ID markers keep shortened names unique.
func truncateBytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

// partTokenPattern matches the tokens Silo reads as "part N of a split file".
var partTokenPattern = regexp.MustCompile(`(?i)(?:^|[.\-_\s])(?:cd|disc|part|pt)(?:\s*|[._-]?)\d{1,2}(?:$|[.\-_\s])`)

// fileTitle is the series title used in link names. Silo takes the show's title
// from the folder, so a token like "Part 2" that belongs to the series name is
// removed here to keep it from being misread as part 2 of a split episode.
func fileTitle(name string) string {
	title := cleanSegment(name)
	for {
		stripped := partTokenPattern.ReplaceAllString(title, " ")
		if stripped == title {
			break
		}
		title = stripped
	}
	return cleanSegment(title)
}

func stableKey(fileID, episodeID int) string {
	return "file:" + strconv.Itoa(fileID) + "/episode:" + strconv.Itoa(episodeID)
}

func safeRelativePath(value string) (string, bool) {
	// Daily servers report paths with a leading slash and stable servers without
	// one. Either way the path is relative to the managed folder.
	value = strings.TrimLeft(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
	if value == "" {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return filepath.FromSlash(cleaned), true
}

func pointerValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func cleanSegment(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			builder.WriteByte('_')
		case unicode.IsControl(r):
			builder.WriteByte('_')
		default:
			builder.WriteRune(r)
		}
	}
	// Silo's scanner skips dot-prefixed folders, which would hide .hack entirely.
	value = strings.TrimSpace(strings.Trim(strings.TrimSpace(builder.String()), "."))
	if value == "" {
		return "Unknown"
	}
	return value
}
