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

const notFoundProblem = `{"type":"https://siloserver.org/docs/api/v2/problems/not_found","title":"Not found","status":404,"detail":"Profile not found"}`

func TestResolveProfile(t *testing.T) {
	const listing = `{"items":[{"id":"p0","name":"Guest"},{"id":"p1","name":"Tan"}]}`
	for name, tc := range map[string]struct {
		wanted string
		// list answers the request without X-Profile-Id, act the one with it.
		listStatus int
		listBody   string
		actStatus  int
		actBody    string
		want       Kind
		wantID     string
		message    string
	}{
		"by ID":           {wanted: "p1", listStatus: 200, listBody: listing, actStatus: 200, actBody: listing, wantID: "p1"},
		"by name":         {wanted: "tan", listStatus: 200, listBody: listing, actStatus: 200, actBody: listing, wantID: "p1"},
		"ID beats name":   {wanted: "p0", listStatus: 200, listBody: `{"items":[{"id":"p0","name":"p1"},{"id":"p1","name":"p0"}]}`, actStatus: 200, actBody: listing, wantID: "p0"},
		"unknown":         {wanted: "1", listStatus: 200, listBody: listing, want: InvalidConfig, message: `no profile "1" on this Silo API key's account; its profiles are: Guest, Tan`},
		"shared name":     {wanted: "Tan", listStatus: 200, listBody: `{"items":[{"id":"a","name":"Tan"},{"id":"b","name":"tan"}]}`, want: InvalidConfig, message: `several profiles are named "Tan"; enter the ID of the one you are connecting: Tan (a), tan (b)`},
		"no profiles":     {wanted: "Tan", listStatus: 200, listBody: `{"items":[]}`, want: InvalidConfig},
		"bad key":         {wanted: "p1", listStatus: http.StatusUnauthorized, want: InvalidCredential},
		"not silo":        {wanted: "p1", listStatus: http.StatusNotFound, listBody: `nope`, want: Permanent},
		"api gone":        {wanted: "p1", listStatus: http.StatusGone, listBody: `{"error":"client_upgrade_required"}`, want: Permanent},
		"server error":    {wanted: "p1", listStatus: http.StatusBadGateway, want: Temporary},
		"locked profile":  {wanted: "Tan", listStatus: 200, listBody: listing, actStatus: http.StatusForbidden, want: PermissionDenied},
		"profile refused": {wanted: "Tan", listStatus: 200, listBody: listing, actStatus: http.StatusNotFound, actBody: notFoundProblem, want: InvalidConfig},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/profiles" {
					t.Errorf("path = %q", r.URL.Path)
				}
				status, body := tc.listStatus, tc.listBody
				if profile := r.Header.Get("X-Profile-Id"); profile != "" {
					if profile != "p0" && profile != "p1" {
						t.Errorf("acted as %q", profile)
					}
					status, body = tc.actStatus, tc.actBody
				}
				w.WriteHeader(status)
				fmt.Fprint(w, body)
			}))
			t.Cleanup(server.Close)
			client := NewClient(Config{BaseURL: server.URL, APIKey: "key", ProfileID: " " + tc.wanted + " "}, nil)
			id, err := client.ResolveProfile(context.Background())
			if tc.wantID != "" {
				if err != nil || id != tc.wantID {
					t.Fatalf("id = %q, err = %v, want %q", id, err, tc.wantID)
				}
				return
			}
			var siloErr *Error
			if !errors.As(err, &siloErr) || siloErr.Kind != tc.want {
				t.Fatalf("err = %v, want kind %d", err, tc.want)
			}
			if tc.message != "" && err.Error() != tc.message {
				t.Fatalf("message = %q, want %q", err.Error(), tc.message)
			}
		})
	}
}

func TestDisplayNameIsOneSafeLine(t *testing.T) {
	if got := displayName(" A\nB\x1b "); got != "AB" {
		t.Fatalf("got %q", got)
	}
	if got := displayName(strings.Repeat("x", 70)); got != strings.Repeat("x", 64)+"…" {
		t.Fatalf("got %q", got)
	}
	if got := displayName(" "); got != "(unnamed)" {
		t.Fatalf("got %q", got)
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
