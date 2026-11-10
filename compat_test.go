package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	compatAPIKey   = "compat-key"
	compatPosterID = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
)

// shokoGeneration describes what differs between the two server generations.
// Field names are PascalCase, which is what real Shoko servers send.
type shokoGeneration struct {
	name         string
	folderRoute  string
	location     string
	poster       string
	wantPoster   string
	wantImageURL string
}

var shokoGenerations = []shokoGeneration{
	{
		name:         "stable",
		folderRoute:  "/ImportFolder",
		location:     `{"ID":1,"FileID":100,"ImportFolderID":1,"RelativePath":"Show/ep1.mkv","IsAccessible":true}`,
		poster:       `{"ID":11,"Type":"Poster","Source":"AniDB","RelativeFilepath":"/11","Preferred":true,"Disabled":false}`,
		wantPoster:   "shoko://image/AniDB/Poster/11",
		wantImageURL: "/api/v3/Image/AniDB/Poster/11",
	},
	{
		name:         "daily",
		folderRoute:  "/ManagedFolder",
		location:     `{"ID":1,"FileID":100,"ManagedFolderID":1,"RelativePath":"/Show/ep1.mkv","IsAccessible":true}`,
		poster:       fmt.Sprintf(`{"ID":11,"UID":%q,"Type":"Poster","Source":"AniDB","Available":true,"Preferred":true,"Disabled":false}`, compatPosterID),
		wantPoster:   "shoko://image/" + compatPosterID,
		wantImageURL: "/api/v3/Image/" + compatPosterID,
	},
}

