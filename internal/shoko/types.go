// Package shoko contains the small, read-only portion of the Shoko API v3
// model needed by the first VFS milestone. Unknown upstream fields are
// intentionally ignored so dev-server additions do not break the adapter.
package shoko

import "time"

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
	Title         string `json:"title"`
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

type Images struct {
	Posters   []Image `json:"posters"`
	Backdrops []Image `json:"backdrops"`
	Banners   []Image `json:"banners"`
	Logos     []Image `json:"logos"`
	Discs     []Image `json:"discs"`
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
