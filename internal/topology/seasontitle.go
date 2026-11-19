package topology

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// SeasonTitles returns a title for each of a show's seasons, in the order of
// members, without the part that repeats the show's title: under the show
// "Demon Slayer", the season "Demon Slayer: Entertainment District Arc" is
// "Entertainment District Arc". show is the title the show is listed under and
// mainSeriesID the series Shoko names the group after.
//
// A season keeps Shoko's name when what would be left says too little: nothing,
// a year, a number, or a few characters such as "II" or "S". Every season keeps
// its name when fewer than half of them start with the show's title, as in a
// group that mixes several franchises, where a bare subtitle would not say
// which one it belongs to.
//
// Shoko gives several seasons the same name when it has no title for them in
// its language and falls back to the show's. Those are first told apart with
// the part of their AniDB main title that the first of them does not have.
func SeasonTitles(show string, mainSeriesID int, members []shoko.Series) []string {
	show = strings.TrimSpace(show)
	names := distinctSeasonNames(mainSeriesID, members)
	matches := func(name string, stem string) bool {
		_, ok := cutTitlePrefix(name, stem)
		return ok
	}
	// The show's title is always stripped. A shorter stem of it, such as "Ghost
	// in the Shell" of "Ghost in the Shell: Stand Alone Complex", only when a
	// season needs it.
	var stems []string
	for _, stem := range titleStems(show) {
		needed := func(name string) bool { return matches(name, stem) && !matches(name, show) }
		if stem == show || slices.ContainsFunc(names, needed) {
			stems = append(stems, stem)
		}
	}
	named := 0
	for _, name := range names {
		if slices.ContainsFunc(stems, func(stem string) bool { return matches(name, stem) }) {
			named++
		}
	}
	if named*2 < len(names) {
		return names
	}
	titles := make([]string, len(names))
	for index, name := range names {
		titles[index] = subtitle(name, stems)
	}
	return titles
}

// distinctSeasonNames returns Shoko's name for each series. Where several have
// the same name, the first keeps it and each of the others gets the rest of its
// AniDB main title appended: after "Tokyo Revengers", "Tokyo Revengers: Seiya
// Kessen Hen" stays apart from it. The first is the group's main series or the
// one that aired first. The rest is in AniDB's romanisation, since a series
// with a title in Shoko's language would not have the show's name.
func distinctSeasonNames(mainSeriesID int, members []shoko.Series) []string {
	names := make([]string, len(members))
	sameName := make(map[string][]int)
	for index, series := range members {
		names[index] = strings.TrimSpace(series.Name)
		key := strings.ToLower(names[index])
		sameName[key] = append(sameName[key], index)
	}
	for _, indexes := range sameName {
		if len(indexes) < 2 {
			continue
		}
		first := indexes[0]
		for _, index := range indexes[1:] {
			if members[first].IDs.ID == mainSeriesID {
				break
			}
			if members[index].IDs.ID == mainSeriesID || airedBefore(members[index], members[first]) {
				first = index
			}
		}
		stems := titleStems(mainTitle(members[first]))
		for _, index := range indexes {
			if index == first {
				continue
			}
			title := mainTitle(members[index])
			for _, stem := range stems {
				if rest, ok := cutTitlePrefix(title, stem); ok {
					names[index] += strings.TrimRightFunc(rest, unicode.IsSpace)
					break
				}
			}
		}
	}
	return names
}

func airedBefore(a, b shoko.Series) bool {
	if a.AniDB == nil || a.AniDB.AirDate == "" {
		return false
	}
	return b.AniDB == nil || b.AniDB.AirDate == "" || a.AniDB.AirDate < b.AniDB.AirDate
}

func mainTitle(series shoko.Series) string {
	if series.AniDB == nil {
		return ""
	}
	for _, title := range series.AniDB.Titles {
		if strings.EqualFold(title.Type, "main") {
			return strings.TrimSpace(title.Name)
		}
	}
	return strings.TrimSpace(series.AniDB.Title)
}

var trailingYear = regexp.MustCompile(`\s*\((?:19|20)\d\d\)$`)

