// Package identity parses the stable Shoko markers embedded in VFS filenames.
// The markers are deliberately provider-specific and are not treated as a
// replacement for Silo's canonical provider-ID policy.
package identity

import (
	"regexp"
	"strconv"
)

type Token struct {
	GroupID   int
	SeriesID  int
	EpisodeID int
	FileID    int
}

var markerPattern = regexp.MustCompile(`\[Shoko\s+(Group|Series|Episode|File)\s*=\s*([0-9]+)\]`)

func Parse(path string) Token {
	var token Token
	for _, match := range markerPattern.FindAllStringSubmatch(path, -1) {
		value, err := strconv.Atoi(match[2])
		if err != nil || value < 1 {
			continue
		}
		switch match[1] {
		case "Group":
			token.GroupID = value
		case "Series":
			token.SeriesID = value
		case "Episode":
			token.EpisodeID = value
		case "File":
			token.FileID = value
		}
	}
	return token
}

func (t Token) Valid() bool {
	return t.GroupID > 0 || t.SeriesID > 0 || t.EpisodeID > 0 || t.FileID > 0
}

func (t Token) ProviderIDs() map[string]string {
	ids := make(map[string]string)
	if t.GroupID > 0 {
		ids["shoko_group"] = strconv.Itoa(t.GroupID)
	}
	if t.SeriesID > 0 {
		ids["shoko_series"] = strconv.Itoa(t.SeriesID)
	}
	if t.EpisodeID > 0 {
		ids["shoko_episode"] = strconv.Itoa(t.EpisodeID)
	}
	if t.FileID > 0 {
		ids["shoko_file"] = strconv.Itoa(t.FileID)
	}
	return ids
}