func (g shokoGeneration) handler(t *testing.T) http.Handler {
	t.Helper()
	series := fmt.Sprintf(`{"IDs":{"ID":7,"ParentGroup":5,"TopLevelGroup":5,"AniDB":70,"TvDB":[],"IMDB":[],"TMDB":{"Movie":[],"Show":[]},"MAL":[]},`+
		`"Name":"Show","Description":"d","Created":"2026-01-01T00:00:00Z","Updated":"2026-01-01T00:00:00Z",`+
		`"AniDB":{"ID":70,"Type":"TV","Title":"Show","AirDate":"2020-01-01"},"Images":{"Posters":[%s],"Backdrops":[],"Banners":[],"Logos":[]}}`, g.poster)
	episode := func(id, number int, kind string) string {
		return fmt.Sprintf(`{"IDs":{"ID":%d,"ParentSeries":7,"AniDB":%d,"TvDB":[],"IMDB":[],"TMDB":{"Episode":[],"Movie":[],"Show":[]}},`+
			`"Name":"Episode %d","Description":"d","Created":"2026-01-01T00:00:00Z","Updated":"2026-01-01T00:00:00Z",`+
			`"AniDB":{"ID":%d,"AnimeID":70,"Type":%q,"EpisodeNumber":%d,"AirDate":null,"Title":"t"}}`, id, id*10, id, id*10, kind, number)
	}
	xref := func(episodeID int) string {
		return fmt.Sprintf(`{"ID":%d,"AniDB":%d,"TMDB":{"Episode":[],"Movie":[],"Show":[]},"ReleaseGroup":null,"ED2K":"abc","FileSize":1,"Percentage":{"Start":0,"End":100,"Size":100,"Group":1},"Source":"AniDB"}`, episodeID, episodeID*10)
	}
	file := fmt.Sprintf(`{"ID":100,"Size":1,"IsVariation":false,"IsIgnored":false,"Created":"2026-01-01T00:00:00Z","Updated":"2026-01-01T00:00:00Z",`+
		`"Locations":[%s],"SeriesIDs":[{"SeriesID":{"ID":7,"AniDB":70,"TMDB":{"Movie":[],"Show":[]}},"EpisodeIDs":[%s]}]}`, g.location, xref(42))
	responses := map[string]string{
		g.folderRoute:             `[{"ID":1,"Name":"Anime","Path":"/anime/"}]`,
		g.folderRoute + "/1/File": fmt.Sprintf(`{"Total":1,"List":[%s]}`, file),
		"/Series/7":               series,
		"/Series/7/Episode":       fmt.Sprintf(`{"Total":2,"List":[%s,%s]}`, episode(42, 1, "Episode"), episode(43, 1, "Special")),
		"/Group/5":                fmt.Sprintf(`{"IDs":{"ID":5,"PreferredSeries":null,"MainSeries":7,"ParentGroup":null,"TopLevelGroup":5},"Name":"Show Group","Images":{"Posters":[%s],"Backdrops":[],"Banners":[],"Logos":[]}}`, g.poster),
		"/Group/5/Series":         fmt.Sprintf(`[%s]`, series),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != compatAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := responses[strings.TrimPrefix(r.URL.Path, "/api/v3")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
}

func configuredRuntime(t *testing.T, baseURL, apiKey, folderMap, vfsRoot string) *runtimeServer {
	t.Helper()
	value, err := structpb.NewStruct(map[string]any{
		"base_url":           baseURL,
		"api_key":            apiKey,
		"vfs_root":           vfsRoot,
		"managed_folder_map": folderMap,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeServer{}
	if _, err := runtime.Configure(context.Background(), &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.cancelBackgroundCrawl)
	return runtime
}

func mediaFixture(t *testing.T) (source, media string) {
	t.Helper()
	source = filepath.Join(t.TempDir(), "source")
	media = filepath.Join(source, "Show", "ep1.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	return source, media
}

// Runs the whole plugin flow against each server generation: connection test,
// reconcile, scan journal, metadata, and artwork.
func TestPluginWorksOnBothShokoGenerations(t *testing.T) {
	for _, generation := range shokoGenerations {
		t.Run(generation.name, func(t *testing.T) {
			server := httptest.NewServer(generation.handler(t))
			defer server.Close()
			source, media := mediaFixture(t)
			outputRoot := filepath.Join(t.TempDir(), "vfs")
			runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), outputRoot)
			metadata := &metadataServer{runtime: runtime}
			ctx := context.Background()

			if _, err := metadata.Search(ctx, &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", Year: 1999}); err != nil {
				t.Fatalf("connection test: %v", err)
			}

			output, err := (&scheduledTaskServer{runtime: runtime}).Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
			if err != nil {
				t.Fatal(err)
			}
			values := output.GetOutput().AsMap()
			if values["complete"] != true || values["failure_count"] != float64(0) || values["diagnostic_count"] != float64(0) {
				t.Fatalf("task output = %#v", values)
			}

			logical := filepath.Join(outputRoot, "Show Group (2020)", "Season 01", "Show - S01E01 [Shoko Group=5] [Shoko Series=7] [AniDB Anime=70] [Shoko Episode=42] [Shoko File=100].mkv")
			resolved, err := filepath.EvalSymlinks(logical)
			if err != nil {
				t.Fatal(err)
			}
			wantMedia, err := filepath.EvalSymlinks(media)
			if err != nil {
				t.Fatal(err)
			}
			if resolved != wantMedia {
				t.Fatalf("link resolves to %q, want %q", resolved, wantMedia)
			}

			pathIDs, err := structpb.NewStruct(map[string]any{"_filepath": logical})
			if err != nil {
				t.Fatal(err)
			}
			found, err := metadata.Search(ctx, &pluginv1.SearchMetadataRequest{Query: "Show Group", ItemType: "series", ProviderIds: pathIDs})
			if err != nil {
				t.Fatal(err)
			}
			if len(found.GetResults()) != 1 || found.GetResults()[0].GetProviderId() != "group:5" {
				t.Fatalf("search results = %#v", found.GetResults())
			}

			groupIDs, err := structpb.NewStruct(map[string]any{"shoko_group": "5"})
			if err != nil {
				t.Fatal(err)
			}
			seasons, err := metadata.GetSeasons(ctx, &pluginv1.GetSeasonsRequest{ProviderIds: groupIDs})
			if err != nil {
				t.Fatal(err)
			}
			if len(seasons.GetSeasons()) != 2 {
				t.Fatalf("seasons = %#v, want season one and specials", seasons.GetSeasons())
			}
			if got := seasons.GetSeasons()[0].GetPosterPath(); got != generation.wantPoster {
				t.Fatalf("season poster = %q, want %q", got, generation.wantPoster)
			}
			episodes, err := metadata.GetEpisodes(ctx, &pluginv1.GetEpisodesRequest{ProviderIds: groupIDs, SeasonNumber: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(episodes.GetEpisodes()) != 1 || episodes.GetEpisodes()[0].GetEpisodeNumber() != 1 {
				t.Fatalf("episodes = %#v", episodes.GetEpisodes())
			}

			images, err := metadata.GetImages(ctx, &pluginv1.GetImagesRequest{ProviderIds: groupIDs})
			if err != nil {
				t.Fatal(err)
			}
			if len(images.GetImages()) != 1 || images.GetImages()[0].GetUrl() != generation.wantPoster {
				t.Fatalf("images = %#v", images.GetImages())
			}
			seasonOne := int32(1)
			gallery, err := metadata.GetImages(ctx, &pluginv1.GetImagesRequest{ProviderId: "group:5", ItemType: "series", ProviderIds: groupIDs, SeasonNumber: &seasonOne})
			if err != nil {
				t.Fatal(err)
			}
			if records := gallery.GetImages(); len(records) != 1 || records[0].GetUrl() != generation.wantPoster || records[0].SeasonNumber == nil || records[0].GetSeasonNumber() != 1 {
				t.Fatalf("season gallery = %#v, want the season's poster marked season 1", records)
			}
			resolvedImage, err := metadata.ResolveImageURL(ctx, &pluginv1.ResolveImageURLRequest{Path: strings.TrimPrefix(generation.wantPoster, "shoko://")})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := resolvedImage.GetUrl(), server.URL+generation.wantImageURL; got != want {
				t.Fatalf("image URL = %q, want %q", got, want)
			}
		})
	}
}

func TestConnectionTestReportsRealProblems(t *testing.T) {
	generation := shokoGenerations[0]
	server := httptest.NewServer(generation.handler(t))
	defer server.Close()
	source, _ := mediaFixture(t)
	probe := &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", Year: 1999}

	tests := []struct {
		name      string
		baseURL   string
		apiKey    string
		folderMap string
		want      string
	}{
		{"wrong api key", server.URL, "wrong", fmt.Sprintf(`{"1":%q}`, source), "rejected the API key"},
		{"unknown folder", server.URL, compatAPIKey, fmt.Sprintf(`{"9":%q}`, source), "no folder 9"},
		{"unmapped path", server.URL, compatAPIKey, `{"1":"/does/not/exist"}`, "not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := configuredRuntime(t, tt.baseURL, tt.apiKey, tt.folderMap, filepath.Join(t.TempDir(), "vfs"))
			_, err := (&metadataServer{runtime: runtime}).Search(context.Background(), probe)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// Library scans send a file path, so unrelated files must never touch Shoko.
func TestScansOfOtherLibrariesDoNotContactShoko(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	source, _ := mediaFixture(t)
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), filepath.Join(t.TempDir(), "vfs"))
	ids, err := structpb.NewStruct(map[string]any{"_filepath": "/movies/The Matrix (1999)/The Matrix.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&metadataServer{runtime: runtime}).Search(context.Background(), &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", ProviderIds: ids})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetResults()) != 0 || requests.Load() != 0 {
		t.Fatalf("results = %d, requests = %d", len(response.GetResults()), requests.Load())
	}
}

func TestReconcileTaskFinishesInBackgroundWithoutAutoscan(t *testing.T) {
	generation := shokoGenerations[1]
	release := make(chan struct{})
	handler := generation.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1/File") {
			<-release
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	source, _ := mediaFixture(t)
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), outputRoot)
	task := &scheduledTaskServer{runtime: runtime, stepBudget: 20 * time.Millisecond}
	ctx := context.Background()

	output, err := task.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
	if err != nil {
		t.Fatal(err)
	}
	values := output.GetOutput().AsMap()
	if values["complete"] != false || !strings.Contains(values["message"].(string), "background") {
		t.Fatalf("task output = %#v", values)
	}
	if !runtime.backgroundCrawlRunning() {
		t.Fatal("no background crawl was started")
	}

	// A second run while the crawl continues reports it rather than racing it.
	output, err = task.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
	if err != nil {
		t.Fatal(err)
	}
	if output.GetOutput().AsMap()["complete"] != false {
		t.Fatalf("second run output = %#v", output.GetOutput().AsMap())
	}

	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for runtime.backgroundCrawlRunning() {
		if time.Now().After(deadline) {
			t.Fatal("background crawl did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	matches, err := filepath.Glob(filepath.Join(outputRoot, "*", "Season 01", "*.mkv"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("links = %v, err = %v", matches, err)
	}
	if err := runtime.takeBackgroundCrawlError(); err != nil {
		t.Fatal(err)
	}
}

func TestNextTaskRunReportsBackgroundFailure(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ManagedFolder") {
			<-release
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	source, _ := mediaFixture(t)
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), filepath.Join(t.TempDir(), "vfs"))
	task := &scheduledTaskServer{runtime: runtime, stepBudget: 20 * time.Millisecond}
	ctx := context.Background()

	if _, err := task.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for runtime.backgroundCrawlRunning() {
		if time.Now().After(deadline) {
			t.Fatal("background crawl did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := task.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the background failure", err)
	}
}

func TestConfigureStopsRunningBackgroundCrawl(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	source, _ := mediaFixture(t)
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), filepath.Join(t.TempDir(), "vfs"))
	task := &scheduledTaskServer{runtime: runtime, stepBudget: 20 * time.Millisecond}
	if _, err := task.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"}); err != nil {
		t.Fatal(err)
	}
	if !runtime.backgroundCrawlRunning() {
		t.Fatal("no background crawl was started")
	}
	value, err := structpb.NewStruct(map[string]any{
		"base_url":           server.URL,
		"api_key":            compatAPIKey,
		"vfs_root":           filepath.Join(t.TempDir(), "other-vfs"),
		"managed_folder_map": fmt.Sprintf(`{"1":%q}`, source),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Configure(context.Background(), &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}}); err != nil {
		t.Fatal(err)
	}
	if runtime.backgroundCrawlRunning() {
		t.Fatal("crawl still reported as running after reconfigure")
	}
	if err := runtime.takeBackgroundCrawlError(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A poll must answer quickly while a crawl runs, and once the crawl has built
// the VFS, a source without a marker must still get the build scanned.
func TestFirstPollAfterTheInitialCrawlScansTheWholeVFS(t *testing.T) {
	generation := shokoGenerations[1]
	release := make(chan struct{})
	handler := generation.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1/File") {
			<-release
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	source, _ := mediaFixture(t)
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), outputRoot)
	task := &scheduledTaskServer{runtime: runtime, stepBudget: 20 * time.Millisecond}
	scanner := &scanSourceServer{runtime: runtime}
	if _, err := task.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"}); err != nil {
		t.Fatal(err)
	}

	during := pollWithin(t, scanner, "", time.Second)
	if len(during.GetSourcePaths()) != 0 || during.GetNextMarker() != "" {
		t.Fatalf("poll during the crawl = paths %v, marker %q; want none and no marker", during.GetSourcePaths(), during.GetNextMarker())
	}

	close(release)
	waitForBackgroundCrawl(t, runtime)
	first := pollWithin(t, scanner, "", time.Second)
	// Silo scans each reported path's parent folder, here the library root.
	if got := first.GetSourcePaths(); len(got) != 1 || filepath.Dir(got[0]) != outputRoot {
		t.Fatalf("first poll after the crawl = paths %v, want one path directly in %s", got, outputRoot)
	}
	if first.GetNextMarker() == "" || first.GetNextMarker() == "0" {
		t.Fatalf("first poll marker = %q, want the journal tail", first.GetNextMarker())
	}
	next := pollWithin(t, scanner, first.GetNextMarker(), time.Second)
	if len(next.GetSourcePaths()) != 0 || next.GetNextMarker() != first.GetNextMarker() {
		t.Fatalf("next poll = paths %v, marker %q", next.GetSourcePaths(), next.GetNextMarker())
	}
}

