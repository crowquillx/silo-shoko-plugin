package topology

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

func TestBuildRendersMovieAsSeriesEpisode(t *testing.T) {
	episodeID := 42
	snapshot := shoko.Snapshot{
		Files: []shoko.File{{
			ID:        9,
			Locations: []shoko.Location{{ManagedFolderID: 1, RelativePath: "Movie/file.mkv", IsAccessible: true}},
			SeriesIDs: []shoko.FileCrossReference{{SeriesID: shoko.SeriesCrossReferenceIDs{ID: intPtr(7)}, EpisodeIDs: []shoko.EpisodeCrossReferenceIDs{{ID: &episodeID}}}},
		}},
		Groups:      map[int]shoko.Group{5: {IDs: shoko.GroupIDs{ID: 5, MainSeries: 7}, Name: "Movie Group"}},
		GroupSeries: map[int][]int{5: {7}},
		Series: map[int]shoko.Series{
			7: {IDs: shoko.IDs{ID: 7}, Name: "Movie", AniDB: &shoko.AnimeMetadata{Type: "Movie", AirDate: "2020-01-01"}},
		},
		Episodes: map[int]shoko.Episode{
			42: {IDs: shoko.IDs{ID: 42, ParentSeries: 7}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
		},
	}
	plan, err := Build(snapshot, Policy{VFSRoot: "/vfs", ManagedFolderMap: map[int]string{1: "/media/anime"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("entries = %#v", plan.Entries)
	}
	entry := plan.Entries[0]
	if entry.Kind != KindEpisode || entry.SeasonNumber != 1 || entry.EpisodeNumber != 1 {
		t.Fatalf("movie entry = %#v", entry)
	}
	if strings.HasPrefix(entry.LogicalPath, "Movies"+string(filepath.Separator)) {
		t.Fatalf("movie retained obsolete Movies root: %q", entry.LogicalPath)
	}
	if !strings.Contains(entry.LogicalPath, filepath.Join("Movie Group (2020)", "Season 01")) || !strings.Contains(entry.LogicalPath, "S01E01") {
		t.Fatalf("movie series path = %q", entry.LogicalPath)
	}
}
