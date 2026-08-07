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
	"github.com/crowquillx/silo-shoko-plugin/internal/config"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMetadataSearchUsesLogicalPathToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "secret" {
			t.Fatalf("apikey = %q", r.Header.Get("apikey"))
		}
		if r.URL.Path != "/api/v3/Series/7" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"ids":{"id":7,"aniDB":100,"tmdb":{"show":[300]}},"name":"Clean Show","description":"Description","aniDB":{"id":100,"type":"TV","airDate":"2014-07-06"}}`)
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
	rt := &runtimeServer{client: client, reconciler: reconciler}
	serverUnderTest := &metadataServer{runtime: rt}
	providerIDs, err := structpb.NewStruct(map[string]any{"_filepath": "/vfs/Clean Show [Shoko Series=7]/Season 01/Episode.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := serverUnderTest.Search(context.Background(), &pluginv1.SearchMetadataRequest{ItemType: "series", ProviderIds: providerIDs})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetResults()) != 1 || response.GetResults()[0].GetTitle() != "Clean Show" {
		t.Fatalf("response = %#v", response)
	}
	if response.GetResults()[0].GetProviderId() != "series:7" {
		t.Fatalf("provider ID = %q, want series:7", response.GetResults()[0].GetProviderId())
	}
	if response.GetResults()[0].GetYear() != 2014 {
		t.Fatalf("year = %d, want 2014", response.GetResults()[0].GetYear())
	}
	if response.GetResults()[0].GetProviderIds().AsMap()["anidb"] != "100" {
		t.Fatalf("provider IDs = %#v", response.GetResults()[0].GetProviderIds().AsMap())
	}
}

func TestMetadataGetSeriesPrefersTypedGroupOverEpisodeFileToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/Group/5":
			fmt.Fprint(w, `{"ids":{"id":5,"mainSeries":7,"mainAnime":100,"topLevelGroup":5},"name":"Clean Group","description":"Group overview"}`)
		case "/api/v3/Series/7":
			fmt.Fprint(w, `{"ids":{"id":7,"parentGroup":5,"aniDB":100},"name":"Member Series","aniDB":{"id":100,"type":"TV","airDate":"2014-07-06"}}`)
		default:
			t.Fatalf("unexpected metadata request path = %q", r.URL.Path)
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
	serverUnderTest := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}
	response, err := serverUnderTest.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType:   "series",
		ProviderId: "group:5",
		FilePath:   "/vfs/Clean Group (2014)/Season 01/Episode [Shoko Group=5] [Shoko Series=7] [Shoko Episode=42] [Shoko File=9].mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := response.GetItem()
	if item.GetTitle() != "Clean Group" || item.GetProviderId() != "group:5" ||
		item.GetProviderIds().AsMap()["shoko_group"] != "5" {
		t.Fatalf("item = %#v", item)
	}
}

func TestMetadataGetEpisodeUsesFilePathToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/Episode/42" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"ids":{"id":42,"parentSeries":7,"aniDB":9001},"name":"Episode one","description":"Overview","aniDB":{"id":9001,"animeID":100,"type":"Episode","episodeNumber":1}}`)
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
	serverUnderTest := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}
	response, err := serverUnderTest.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType: "episode",
		FilePath: "/vfs/Show/Season 01/Episode [Shoko Series=7] [Shoko Episode=42] [Shoko File=9].mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetItem().GetTitle() != "Episode one" || response.GetItem().GetProviderIds().AsMap()["shoko_episode"] != "42" {
		t.Fatalf("item = %#v", response.GetItem())
	}
}

func TestMetadataGetEpisodeUsesTypedProviderIDFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/Episode/42" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"ids":{"id":42,"parentSeries":7},"name":"Episode one","aniDB":{"type":"Episode","episodeNumber":1}}`)
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
	serverUnderTest := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}
	response, err := serverUnderTest.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType:   "episode",
		ProviderId: "episode:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetItem().GetTitle() != "Episode one" {
		t.Fatalf("item = %#v", response)
	}
}