// A crawl left pending, for example by a restart, is finished in the
// background; the poll neither blocks on it nor loses its changes.
func TestPollHandsAPendingCrawlToTheBackground(t *testing.T) {
	generation := shokoGenerations[1]
	release := make(chan struct{})
	handler := generation.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1/File") {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	source, _ := mediaFixture(t)
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), outputRoot)
	task := &scheduledTaskServer{runtime: runtime, stepBudget: 20 * time.Millisecond}
	scanner := &scanSourceServer{runtime: runtime}
	start := pollWithin(t, scanner, "", time.Second)
	if start.GetNextMarker() != "0" {
		t.Fatalf("first poll on an empty VFS = marker %q, want 0", start.GetNextMarker())
	}
	if _, err := task.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"}); err != nil {
		t.Fatal(err)
	}
	runtime.cancelBackgroundCrawl()

	during := pollWithin(t, scanner, "0", time.Second)
	if len(during.GetSourcePaths()) != 0 || during.GetNextMarker() != "0" {
		t.Fatalf("poll with a pending crawl = paths %v, marker %q", during.GetSourcePaths(), during.GetNextMarker())
	}
	if !runtime.backgroundCrawlRunning() {
		t.Fatal("the poll did not hand the pending crawl to the background")
	}

	close(release)
	waitForBackgroundCrawl(t, runtime)
	after := pollWithin(t, scanner, "0", time.Second)
	if len(after.GetSourcePaths()) != 1 || filepath.Dir(filepath.Dir(filepath.Dir(after.GetSourcePaths()[0]))) != outputRoot {
		t.Fatalf("poll after the crawl = paths %v, want the new link", after.GetSourcePaths())
	}
}

