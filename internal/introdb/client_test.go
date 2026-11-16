package introdb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupSendsTargetAndDecodesSegments(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/media" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("tmdb_id") != "45790" || q.Get("season") != "2" || q.Get("episode") != "27" ||
			q.Get("duration_ms") != "1440000" || q.Get("details") != "true" {
			t.Errorf("query = %v", q)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"group_duration_ms":1440000,"intro":[{"start_ms":null,"end_ms":90000,"confidence":0.9,"submission_count":3}],"credits":[{"start_ms":1300000,"end_ms":null}]}`)
	}))
	defer server.Close()
	client := New(server.URL, "key", nil)
	target := Target{Param: "tmdb_id", ID: "45790", Episode: true, Season: 2, Number: 27}
	media, err := client.Lookup(context.Background(), target, 1440000)
	if err != nil {
		t.Fatal(err)
	}
	if media == nil || len(media.Intro) != 1 || *media.Intro[0].EndMs != 90000 || media.Intro[0].StartMs != nil ||
		*media.Intro[0].Confidence != 0.9 || len(media.Credits) != 1 || *media.GroupDurationMs != 1440000 {
		t.Fatalf("media = %+v", media)
	}
	// A complete answer is cached.
	if _, err := client.Lookup(context.Background(), target, 1440000); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestLookupJSONNotFoundIsNoMarkers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anonymous lookup sent authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}))
	defer server.Close()
	media, err := New(server.URL, "", nil).Lookup(context.Background(), Target{Param: "tvdb_id", ID: "1"}, 0)
	if err != nil || media != nil {
		t.Fatalf("media = %+v, err = %v", media, err)
	}
}

func TestLookupPlainNotFoundIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	if _, err := New(server.URL, "", nil).Lookup(context.Background(), Target{Param: "tvdb_id", ID: "1"}, 0); err == nil {
		t.Fatal("a non-JSON 404 must not read as no markers")
	}
}

func TestLookupRateLimitPausesLookups(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"code":"rate_limited"}`)
	}))
	defer server.Close()
	client := New(server.URL, "", nil)
	_, err := client.Lookup(context.Background(), Target{Param: "tvdb_id", ID: "1"}, 0)
	var retry *RetryError
	if !errors.As(err, &retry) || retry.RetryAfter != 30*time.Second {
		t.Fatalf("err = %v", err)
	}
	// Another title waits too, without a request.
	_, err = client.Lookup(context.Background(), Target{Param: "tvdb_id", ID: "2"}, 0)
	if !errors.As(err, &retry) || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestLookupTitleLimitDoesNotPauseOthers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("tvdb_id") == "1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":"specific_media_usage_limit_exceeded"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client := New(server.URL, "", nil)
	_, err := client.Lookup(context.Background(), Target{Param: "tvdb_id", ID: "1"}, 0)
	var retry *RetryError
	if err == nil || errors.As(err, &retry) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if _, err := client.Lookup(context.Background(), Target{Param: "tvdb_id", ID: "2"}, 0); err != nil {
		t.Fatalf("other title: %v", err)
	}
}

func TestLookupCloudflareBlockPauses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("CF-Ray", "abc")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>blocked</html>")
	}))
	defer server.Close()
	_, err := New(server.URL, "", nil).Lookup(context.Background(), Target{Param: "tvdb_id", ID: "1"}, 0)
	var retry *RetryError
	if !errors.As(err, &retry) || retry.RetryAfter != blockedCooldown {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryAfterUsesSpentDailyBudget(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-UsageLimit-Remaining", "0")
	headers.Set("X-UsageLimit-Reset", "0")
	if got := retryAfter(headers, time.Now()); got != exhaustedCooldown {
		t.Fatalf("retryAfter = %s", got)
	}
	if got := retryAfter(http.Header{}, time.Now()); got != 10*time.Second {
		t.Fatalf("default retryAfter = %s", got)
	}
}

func TestConcurrentLookupsShareOneRequest(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"intro":[{"end_ms":1000}]}`)
	}))
	defer server.Close()
	client := New(server.URL, "", nil)
	target := Target{Param: "tvdb_id", ID: "1", Episode: true, Season: 1, Number: 1}
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			media, err := client.Lookup(context.Background(), target, 0)
			if err == nil && (media == nil || len(media.Intro) != 1) {
				err = fmt.Errorf("media = %+v", media)
			}
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 5 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}
