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

func TestMovieOnlyGroupReturnsArtworkSeasonAndSequentialEpisodes(t *testing.T) {
	const (
		groupPoster = "11111111-1111-4111-8111-111111111111"
		firstPoster = "22222222-2222-4222-8222-222222222222"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/Group/5":
			fmt.Fprintf(w, `{"ids":{"id":5,"mainSeries":10},"name":"Movie Collection","images":{"posters":[{"uid":%q,"available":true,"preferred":true}]}}`, groupPoster)
		case "/api/v3/Group/5/Series":
			fmt.Fprintf(w, `[{"ids":{"id":20},"name":"Second Movie","aniDB":{"type":"Movie","airDate":"2020-01-01"}},{"ids":{"id":10},"name":"First Movie","aniDB":{"type":"Movie","airDate":"2010-01-01"},"images":{"posters":[{"uid":%q,"available":true,"preferred":true}]}}]`, firstPoster)
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
	if len(seasons.GetSeasons()) != 1 {
		t.Fatalf("seasons = %#v, want one movie season", seasons.GetSeasons())
	}
	season := seasons.GetSeasons()[0]
	if season.GetSeasonNumber() != 1 || season.GetTitle() != "Movie Collection" {
		t.Fatalf("movie season = %#v", season)
	}
	if got, want := season.GetPosterPath(), shokoImageScheme+firstPoster; got != want {
		t.Fatalf("movie season poster = %q, want %q", got, want)
	}

	episodes, err := metadata.GetEpisodes(context.Background(), &pluginv1.GetEpisodesRequest{ProviderIds: providerIDs, SeasonNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes.GetEpisodes()) != 3 {
		t.Fatalf("movie episodes = %#v, want three", episodes.GetEpisodes())
	}
	for index, episode := range episodes.GetEpisodes() {
		if got, want := episode.GetEpisodeNumber(), int32(index+1); got != want {
			t.Fatalf("movie episode %d number = %d, want %d", index, got, want)
		}
		if episode.GetSeasonNumber() != 1 {
			t.Fatalf("movie episode %d season = %d, want 1", index, episode.GetSeasonNumber())
		}
	}
}

func TestGroupSeasonsUseEachTVSeriesPoster(t *testing.T) {
	members := []shoko.Series{
		{IDs: shoko.IDs{ID: 10}, Name: "First", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2010-01-01"}, Images: shoko.Images{Posters: []shoko.Image{{UID: "11111111-1111-4111-8111-111111111111", Available: true}}}},
		{IDs: shoko.IDs{ID: 20}, Name: "Second", AniDB: &shoko.AnimeMetadata{Type: "TV", AirDate: "2020-01-01"}, Images: shoko.Images{Posters: []shoko.Image{{UID: "22222222-2222-4222-8222-222222222222", Available: true}}}},
	}
	response := groupSeasons(shoko.Group{IDs: shoko.GroupIDs{ID: 5}, Name: "Group"}, members)
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
