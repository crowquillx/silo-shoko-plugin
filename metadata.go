package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// A show is a Shoko group, so its details are built from every series in the
// group, the way Shokofin builds a show: dates, status, genres, studios, cast
// and rating are combined across the series, and title aliases come from the
// main series.

// seriesDetails is what one series contributes beyond its own record. Each part
// is fetched separately, so a failed part is left out without losing the rest.
type seriesDetails struct {
	genres    []shoko.Tag
	cast      []shoko.Role
	tmdbShows []shoko.TMDBShow
	genresErr error
	castErr   error
	tmdbErr   error
}

const detailConcurrency = 8

func fetchSeriesDetails(ctx context.Context, client *shoko.Client, members []shoko.Series) []seriesDetails {
	details := make([]seriesDetails, len(members))
	limit := make(chan struct{}, detailConcurrency)
	var wg sync.WaitGroup
	for i, series := range members {
		wg.Add(1)
		go func(i, id int) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			detail := &details[i]
			detail.genres, detail.genresErr = client.SeriesGenres(ctx, id)
			detail.cast, detail.castErr = client.SeriesCast(ctx, id)
			detail.tmdbShows, detail.tmdbErr = client.SeriesTMDBShows(ctx, id)
		}(i, series.IDs.ID)
	}
	wg.Wait()
	return details
}

// addShowDetails fills the show-level fields of item. A field whose source
// could not be fetched for every series is left empty rather than sent
// incomplete, since Silo replaces lists like the cast as a whole.
func addShowDetails(ctx context.Context, client *shoko.Client, item *pluginv1.MetadataItem, members []shoko.Series, mainSeriesID int, now time.Time) error {
	if len(members) == 0 {
		return nil
	}
	details := fetchSeriesDetails(ctx, client, members)
	if err := ctx.Err(); err != nil {
		return err
	}

	item.FirstAirDate, item.LastAirDate, item.Status = showSchedule(members, now)
	item.Ratings = anidbRatings(members)
	item.TitleAliases = titleAliases(mainSeries(members, mainSeriesID))
	item.TitleAliasesComplete = len(item.TitleAliases) > 0

	if err := firstError(details, func(d seriesDetails) error { return d.genresErr }); err != nil {
		logDetailError("genres", err)
	} else {
		item.Genres = genres(details)
	}
	if err := firstError(details, func(d seriesDetails) error { return d.castErr }); err != nil {
		logDetailError("cast and studios", err)
	} else {
		item.Studios = studios(details)
		item.People = people(details)
	}
	restricted := false
	for _, series := range members {
		restricted = restricted || (series.AniDB != nil && series.AniDB.Restricted)
	}
	if err := firstError(details, func(d seriesDetails) error { return d.tmdbErr }); err != nil && !restricted {
		logDetailError("content rating", err)
	} else {
		item.ContentRating = contentRating(details, restricted)
	}
	return nil
}

func firstError(details []seriesDetails, pick func(seriesDetails) error) error {
	for _, detail := range details {
		if err := pick(detail); err != nil {
			return err
		}
	}
	return nil
}

func logDetailError(field string, err error) {
	fmt.Fprintf(os.Stderr, "shokoanime: leaving out %s: %v\n", field, err)
}

func mainSeries(members []shoko.Series, mainSeriesID int) shoko.Series {
	for _, series := range members {
		if series.IDs.ID == mainSeriesID {
			return series
		}
	}
	return members[0]
}

// startDate returns a Shoko date as YYYY-MM-DD. Daily servers can send a partial
// "YYYY" or "YYYY-MM", which becomes the first day of that period, as Shokofin
// reads it.
func startDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if date, err := time.Parse(layout, value); err == nil {
			return date.Format("2006-01-02")
		}
	}
	return ""
}

// endDate is startDate for the end of a period: a partial date becomes the last
// day of that period.
func endDate(value string) string {
	value = strings.TrimSpace(value)
	if date, err := time.Parse("2006-01-02", value); err == nil {
		return date.Format("2006-01-02")
	}
	if date, err := time.Parse("2006-01", value); err == nil {
		return date.AddDate(0, 1, -1).Format("2006-01-02")
	}
	if date, err := time.Parse("2006", value); err == nil {
		return date.AddDate(1, 0, -1).Format("2006-01-02")
	}
	return ""
}

