package shoko

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilesForManagedFolderUsesAPIKeyAndPaginationShape(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "test-key" {
			t.Fatalf("apikey header = %q", r.Header.Get("apikey"))
		}
		if r.URL.Path != "/api/v3/ManagedFolder/1/File" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("include") != "XRefs" {
			t.Fatalf("include = %q", r.URL.Query().Get("include"))
		}
		requests++
		if r.URL.Query().Get("page") != "1" {
			t.Fatalf("page = %q", r.URL.Query().Get("page"))
		}
		fmt.Fprint(w, `{"total":1,"list":[{"id":9,"size":123,"locations":[{"id":3,"fileId":9,"managedFolderId":1,"relativePath":"Show/Episode.mkv","isAccessible":true}],"seriesIds":[]}]}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := client.FilesForManagedFolder(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != 9 || requests != 1 {
		t.Fatalf("files = %#v, requests = %d", files, requests)
	}
}

func TestDecodePageAcceptsArray(t *testing.T) {
	items, total, err := decodePage[ManagedFolder]([]byte(`[{"id":1,"name":"Anime","path":"/media"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != 1 {
		t.Fatalf("items=%#v total=%d", items, total)
	}
}

func TestNewClientRejectsCredentialURL(t *testing.T) {
	if _, err := NewClient(Config{BaseURL: "http://user:pass@example", APIKey: "secret"}); err == nil {
		t.Fatal("credential URL accepted")
	}
}

func TestSnapshotBuildsReferencedGraph(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"Anime","path":"/media/anime"}]`)
		case r.URL.Path == "/api/v3/ManagedFolder/1/File":
			fmt.Fprint(w, `{"total":1,"list":[{"id":9,"locations":[{"id":3,"fileId":9,"managedFolderId":1,"relativePath":"Show/Episode.mkv","isAccessible":true}],"seriesIds":[{"seriesId":{"id":7},"episodeIds":[{"id":42,"aniDB":9001}]}]}]}`)
		case r.URL.Path == "/api/v3/Series/7":
			fmt.Fprint(w, `{"ids":{"id":7,"aniDB":100},"name":"Show","aniDB":{"id":100,"type":"TV"}}`)
		case r.URL.Path == "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":42,"parentSeries":7,"aniDB":9001},"name":"Episode one","aniDB":{"id":9001,"animeID":100,"type":"Episode","episodeNumber":1}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background(), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || len(snapshot.Series) != 1 || len(snapshot.Episodes) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Groups == nil || snapshot.GroupSeries == nil {
		t.Fatalf("ungrouped snapshot maps are nil: %#v", snapshot)
	}
}

func TestAnimeMetadataAirYear(t *testing.T) {
	tests := []struct {
		name     string
		metadata *AnimeMetadata
		want     int
	}{
		{name: "date", metadata: &AnimeMetadata{AirDate: "2014-07-06"}, want: 2014},
		{name: "invalid", metadata: &AnimeMetadata{AirDate: "2014"}, want: 0},
		{name: "missing", metadata: nil, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.metadata.AirYear(); got != tt.want {
				t.Fatalf("AirYear() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSnapshotFetchesDirectGroupMembersWithoutTheirEpisodes(t *testing.T) {
	var unrelatedEpisodeRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/ManagedFolder":
			fmt.Fprint(w, `[{"id":1,"name":"Anime","path":"/media/anime"}]`)
		case "/api/v3/ManagedFolder/1/File":
			fmt.Fprint(w, `{"total":1,"list":[{"id":9,"seriesIds":[{"seriesId":{"id":7},"episodeIds":[{"id":42}]}]}]}`)
		case "/api/v3/Series/7":
			fmt.Fprint(w, `{"ids":{"id":7,"parentGroup":3},"name":"Show","aniDB":{"id":100,"type":"TV"}}`)
		case "/api/v3/Series/7/Episode":
			fmt.Fprint(w, `{"total":1,"list":[{"ids":{"id":42,"parentSeries":7},"name":"Episode one"}]}`)
		case "/api/v3/Group/3":
			fmt.Fprint(w, `{"ids":{"id":3,"mainSeries":7,"mainAnime":100,"topLevelGroup":3},"name":"Group","images":{"posters":[],"backdrops":[],"banners":[],"logos":[],"discs":[]}}`)
		case "/api/v3/Group/3/Series":
			if r.URL.Query().Get("recursive") != "false" || r.URL.Query().Get("includeDataFrom") != "AniDB" {
				t.Fatalf("group series query = %v", r.URL.Query())
			}
			fmt.Fprint(w, `[{"ids":{"id":11},"name":"Unrelated group member","aniDB":{"id":101,"type":"TV"}},{"ids":{"id":7},"name":"Show"}]`)
		case "/api/v3/Series/11/Episode":
			unrelatedEpisodeRequests++
			http.Error(w, "unrelated group member episode request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background(), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Groups) != 1 || len(snapshot.Series) != 2 || len(snapshot.Episodes) != 1 {
		t.Fatalf("snapshot graph = %#v", snapshot)
	}
	gotMembers := snapshot.GroupSeries[3]
	if len(gotMembers) != 2 || gotMembers[0] != 7 || gotMembers[1] != 11 {
		t.Fatalf("group members = %v", gotMembers)
	}
	if unrelatedEpisodeRequests != 0 {
		t.Fatalf("unrelated episode requests = %d", unrelatedEpisodeRequests)
	}
}
