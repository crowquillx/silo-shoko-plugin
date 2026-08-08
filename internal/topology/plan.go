// Package topology turns a Shoko graph into a deterministic logical library.
// It has no filesystem or Silo SDK dependency, which lets the same plan later
// be rendered by a native catalog source instead of symlinks.
package topology

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

type Mode string

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
	StableKey      string `json:"stable_key"`
	LogicalPath    string `json:"logical_path"`
	TargetPath     string `json:"target_path"`
	SourcePath     string `json:"source_path"`
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
	for _, file := range snapshot.Files {
		if file.IsIgnored && !policy.IncludeIgnored {
			continue
		}
		if file.IsVariation && !policy.IncludeVariations {
			continue
		}
		location, ok, diagnostic := chooseLocation(file, policy.ManagedFolderMap)
		if !ok {
			if diagnostic.Message != "" {
				plan.Diagnostics = append(plan.Diagnostics, diagnostic)
			}
			continue
		}
		relative, safe := safeRelativePath(location.RelativePath)
		if !safe {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{
				FileID:   file.ID,
				Message:  fmt.Sprintf("managed-folder relative path %q escapes its source root", location.RelativePath),
				Severity: "error",
			})
			continue
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
				group, groupID, grouped := groupForSeries(snapshot, seriesID)
				season := 0
				if grouped {
					season = groupLayouts[groupID].SeasonNumber(seriesID)
				}
				entry := makeEntry(sourceRoot, relative, file, series, seriesID, episode, episodeID, group, groupID, season, groupLayouts[groupID], snapshot)
				if _, seen := seenKeys[entry.StableKey]; seen {
					continue
				}
				seenKeys[entry.StableKey] = struct{}{}
				plan.Entries = append(plan.Entries, entry)
			}
		}
	}
	sort.Slice(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].LogicalPath == plan.Entries[j].LogicalPath {
			return plan.Entries[i].StableKey < plan.Entries[j].StableKey
		}
		return plan.Entries[i].LogicalPath < plan.Entries[j].LogicalPath
	})
	return plan, nil
}

func groupForSeries(snapshot shoko.Snapshot, seriesID int) (shoko.Group, int, bool) {
	groupIDs := make([]int, 0)
	for groupID, seriesIDs := range snapshot.GroupSeries {
		for _, memberID := range seriesIDs {
			if memberID == seriesID {
				groupIDs = append(groupIDs, groupID)
				break
			}
		}
	}
	sort.Ints(groupIDs)
	for _, groupID := range groupIDs {
		group, ok := snapshot.Groups[groupID]
		if ok {
			return group, groupID, true
		}
	}
	return shoko.Group{}, 0, false
}

func completeAirDate(series shoko.Series) (time.Time, bool) {
	if series.AniDB == nil {
		return time.Time{}, false
	}
	date, err := time.Parse("2006-01-02", series.AniDB.AirDate)
	return date, err == nil
}

func chooseLocation(file shoko.File, roots map[int]string) (shoko.Location, bool, Diagnostic) {
	var fallback *shoko.Location
	for i := range file.Locations {
		location := &file.Locations[i]
		root, mapped := roots[location.ManagedFolderID]
		if !mapped || strings.TrimSpace(root) == "" {
			continue
		}
		if fallback == nil {
			fallback = location
		}
		if location.IsAccessible {
			return *location, true, Diagnostic{}
		}
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

func makeEntry(sourceRoot, relative string, file shoko.File, series shoko.Series, seriesID int, episode shoko.Episode, episodeID int, group shoko.Group, groupID, groupedSeasonNumber int, groupLayout GroupLayout, snapshot shoko.Snapshot) Entry {
	relative = filepath.FromSlash(relative)
	target := filepath.Join(sourceRoot, relative)
	ext := filepath.Ext(relative)
	baseTitle := cleanSegment(series.Name)
	seriesSegment := rootSegment(baseTitle, series.AniDB.AirYear())
	if groupID != 0 {
		seriesSegment = groupRootSegment(snapshot, group, groupID, series)
	}
	groupMarker := ""
	if groupID != 0 {
		groupMarker = fmt.Sprintf(" [Shoko Group=%d]", groupID)
	}
	season, number := episodeNumbers(episode)
	if groupID != 0 {
		season, number = groupLayout.Position(series, episode)
	}
	name := fmt.Sprintf("%s - S%02dE%02d%s [Shoko Series=%d] [Shoko Episode=%d] [Shoko File=%d]%s", baseTitle, season, number, groupMarker, seriesID, episodeID, file.ID, ext)
	return Entry{
		StableKey:      stableKey(file.ID, episodeID),
		LogicalPath:    filepath.Join(seriesSegment, fmt.Sprintf("Season %02d", season), cleanSegment(name)),
		TargetPath:     target,
		SourcePath:     target,
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
		return title
	}
	yearToken := fmt.Sprintf("(%d)", year)
	if strings.Contains(title, yearToken) {
		return title
	}
	return title + " " + yearToken
}

func stableKey(fileID, episodeID int) string {
	return "file:" + strconv.Itoa(fileID) + "/episode:" + strconv.Itoa(episodeID)
}

func safeRelativePath(value string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return filepath.FromSlash(cleaned), true
}

func episodeNumbers(episode shoko.Episode) (int, int) {
	if episode.AniDB == nil {
		return 1, 1
	}
	if strings.EqualFold(episode.AniDB.Type, "Episode") {
		if episode.AniDB.EpisodeNumber > 0 {
			return 1, episode.AniDB.EpisodeNumber
		}
		return 1, 1
	}
	if episode.AniDB.EpisodeNumber > 0 {
		return 0, episode.AniDB.EpisodeNumber
	}
	return 0, 1
}

func isMovie(series shoko.Series) bool {
	return series.AniDB != nil && strings.EqualFold(series.AniDB.Type, "Movie")
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
	value = strings.TrimRight(strings.TrimSpace(builder.String()), ".")
	if value == "" || value == "." || value == ".." {
		return "Unknown"
	}
	return value
}