func seriesAirDate(series shoko.Series) string {
	if series.AniDB == nil {
		return ""
	}
	return startDate(series.AniDB.AirDate)
}

// showSchedule returns the show's first air date, last air date and status.
// The show is still running while any series is, and has a last air date only
// once every series has ended. Each series' state follows Shoko's own release
// status rule (AnidbReleaseStatus.Infer on daily servers), without its check of
// every episode's air date: a partial date has passed only once its whole
// period has, and a movie or single-episode series without an end date is
// finished once it has started.
func showSchedule(members []shoko.Series, now time.Time) (first, last, status string) {
	today := now.UTC().Format("2006-01-02")
	var airing, upcoming, ended int
	for _, series := range members {
		if series.AniDB == nil {
			continue
		}
		start := startDate(series.AniDB.AirDate)
		if start == "" {
			continue
		}
		if first == "" || start < first {
			first = start
		}
		started := endDate(series.AniDB.AirDate) <= today
		end := endDate(series.AniDB.EndDate)
		if end == "" && started && (strings.EqualFold(series.AniDB.Type, "Movie") || series.AniDB.EpisodeCount == 1) {
			end = start
		}
		switch {
		case !started:
			upcoming++
		case end != "" && end <= today:
			ended++
			if end > last {
				last = end
			}
		default:
			airing++
		}
	}
	switch {
	case airing > 0 || (upcoming > 0 && ended > 0):
		return first, "", "returning"
	case upcoming > 0:
		return first, "", "upcoming"
	case ended > 0:
		return first, last, "ended"
	}
	return first, "", ""
}

// anidbRatings averages the AniDB scores of the show's series, as Shokofin
// does, on Silo's 0-100 scale. Silo shows it once an administrator enables the
// AniDB rating source the manifest declares.
func anidbRatings(members []shoko.Series) *structpb.Struct {
	var total float64
	var votes int64
	var count int
	for _, series := range members {
		if series.AniDB == nil || series.AniDB.Rating == nil {
			continue
		}
		rating := series.AniDB.Rating
		if rating.Value <= 0 || rating.MaxValue <= 0 {
			continue
		}
		total += math.Min(rating.Value/rating.MaxValue, 1) * 100
		votes += max(rating.Votes, 0)
		count++
	}
	if count == 0 {
		return nil
	}
	source := map[string]any{"score": math.Round(total/float64(count)*100) / 100}
	if votes > 0 {
		source["votes"] = float64(votes)
	}
	ratings, err := structpb.NewStruct(map[string]any{"sources": map[string]any{"anidb": source}})
	if err != nil {
		return nil
	}
	return ratings
}

// titleAliases turns the main series' AniDB titles into Silo aliases.
func titleAliases(series shoko.Series) []*pluginv1.TitleAlias {
	if series.AniDB == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var aliases []*pluginv1.TitleAlias
	for _, title := range series.AniDB.Titles {
		name := strings.TrimSpace(title.Name)
		var kind string
		switch strings.ToLower(title.Type) {
		case "main":
			kind = "original"
		case "official":
			kind = "localized"
		case "kanjireading":
			continue // a reading aid, not a title
		default:
			kind = "alternate"
		}
		language := aliasLanguage(title.Language)
		key := strings.ToLower(name) + "\x00" + language + "\x00" + kind
		if _, duplicate := seen[key]; name == "" || duplicate {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, &pluginv1.TitleAlias{Title: name, Language: language, Kind: kind})
	}
	return aliases
}

// aliasLanguage maps AniDB's private language codes to BCP 47. Romanized
// titles keep their language with a Latin script, which Silo reduces to the
// base language.
func aliasLanguage(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "x-jat":
		return "ja-Latn"
	case "x-kot":
		return "ko-Latn"
	case "x-zht":
		return "zh-Latn"
	case "x-tht":
		return "th-Latn"
	case "", "x-other", "x-unk", "unk":
		return ""
	}
	return code
}