func TestManifestAdvertisesScanSource(t *testing.T) {
	manifest, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range manifest.GetCapabilities() {
		if capability.GetType() == "scan_source.v1" && capability.GetId() == "shoko" {
			return
		}
	}
	t.Fatalf("scan_source.v1 capability missing from manifest: %#v", manifest.GetCapabilities())
}

func TestReconcileTaskKeyAcceptsSiloQualifiedKey(t *testing.T) {
	accepted := []string{"", "reconcile", "plugin:8:reconcile"}
	for _, value := range accepted {
		if !isReconcileTaskKey(value) {
			t.Errorf("isReconcileTaskKey(%q) = false, want true", value)
		}
	}

	rejected := []string{"plugin:0:reconcile", "plugin:x:reconcile", "plugin:8:other", "reconcile:extra"}
	for _, value := range rejected {
		if isReconcileTaskKey(value) {
			t.Errorf("isReconcileTaskKey(%q) = true, want false", value)
		}
	}
}

func TestManifestAdvertisesImageResolver(t *testing.T) {
	manifest, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range manifest.GetCapabilities() {
		if capability.GetType() == "image_resolver.v1" && capability.GetId() == "shoko" {
			return
		}
	}
	t.Fatalf("image_resolver.v1 capability missing from manifest: %#v", manifest.GetCapabilities())
}

func TestSelectImagePrefersPreferredAvailable(t *testing.T) {
	preferred := "00000000-0000-4000-8000-000000000002"
	images := []shoko.Image{
		{UID: "00000000-0000-4000-8000-000000000001", Available: true},
		{UID: preferred, Available: true, Preferred: true},
		{UID: "00000000-0000-4000-8000-000000000003", Available: true, Preferred: true, Disabled: true},
		{UID: "00000000-0000-4000-8000-000000000004", Available: true, Preferred: true, Disabled: false},
	}
	if got := selectImage(images); got == nil || got.UID != preferred {
		t.Fatalf("selectImage = %#v, want preferred available %q", got, preferred)
	}

	firstAvailable := "00000000-0000-4000-8000-000000000012"
	fallback := []shoko.Image{
		{UID: "00000000-0000-4000-8000-000000000011", Available: false},
		{UID: firstAvailable, Available: true},
		{UID: "00000000-0000-4000-8000-000000000013", Available: true},
	}
	if got := selectImage(fallback); got == nil || got.UID != firstAvailable {
		t.Fatalf("selectImage fallback = %#v, want first available %q", got, firstAvailable)
	}

	if got := selectImage(nil); got != nil {
		t.Fatalf("selectImage(nil) = %#v, want nil", got)
	}
	if got := selectImage([]shoko.Image{{UID: firstAvailable, Disabled: true}}); got != nil {
		t.Fatalf("selectImage(disabled) = %#v, want nil", got)
	}
}

func TestImagePathRequiresCanonicalUUID(t *testing.T) {
	const uuid = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
	if got := imagePath(shoko.Image{UID: uuid}); got != "shoko://image/"+uuid {
		t.Fatalf("imagePath = %q", got)
	}
	for _, uid := range []string{"", "not-a-uuid", uuid + "/full", uuid + "?x=1", "../" + uuid, "shoko://image/" + uuid} {
		if got := imagePath(shoko.Image{UID: uid}); got != "" {
			t.Fatalf("imagePath(%q) = %q, want empty", uid, got)
		}
	}
}

