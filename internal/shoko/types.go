// Package shoko contains the small, read-only portion of the Shoko API v3
// model needed by the first VFS milestone. Unknown upstream fields are
// intentionally ignored so dev-server additions do not break the adapter.
//
// Two API generations are supported. Stable servers (5.x) expose import
// folders and integer image IDs; daily servers (6.x) renamed import folders to
// managed folders and address images by UUID. The differences are normalized
// while decoding, so the rest of the plugin sees one model.
package shoko

import (
	"encoding/json"
	"time"
)

type Version struct {
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	ReleaseDate    string `json:"releaseDate"`
	ReleaseChannel string `json:"releaseChannel"`
}

type VersionSet struct {
	Server Version `json:"server"`
}

type listResult[T any] struct {
	Total int `json:"total"`
	List  []T `json:"list"`
}

type IDs struct {
	ID            int      `json:"id"`
	ParentSeries  int      `json:"parentSeries"`
	ParentGroup   int      `json:"parentGroup"`
	TopLevelGroup int      `json:"topLevelGroup"`
	AniDB         int      `json:"aniDB"`
	TvDB          []int    `json:"tvDB"`
	IMDB          []string `json:"imDB"`
	TMDB          TMDBIDs  `json:"tmdb"`
}

type TMDBIDs struct {
	Episode []int `json:"episode"`
	Movie   []int `json:"movie"`
	Show    []int `json:"show"`
}

type ManagedFolder struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type Location struct {
	ID              int    `json:"id"`
	FileID          int    `json:"fileId"`
	ManagedFolderID int    `json:"managedFolderId"`
	RelativePath    string `json:"relativePath"`
	AbsolutePath    string `json:"absolutePath"`
	IsAccessible    bool   `json:"isAccessible"`
}