// genres is the union of the tags Shoko classifies as genres, without the
// "18 restricted" flag, which AniDB files under genres.
func genres(details []seriesDetails) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, detail := range details {
		for _, tag := range detail.genres {
			name := strings.ToLower(strings.TrimSpace(tag.Name))
			if _, duplicate := seen[name]; name == "" || name == "18 restricted" || duplicate {
				continue
			}
			seen[name] = struct{}{}
			result = append(result, titleCase(name))
		}
	}
	return result
}

func titleCase(value string) string {
	words := strings.Fields(value)
	for i, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// studios are the animation studios credited on the show's series, without
// production committees, as Shokofin filters them.
func studios(details []seriesDetails) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, detail := range details {
		for _, role := range detail.cast {
			name := strings.TrimSpace(role.Staff.Name)
			if role.RoleName != "Studio" || name == "" || (role.Staff.Type != "" && role.Staff.Type != "Company") {
				continue
			}
			key := strings.ToLower(name)
			if _, duplicate := seen[key]; duplicate || strings.Contains(key, "seisaku iinkai") {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, name)
		}
	}
	return result
}

// personKinds maps Shoko's credit types to Silo's. Other credits, such as
// character design and general staff, have no Silo kind and are left out.
var personKinds = map[string]string{
	"Actor":          "actor",
	"Director":       "director",
	"SeriesComposer": "writer",
	"SourceWork":     "creator",
	"Music":          "composer",
	"Producer":       "producer",
}

// characterRank orders voice actors from main to background characters.
var characterRank = map[string]int{
	"Main Character":       0,
	"Minor Character":      1,
	"Background Character": 2,
	"Cameo":                3,
}

func people(details []seriesDetails) []*pluginv1.PersonRecord {
	type credit struct {
		record *pluginv1.PersonRecord
		rank   int
	}
	var credits []credit
	seen := make(map[string]struct{})
	for _, detail := range details {
		for _, role := range detail.cast {
			kind, known := personKinds[role.RoleName]
			name := strings.TrimSpace(role.Staff.Name)
			if !known || name == "" {
				continue
			}
			character := ""
			if role.Character != nil {
				character = strings.TrimSpace(role.Character.Name)
			}
			personKey := strings.ToLower(name)
			if role.Staff.ID != nil && *role.Staff.ID > 0 {
				personKey = "id:" + strconv.Itoa(*role.Staff.ID)
			}
			key := kind + "\x00" + personKey + "\x00" + strings.ToLower(character)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			rank, ranked := characterRank[role.RoleDetails]
			if !ranked {
				rank = len(characterRank)
			}
			record := &pluginv1.PersonRecord{Name: name, Kind: kind, Character: character}
			if image := role.Staff.Image; image != nil && image.Available && !image.Disabled {
				record.PhotoPath = imagePath(*image)
			}
			credits = append(credits, credit{record: record, rank: rank})
		}
	}
	sort.SliceStable(credits, func(i, j int) bool { return credits[i].rank < credits[j].rank })
	records := make([]*pluginv1.PersonRecord, len(credits))
	for i, credit := range credits {
		credit.record.SortOrder = int32(i)
		records[i] = credit.record
	}
	return records
}

// usContentRatingAge orders the US ratings by the age Silo reads from them.
// Only ratings in this list are sent, because Silo hides any title whose
// rating it cannot read from every viewer with an age limit.
var usContentRatingAge = map[string]int{
	"TV-Y": 0, "TV-G": 0, "G": 0, "TV-Y7": 7, "TV-PG": 8, "PG": 8,
	"PG-13": 13, "TV-14": 14, "R": 17, "TV-MA": 17, "NC-17": 18,
}

// contentRating is the strictest US rating of the show's linked TMDB shows. A
// series AniDB marks restricted makes the whole show 18+.
func contentRating(details []seriesDetails, restricted bool) string {
	if restricted {
		return "18+"
	}
	best, bestAge := "", -1
	for _, detail := range details {
		for _, show := range detail.tmdbShows {
			for _, rating := range show.ContentRatings {
				value := strings.ToUpper(strings.TrimSpace(rating.Rating))
				age, known := usContentRatingAge[value]
				if known && strings.EqualFold(rating.Country, "US") && age > bestAge {
					best, bestAge = value, age
				}
			}
		}
	}
	return best
}