func TestResolveShokoImagePathStrict(t *testing.T) {
	const (
		base = "http://shoko:8111"
		uuid = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
	)
	tests := []struct {
		name string
		path string
		base string
		want string
	}{
		{name: "valid bare path", path: "image/" + uuid, base: base, want: base + "/api/v3/Image/" + uuid},
		{name: "trailing slash base", path: "image/" + uuid, base: base + "/", want: base + "/api/v3/Image/" + uuid},
		{name: "uppercase uuid", path: "image/" + strings.ToUpper(uuid), base: base, want: base + "/api/v3/Image/" + strings.ToUpper(uuid)},
		{name: "empty path", path: "", base: base, want: ""},
		{name: "scheme was not stripped", path: "shoko://image/" + uuid, base: base, want: ""},
		{name: "wrong prefix", path: "poster/" + uuid, base: base, want: ""},
		{name: "not a uuid", path: "image/not-a-uuid", base: base, want: ""},
		{name: "extra segment", path: "image/" + uuid + "/full", base: base, want: ""},
		{name: "query string", path: "image/" + uuid + "?apikey=secret", base: base, want: ""},
		{name: "fragment", path: "image/" + uuid + "#thumb", base: base, want: ""},
		{name: "path traversal", path: "image/../etc/passwd", base: base, want: ""},
		{name: "missing uuid", path: "image/", base: base, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveShokoImagePath(tt.path, tt.base); got != tt.want {
				t.Fatalf("resolveShokoImagePath(%q, %q) = %q, want %q", tt.path, tt.base, got, tt.want)
			}
		})
	}
}