// UnmarshalJSON accepts the import-folder field used by stable servers.
func (l *Location) UnmarshalJSON(data []byte) error {
	type plain Location
	var decoded struct {
		plain
		ImportFolderID int `json:"importFolderId"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*l = Location(decoded.plain)
	if l.ManagedFolderID == 0 {
		l.ManagedFolderID = decoded.ImportFolderID
	}
	return nil
}

type CrossReferencePercentage struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Size  int `json:"size"`
	Group int `json:"group"`
}

type SeriesCrossReferenceIDs struct {
	ID    *int    `json:"id"`
	AniDB int     `json:"aniDB"`
	TMDB  TMDBIDs `json:"tmdb"`
}

type EpisodeCrossReferenceIDs struct {
	ID           *int                     `json:"id"`
	AniDB        int                      `json:"aniDB"`
	TMDB         TMDBIDs                  `json:"tmdb"`
	ReleaseGroup *int                     `json:"releaseGroup"`
	ED2K         string                   `json:"ed2k"`
	FileSize     int64                    `json:"fileSize"`
	Percentage   CrossReferencePercentage `json:"percentage"`
	Source       string                   `json:"source"`
}

type FileCrossReference struct {
	SeriesID   SeriesCrossReferenceIDs    `json:"seriesId"`
	EpisodeIDs []EpisodeCrossReferenceIDs `json:"episodeIds"`
}

type File struct {
	ID          int                  `json:"id"`
	Size        int64                `json:"size"`
	IsVariation bool                 `json:"isVariation"`
	IsIgnored   bool                 `json:"isIgnored"`
	Locations   []Location           `json:"locations"`
	SeriesIDs   []FileCrossReference `json:"seriesIds"`
	Created     time.Time            `json:"created"`
	Updated     time.Time            `json:"updated"`
}

type AnimeMetadata struct {
	ID            int    `json:"id"`
	AnimeID       int    `json:"animeId"`
	Type          string `json:"type"`
	EpisodeNumber int    `json:"episodeNumber"`
	AirDate       string `json:"airDate"`
	EndDate       string `json:"endDate"`
	// EpisodeCount is the number of regular episodes, sent for series.
	EpisodeCount int     `json:"episodeCount"`
	Title        string  `json:"title"`
	Titles       []Title `json:"titles"`
	Rating       *Rating `json:"rating"`
	Restricted   bool    `json:"restricted"`
}

// Rating is a community score. AniDB series use a MaxValue of 1000 and AniDB
// and TMDB episodes 10.
type Rating struct {
	Value    float64 `json:"value"`
	MaxValue float64 `json:"maxValue"`
	Votes    int64   `json:"votes"`
	Source   string  `json:"source"`
}

// Title is one AniDB title. Type is Main, Official, Short, Synonym, TitleCard,
// KanjiReading or None, and Language is an AniDB code such as x-jat or en.
type Title struct {
	Name      string `json:"name"`
	Language  string `json:"language"`
	Type      string `json:"type"`
	Default   bool   `json:"default"`
	Preferred bool   `json:"preferred"`
}

// Tag is one series tag from /Series/{id}/Tags.
type Tag struct {
	ID       int    `json:"id"`
	ParentID *int   `json:"parentId"`
	Name     string `json:"name"`
	Weight   *int   `json:"weight"`
	Source   string `json:"source"`
}

// Role is one cast or staff credit from /Series/{id}/Cast. RoleName is one of
// Actor, Staff, Studio, Producer, Director, SeriesComposer, CharacterDesign,
// Music or SourceWork.
type Role struct {
	Character   *Person `json:"character"`
	Staff       Person  `json:"staff"`
	RoleName    string  `json:"roleName"`
	RoleDetails string  `json:"roleDetails"`
}

// Person is a credited person, character or company. Type is Person, Company,
// Collaboration, Other or Unknown.
type Person struct {
	ID    *int   `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Image *Image `json:"image"`
}

// TMDBShow is the part of a linked TMDB show the plugin reads.
type TMDBShow struct {
	ContentRatings []ContentRating `json:"contentRatings"`
}

// ContentRating is one country's rating, such as TV-14 for US.
type ContentRating struct {
	Rating  string `json:"rating"`
	Country string `json:"country"`
}

// AirYear returns the AniDB release year when AirDate is a complete ISO date.
// Invalid or unavailable dates deliberately remain unknown instead of
// manufacturing a year that could make Silo trust the wrong metadata match.
func (m *AnimeMetadata) AirYear() int {
	if m == nil {
		return 0
	}
	date, err := time.Parse("2006-01-02", m.AirDate)
	if err != nil {
		return 0
	}
	return date.Year()
}

type Image struct {
	ID           int     `json:"id"`
	UID          string  `json:"uid"`
	Type         string  `json:"type"`
	Source       string  `json:"source"`
	ResourceID   string  `json:"resourceID"`
	ContentType  string  `json:"contentType"`
	Available    bool    `json:"available"`
	Disabled     bool    `json:"disabled"`
	Preferred    bool    `json:"preferred"`
	Desired      bool    `json:"desired"`
	LanguageCode *string `json:"languageCode"`
	Width        *int    `json:"width"`
	Height       *int    `json:"height"`
}

// UnmarshalJSON derives availability on stable servers, which have no
// available field and instead leave relativeFilepath null for images that are
// not stored locally.
func (i *Image) UnmarshalJSON(data []byte) error {
	type plain Image
	var decoded struct {
		plain
		Available        *bool   `json:"available"`
		RelativeFilepath *string `json:"relativeFilepath"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = Image(decoded.plain)
	if decoded.Available != nil {
		i.Available = *decoded.Available
	} else {
		i.Available = decoded.RelativeFilepath != nil
	}
	return nil
}

type Images struct {
	Posters   []Image `json:"posters"`
	Backdrops []Image `json:"backdrops"`
	Banners   []Image `json:"banners"`
	Logos     []Image `json:"logos"`
	Discs     []Image `json:"discs"`
	// Thumbnails holds episode stills on stable servers. Daily servers put an
	// episode's still in Backdrops instead.
	Thumbnails []Image `json:"thumbnails"`
}

type GroupIDs struct {
	ID              int  `json:"id"`
	PreferredSeries *int `json:"preferredSeries"`
	MainSeries      int  `json:"mainSeries"`
	MainAnime       int  `json:"mainAnime"`
	ParentGroup     *int `json:"parentGroup"`
	TopLevelGroup   int  `json:"topLevelGroup"`
}

type Group struct {
	IDs         GroupIDs  `json:"ids"`
	Name        string    `json:"name"`
	SortName    string    `json:"sortName"`
	Description string    `json:"description"`
	Images      Images    `json:"images"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

type Episode struct {
	IDs         IDs            `json:"ids"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Created     time.Time      `json:"created"`
	Updated     time.Time      `json:"updated"`
	AniDB       *AnimeMetadata `json:"aniDB"`
	Images      Images         `json:"images"`
	// TMDB is only filled when requested with includeDataFrom=TMDB. It is kept
	// raw and read leniently so a TMDB shape change cannot break decoding.
	TMDB json.RawMessage `json:"tmdb,omitempty"`
}

// TMDBRating returns the community rating of the first linked TMDB episode
// that has one.
func (e Episode) TMDBRating() (Rating, bool) {
	if len(e.TMDB) == 0 {
		return Rating{}, false
	}
	var data struct {
		Episodes []struct {
			UserRating *Rating `json:"userRating"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(e.TMDB, &data); err != nil {
		return Rating{}, false
	}
	for _, episode := range data.Episodes {
		if rating := episode.UserRating; rating != nil && rating.Value > 0 && rating.MaxValue > 0 {
			return *rating, true
		}
	}
	return Rating{}, false
}

type Series struct {
	IDs         IDs            `json:"ids"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Created     time.Time      `json:"created"`
	Updated     time.Time      `json:"updated"`
	AniDB       *AnimeMetadata `json:"aniDB"`
	Images      Images         `json:"images"`
}

// Snapshot is the graph consumed by the topology planner.
type Snapshot struct {
	ManagedFolders map[int]ManagedFolder
	Files          []File
	Groups         map[int]Group
	GroupSeries    map[int][]int
	Series         map[int]Series
	Episodes       map[int]Episode
}
