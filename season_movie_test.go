package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMovieGroupGivesEachMovieASeason(t *testing.T) {
	const (
		groupPoster = "11111111-1111-4111-8111-111111111111"
		firstPoster = "22222222-2222-4222-8222-222222222222"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/Group/5":
			fmt.Fprintf(w, `{"ids":{"id":5,"mainSeries":10},"name":"Movie Collection","images":{"posters":[{"uid":%q,"available":true,"preferred":true}]}}`, groupPoster)
		case "/api/v3/Group/5/Series":
			fmt.Fprintf(w, `[{"ids":{"id":10},"name":"First Movie","aniDB":{"type":"Movie","airDate":"2010-01-01"},"images":{"posters":[{"uid":%q,"available":true,"preferred":true}]}},{"ids":{"id":20},"name":"Second Movie","aniDB":{"type":"Movie","airDate":"2020-01-01"}}]`, firstPoster)
		case "/api/v3/Series/10/Episode":
			fmt.Fprint(w, `[{"ids":{"id":101,"parentSeries":10},"name":"Part One","aniDB":{"type":"Episode","episodeNumber":1}},{"ids":{"id":102,"parentSeries":10},"name":"Part Two","aniDB":{"type":"Episode","episodeNumber":2}}]`)
		case "/api/v3/Series/20/Episode":
			fmt.Fprint(w, `[{"ids":{"id":201,"parentSeries":20},"name":"Second Movie","aniDB":{"type":"Episode","episodeNumber":1}}]`)
		default:
			t.Fatalf("unexpected Shoko request path = %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	providerIDs, err := structpb.NewStruct(map[string]any{"shoko_group": "5"})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}

	seasons, err := metadata.GetSeasons(context.Background(), &pluginv1.GetSeasonsRequest{ProviderIds: providerIDs})
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons.GetSeasons()) != 2 {
		t.Fatalf("seasons = %#v, want one season per movie", seasons.GetSeasons())
	}
	first, second := seasons.GetSeasons()[0], seasons.GetSeasons()[1]
	if first.GetSeasonNumber() != 1 || first.GetTitle() != "First Movie" || first.GetPosterPath() != shokoImageScheme+firstPoster {
		t.Fatalf("first season = %#v", first)
	}
	// A series without its own poster falls back to the group's.
	if second.GetSeasonNumber() != 2 || second.GetTitle() != "Second Movie" || second.GetPosterPath() != shokoImageScheme+groupPoster {
		t.Fatalf("second season = %#v", second)
	}

	episodes, err := metadata.GetEpisodes(context.Background(), &pluginv1.GetEpisodesRequest{ProviderIds: providerIDs, SeasonNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes.GetEpisodes()) != 2 {
		t.Fatalf("first movie episodes = %#v, want two parts", episodes.GetEpisodes())
	}
	for index, episode := range episodes.GetEpisodes() {
		if episode.GetSeasonNumber() != 1 || episode.GetEpisodeNumber() != int32(index+1) {
			t.Fatalf("episode %d = %#v", index, episode)
		}
	}
}

func TestGroupSeasonsUseEachTVSeriesPoster(t *testing.T) {
	members := []shoko.Series{
		{IDs: shoko.IDs{ID: 10}, Name: "First", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2010-01-01"}, Images: shoko.Images{Posters: []shoko.Image{{UID: "11111111-1111-4111-8111-111111111111", Available: true}}}},
		{IDs: shoko.IDs{ID: 20}, Name: "Second", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2020-01-01"}, Images: shoko.Images{Posters: []shoko.Image{{UID: "22222222-2222-4222-8222-222222222222", Available: true}}}},
	}
	response := groupSeasons(shoko.Group{IDs: shoko.GroupIDs{ID: 5}, Name: "Group"}, members, nil)
	if len(response.GetSeasons()) != 2 {
		t.Fatalf("seasons = %#v", response.GetSeasons())
	}
	for index, season := range response.GetSeasons() {
		if season.GetPosterPath() == "" {
			t.Fatalf("season %d has no poster path", index+1)
		}
	}
	if response.GetSeasons()[0].GetPosterPath() == response.GetSeasons()[1].GetPosterPath() {
		t.Fatalf("season posters were collapsed: %#v", response.GetSeasons())
	}
}

func TestGroupSeasonsListSpecialsOnlyWhenTheyExist(t *testing.T) {
	tv := shoko.Series{IDs: shoko.IDs{ID: 10}, Name: "Show", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2010-01-01"}}
	group := shoko.Group{IDs: shoko.GroupIDs{ID: 5}, Name: "Group"}
	regular := shoko.Episode{IDs: shoko.IDs{ID: 1}, AniDB: &shoko.AnimeMetadata{Type: "Episode", EpisodeNumber: 1}}
	special := shoko.Episode{IDs: shoko.IDs{ID: 2}, AniDB: &shoko.AnimeMetadata{Type: "Special", EpisodeNumber: 1}}

	without := groupSeasons(group, []shoko.Series{tv}, map[int][]shoko.Episode{10: {regular}})
	if len(without.GetSeasons()) != 1 {
		t.Fatalf("seasons without specials = %#v", without.GetSeasons())
	}
	with := groupSeasons(group, []shoko.Series{tv}, map[int][]shoko.Episode{10: {regular, special}})
	if len(with.GetSeasons()) != 2 || with.GetSeasons()[1].GetSeasonNumber() != 0 || with.GetSeasons()[1].GetTitle() != "Specials" {
		t.Fatalf("seasons with specials = %#v", with.GetSeasons())
	}
}

func TestGroupSeasonsFollowShokoOrderAcrossTypes(t *testing.T) {
	members := []shoko.Series{
		{IDs: shoko.IDs{ID: 30}, Name: "The OVA", AniDB: &shoko.AnimeMetadata{Type: "OVA"}},
		{IDs: shoko.IDs{ID: 10}, Name: "The Show", AniDB: &shoko.AnimeMetadata{Type: "TV"}},
		{IDs: shoko.IDs{ID: 20}, Name: "The Movie", AniDB: &shoko.AnimeMetadata{Type: "Movie"}},
	}
	seasons := groupSeasons(shoko.Group{IDs: shoko.GroupIDs{ID: 5}, Name: "Group"}, members, nil).GetSeasons()
	if len(seasons) != len(members) {
		t.Fatalf("seasons = %#v", seasons)
	}
	for index, series := range members {
		if seasons[index].GetSeasonNumber() != int32(index+1) || seasons[index].GetTitle() != series.Name {
			t.Errorf("season %d = %#v, want %q", index+1, seasons[index], series.Name)
		}
	}
}

// Silo asks for a season's gallery with the show's IDs and the season number,
// and drops every image that does not carry that number back.
func TestSeasonArtworkGallery(t *testing.T) {
	const (
		groupPoster    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		groupBackdrop  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		firstPoster    = "11111111-1111-4111-8111-111111111111"
		secondPosterA  = "22222222-2222-4222-8222-222222222222"
		secondPosterB  = "33333333-3333-4333-8333-333333333333"
		secondBackdrop = "44444444-4444-4444-8444-444444444444"
		secondLogo     = "55555555-5555-4555-8555-555555555555"
		lonePoster     = "66666666-6666-4666-8666-666666666666"
	)
	image := func(uid string) string { return fmt.Sprintf(`{"uid":%q,"available":true}`, uid) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/Group/5":
			fmt.Fprintf(w, `{"ids":{"id":5,"mainSeries":10},"name":"Group","images":{"posters":[%s],"backdrops":[%s]}}`, image(groupPoster), image(groupBackdrop))
		case "/api/v3/Group/5/Series":
			fmt.Fprintf(w, `[{"ids":{"id":10},"name":"First","images":{"posters":[%s]}},`+
				`{"ids":{"id":20},"name":"Second","images":{"posters":[%s,%s,{"uid":"77777777-7777-4777-8777-777777777777","available":false}],"backdrops":[%s],"logos":[%s]}}]`,
				image(firstPoster), image(secondPosterA), image(secondPosterB), image(secondBackdrop), image(secondLogo))
		case "/api/v3/Series/7":
			fmt.Fprintf(w, `{"ids":{"id":7},"name":"Lone","images":{"posters":[%s]}}`, image(lonePoster))
		default:
			t.Errorf("unexpected Shoko request path = %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}
	season := func(number int32) *int32 { return &number }
	ids := func(values map[string]any) *structpb.Struct {
		t.Helper()
		result, err := structpb.NewStruct(values)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	group := ids(map[string]any{"shoko_group": "5"})
	lone := ids(map[string]any{"shoko_series": "7"})

	tests := []struct {
		name    string
		request *pluginv1.GetImagesRequest
		want    []string // kind and image ID, in order
	}{
		{"a group's season is its member series", &pluginv1.GetImagesRequest{ProviderId: "group:5", ItemType: "series", ProviderIds: group, SeasonNumber: season(2)},
			[]string{"poster " + secondPosterA, "poster " + secondPosterB, "backdrop " + secondBackdrop}},
		{"the first season", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: group, SeasonNumber: season(1)},
			[]string{"poster " + firstPoster}},
		{"the typed show ID alone", &pluginv1.GetImagesRequest{ProviderId: "group:5", ItemType: "series", SeasonNumber: season(1)},
			[]string{"poster " + firstPoster}},
		{"specials share the group's images", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: group, SeasonNumber: season(0)},
			[]string{"poster " + groupPoster, "backdrop " + groupBackdrop}},
		{"a season the group does not have", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: group, SeasonNumber: season(3)}, nil},
		{"the group decides when a series ID is also present", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: ids(map[string]any{"shoko_group": "5", "shoko_series": "10"}), SeasonNumber: season(2)},
			[]string{"poster " + secondPosterA, "poster " + secondPosterB, "backdrop " + secondBackdrop}},
		{"a series outside a group is season one", &pluginv1.GetImagesRequest{ProviderId: "series:7", ItemType: "series", ProviderIds: lone, SeasonNumber: season(1)},
			[]string{"poster " + lonePoster}},
		{"and its own specials season", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: lone, SeasonNumber: season(0)},
			[]string{"poster " + lonePoster}},
		{"but has no second season", &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: lone, SeasonNumber: season(2)}, nil},
		{"an unknown show", &pluginv1.GetImagesRequest{ProviderId: "episode:42", SeasonNumber: season(1)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := metadata.GetImages(context.Background(), tt.request)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, record := range response.GetImages() {
				if record.SeasonNumber == nil || record.GetSeasonNumber() != tt.request.GetSeasonNumber() {
					t.Errorf("image %s season = %v, want %d", record.GetUrl(), record.SeasonNumber, tt.request.GetSeasonNumber())
				}
				got = append(got, record.GetKind()+" "+strings.TrimPrefix(record.GetUrl(), shokoImageScheme))
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("images = %v, want %v", got, tt.want)
			}
		})
	}

	// Without a season number the show's own gallery is unchanged and unmarked.
	show, err := metadata.GetImages(context.Background(), &pluginv1.GetImagesRequest{ItemType: "series", ProviderIds: group, ProviderId: "group:5:season:0"})
	if err != nil {
		t.Fatal(err)
	}
	if records := show.GetImages(); len(records) != 2 || records[0].GetUrl() != shokoImageScheme+groupPoster || records[0].SeasonNumber != nil || records[1].SeasonNumber != nil {
		t.Fatalf("show images = %#v, want the group's two images without a season number", records)
	}
}

// Each season is a Shoko series with its own backdrops, picked like a series'
// backdrop: the preferred one, else the first available.
func TestSeasonsCarryTheirOwnBackdrop(t *testing.T) {
	const (
		firstBackdrop  = "11111111-1111-4111-8111-111111111111"
		secondBackdrop = "22222222-2222-4222-8222-222222222222"
		groupBackdrop  = "33333333-3333-4333-8333-333333333333"
	)
	members := []shoko.Series{
		{IDs: shoko.IDs{ID: 10}, Name: "First", Images: shoko.Images{Backdrops: []shoko.Image{
			{UID: "99999999-9999-4999-8999-999999999999", Available: false, Preferred: true},
			{UID: "88888888-8888-4888-8888-888888888888", Available: true},
			{UID: firstBackdrop, Available: true, Preferred: true},
		}}},
		{IDs: shoko.IDs{ID: 20}, Name: "Second", Images: shoko.Images{Backdrops: []shoko.Image{{UID: secondBackdrop, Available: true}}}},
		{IDs: shoko.IDs{ID: 30}, Name: "Third"},
	}
	group := shoko.Group{IDs: shoko.GroupIDs{ID: 5}, Name: "Group", Images: shoko.Images{Backdrops: []shoko.Image{{UID: groupBackdrop, Available: true}}}}
	special := shoko.Episode{IDs: shoko.IDs{ID: 2}, AniDB: &shoko.AnimeMetadata{Type: "Special", EpisodeNumber: 1}}
	seasons := groupSeasons(group, members, map[int][]shoko.Episode{10: {special}}).GetSeasons()
	// A series without a backdrop has none: the show's backdrop is not its own.
	want := []string{shokoImageScheme + firstBackdrop, shokoImageScheme + secondBackdrop, "", shokoImageScheme + groupBackdrop}
	if len(seasons) != len(want) {
		t.Fatalf("seasons = %#v", seasons)
	}
	for index, season := range seasons {
		if season.GetBackdropPath() != want[index] {
			t.Errorf("season %d backdrop = %q, want %q", season.GetSeasonNumber(), season.GetBackdropPath(), want[index])
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/Series/7":
			fmt.Fprintf(w, `{"ids":{"id":7},"name":"Lone","images":{"backdrops":[{"uid":%q,"available":true}]}}`, firstBackdrop)
		case "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `[{"ids":{"id":101,"parentSeries":7},"name":"One","aniDB":{"type":"Episode","episodeNumber":1}}]`)
		default:
			t.Errorf("unexpected Shoko request path = %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := shoko.NewClient(shoko.Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}
	lone, err := metadata.GetSeasons(context.Background(), &pluginv1.GetSeasonsRequest{SeriesProviderId: "series:7"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lone.GetSeasons(); len(got) != 1 || got[0].GetBackdropPath() != shokoImageScheme+firstBackdrop {
		t.Fatalf("seasons of a series outside a group = %#v", got)
	}
}
