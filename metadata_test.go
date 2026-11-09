package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

var metadataNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func animeSeries(id int, kind, airDate, endDate string) shoko.Series {
	return shoko.Series{IDs: shoko.IDs{ID: id}, AniDB: &shoko.AnimeMetadata{Type: kind, AirDate: airDate, EndDate: endDate}}
}

func TestPartialDatesBecomeFullDates(t *testing.T) {
	for _, tt := range []struct{ in, start, end string }{
		{"2020-04-01", "2020-04-01", "2020-04-01"},
		{"2020-02", "2020-02-01", "2020-02-29"},
		{"2021", "2021-01-01", "2021-12-31"},
		{"", "", ""},
		{"not a date", "", ""},
	} {
		if got := startDate(tt.in); got != tt.start {
			t.Errorf("startDate(%q) = %q, want %q", tt.in, got, tt.start)
		}
		if got := endDate(tt.in); got != tt.end {
			t.Errorf("endDate(%q) = %q, want %q", tt.in, got, tt.end)
		}
	}
}

func TestShowSchedule(t *testing.T) {
	tests := []struct {
		name                string
		members             []shoko.Series
		first, last, status string
	}{
		{"all ended", []shoko.Series{animeSeries(1, "TV", "2010-01-01", "2010-03-31"), animeSeries(2, "TV", "2012-01-01", "2012-03-31")}, "2010-01-01", "2012-03-31", "ended"},
		{"one still airing", []shoko.Series{animeSeries(1, "TV", "2010-01-01", "2010-03-31"), animeSeries(2, "TV", "2026-10-01", "")}, "2010-01-01", "", "returning"},
		{"ends later this year", []shoko.Series{animeSeries(1, "TV", "2026-07-01", "2026-12-20")}, "2026-07-01", "", "returning"},
		{"new season announced", []shoko.Series{animeSeries(1, "TV", "2010-01-01", "2010-03-31"), animeSeries(2, "TV", "2027-04-01", "")}, "2010-01-01", "", "returning"},
		{"not started", []shoko.Series{animeSeries(1, "TV", "2027-01", "")}, "2027-01-01", "", "upcoming"},
		{"released movie without end date", []shoko.Series{animeSeries(1, "Movie", "2019-06-01", "")}, "2019-06-01", "2019-06-01", "ended"},
		{"no dates", []shoko.Series{animeSeries(1, "TV", "", "")}, "", "", ""},
		{"single-episode OVA without end date", []shoko.Series{{AniDB: &shoko.AnimeMetadata{Type: "OVA", AirDate: "2021-06-01", EpisodeCount: 1}}}, "2021-06-01", "2021-06-01", "ended"},
		{"multi-episode OVA without end date", []shoko.Series{{AniDB: &shoko.AnimeMetadata{Type: "OVA", AirDate: "2021-06-01", EpisodeCount: 2}}}, "2021-06-01", "", "returning"},
		{"partial start this year", []shoko.Series{animeSeries(1, "TV", "2026", "")}, "2026-01-01", "", "upcoming"},
		{"partial end date", []shoko.Series{animeSeries(1, "TV", "2025", "2025-03")}, "2025-01-01", "2025-03-31", "ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, last, status := showSchedule(tt.members, metadataNow)
			if first != tt.first || last != tt.last || status != tt.status {
				t.Fatalf("got %q %q %q, want %q %q %q", first, last, status, tt.first, tt.last, tt.status)
			}
		})
	}
}

func TestAniDBRatingsAverageOnSiloScale(t *testing.T) {
	members := []shoko.Series{
		{AniDB: &shoko.AnimeMetadata{Rating: &shoko.Rating{Value: 800, MaxValue: 1000, Votes: 100}}},
		{AniDB: &shoko.AnimeMetadata{Rating: &shoko.Rating{Value: 700, MaxValue: 1000, Votes: 50}}},
		{AniDB: &shoko.AnimeMetadata{Rating: &shoko.Rating{Value: 0, MaxValue: 1000}}},
		{AniDB: &shoko.AnimeMetadata{}},
	}
	source := anidbRatings(members).AsMap()["sources"].(map[string]any)["anidb"].(map[string]any)
	if source["score"] != float64(75) || source["votes"] != float64(150) {
		t.Fatalf("anidb rating = %#v", source)
	}
	if anidbRatings([]shoko.Series{{AniDB: &shoko.AnimeMetadata{}}}) != nil {
		t.Fatal("unrated series produced a rating")
	}
}

