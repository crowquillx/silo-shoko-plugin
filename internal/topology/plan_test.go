package topology

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

func TestBuildCreatesStableLogicalEpisodeEntry(t *testing.T) {
	episodeID := 42
	snapshot := shoko.Snapshot{
		Files: []shoko.File{{
			ID:        9,
			Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "Show/Episode.mkv", IsAccessible: true}},
			SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(7)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &episodeID}}}},
		}},
		Series:   map[int]shoko.Series{7: {IDs: shoko.IDs{ID: 7}, Name: "My Show", AniDB: &shoko.AnimeMetadata{AirDate: "2014-07-06"}}},
		Episodes: map[int]shoko.Episode{42: {IDs: shoko.IDs{ID: 42, ParentSeries: 7}, Name: "Episode one", AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}}},
	}
	plan, err := Build(snapshot, Policy{Mode: ModeAniDB, VFSRoot: "/vfs", ManagedFolderMap: map[int]string{1: "/media/anime"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("entries = %#v", plan.Entries)
	}
	entry := plan.Entries[0]
	if !strings.HasPrefix(entry.LogicalPath, filepath.Join("My Show (2014)", "Season 01")+string(filepath.Separator)) ||
		!strings.Contains(entry.LogicalPath, "S01E01") || !strings.Contains(entry.LogicalPath, "Shoko Episode=42") {
		t.Fatalf("logical path = %q", entry.LogicalPath)
	}
	if entry.TargetPath != filepath.Join("/media/anime", "Show", "Episode.mkv") {
		t.Fatalf("target path = %q", entry.TargetPath)
	}
}

func TestBuildKeepsMultipleEpisodeBindings(t *testing.T) {
	first, second := 42, 43
	snapshot := shoko.Snapshot{
		Files: []shoko.File{{
			ID:        9,
			Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "Show/Episode.mkv", IsAccessible: true}},
			SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(7)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &first}, {ID: &second}}}},
		}},
		Series: map[int]shoko.Series{7: {IDs: shoko.IDs{ID: 7}, Name: "My Show"}},
		Episodes: map[int]shoko.Episode{
			42: {IDs: shoko.IDs{ID: 42, ParentSeries: 7}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
			43: {IDs: shoko.IDs{ID: 43, ParentSeries: 7}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 2}},
		},
	}
	plan, err := Build(snapshot, Policy{VFSRoot: "/vfs", ManagedFolderMap: map[int]string{1: "/media/anime"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 2 || plan.Entries[0].TargetPath != plan.Entries[1].TargetPath {
		t.Fatalf("multiple binding plan = %#v", plan.Entries)
	}
}

func TestBuildRejectsTraversalLocation(t *testing.T) {
	episodeID := 42
	snapshot := shoko.Snapshot{
		Files: []shoko.File{{
			ID:        9,
			Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "../outside.mkv", IsAccessible: true}},
			SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(7)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &episodeID}}}},
		}},
		Series:   map[int]shoko.Series{7: {IDs: shoko.IDs{ID: 7}, Name: "My Show"}},
		Episodes: map[int]shoko.Episode{42: {IDs: shoko.IDs{ID: 42, ParentSeries: 7}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}}},
	}
	plan, err := Build(snapshot, Policy{VFSRoot: "/vfs", ManagedFolderMap: map[int]string{1: "/media/anime"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 0 || len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Severity != "error" {
		t.Fatalf("plan = %#v", plan)
	}
}

func intPtr(value int) *int { return &value }

func TestBuildGroupsChronologicalTVSeries(t *testing.T) {
	firstEpisodeID, secondEpisodeID := 1001, 1002
	snapshot := shoko.Snapshot{
		Files: []shoko.File{
			{
				ID:        11,
				Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "first.mkv", IsAccessible: true}},
				SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(101)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &firstEpisodeID}}}},
			},
			{
				ID:        12,
				Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "second.mkv", IsAccessible: true}},
				SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(102)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &secondEpisodeID}}}},
			},
		},
		Groups: map[int]shoko.Group{
			50: {IDs: shoko.GroupIDs{ID: 50, MainSeries: 101}, Name: "Monogatari"},
		},
		GroupSeries: map[int][]int{50: {102, 101}},
		Series: map[int]shoko.Series{
			101: {IDs: shoko.IDs{ID: 101}, Name: "Bakemonogatari", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2009-07-03"}},
			102: {IDs: shoko.IDs{ID: 102}, Name: "Nisemonogatari", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2012-01-08"}},
		},
		Episodes: map[int]shoko.Episode{
			1001: {IDs: shoko.IDs{ID: 1001, ParentSeries: 101}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
			1002: {IDs: shoko.IDs{ID: 1002, ParentSeries: 102}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
		},
	}
	plan, err := Build(snapshot, Policy{ManagedFolderMap: map[int]string{1: "/media/anime"}, VFSRoot: "/vfs"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		seriesID int
		season   int
	}{
		{seriesID: 101, season: 1},
		{seriesID: 102, season: 2},
	}
	for _, test := range tests {
		var entry Entry
		found := false
		for _, candidate := range plan.Entries {
			if candidate.ShokoSeriesID == test.seriesID {
				entry, found = candidate, true
				break
			}
		}
		if !found || entry.ShokoGroupID != 50 || entry.SeasonNumber != test.season {
			t.Fatalf("series %d entry = %#v", test.seriesID, entry)
		}
		if !strings.Contains(entry.LogicalPath, filepath.Join("Monogatari (2009)", fmt.Sprintf("Season %02d", test.season))) ||
			!strings.Contains(entry.LogicalPath, "[Shoko Group=50]") {
			t.Fatalf("series %d grouped path = %q", test.seriesID, entry.LogicalPath)
		}
	}
}
