package silocatalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(Config{BaseURL: server.URL + "/", APIKey: " key ", ProfileID: "p1"}, nil)
}

func TestItemSendsProfileAndReadsFiles(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("X-Profile-Id") != "p1" {
			t.Errorf("headers = %v", r.Header)
		}
		if r.URL.EscapedPath() != "/api/v2/catalog/items/a%2Fb" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		fmt.Fprint(w, `{"content_id":"a/b","user_state":{"played":true},"versions":[{"file_name":"x [Shoko Episode=1].mkv","file_path":"/vfs/x [Shoko Episode=1].mkv"},{"file_name":"y [Shoko Episode=2].mkv"},{}]}`)
	})
	item, found, err := client.Item(context.Background(), "a/b")
	if err != nil || !found {
		t.Fatalf("found = %t, err = %v", found, err)
	}
	if !item.Played {
		t.Fatal("item should be played")
	}
	if want := []string{"/vfs/x [Shoko Episode=1].mkv", "y [Shoko Episode=2].mkv"}; !reflect.DeepEqual(item.Files, want) {
		t.Fatalf("files = %v, want %v", item.Files, want)
	}
}

func TestItemNotFoundAndMismatch(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"content_id":"other"}`)
	})
	if _, found, err := client.Item(context.Background(), "gone"); found || err != nil {
		t.Fatalf("gone: found = %t, err = %v", found, err)
	}
	_, _, err := client.Item(context.Background(), "asked")
	var siloErr *Error
	if !errors.As(err, &siloErr) || siloErr.Kind != Permanent {
		t.Fatalf("mismatch err = %v", err)
	}
}

func TestValidateProfile(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   Kind
		ok     bool
	}{
		"listed":       {http.StatusOK, `{"items":[{"id":"p0"},{"id":"p1"}]}`, 0, true},
		"not listed":   {http.StatusOK, `{"items":[{"id":"p0"}]}`, PermissionDenied, false},
		"bad key":      {http.StatusUnauthorized, ``, InvalidCredential, false},
		"bad profile":  {http.StatusNotFound, `{"type":"https://siloserver.org/docs/api/v2/problems/not_found","title":"Not found","status":404,"detail":"Profile not found"}`, InvalidConfig, false},
		"not silo":     {http.StatusNotFound, `nope`, Permanent, false},
		"server error": {http.StatusBadGateway, ``, Temporary, false},
		"api gone":     {http.StatusGone, `{"error":"client_upgrade_required"}`, Permanent, false},
	} {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/profiles" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			err := client.ValidateProfile(context.Background())
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var siloErr *Error
			if !errors.As(err, &siloErr) || siloErr.Kind != tc.want {
				t.Fatalf("err = %v, want kind %d", err, tc.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	for _, config := range []Config{
		{BaseURL: "ftp://silo", APIKey: "k", ProfileID: "p"},
		{BaseURL: "http://user:pass@silo", APIKey: "k", ProfileID: "p"},
		{BaseURL: "http://silo", APIKey: "", ProfileID: "p"},
		{BaseURL: "http://silo", APIKey: "k", ProfileID: " "},
	} {
		var siloErr *Error
		if err := config.Validate(); !errors.As(err, &siloErr) || siloErr.Kind != InvalidConfig {
			t.Fatalf("%+v: err = %v", config, err)
		}
	}
}