func pollWithin(t *testing.T, scanner *scanSourceServer, marker string, limit time.Duration) *pluginv1.PollChangesResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	response, err := scanner.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "shoko", Marker: marker})
	if err != nil {
		t.Fatalf("poll with marker %q: %v", marker, err)
	}
	return response
}

func waitForBackgroundCrawl(t *testing.T, runtime *runtimeServer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.backgroundCrawlRunning() {
		if time.Now().After(deadline) {
			t.Fatal("background crawl did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := runtime.takeBackgroundCrawlError(); err != nil {
		t.Fatal(err)
	}
}

// In the S3 image mode, an image that cannot be copied falls back to Shoko's own
// URL, so clients that can reach Shoko still show it.
func TestS3ImageModeFallsBackToShokoWhenTheCopyFails(t *testing.T) {
	brokenS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer brokenS3.Close()
	source, _ := mediaFixture(t)
	value, err := structpb.NewStruct(map[string]any{
		"base_url":             "http://shoko.invalid:8111",
		"api_key":              compatAPIKey,
		"vfs_root":             filepath.Join(t.TempDir(), "vfs"),
		"managed_folder_map":   fmt.Sprintf(`{"1":%q}`, source),
		"image_mode":           "s3",
		"s3_endpoint":          brokenS3.URL,
		"s3_bucket":            "shoko-images",
		"s3_access_key_id":     "key",
		"s3_secret_access_key": "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeServer{}
	if _, err := runtime.Configure(context.Background(), &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.cancelBackgroundCrawl)
	if runtime.images == nil {
		t.Fatal("the S3 image mode built no image store")
	}

	const uuid = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := (&metadataServer{runtime: runtime}).ResolveImageURLs(ctx, &pluginv1.ResolveImageURLsRequest{Paths: []string{"image/" + uuid, "image/bogus"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := response.GetUrls()["image/"+uuid], "http://shoko.invalid:8111/api/v3/Image/"+uuid; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if got := response.GetUrls()["image/bogus"]; got != "" {
		t.Fatalf("invalid path resolved to %q", got)
	}
}

// Silo stops a plugin whose Configure fails, so settings the plugin cannot use
// are accepted and reported by the calls that need them. Silo's connection
// check is Configure with the staged settings followed by a title search, and
// must still fail on them.
func TestUnusableSettingsAreReportedByCallsNotByConfigure(t *testing.T) {
	source, _ := mediaFixture(t)
	folderMap := fmt.Sprintf(`{"1":%q}`, source)
	vfsRoot := filepath.Join(t.TempDir(), "vfs")
	complete := func(overrides map[string]any) map[string]any {
		values := map[string]any{"base_url": "http://127.0.0.1:1", "api_key": compatAPIKey, "vfs_root": vfsRoot, "managed_folder_map": folderMap}
		for key, value := range overrides {
			if value == nil {
				delete(values, key)
			} else {
				values[key] = value
			}
		}
		return values
	}
	entries := func(values map[string]any) []*pluginv1.ConfigEntry {
		t.Helper()
		if values == nil {
			return nil
		}
		value, err := structpb.NewStruct(values)
		if err != nil {
			t.Fatal(err)
		}
		return []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}
	}
	probe := &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", Year: 1999, Language: "en"}
	ctx := context.Background()

	tests := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{"no settings", nil, "shokoanime: not configured: connection config is required"},
		{"empty settings", map[string]any{}, "shokoanime: not configured: managed_folder_map is required"},
		{"missing URL", complete(map[string]any{"base_url": nil}), "shokoanime: not configured: base_url is required"},
		{"bad URL", complete(map[string]any{"base_url": "shoko:8111"}), "shokoanime: not configured: base_url must be an http or https URL"},
		{"missing API key", complete(map[string]any{"api_key": ""}), "shokoanime: not configured: api_key is required"},
		{"missing VFS root", complete(map[string]any{"vfs_root": nil}), "shokoanime: not configured: vfs_root must be an absolute path"},
		{"malformed folder map", complete(map[string]any{"managed_folder_map": "{"}), "shokoanime: not configured: decode managed_folder_map"},
		{"overlapping roots", complete(map[string]any{"vfs_root": filepath.Join(source, "vfs")}), "overlaps managed-folder 1 root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &runtimeServer{}
			if _, err := runtime.Configure(ctx, &pluginv1.ConfigureRequest{Config: entries(tt.values)}); err != nil {
				t.Fatalf("Configure failed, which makes Silo stop the plugin: %v", err)
			}
			metadata := &metadataServer{runtime: runtime}
			calls := map[string]func() error{
				"connection check": func() error { _, err := metadata.Search(ctx, probe); return err },
				"metadata": func() error {
					_, err := metadata.GetMetadata(ctx, &pluginv1.GetMetadataRequest{ItemType: "series", ProviderId: "group:5"})
					return err
				},
				"seasons": func() error {
					_, err := metadata.GetSeasons(ctx, &pluginv1.GetSeasonsRequest{SeriesProviderId: "group:5"})
					return err
				},
				"episodes": func() error {
					_, err := metadata.GetEpisodes(ctx, &pluginv1.GetEpisodesRequest{SeriesProviderId: "group:5"})
					return err
				},
				"images": func() error {
					_, err := metadata.GetImages(ctx, &pluginv1.GetImagesRequest{ProviderId: "group:5"})
					return err
				},
				"image URL": func() error {
					_, err := metadata.ResolveImageURL(ctx, &pluginv1.ResolveImageURLRequest{Path: "image/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
					return err
				},
				"image URLs": func() error {
					_, err := metadata.ResolveImageURLs(ctx, &pluginv1.ResolveImageURLsRequest{Paths: []string{"image/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}})
					return err
				},
				"task": func() error {
					_, err := (&scheduledTaskServer{runtime: runtime}).Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
					return err
				},
				"dry run": func() error {
					input, _ := structpb.NewStruct(map[string]any{"dry_run": true})
					_, err := (&scheduledTaskServer{runtime: runtime}).Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile", Input: input})
					return err
				},
				"poll": func() error {
					_, err := (&scanSourceServer{runtime: runtime}).PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "shoko"})
					return err
				},
			}
			for name, call := range calls {
				if err := call(); err == nil || !strings.Contains(err.Error(), "not configured: ") || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s: err = %v, want %q", name, err, tt.want)
				}
			}
			// A scan of another library's file needs no settings to learn the file
			// is not Shoko's.
			pathIDs, err := structpb.NewStruct(map[string]any{"_filepath": "/movies/The Matrix (1999)/The Matrix.mkv"})
			if err != nil {
				t.Fatal(err)
			}
			other, err := metadata.Search(ctx, &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", ProviderIds: pathIDs})
			if err != nil || len(other.GetResults()) != 0 {
				t.Errorf("search for another library's file = %v, %v; want no results and no error", other.GetResults(), err)
			}
			if _, err := os.Stat(vfsRoot); !os.IsNotExist(err) {
				t.Errorf("unusable settings touched the VFS root: %v", err)
			}
		})
	}
}

// Settings the plugin cannot use replace working ones rather than leaving the
// old server in use, and working settings clear the problem.
func TestConfigureReplacesSettingsEitherWay(t *testing.T) {
	generation := shokoGenerations[0]
	server := httptest.NewServer(generation.handler(t))
	defer server.Close()
	source, _ := mediaFixture(t)
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), filepath.Join(t.TempDir(), "vfs"))
	metadata := &metadataServer{runtime: runtime}
	probe := &pluginv1.SearchMetadataRequest{Query: "The Matrix", ItemType: "movie", Year: 1999}
	ctx := context.Background()
	if _, err := metadata.Search(ctx, probe); err != nil {
		t.Fatalf("connection check with working settings: %v", err)
	}
	working := runtime.cfg

	if _, err := runtime.Configure(ctx, &pluginv1.ConfigureRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.Search(ctx, probe); err == nil || !strings.Contains(err.Error(), "not configured: connection config is required") {
		t.Fatalf("connection check after the settings were removed: err = %v", err)
	}

	value, err := structpb.NewStruct(map[string]any{
		"base_url":           working.BaseURL,
		"api_key":            working.APIKey,
		"vfs_root":           working.VFSRoot,
		"managed_folder_map": fmt.Sprintf(`{"1":%q}`, source),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Configure(ctx, &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.Search(ctx, probe); err != nil {
		t.Fatalf("connection check after the settings were restored: %v", err)
	}
}

// A marker ahead of the journal means the VFS root was wiped or recreated and
// the journal started again. The source starts over like a first poll instead
// of skipping every change until the new journal passes the old marker.
func TestPollStartsOverWhenTheMarkerIsNotInTheJournal(t *testing.T) {
	generation := shokoGenerations[1]
	server := httptest.NewServer(generation.handler(t))
	defer server.Close()
	source, _ := mediaFixture(t)
	outputRoot := filepath.Join(t.TempDir(), "vfs")
	runtime := configuredRuntime(t, server.URL, compatAPIKey, fmt.Sprintf(`{"1":%q}`, source), outputRoot)
	task := &scheduledTaskServer{runtime: runtime}
	scanner := &scanSourceServer{runtime: runtime}
	ctx := context.Background()
	reconcileVFS := func() {
		t.Helper()
		output, err := task.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "reconcile"})
		if err != nil {
			t.Fatal(err)
		}
		if output.GetOutput().AsMap()["complete"] != true {
			t.Fatalf("task output = %#v", output.GetOutput().AsMap())
		}
	}
	reconcileVFS()
	tail := pollWithin(t, scanner, "", time.Second).GetNextMarker()
	if tail == "" || tail == "0" {
		t.Fatalf("marker after the first build = %q", tail)
	}

	for _, marker := range []string{"100000", "from-another-version"} {
		response := pollWithin(t, scanner, marker, time.Second)
		// The whole VFS is scanned once, through the path that sits in its root.
		if got := response.GetSourcePaths(); len(got) != 1 || filepath.Dir(got[0]) != outputRoot || response.GetNextMarker() != tail {
			t.Fatalf("poll with marker %q = paths %v, marker %q; want one path directly in %s and marker %q", marker, got, response.GetNextMarker(), outputRoot, tail)
		}
	}

	// The VFS root is deleted: the journal is gone and its sequence restarts.
	if err := os.RemoveAll(outputRoot); err != nil {
		t.Fatal(err)
	}
	emptied := pollWithin(t, scanner, tail, time.Second)
	if len(emptied.GetSourcePaths()) != 0 || emptied.GetNextMarker() != "0" {
		t.Fatalf("poll after the root was deleted = paths %v, marker %q; want none and marker 0", emptied.GetSourcePaths(), emptied.GetNextMarker())
	}
	reconcileVFS()
	rebuilt := pollWithin(t, scanner, emptied.GetNextMarker(), time.Second)
	if len(rebuilt.GetSourcePaths()) == 0 || rebuilt.GetNextMarker() == "0" {
		t.Fatalf("poll after the rebuild = paths %v, marker %q; want the rebuilt links", rebuilt.GetSourcePaths(), rebuilt.GetNextMarker())
	}

	// The same deletion, but the rebuild lands before the next poll: the stale
	// marker is still ahead of the short new journal.
	stale := "100000"
	if err := os.RemoveAll(outputRoot); err != nil {
		t.Fatal(err)
	}
	reconcileVFS()
	resynced := pollWithin(t, scanner, stale, time.Second)
	if got := resynced.GetSourcePaths(); len(got) != 1 || filepath.Dir(got[0]) != outputRoot || resynced.GetNextMarker() == stale || resynced.GetNextMarker() == "0" {
		t.Fatalf("poll after a rebuild = paths %v, marker %q; want a whole-VFS scan and the new tail", got, resynced.GetNextMarker())
	}
	settled := pollWithin(t, scanner, resynced.GetNextMarker(), time.Second)
	if len(settled.GetSourcePaths()) != 0 || settled.GetNextMarker() != resynced.GetNextMarker() {
		t.Fatalf("next poll = paths %v, marker %q", settled.GetSourcePaths(), settled.GetNextMarker())
	}
}