func TestTitleAliasesFromAniDB(t *testing.T) {
	series := shoko.Series{AniDB: &shoko.AnimeMetadata{Titles: []shoko.Title{
		{Name: "Shingeki no Kyojin", Language: "x-jat", Type: "Main"},
		{Name: "Attack on Titan", Language: "en", Type: "Official"},
		{Name: "進撃の巨人", Language: "ja", Type: "Official"},
		{Name: "AoT", Language: "en", Type: "Short"},
		{Name: "しんげきのきょじん", Language: "ja", Type: "KanjiReading"},
		{Name: "Attack on Titan", Language: "en", Type: "Official"},
		{Name: " ", Language: "en", Type: "Synonym"},
	}}}
	got := titleAliases(series)
	// Official titles come first, then the main title, then synonyms.
	want := []string{"Attack on Titan|en|localized", "進撃の巨人|ja|localized", "Shingeki no Kyojin|ja-Latn|original", "AoT|en|alternate"}
	if len(got) != len(want) {
		t.Fatalf("aliases = %v", got)
	}
	for i, alias := range got {
		if line := alias.GetTitle() + "|" + alias.GetLanguage() + "|" + alias.GetKind(); line != want[i] {
			t.Errorf("alias %d = %q, want %q", i, line, want[i])
		}
	}
}

func id(value int) *int { return &value }

