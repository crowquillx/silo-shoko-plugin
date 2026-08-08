package topology

import (
	"sort"
	"strings"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// GroupLayout assigns every Shoko series and episode in a group to the logical
// series coordinates exposed to Silo. TV series become chronological seasons.
// Movie-only groups become one season whose movies are sequential episodes;
// movies attached to a TV franchise are exposed as season-zero specials.
type GroupLayout struct {
	OrderedMembers         []shoko.Series
	SeasonBySeries         map[int]int
	MovieEpisodeNumberByID map[int]int
	hasTV                  bool
}

func NewGroupLayout(members []shoko.Series, episodesBySeries map[int][]shoko.Episode) GroupLayout {
	ordered := append([]shoko.Series(nil), members...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftKind, rightKind := groupMemberKind(left), groupMemberKind(right)
		if leftKind != rightKind {
			return leftKind < rightKind
		}
		leftDate, leftOK := groupSeriesAirDate(left)
		rightDate, rightOK := groupSeriesAirDate(right)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !leftDate.Equal(rightDate) {
			return leftDate.Before(rightDate)
		}
		return left.IDs.ID < right.IDs.ID
	})

	layout := GroupLayout{
		OrderedMembers:         ordered,
		SeasonBySeries:         make(map[int]int, len(ordered)),
		MovieEpisodeNumberByID: make(map[int]int),
	}
	for _, series := range ordered {
		if isTV(series) {
			layout.hasTV = true
			break
		}
	}

	tvSeason := 0
	for _, series := range ordered {
		switch {
		case isTV(series):
			tvSeason++
			layout.SeasonBySeries[series.IDs.ID] = tvSeason
		case isMovie(series) && !layout.hasTV:
			layout.SeasonBySeries[series.IDs.ID] = 1
		default:
			layout.SeasonBySeries[series.IDs.ID] = 0
		}
	}

	movieNumber := 0
	if layout.hasTV {
		for _, series := range ordered {
			if isMovie(series) {
				continue
			}
			for _, episode := range episodesBySeries[series.IDs.ID] {
				season, number := groupedNaturalPosition(series, layout.SeasonBySeries[series.IDs.ID], episode)
				if season == 0 && number > movieNumber {
					movieNumber = number
				}
			}
		}
	}
	for _, series := range ordered {
		if !isMovie(series) {
			continue
		}
		episodes := append([]shoko.Episode(nil), episodesBySeries[series.IDs.ID]...)
		sort.SliceStable(episodes, func(i, j int) bool {
			_, left := naturalEpisodePosition(episodes[i])
			_, right := naturalEpisodePosition(episodes[j])
			if left != right {
				return left < right
			}
			return episodes[i].IDs.ID < episodes[j].IDs.ID
		})
		for _, episode := range episodes {
			movieNumber++
			layout.MovieEpisodeNumberByID[episode.IDs.ID] = movieNumber
		}
	}
	return layout
}

func (l GroupLayout) HasTV() bool {
	return l.hasTV
}

func (l GroupLayout) SeasonNumber(seriesID int) int {
	return l.SeasonBySeries[seriesID]
}

func (l GroupLayout) Position(series shoko.Series, episode shoko.Episode) (int, int) {
	season := l.SeasonBySeries[series.IDs.ID]
	if isMovie(series) {
		if number := l.MovieEpisodeNumberByID[episode.IDs.ID]; number > 0 {
			return season, number
		}
	}
	return groupedNaturalPosition(series, season, episode)
}

func isTV(series shoko.Series) bool {
	return series.AniDB != nil && strings.EqualFold(series.AniDB.Type, "TV")
}

func groupMemberKind(series shoko.Series) int {
	switch {
	case isTV(series):
		return 0
	case isMovie(series):
		return 2
	default:
		return 1
	}
}

func groupSeriesAirDate(series shoko.Series) (time.Time, bool) {
	if series.AniDB == nil {
		return time.Time{}, false
	}
	date, err := time.Parse("2006-01-02", series.AniDB.AirDate)
	return date, err == nil
}

func groupedNaturalPosition(series shoko.Series, seriesSeason int, episode shoko.Episode) (int, int) {
	season, number := naturalEpisodePosition(episode)
	if isTV(series) && episode.AniDB != nil && strings.EqualFold(episode.AniDB.Type, "Episode") {
		season = seriesSeason
	} else {
		season = 0
	}
	return season, number
}

func naturalEpisodePosition(episode shoko.Episode) (int, int) {
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

func newSnapshotGroupLayouts(snapshot shoko.Snapshot) map[int]GroupLayout {
	episodesBySeries := make(map[int][]shoko.Episode)
	for _, episode := range snapshot.Episodes {
		seriesID := episode.IDs.ParentSeries
		episodesBySeries[seriesID] = append(episodesBySeries[seriesID], episode)
	}
	layouts := make(map[int]GroupLayout, len(snapshot.GroupSeries))
	for groupID, memberIDs := range snapshot.GroupSeries {
		members := make([]shoko.Series, 0, len(memberIDs))
		seen := make(map[int]struct{}, len(memberIDs))
		for _, seriesID := range memberIDs {
			if _, duplicate := seen[seriesID]; duplicate {
				continue
			}
			seen[seriesID] = struct{}{}
			if series, ok := snapshot.Series[seriesID]; ok {
				members = append(members, series)
			}
		}
		layouts[groupID] = NewGroupLayout(members, episodesBySeries)
	}
	return layouts
}
