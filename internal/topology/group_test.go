package topology

import (
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

func TestMovieOnlyGroupUsesOneSeasonWithSequentialEpisodes(t *testing.T) {
	members := []shoko.Series{
		{IDs: shoko.IDs{ID: 20}, AniDB: &shoko.AnimeMetadata{Type: "Movie", AirDate: "2020-01-01"}},
		{IDs: shoko.IDs{ID: 10}, AniDB: &shoko.AnimeMetadata{Type: "Movie", AirDate: "2010-01-01"}},
	}
	episodes := map[int][]shoko.Episode{
		10: {
			{IDs: shoko.IDs{ID: 101}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
			{IDs: shoko.IDs{ID: 102}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 2}},
		},
		20: {{IDs: shoko.IDs{ID: 201}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}}},
	}

	layout := NewGroupLayout(members, episodes)
	if layout.HasTV() {
		t.Fatal("movie-only layout reports TV members")
	}
	if got := layout.SeasonNumber(10); got != 1 {
		t.Fatalf("first movie season = %d, want 1", got)
	}
	if got := layout.SeasonNumber(20); got != 1 {
		t.Fatalf("second movie season = %d, want 1", got)
	}
	for episodeID, want := range map[int]int{101: 1, 102: 2, 201: 3} {
		if got := layout.MovieEpisodeNumberByID[episodeID]; got != want {
			t.Fatalf("movie episode %d number = %d, want %d", episodeID, got, want)
		}
	}
}

func TestTVGroupPlacesMoviesAfterExistingSeasonZeroEpisodes(t *testing.T) {
	tv := shoko.Series{IDs: shoko.IDs{ID: 10}, AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2010-01-01"}}
	movie := shoko.Series{IDs: shoko.IDs{ID: 20}, AniDB: &shoko.AnimeMetadata{Type: "Movie", AirDate: "2015-01-01"}}
	special := shoko.Episode{IDs: shoko.IDs{ID: 102}, AniDB: &shoko.AnimeMetadata{Type: "Special", EpisodeNumber: 3}}
	movieEpisode := shoko.Episode{IDs: shoko.IDs{ID: 201}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}}
	layout := NewGroupLayout([]shoko.Series{movie, tv}, map[int][]shoko.Episode{
		10: {
			{IDs: shoko.IDs{ID: 101}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}},
			special,
		},
		20: {movieEpisode},
	})

	if !layout.HasTV() {
		t.Fatal("mixed layout does not report TV members")
	}
	if season, number := layout.Position(tv, special); season != 0 || number != 3 {
		t.Fatalf("special position = S%02dE%02d, want S00E03", season, number)
	}
	if season, number := layout.Position(movie, movieEpisode); season != 0 || number != 4 {
		t.Fatalf("movie position = S%02dE%02d, want S00E04", season, number)
	}
}