func TestPeopleAndStudiosFromCast(t *testing.T) {
	photo := &shoko.Image{ID: 5, Type: "Staff", Source: "AniDB", Available: true}
	details := []seriesDetails{
		{cast: []shoko.Role{
			{RoleName: "Actor", RoleDetails: "Minor Character", Staff: shoko.Person{ID: id(2), Name: "Minor VA"}, Character: &shoko.Person{Name: "Sidekick"}},
			{RoleName: "Actor", RoleDetails: "Main Character", Staff: shoko.Person{ID: id(1), Name: "Lead VA", Image: photo}, Character: &shoko.Person{Name: "Hero"}},
			{RoleName: "Actor", RoleDetails: "Main Character", Staff: shoko.Person{Name: ""}, Character: &shoko.Person{Name: "Unvoiced"}},
			{RoleName: "Director", Staff: shoko.Person{ID: id(3), Name: "A Director"}},
			{RoleName: "SourceWork", Staff: shoko.Person{ID: id(4), Name: "Mangaka"}},
			{RoleName: "CharacterDesign", Staff: shoko.Person{ID: id(6), Name: "Designer"}},
			{RoleName: "Studio", Staff: shoko.Person{Type: "Company", Name: "Studio A"}},
			{RoleName: "Studio", Staff: shoko.Person{Type: "Company", Name: "Show Seisaku Iinkai"}},
			{RoleName: "Studio", Staff: shoko.Person{Type: "Person", Name: "Not A Studio"}},
		}},
		{cast: []shoko.Role{
			{RoleName: "Actor", RoleDetails: "Main Character", Staff: shoko.Person{ID: id(1), Name: "Lead VA"}, Character: &shoko.Person{Name: "Hero"}},
			{RoleName: "Actor", RoleDetails: "Main Character", Staff: shoko.Person{ID: id(1), Name: "Lead VA"}, Character: &shoko.Person{Name: "Hero's Twin"}},
			{RoleName: "Studio", Staff: shoko.Person{Name: "studio a"}},
			{RoleName: "Studio", Staff: shoko.Person{Name: "Studio B"}},
		}},
	}
	var got []string
	for _, person := range people(details) {
		got = append(got, fmt.Sprintf("%d %s %s/%s %s", person.GetSortOrder(), person.GetKind(), person.GetName(), person.GetCharacter(), person.GetPhotoPath()))
	}
	want := []string{
		"0 actor Lead VA/Hero shoko://image/AniDB/Staff/5",
		"1 actor Lead VA/Hero's Twin ",
		"2 actor Minor VA/Sidekick ",
		"3 director A Director/ ",
		"4 creator Mangaka/ ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("people =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(studios(details), ","); got != "Studio A,Studio B" {
		t.Fatalf("studios = %q", got)
	}
}

func TestGenresAreShokoGenreTags(t *testing.T) {
	details := []seriesDetails{
		{genres: []shoko.Tag{{Name: "action"}, {Name: "18 restricted"}, {Name: "science fiction"}}},
		{genres: []shoko.Tag{{Name: "Action"}, {Name: "comedy"}}},
	}
	if got := strings.Join(genres(details), ","); got != "Action,Science Fiction,Comedy" {
		t.Fatalf("genres = %q", got)
	}
}

func TestContentRatingOnlyUsesRatingsSiloCanRead(t *testing.T) {
	shows := func(ratings ...shoko.ContentRating) []seriesDetails {
		return []seriesDetails{{tmdbShows: []shoko.TMDBShow{{ContentRatings: ratings}}}}
	}
	tests := []struct {
		name       string
		details    []seriesDetails
		restricted bool
		want       string
	}{
		{"strictest US rating", append(shows(shoko.ContentRating{Rating: "TV-PG", Country: "US"}), shows(shoko.ContentRating{Rating: "TV-14", Country: "US"})...), false, "TV-14"},
		{"other countries ignored", shows(shoko.ContentRating{Rating: "16", Country: "DE"}), false, ""},
		{"unreadable rating ignored", shows(shoko.ContentRating{Rating: "NR", Country: "US"}, shoko.ContentRating{Rating: "TV-Y7", Country: "us"}), false, "TV-Y7"},
		{"restricted wins", shows(shoko.ContentRating{Rating: "TV-14", Country: "US"}), true, "18+"},
		{"unlinked", nil, false, ""},
	}
	for _, tt := range tests {
		if got := contentRating(tt.details, tt.restricted); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSanitizeDescription(t *testing.T) {
	in := "[i]Hero[/i] meets http://anidb.net/ch123 [Rival] in [url=http://x]town[/url].\r\n\r\n\r\n" +
		"[spoiler]They become friends.[/spoiler]\n" +
		"* Based on the manga.\n" +
		"Source: ANN\n" +
		"Note: Aired out of order."
	want := "Hero meets Rival in town.\n\nThey become friends."
	if got := sanitizeDescription(in); got != want {
		t.Fatalf("sanitizeDescription =\n%q\nwant\n%q", got, want)
	}
	if got := sanitizeDescription("A sentence about the source of power (Source: Crunchyroll)"); got != "A sentence about the source of power" {
		t.Fatalf("inline source = %q", got)
	}
}

func TestEpisodeRecordStillAirDateAndRating(t *testing.T) {
	const uid = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
	stable := shoko.Episode{
		AniDB:  &shoko.AnimeMetadata{AirDate: "2020-04-05"},
		Images: shoko.Images{Thumbnails: []shoko.Image{{ID: 9, Type: "Thumbnail", Source: "TMDB", Available: true}}, Backdrops: []shoko.Image{{ID: 3, Type: "Backdrop", Source: "AniDB", Available: true}}},
		TMDB:   []byte(`{"Episodes":[{"UserRating":{"Value":7.5,"MaxValue":10,"Votes":12}}]}`),
	}
	record := episodeRecord(1, 1, stable)
	if record.GetStillPath() != "shoko://image/TMDB/Thumbnail/9" || record.GetAirDate() != "2020-04-05" || record.GetRatings().AsMap()["tmdb"] != 7.5 {
		t.Fatalf("stable record = %#v", record)
	}
	// A stable episode's backdrops are the series' fan art, not a still.
	if still := episodeStill(shoko.Images{Backdrops: stable.Images.Backdrops}); still != "" {
		t.Fatalf("stable backdrop used as still: %q", still)
	}
	daily := shoko.Episode{Images: shoko.Images{Backdrops: []shoko.Image{{UID: uid, Available: true}}}, TMDB: []byte(`{"Episodes":"unexpected"}`)}
	record = episodeRecord(1, 1, daily)
	if record.GetStillPath() != "shoko://image/"+uid || record.GetRatings() != nil {
		t.Fatalf("daily record = %#v", record)
	}
}

// One Shoko server for the end-to-end checks, in each generation's shapes.
func showServer(t *testing.T, stable bool, failCast bool) *httptest.Server {
	t.Helper()
	poster := `{"ID":5,"Type":"Staff","Source":"AniDB","RelativeFilepath":"/5"}`
	still := `"Thumbnails":[{"ID":9,"Type":"Thumbnail","Source":"TMDB","RelativeFilepath":"/9"}],"Backdrops":[]`
	if !stable {
		poster = `{"ID":5,"UID":"11111111-1111-4111-8111-111111111111","Available":true}`
		still = `"Backdrops":[{"ID":9,"UID":"22222222-2222-4222-8222-222222222222","Available":true}]`
	}
	series := func(id int, name, kind, air, end string, rating int, restricted bool) string {
		return fmt.Sprintf(`{"IDs":{"ID":%d,"ParentGroup":5},"Name":%q,"Description":"%s overview\nSource: AniDB","AniDB":{"ID":%d,"Type":%q,"Title":%q,"AirDate":%q,"EndDate":%s,"Restricted":%v,"Rating":{"Value":%d,"MaxValue":1000,"Source":"AniDB","Votes":10},"Titles":[{"Name":%q,"Language":"x-jat","Type":"Main","Default":true,"Preferred":true,"Source":"AniDB"},{"Name":"%s EN","Language":"en","Type":"Official","Default":false,"Preferred":false,"Source":"AniDB"}]}}`,
			id, name, name, id*10, kind, name, air, end, restricted, rating, name, name)
	}
	responses := map[string]string{
		"/Group/5":            `{"IDs":{"ID":5,"MainSeries":7},"Name":"The Group","Description":"[b]Group[/b] overview"}`,
		"/Group/5/Series":     "[" + series(7, "First", "TV", "2010-01-01", `"2010-03-31"`, 800, false) + "," + series(8, "Second", "Movie", "2012-05-01", "null", 600, false) + "]",
		"/Series/7/Tags":      `[{"ID":1,"Name":"action","Weight":600,"Source":"AniDB"},{"ID":2,"Name":"18 restricted","Source":"AniDB"}]`,
		"/Series/8/Tags":      `[{"ID":3,"Name":"comedy","Source":"AniDB"}]`,
		"/Series/7/Cast":      fmt.Sprintf(`[{"Character":{"Name":"Hero"},"Staff":{"ID":1,"Type":"Person","Name":"Lead VA","Image":%s},"RoleName":"Actor","RoleDetails":"Main Character"},{"Staff":{"ID":2,"Type":"Company","Name":"Studio A"},"RoleName":"Studio","RoleDetails":"Animation Work"}]`, poster),
		"/Series/8/Cast":      `[{"Staff":{"ID":3,"Type":"Person","Name":"A Director"},"RoleName":"Director","RoleDetails":"Direction"}]`,
		"/Series/7/TMDB/Show": `[{"ContentRatings":[{"Rating":"TV-14","Country":"US","Language":"en","Source":"TMDB"}]}]`,
		"/Series/8/TMDB/Show": `[]`,
		"/Series/7/Episode":   fmt.Sprintf(`{"Total":1,"List":[{"IDs":{"ID":42,"ParentSeries":7},"Name":"Pilot","Description":"[i]It[/i] begins.","Images":{"Posters":[],%s},"AniDB":{"ID":420,"Type":"Episode","EpisodeNumber":1,"AirDate":"2010-01-07"},"TMDB":{"Episodes":[{"UserRating":{"Value":8,"MaxValue":10,"Votes":3}}]}}]}`, still),
		"/Series/8/Episode":   `{"Total":0,"List":[]}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		if path == "/api/plugin/season-posters/show-posters" {
			http.NotFound(w, r)
			return
		}
		if failCast && strings.HasSuffix(path, "/Cast") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(path, "/Tags") && r.URL.Query().Get("filter") != fmt.Sprint(genreTagFilterValue):
			t.Errorf("tags requested without the genre filter: %s", r.URL.RawQuery)
		case strings.HasSuffix(path, "/TMDB/Show") && r.URL.Query().Get("include") != "ContentRatings":
			t.Errorf("TMDB shows requested without content ratings: %s", r.URL.RawQuery)
		}
		body, ok := responses[path]
		if !ok {
			t.Errorf("unexpected request %s", path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
}

const genreTagFilterValue = 1<<31 | 1<<7

func showMetadataServer(t *testing.T, server *httptest.Server) *metadataServer {
	t.Helper()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}, now: func() time.Time { return metadataNow }}
}

func TestShowMetadataOnBothShokoGenerations(t *testing.T) {
	for _, stable := range []bool{true, false} {
		t.Run(map[bool]string{true: "stable", false: "daily"}[stable], func(t *testing.T) {
			server := showServer(t, stable, false)
			defer server.Close()
			metadata := showMetadataServer(t, server)
			ctx := context.Background()

			response, err := metadata.GetMetadata(ctx, &pluginv1.GetMetadataRequest{ItemType: "series", ProviderId: "group:5"})
			if err != nil {
				t.Fatal(err)
			}
			item := response.GetItem()
			checks := map[string][2]string{
				"overview":       {item.GetOverview(), "Group overview"},
				"first air date": {item.GetFirstAirDate(), "2010-01-01"},
				"last air date":  {item.GetLastAirDate(), "2012-05-01"},
				"status":         {item.GetStatus(), "ended"},
				"genres":         {strings.Join(item.GetGenres(), ","), "Action,Comedy"},
				"studios":        {strings.Join(item.GetStudios(), ","), "Studio A"},
				"content rating": {item.GetContentRating(), "TV-14"},
			}
			for field, pair := range checks {
				if pair[0] != pair[1] {
					t.Errorf("%s = %q, want %q", field, pair[0], pair[1])
				}
			}
			if source := item.GetRatings().AsMap()["sources"].(map[string]any)["anidb"].(map[string]any); source["score"] != float64(70) || source["votes"] != float64(20) {
				t.Errorf("rating = %#v", source)
			}
			if len(item.GetPeople()) != 2 || item.GetPeople()[0].GetKind() != "actor" || item.GetPeople()[0].GetPhotoPath() == "" || item.GetPeople()[1].GetKind() != "director" {
				t.Errorf("people = %#v", item.GetPeople())
			}
			aliases := item.GetTitleAliases()
			hasMain := false
			for _, alias := range aliases {
				hasMain = hasMain || (alias.GetTitle() == "First" && alias.GetKind() == "original")
			}
			if len(aliases) != 2 || !hasMain || !item.GetTitleAliasesComplete() {
				t.Errorf("aliases = %#v complete=%v", aliases, item.GetTitleAliasesComplete())
			}

			groupIDs, err := structpb.NewStruct(map[string]any{"shoko_group": "5"})
			if err != nil {
				t.Fatal(err)
			}
			seasons, err := metadata.GetSeasons(ctx, &pluginv1.GetSeasonsRequest{ProviderIds: groupIDs})
			if err != nil {
				t.Fatal(err)
			}
			if first := seasons.GetSeasons()[0]; first.GetOverview() != "First overview" || first.GetAirDate() != "2010-01-01" {
				t.Errorf("season = %#v", first)
			}
			episodes, err := metadata.GetEpisodes(ctx, &pluginv1.GetEpisodesRequest{ProviderIds: groupIDs, SeasonNumber: 1})
			if err != nil {
				t.Fatal(err)
			}
			episode := episodes.GetEpisodes()[0]
			if episode.GetOverview() != "It begins." || episode.GetAirDate() != "2010-01-07" || episode.GetStillPath() == "" || episode.GetRatings().AsMap()["tmdb"] != float64(8) {
				t.Errorf("episode = %#v", episode)
			}
		})
	}
}

// A failed cast request leaves out cast and studios but keeps everything else.
func TestShowMetadataSurvivesAFailedDetailRequest(t *testing.T) {
	server := showServer(t, true, true)
	defer server.Close()
	response, err := showMetadataServer(t, server).GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{ItemType: "series", ProviderId: "group:5"})
	if err != nil {
		t.Fatal(err)
	}
	item := response.GetItem()
	if len(item.GetPeople()) != 0 || len(item.GetStudios()) != 0 {
		t.Fatalf("partial cast was sent: %#v %#v", item.GetPeople(), item.GetStudios())
	}
	if item.GetFirstAirDate() == "" || len(item.GetGenres()) == 0 || item.GetContentRating() == "" {
		t.Fatalf("other fields were lost: %#v", item)
	}
}

func TestManifestDeclaresAniDBRatingSource(t *testing.T) {
	manifest, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range manifest.GetCapabilities() {
		if capability.GetType() != "metadata_provider.v1" {
			continue
		}
		sources, _ := capability.GetMetadata().AsMap()["rating_sources"].([]any)
		if len(sources) == 1 && sources[0].(map[string]any)["id"] == "anidb" {
			return
		}
		t.Fatalf("rating sources = %#v", capability.GetMetadata().AsMap()["rating_sources"])
	}
	t.Fatal("no metadata capability")
}

func TestShowTitleIsMarkedInTheLibraryLanguage(t *testing.T) {
	series := shoko.Series{AniDB: &shoko.AnimeMetadata{Titles: []shoko.Title{
		{Name: "Advancing Giants", Language: "en", Type: "Synonym"},
		{Name: "Shingeki no Kyojin", Language: "x-jat", Type: "Main"},
		{Name: "Attack on Titan", Language: "en", Type: "Official"},
		{Name: "AoT", Language: "en", Type: "Short"},
	}}}
	item := &pluginv1.MetadataItem{Title: "Attack on Titan", OriginalTitle: "Attack on Titan"}

	setTitleLanguage(item, "en-US", series)

	if item.GetTitleLanguage() != "en" || item.GetOriginalTitle() != "Shingeki no Kyojin" || item.GetOriginalLanguage() != "ja" {
		t.Fatalf("title %q (%s), original %q (%s)", item.GetTitle(), item.GetTitleLanguage(), item.GetOriginalTitle(), item.GetOriginalLanguage())
	}
	aliases := titleAliases(series)
	if len(aliases) == 0 || aliases[0].GetTitle() != "Attack on Titan" || aliases[0].GetKind() != "localized" {
		t.Fatalf("first alias = %v, want the official title", aliases)
	}
	for i, alias := range aliases[1:] {
		if aliasKindOrder[alias.GetKind()] < aliasKindOrder[aliases[i].GetKind()] {
			t.Fatalf("aliases out of order: %v", aliases)
		}
	}
}

func TestShowTitleWithoutALibraryLanguageKeepsNoLanguage(t *testing.T) {
	item := &pluginv1.MetadataItem{Title: "Bocchi the Rock!"}
	setTitleLanguage(item, "", shoko.Series{})
	if item.GetTitleLanguage() != "" {
		t.Fatalf("title language = %q", item.GetTitleLanguage())
	}
}

func TestEpisodeTitleReplacesAniDBPlaceholders(t *testing.T) {
	withTMDB := func(name, tmdb string) shoko.Episode {
		return shoko.Episode{Name: name, TMDB: json.RawMessage(`{"Episodes":[{"Title":` + strconv.Quote(tmdb) + `}]}`)}
	}
	tests := []struct {
		episode shoko.Episode
		want    string
	}{
		{withTMDB("A Good Word", "Something Else"), "A Good Word"},
		{withTMDB("Episode 12", "The Return"), "The Return"},
		{withTMDB("TBA", "After Dark"), "After Dark"},
		{withTMDB("Episode 5", "Episode 5"), "Episode 5"},
		{shoko.Episode{Name: "Episode 3"}, "Episode 3"},
		{withTMDB("Episode 1.5", "Recap"), "Recap"},
		{withTMDB("Episode Zero", "Prologue"), "Episode Zero"},
	}
	for _, tt := range tests {
		if got := episodeTitle(tt.episode); got != tt.want {
			t.Errorf("episodeTitle(%q) = %q, want %q", tt.episode.Name, got, tt.want)
		}
	}
}
