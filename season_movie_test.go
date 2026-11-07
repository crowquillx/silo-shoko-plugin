package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