func TestMetadataServerResolveImageURLs(t *testing.T) {
	client, err := shoko.NewClient(shoko.Config{BaseURL: "http://shoko:8111", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	serverUnderTest := &metadataServer{runtime: &runtimeServer{
		client:     client,
		reconciler: reconciler,
		cfg:        config.Connection{BaseURL: "http://shoko:8111"},
	}}

	const uuid = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"
	single, err := serverUnderTest.ResolveImageURL(context.Background(), &pluginv1.ResolveImageURLRequest{Path: "image/" + uuid})
	if err != nil {
		t.Fatal(err)
	}
	if single.GetUrl() != "http://shoko:8111/api/v3/Image/"+uuid {
		t.Fatalf("ResolveImageURL = %q", single.GetUrl())
	}

	batch, err := serverUnderTest.ResolveImageURLs(context.Background(), &pluginv1.ResolveImageURLsRequest{
		Paths: []string{"image/" + uuid, "image/bogus", "poster/" + uuid, ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	urls := batch.GetUrls()
	if urls["image/"+uuid] != "http://shoko:8111/api/v3/Image/"+uuid {
		t.Fatalf("urls[valid] = %q", urls["image/"+uuid])
	}
	for _, invalid := range []string{"image/bogus", "poster/" + uuid, ""} {
		if urls[invalid] != "" {
			t.Fatalf("urls[%q] = %q, want empty", invalid, urls[invalid])
		}
	}
}

const seriesWithImagesFixture = `{
  "ids": {"id": 7},
  "name": "Clean Show",
  "images": {
    "posters": [
      {"uid": "11111111-1111-4111-8111-111111111111", "available": true, "preferred": false},
      {"uid": "22222222-2222-4222-8222-222222222222", "available": true, "preferred": true, "languageCode": "en", "width": 500, "height": 700},
      {"uid": "33333333-3333-4333-8333-333333333333", "available": true, "preferred": true, "disabled": true},
      {"uid": "44444444-4444-4444-8444-444444444444", "available": false, "preferred": true}
    ],
    "backdrops": [
      {"uid": "55555555-5555-4555-8555-555555555555", "available": true, "preferred": false}
    ],
    "banners": [],
    "logos": [
      {"uid": "66666666-6666-4666-8666-666666666666", "available": true, "preferred": false}
    ],
    "discs": []
  }
}`

const groupWithImagesFixture = `{
  "ids": {"id": 5},
  "name": "Clean Group",
  "images": {
    "posters": [
      {"uid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "available": true, "preferred": true}
    ],
    "backdrops": [
      {"uid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "available": true, "preferred": false}
    ],
    "banners": [],
    "logos": [],
    "discs": []
  }
}`

func TestGetImagesSeriesAndGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "secret" {
			t.Fatalf("apikey = %q", r.Header.Get("apikey"))
		}
		switch r.URL.Path {
		case "/api/v3/Series/7":
			fmt.Fprint(w, seriesWithImagesFixture)
		case "/api/v3/Group/5":
			fmt.Fprint(w, groupWithImagesFixture)
		default:
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
	serverUnderTest := &metadataServer{runtime: &runtimeServer{client: client, reconciler: reconciler}}

	providerIDs, err := structpb.NewStruct(map[string]any{"shoko_series": "7"})
	if err != nil {
		t.Fatal(err)
	}
	seriesResponse, err := serverUnderTest.GetImages(context.Background(), &pluginv1.GetImagesRequest{ProviderIds: providerIDs})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		kind, url, language string
		width, height       int32
	}{
		{kind: "poster", url: "shoko://image/11111111-1111-4111-8111-111111111111"},
		{kind: "poster", url: "shoko://image/22222222-2222-4222-8222-222222222222", language: "en", width: 500, height: 700},
		{kind: "backdrop", url: "shoko://image/55555555-5555-4555-8555-555555555555"},
		{kind: "logo", url: "shoko://image/66666666-6666-4666-8666-666666666666"},
	}
	records := seriesResponse.GetImages()
	if len(records) != len(want) {
		t.Fatalf("series images = %#v, want %d records", records, len(want))
	}
	for i, expected := range want {
		record := records[i]
		if record.GetKind() != expected.kind || record.GetUrl() != expected.url || record.GetLanguage() != expected.language ||
			record.GetWidth() != expected.width || record.GetHeight() != expected.height {
			t.Fatalf("series image %d = %#v, want %#v", i, record, expected)
		}
	}

	groupResponse, err := serverUnderTest.GetImages(context.Background(), &pluginv1.GetImagesRequest{ProviderId: "group:5"})
	if err != nil {
		t.Fatal(err)
	}
	records = groupResponse.GetImages()
	if len(records) != 2 || records[0].GetKind() != "poster" || records[0].GetUrl() != "shoko://image/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" ||
		records[1].GetKind() != "backdrop" {
		t.Fatalf("group images = %#v", records)
	}

	groupIDs, err := structpb.NewStruct(map[string]any{"shoko_group": "5"})
	if err != nil {
		t.Fatal(err)
	}
	groupResponse, err = serverUnderTest.GetImages(context.Background(), &pluginv1.GetImagesRequest{ProviderIds: groupIDs})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupResponse.GetImages()) != 2 {
		t.Fatalf("group images via provider_ids = %#v", groupResponse.GetImages())
	}

	// Ambiguous bare and episode provider IDs are rejected: no records, no
	// error.
	bare, err := serverUnderTest.GetImages(context.Background(), &pluginv1.GetImagesRequest{ProviderId: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.GetImages()) != 0 {
		t.Fatalf("bare provider ID images = %#v, want none", bare.GetImages())
	}
	empty, err := serverUnderTest.GetImages(context.Background(), &pluginv1.GetImagesRequest{ProviderId: "episode:42"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.GetImages()) != 0 {
		t.Fatalf("episode images = %#v, want none", empty.GetImages())
	}
}

func TestSeriesMetadataCarriesArtworkPaths(t *testing.T) {
	series := shoko.Series{
		IDs:  shoko.IDs{ID: 7},
		Name: "Clean Show",
		Images: shoko.Images{
			Posters: []shoko.Image{
				{UID: "11111111-1111-4111-8111-111111111111", Available: true, Preferred: true},
			},
			Backdrops: []shoko.Image{
				{UID: "22222222-2222-4222-8222-222222222222", Available: true},
			},
			Logos: []shoko.Image{
				{UID: "33333333-3333-4333-8333-333333333333", Available: true},
			},
		},
	}
	item := seriesMetadata(series, "series")
	if item.GetPosterPath() != "shoko://image/11111111-1111-4111-8111-111111111111" ||
		item.GetBackdropPath() != "shoko://image/22222222-2222-4222-8222-222222222222" ||
		item.GetLogoPath() != "shoko://image/33333333-3333-4333-8333-333333333333" {
		t.Fatalf("artwork paths = poster %q backdrop %q logo %q", item.GetPosterPath(), item.GetBackdropPath(), item.GetLogoPath())
	}
	if result := seriesSearchResult(series, "series"); result.GetImageUrl() != "shoko://image/11111111-1111-4111-8111-111111111111" {
		t.Fatalf("search image URL = %q", result.GetImageUrl())
	}

	// Preferred wins over an earlier available image; invalid UIDs never
	// produce paths.
	series.Images.Posters = []shoko.Image{
		{UID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Available: true},
		{UID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Available: true, Preferred: true},
	}
	if got := seriesMetadata(series, "series").GetPosterPath(); got != "shoko://image/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" {
		t.Fatalf("preferred poster path = %q", got)
	}
	series.Images.Posters = []shoko.Image{{UID: "not-a-uuid", Available: true}}
	if got := seriesMetadata(series, "series").GetPosterPath(); got != "" {
		t.Fatalf("invalid poster path = %q, want empty", got)
	}
	if got := seriesSearchResult(shoko.Series{}, "series").GetImageUrl(); got != "" {
		t.Fatalf("empty series image URL = %q, want empty", got)
	}
}

func TestGroupMetadataCarriesArtworkPaths(t *testing.T) {
	group := shoko.Group{
		IDs:  shoko.GroupIDs{ID: 5},
		Name: "Clean Group",
		Images: shoko.Images{
			Posters: []shoko.Image{
				{UID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Available: true},
				{UID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Available: true, Preferred: true},
			},
			Backdrops: []shoko.Image{
				{UID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Available: true},
			},
		},
	}
	item := groupMetadata(group, "series", 2014)
	if item.GetProviderId() != "group:5" || item.GetProviderIds().AsMap()["shoko_group"] != "5" {
		t.Fatalf("group identity = provider %q ids %#v", item.GetProviderId(), item.GetProviderIds().AsMap())
	}
	if item.GetPosterPath() != "shoko://image/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" ||
		item.GetBackdropPath() != "shoko://image/cccccccc-cccc-4ccc-8ccc-cccccccccccc" {
		t.Fatalf("group artwork = poster %q backdrop %q", item.GetPosterPath(), item.GetBackdropPath())
	}
	if got := groupMetadata(shoko.Group{IDs: shoko.GroupIDs{ID: 5}}, "series", 0).GetPosterPath(); got != "" {
		t.Fatalf("empty group poster path = %q, want empty", got)
	}
}

func TestTaskOutputBoundsLargeReconcileResponse(t *testing.T) {
	actions := make([]vfs.Action, 16_301)
	for i := range actions {
		actions[i] = vfs.Action{
			Kind:        "create",
			LogicalPath: strings.Repeat("logical-path/", 20),
			TargetPath:  strings.Repeat("target-path/", 20),
		}
	}

	output, err := taskOutput(false, true, "ready", 16_278, 1_336, 16_375, topology.Plan{}, vfs.Result{Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	values := output.AsMap()
	if got := values["action_count"]; got != float64(len(actions)) {
		t.Fatalf("action_count = %#v, want %d", got, len(actions))
	}
	if got := values["actions_returned"]; got != float64(maxTaskOutputActions) {
		t.Fatalf("actions_returned = %#v, want %d", got, maxTaskOutputActions)
	}
	if got := values["actions_truncated"]; got != float64(len(actions)-maxTaskOutputActions) {
		t.Fatalf("actions_truncated = %#v, want %d", got, len(actions)-maxTaskOutputActions)
	}
	if got := len(values["actions"].([]any)); got != maxTaskOutputActions {
		t.Fatalf("len(actions) = %d, want %d", got, maxTaskOutputActions)
	}
	if got := proto.Size(output); got >= 4*1024*1024 {
		t.Fatalf("protobuf output size = %d, must fit the default gRPC receive limit", got)
	}
}
