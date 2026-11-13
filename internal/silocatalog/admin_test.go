package silocatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
