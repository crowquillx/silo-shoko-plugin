package topology

import (
	"strings"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// GroupLayout follows the Shoko group: the group is the show, and each of its
// series is one season, whatever the series' type. Seasons are numbered in the
// order Shoko lists the group's series, so no ordering is decided here. Regular
// episodes go in their series' season and everything else in season zero.
type GroupLayout struct {
	OrderedMembers []shoko.Series
	SeasonBySeries map[int]int
}

// NewGroupLayout takes members in the order Shoko returned them.
func NewGroupLayout(members []shoko.Series) GroupLayout {
	layout := GroupLayout{
		OrderedMembers: append([]shoko.Series(nil), members...),
		SeasonBySeries: make(map[int]int, len(members)),
	}
	for index, series := range members {
		layout.SeasonBySeries[series.IDs.ID] = index + 1
	}
	return layout
}

func (l GroupLayout) SeasonNumber(seriesID int) int {
	return l.SeasonBySeries[seriesID]
}

func (l GroupLayout) Position(series shoko.Series, episode shoko.Episode) (int, int) {
	season := l.SeasonBySeries[series.IDs.ID]
	if isRegularEpisode(episode) {
		return season, naturalNumber(episode)
	}
	return 0, specialNumber(max(season-1, 0), episode)
}

// EpisodeNumbers returns the coordinates of an episode of a series that belongs
// to no group.
func EpisodeNumbers(episode shoko.Episode) (int, int) {
	if isRegularEpisode(episode) {
		return 1, naturalNumber(episode)
	}
	return 0, specialNumber(0, episode)
}

// Season-zero numbers must be unique within a show because Silo stores one
// episode per season and number, yet AniDB restarts numbering for every series
// and episode type. They are derived from the episode rather than counted, so a
// number does not move when other files appear and the VFS and metadata always
// agree. Silo reads at most five digits, which leaves 99 series per group.
const (
	seasonZeroMemberBlock = 1000
	seasonZeroMaxMember   = 99
	seasonZeroCredits     = 400
	seasonZeroTrailers    = 600
	seasonZeroParodies    = 800
	seasonZeroOther       = 900
)

func specialNumber(memberIndex int, episode shoko.Episode) int {
	offset := seasonZeroOther
	if episode.AniDB != nil {
		switch strings.ToLower(episode.AniDB.Type) {
		case "special":
			offset = 0
		case "credits":
			offset = seasonZeroCredits
		case "trailer":
			offset = seasonZeroTrailers
		case "parody":
			offset = seasonZeroParodies
		}
	}
	return min(memberIndex, seasonZeroMaxMember)*seasonZeroMemberBlock + offset + naturalNumber(episode)
}

func naturalNumber(episode shoko.Episode) int {
	if episode.AniDB != nil && episode.AniDB.EpisodeNumber > 0 {
		return episode.AniDB.EpisodeNumber
	}
	return 1
}

func isRegularEpisode(episode shoko.Episode) bool {
	if episode.AniDB == nil {
		return false
	}
	switch strings.ToLower(episode.AniDB.Type) {
	case "episode", "normal":
		return true
	}
	return false
}

func newSnapshotGroupLayouts(snapshot shoko.Snapshot) map[int]GroupLayout {
	layouts := make(map[int]GroupLayout, len(snapshot.GroupSeries))
	for groupID, memberIDs := range snapshot.GroupSeries {
		members := make([]shoko.Series, 0, len(memberIDs))
		for _, seriesID := range memberIDs {
			if series, ok := snapshot.Series[seriesID]; ok {
				members = append(members, series)
			}
		}
		layouts[groupID] = NewGroupLayout(members)
	}
	return layouts
}