// titleStems returns title and the shorter forms of it a related title may
// start with instead, longest first: without a year at the end, and up to each
// ": ", " - " and "! " in it.
func titleStems(title string) []string {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	stems := []string{title}
	base := trailingYear.ReplaceAllString(title, "")
	stems = append(stems, base)
	for _, separator := range []struct {
		text string
		keep int
	}{{": ", 0}, {" - ", 0}, {"! ", 1}} {
		for offset := 0; ; {
			at := strings.Index(base[offset:], separator.text)
			if at < 0 {
				break
			}
			offset += at
			stems = append(stems, base[:offset+separator.keep])
			offset += len(separator.text)
		}
	}
	slices.SortStableFunc(stems, func(a, b string) int { return len(b) - len(a) })
	stems = slices.Compact(stems)
	return slices.DeleteFunc(stems, func(stem string) bool { return !strings.ContainsFunc(stem, isTitleCharacter) })
}

func isTitleCharacter(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// cutTitlePrefix returns what follows prefix in title. Only letters and digits
// are compared, in any case, since Shoko's names for one franchise differ in
// case and punctuation: "Armored Trooper Votoms" and "Armored Trooper VOTOMS",
// "One Punch Man" and "One-Punch Man". The prefix must end where a word of
// title ends, so "Gate" is not a prefix of "Gatekeepers" or of "Gate-keepers".
func cutTitlePrefix(title, prefix string) (string, bool) {
	rest := title
	matched := false
	for _, want := range prefix {
		if !isTitleCharacter(want) {
			continue
		}
		rest = strings.TrimLeftFunc(rest, func(r rune) bool { return !isTitleCharacter(r) })
		got, size := utf8.DecodeRuneInString(rest)
		if size == 0 || unicode.ToLower(got) != unicode.ToLower(want) {
			return "", false
		}
		rest = rest[size:]
		matched = true
	}
	if !matched {
		return "", false
	}
	next, size := utf8.DecodeRuneInString(rest)
	if size == 0 {
		return "", true
	}
	if isTitleCharacter(next) {
		return "", false
	}
	if next == '-' || next == '\'' || next == '’' {
		if after, _ := utf8.DecodeRuneInString(rest[size:]); isTitleCharacter(after) {
			return "", false
		}
	}
	return rest, true
}

// subtitle returns name without the longest of stems it starts with, or name
// when it starts with none or the rest says too little.
func subtitle(name string, stems []string) string {
	for _, stem := range stems {
		rest, ok := cutTitlePrefix(name, stem)
		if !ok {
			continue
		}
		if rest = tidySubtitle(rest); saysEnough(rest) {
			return rest
		}
		return name
	}
	return name
}

const subtitleSeparators = " \t:-–—~?!.…,;/"

// tidySubtitle drops the separator a subtitle followed, as in ": Pailsen Files"
// and " - Solid State Society", and starts its first word with a capital, as in
// "the Movie".
func tidySubtitle(rest string) string {
	trimmed := strings.TrimLeft(rest, subtitleSeparators)
	if strings.Contains(rest[:len(rest)-len(trimmed)], "~") {
		trimmed = strings.TrimRight(trimmed, "~")
	}
	trimmed = strings.TrimSpace(trimmed)
	first, size := utf8.DecodeRuneInString(trimmed)
	if next, _ := utf8.DecodeRuneInString(trimmed[size:]); !unicode.IsLetter(next) {
		// A single letter is a word of its own, as in "x Bridge".
		return trimmed
	}
	return string(unicode.ToUpper(first)) + trimmed[size:]
}

var (
	parenthetical = regexp.MustCompile(`\([^()]*\)`)
	onlyNumber    = regexp.MustCompile(`^(?:\d+(?:st|nd|rd|th)?|[IVX]+)$`)
)

// minSubtitleCharacters is the fewest letters and digits a subtitle may have
// to stand without the show's title. "Next" and "Arise" do; "II", "S", "2nd"
// and "OVA" do not.
const minSubtitleCharacters = 4

// saysEnough reports whether a subtitle can be shown without the show's title.
// AniDB's notes in parentheses, such as "(2023)" and "(2021 Part 2)", do not
// count, so a season that differs only by its year keeps its whole name.
func saysEnough(subtitle string) bool {
	core := strings.TrimSpace(parenthetical.ReplaceAllString(subtitle, ""))
	if onlyNumber.MatchString(core) {
		return false
	}
	count := 0
	for _, r := range core {
		if isTitleCharacter(r) {
			count++
		}
	}
	return count >= minSubtitleCharacters
}
