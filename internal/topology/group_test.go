package topology

import (
	"fmt"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

func member(id int, kind string) shoko.Series {
	return shoko.Series{IDs: shoko.IDs{ID: id}, AniDB: &shoko.AnimeMetadata{Type: kind}}
}

func regular(number int) shoko.Episode {
	return shoko.Episode{AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: number}}
}

func extra(kind string, number int) shoko.Episode {
	return shoko.Episode{AniDB: &shoko.AnimeMetadata{Type: kind, EpisodeNumber: number}}
}

// Every series in a group is a season, in the order Shoko lists them. The IDs and
// types here are deliberately out of order to show neither affects the result.
func TestEverySeriesIsASeasonInShokoOrder(t *testing.T) {
	members := []shoko.Series{
		member(50, "Movie"),
		member(10, "TV"),
		member(40, "OVA"),
		member(20, "Web"),
		member(30, "Movie"),
		member(60, "TVSpecial"),
		member(70, "Unknown"),
	}
	layout := NewGroupLayout(members)
	for index, series := range members {
		want := index + 1
		if got := layout.SeasonNumber(series.IDs.ID); got != want {
			t.Errorf("%s series %d season = %d, want %d", series.AniDB.Type, series.IDs.ID, got, want)
		}
		if season, number := layout.Position(series, regular(3)); season != want || number != 3 {
			t.Errorf("%s episode = S%02dE%02d, want S%02dE03", series.AniDB.Type, season, number, want)
		}
	}
}

func TestSeriesWithoutMetadataStillGetsASeason(t *testing.T) {
	series := shoko.Series{IDs: shoko.IDs{ID: 1}}
	layout := NewGroupLayout([]shoko.Series{series})
	if season, number := layout.Position(series, regular(2)); season != 1 || number != 2 {
		t.Fatalf("episode = S%02dE%02d, want S01E02", season, number)
	}
}

func TestMoviesAreNotCollapsedIntoOneSeason(t *testing.T) {
	first, second := member(1, "Movie"), member(2, "Movie")
	layout := NewGroupLayout([]shoko.Series{first, second})
	if season, number := layout.Position(first, regular(1)); season != 1 || number != 1 {
		t.Fatalf("first movie = S%02dE%02d, want S01E01", season, number)
	}
	if season, number := layout.Position(second, regular(1)); season != 2 || number != 1 {
		t.Fatalf("second movie = S%02dE%02d, want S02E01", season, number)
	}
}

func TestSeasonZeroNumbersAreUniqueAcrossSeriesAndTypes(t *testing.T) {
	members := []shoko.Series{member(1, "TV"), member(2, "OVA"), member(3, "Movie")}
	layout := NewGroupLayout(members)
	seen := map[int]string{}
	for _, series := range members {
		for _, kind := range []string{"Special", "Credits", "Trailer", "Parody", "Other"} {
			label := fmt.Sprintf("%s of series %d", kind, series.IDs.ID)
			season, number := layout.Position(series, extra(kind, 1))
			if season != 0 {
				t.Fatalf("%s season = %d, want 0", label, season)
			}
			if previous, duplicate := seen[number]; duplicate {
				t.Fatalf("%s and %s both use S00E%d", label, previous, number)
			}
			seen[number] = label
		}
	}
	// The first series keeps its natural special numbers.
	if season, number := layout.Position(members[0], extra("Special", 2)); season != 0 || number != 2 {
		t.Fatalf("special = S%02dE%02d, want S00E02", season, number)
	}
}

func TestSeasonZeroNumberStaysInFiveDigitsForHugeGroups(t *testing.T) {
	members := make([]shoko.Series, 150)
	for i := range members {
		members[i] = member(i+1, "Movie")
	}
	layout := NewGroupLayout(members)
	if _, number := layout.Position(members[149], extra("Other", 99)); number > 99999 {
		t.Fatalf("number = %d, want at most 99999", number)
	}
}
