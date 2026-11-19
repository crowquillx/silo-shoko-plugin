package silocatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLibrariesUnder(t *testing.T) {
	libraries := []Library{
		{ID: "1", Paths: []string{"/media/movies"}},
		{ID: "2", Paths: []string{"/srv/vfs"}},
		{ID: "3", Paths: []string{"/srv/vfs/Some Show"}},
		{ID: "4", Paths: []string{"/srv"}},
		{ID: "5", Paths: []string{"/srv/vfs-other"}},
	}
	if got := LibrariesUnder(libraries, "/srv/vfs/"); !reflect.DeepEqual(got, []string{"2", "3", "4"}) {
		t.Fatalf("LibrariesUnder = %v", got)
	}
}

func TestAdminFindsAndRefreshesAShow(t *testing.T) {
	var refreshed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa_admin" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/libraries":
			_, _ = w.Write([]byte(`{"items":[{"id":"7","paths":["/srv/vfs"],"name":"Anime"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/libraries/roots":
			if r.URL.Query().Get("library_id") != "7" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Silo's search is a substring match, so a longer ID matches too.
			_, _ = w.Write([]byte(`{"items":[
				{"root_path":"/srv/vfs/Other (2020)","sample_file_path":"/srv/vfs/Other (2020)/Season 01/x [Shoko Group=120] [Shoko Series=1].mkv","content_id":"wrong"},
				{"root_path":"/srv/vfs/Show (2020)","sample_file_path":"/srv/vfs/Show (2020)/Season 01/x [Shoko Group=12] [Shoko Series=2].mkv","content_id":"show-12"}
			],"total":2}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/admin/items/show-12/refresh-metadata":
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["mode"] != "quick" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			refreshed = append(refreshed, "show-12")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	admin := NewAdmin(AdminConfig{BaseURL: server.URL + "/", APIKey: "sa_admin"}, nil)
	ctx := context.Background()

	libraries, err := admin.Libraries(ctx)
	if err != nil || len(libraries) != 1 || libraries[0].ID != "7" {
		t.Fatalf("Libraries = %#v, %v", libraries, err)
	}
	contentID, found, err := admin.ShowContentID(ctx, "7", "[Shoko Group=12]")
	if err != nil || !found || contentID != "show-12" {
		t.Fatalf("ShowContentID = %q, %v, %v", contentID, found, err)
	}
	if _, found, err := admin.ShowContentID(ctx, "7", "[Shoko Group=99]"); err != nil || found {
		t.Fatalf("ShowContentID for an unknown group = %v, %v", found, err)
	}
	if found, err := admin.Refresh(ctx, "show-12"); err != nil || !found {
		t.Fatalf("Refresh = %v, %v", found, err)
	}
	if found, err := admin.Refresh(ctx, "gone"); err != nil || found {
		t.Fatalf("Refresh of a missing item = %v, %v", found, err)
	}
	if !reflect.DeepEqual(refreshed, []string{"show-12"}) {
		t.Fatalf("refreshed = %v", refreshed)
	}

	_, err = NewAdmin(AdminConfig{BaseURL: server.URL, APIKey: "wrong"}, nil).Libraries(ctx)
	var siloErr *Error
	if !errors.As(err, &siloErr) || siloErr.Kind != InvalidCredential {
		t.Fatalf("Libraries with a wrong key = %v", err)
	}
}

func TestAdminReleasesEpisodeIDs(t *testing.T) {
	type patch struct {
		id   string
		body map[string]*string
	}
	var patches []patch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v2/catalog") && r.Header.Get("X-Profile-Id") != "p-main" {
			// Silo requires a profile on its catalog endpoints.
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/profiles":
			_, _ = w.Write([]byte(`{"items":[
				{"id":"p-kid","is_child":true},
				{"id":"p-pin","has_pin":true},
				{"id":"p-main","allowed_library_ids":[]}
			]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/catalog/series/old-show/seasons":
			_, _ = w.Write([]byte(`{"items":[{"season_number":0},{"season_number":1},{"season_number":2}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/catalog/series/old-show/seasons/0/episodes":
			_, _ = w.Write([]byte(`{"items":[{"content_id":"s0e1","tmdb_id":"","tvdb_id":"","imdb_id":""}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/catalog/series/old-show/seasons/1/episodes":
			_, _ = w.Write([]byte(`{"items":[
				{"content_id":"s1e1","tmdb_id":"101","tvdb_id":"201"},
				{"content_id":"s1e2","imdb_id":"tt1"},
				{"content_id":"gone","tmdb_id":"103"}
			]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/catalog/series/old-show/seasons/2/episodes":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v2/admin/items/gone/metadata":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v2/admin/items/") && strings.HasSuffix(r.URL.Path, "/metadata"):
			var body map[string]*string
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/items/"), "/metadata")
			patches = append(patches, patch{id, body})
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	admin := NewAdmin(AdminConfig{BaseURL: server.URL, APIKey: "sa_admin"}, nil)
	ctx := context.Background()

	released, found, err := admin.ReleaseEpisodeIDs(ctx, "old-show")
	if err != nil || !found || released != 2 {
		t.Fatalf("ReleaseEpisodeIDs = %d, %v, %v", released, found, err)
	}
	if len(patches) != 2 || patches[0].id != "s1e1" || patches[1].id != "s1e2" {
		t.Fatalf("patched %+v; want s1e1 and s1e2 only, not episodes without IDs", patches)
	}
	for _, p := range patches {
		// Each edit names exactly the three ID fields, set to empty strings, and
		// no locked_fields, so nothing is locked.
		if len(p.body) != 3 {
			t.Fatalf("patch for %s = %v", p.id, p.body)
		}
		for _, field := range []string{"imdb_id", "tmdb_id", "tvdb_id"} {
			if value, ok := p.body[field]; !ok || value == nil || *value != "" {
				t.Fatalf("patch for %s sets %s to %v", p.id, field, value)
			}
		}
	}
	if released, found, err := admin.ReleaseEpisodeIDs(ctx, "unknown"); err != nil || found || released != 0 {
		t.Fatalf("ReleaseEpisodeIDs for an unknown show = %d, %v, %v", released, found, err)
	}
}