func episodeRecord(season, number int, episode shoko.Episode) *pluginv1.EpisodeRecord {
	record := &pluginv1.EpisodeRecord{
		SeasonNumber:  int32(season),
		EpisodeNumber: int32(number),
		Title:         episode.Name,
		Overview:      sanitizeDescription(episode.Description),
		StillPath:     episodeStill(episode.Images),
		ProviderId:    fmt.Sprintf("episode:%d", episode.IDs.ID),
		ProviderIds:   idsStruct(episodeIDs(episode)),
	}
	if episode.AniDB != nil {
		record.AirDate = startDate(episode.AniDB.AirDate)
	}
	// Silo stores TMDB and IMDb scores for episodes, so AniDB's episode score
	// has nowhere to go.
	if rating, ok := episode.TMDBRating(); ok {
		if ratings, err := structpb.NewStruct(map[string]any{"tmdb": math.Min(rating.Value/rating.MaxValue, 1) * 10}); err == nil {
			record.Ratings = ratings
		}
	}
	return record
}

// episodeStill picks an episode's still. Stable servers list stills as
// thumbnails. Daily servers have one image per episode, its backdrop, while a
// stable episode's backdrops are the series' fan art, so backdrops are only
// used when they come from a daily server.
func episodeStill(images shoko.Images) string {
	if still := selectImage(images.Thumbnails); still != nil {
		return imagePath(*still)
	}
	if backdrop := selectImage(images.Backdrops); backdrop != nil && shokoImageUUIDPattern.MatchString(backdrop.UID) {
		return imagePath(*backdrop)
	}
	return ""
}

// AniDB descriptions carry BBCode and link markup and trailing source and note
// lines. The patterns follow Shokofin's description cleaner.
var (
	descriptionLink         = regexp.MustCompile(`https?://[^\s\[\]]+ \[([^\]]+)\]|\[url=[^\]]+\]([^\[]*)\[/url\]`)
	descriptionSpoiler      = regexp.MustCompile(`(?is)\[spoiler(?:="?[^"\]]*"?)?\](.*?)\[/spoiler\]`)
	descriptionBBCode       = regexp.MustCompile(`(?i)\[/?[ibu]\]`)
	descriptionMiscLine     = regexp.MustCompile(`(?m)^\*[^\n]*$|\n-{3,6}\s*`)
	descriptionSourceLine   = regexp.MustCompile(`(?im)^[ \t]*[\(\[]?sources?\s*:[^\n]*$`)
	descriptionSourceInline = regexp.MustCompile(`(?i)\s*[\(\[]sources?\s*:[^\)\]\n]*[\)\]]`)
	descriptionSummaryLine  = regexp.MustCompile(`\nSummary(?::| by| written by| of the previous)[^\n]*`)
	descriptionNote         = regexp.MustCompile(`(?m)(?:^|\* ?)Note(?: [1-9][0-9]?)?:`)
	descriptionEmptyBracket = regexp.MustCompile(`\[\s*\]|\(\s*\)|\{\s*\}`)
	descriptionBlankLines   = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)
)

func sanitizeDescription(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	value = descriptionLink.ReplaceAllString(value, "$1$2")
	value = descriptionSpoiler.ReplaceAllString(value, "$1")
	value = descriptionBBCode.ReplaceAllString(value, "")
	if match := descriptionNote.FindStringIndex(value); match != nil {
		value = value[:match[0]]
	}
	value = descriptionMiscLine.ReplaceAllString(value, "")
	value = descriptionSourceLine.ReplaceAllString(value, "")
	value = descriptionSourceInline.ReplaceAllString(value, "")
	value = descriptionSummaryLine.ReplaceAllString(value, "")
	value = descriptionEmptyBracket.ReplaceAllString(value, "")
	value = descriptionBlankLines.ReplaceAllString(value, "\n\n")
	return strings.TrimSpace(value)
}
