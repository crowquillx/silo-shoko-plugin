package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/introdb"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// markerFixture serves a fake Shoko, which knows TMDB episode 1183965 as
// TMDB show 45790 S02E27, and a fake TheIntroDB that records each lookup.
type markerFixture struct {
	server  *markerServer
	mu      sync.Mutex
	queries []string
	shoko   int
	answer  string
	status  int
}

func newMarkerFixture(t *testing.T) *markerFixture {
	t.Helper()
	f := &markerFixture{
		answer: `{"group_duration_ms":1420000,"intro":[{"start_ms":30000,"end_ms":120000,"confidence":0.8,"submission_count":4}],"credits":[{"start_ms":1300000,"end_ms":null,"confidence":0.7}]}`,
		status: http.StatusOK,
	}
	shokoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.shoko++
		f.mu.Unlock()
		if r.URL.Query().Get("alternateOrderingID") != "default" {
			t.Errorf("TMDB episode lookup without the default ordering: %s", r.URL)
		}
		switch r.URL.Path {
		case "/api/v3/TMDB/Episode/1183965":
			fmt.Fprint(w, `{"ID":1183965,"ShowID":45790,"SeasonNumber":2,"EpisodeNumber":27,"Title":"Dark Blue Moon"}`)
		case "/api/v3/TMDB/Episode/500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(shokoServer.Close)
	introServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		answer, code := f.answer, f.status
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		w.WriteHeader(code)
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(introServer.Close)
	client, err := shoko.NewClient(shoko.Config{BaseURL: shokoServer.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f.server = &markerServer{runtime: &runtimeServer{client: client, reconciler: reconciler, introBaseURL: introServer.URL}}
	return f
}

func (f *markerFixture) lookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// shokoEpisode is a Shoko VFS episode as Silo describes it: numbered by its
// place in the Shoko group, which TheIntroDB would read as another episode.
func shokoEpisode(tmdbEpisode string) *pluginv1.FetchMarkersRequest {
	return &pluginv1.FetchMarkersRequest{
		ItemType:           "episode",
		ExternalIds:        &pluginv1.MarkerExternalIDs{TmdbId: "45790", TvdbId: "262954"},
		SeasonNumber:       3,
		EpisodeNumber:      3,
		EpisodeExternalIds: &pluginv1.MarkerExternalIDs{TmdbId: tmdbEpisode},
		DurationSeconds:    1420,
	}
}

func TestFetchMarkersUsesShokoTMDBNumbering(t *testing.T) {
	f := newMarkerFixture(t)
	response, err := f.server.FetchMarkers(context.Background(), shokoEpisode("1183965"))
	if err != nil {
		t.Fatal(err)
	}
	lookups := f.lookups()
	if len(lookups) != 1 || lookups[0] != "details=true&duration_ms=1420000&episode=27&season=2&tmdb_id=45790" {
		t.Fatalf("lookups = %v", lookups)
	}
	markers := response.GetMarkers()
	if len(markers) != 2 {
		t.Fatalf("markers = %v", markers)
	}
	intro, credits := markers[0], markers[1]
	if intro.GetSegment() != "intro" || intro.GetStartSeconds() != 30 || intro.GetEndSeconds() != 120 ||
		intro.GetConfidence() != 0.8 || intro.GetSubmissionCount() != 4 || intro.GetAlgorithm() != markerAlgorithm {
		t.Fatalf("intro = %v", intro)
	}
	// Credits that run to the end of the release end with the file.
	if credits.GetSegment() != "credits" || credits.GetStartSeconds() != 1300 || credits.GetEndSeconds() != 1420 {
		t.Fatalf("credits = %v", credits)
	}

	// The Shoko answer is cached.
	if _, err := f.server.FetchMarkers(context.Background(), shokoEpisode("1183965")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shoko != 1 {
		t.Fatalf("Shoko lookups = %d, want 1", f.shoko)
	}
}

func TestFetchMarkersSkipsUnknownShokoNumbering(t *testing.T) {
	f := newMarkerFixture(t)
	for _, request := range []*pluginv1.FetchMarkersRequest{
		shokoEpisode("999"), // Shoko does not know this TMDB episode.
		shokoEpisode(""),    // The episode has no TMDB link.
	} {
		response, err := f.server.FetchMarkers(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.GetMarkers()) != 0 {
			t.Fatalf("markers = %v", response.GetMarkers())
		}
	}
	if lookups := f.lookups(); len(lookups) != 0 {
		t.Fatalf("looked up group-numbered episodes: %v", lookups)
	}
}

func TestFetchMarkersFollowsSiloNumberingWhenItIsTVDBOrTMDB(t *testing.T) {
	f := newMarkerFixture(t)
	for _, test := range []struct {
		numbering string
		want      string
	}{
		{"tvdb", "details=true&duration_ms=1420000&episode=3&season=3&tvdb_id=262954"},
		{"tmdb", "details=true&duration_ms=1420000&episode=3&season=3&tmdb_id=45790"},
	} {
		request := shokoEpisode("999")
		request.EpisodeNumbering = test.numbering
		if _, err := f.server.FetchMarkers(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		lookups := f.lookups()
		if got := lookups[len(lookups)-1]; got != test.want {
			t.Fatalf("%s numbering looked up %q, want %q", test.numbering, got, test.want)
		}
	}
}

func TestFetchMarkersShokoFailureIsAnError(t *testing.T) {
	f := newMarkerFixture(t)
	if _, err := f.server.FetchMarkers(context.Background(), shokoEpisode("500")); err == nil {
		t.Fatal("a failed Shoko lookup must not fall back to the group numbering")
	}
	if lookups := f.lookups(); len(lookups) != 0 {
		t.Fatalf("lookups = %v", lookups)
	}

	// An episode Silo numbers by TVDB does not need Shoko's numbering.
	request := shokoEpisode("500")
	request.EpisodeNumbering = "tvdb"
	if _, err := f.server.FetchMarkers(context.Background(), request); err != nil {
		t.Fatalf("TVDB-numbered episode: %v", err)
	}
	if lookups := f.lookups(); len(lookups) != 1 || lookups[0] != "details=true&duration_ms=1420000&episode=3&season=3&tvdb_id=262954" {
		t.Fatalf("lookups = %v", lookups)
	}
}

func TestFetchMarkersMoviePrefersTVDB(t *testing.T) {
	f := newMarkerFixture(t)
	_, err := f.server.FetchMarkers(context.Background(), &pluginv1.FetchMarkersRequest{
		ItemType:    "movie",
		ExternalIds: &pluginv1.MarkerExternalIDs{TmdbId: "372058", TvdbId: "1234", ImdbId: "tt5311514"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookups := f.lookups(); len(lookups) != 1 || lookups[0] != "details=true&tvdb_id=1234" {
		t.Fatalf("lookups = %v", lookups)
	}
}

func TestFetchMarkersRateLimitAsksSiloToWait(t *testing.T) {
	f := newMarkerFixture(t)
	f.status, f.answer = http.StatusTooManyRequests, `{"code":"rate_limited"}`
	_, err := f.server.FetchMarkers(context.Background(), shokoEpisode("1183965"))
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("err = %v", err)
	}
	var delay time.Duration
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.RetryInfo); ok {
			delay = info.GetRetryDelay().AsDuration()
		}
	}
	if delay != time.Minute {
		t.Fatalf("retry delay = %s", delay)
	}
}

func TestMarkerSegmentsDropsEndsOfAnotherRelease(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	media := &introdb.Media{
		GroupDurationMs: ms(48 * 60 * 1000),
		Intro:           []introdb.Segment{{EndMs: ms(90000)}},
		Credits:         []introdb.Segment{{StartMs: ms(46 * 60 * 1000)}},
	}
	markers := markerSegments(media, 24*time.Minute)
	if len(markers) != 1 || markers[0].GetSegment() != "intro" || markers[0].GetStartSeconds() != 0 {
		t.Fatalf("markers = %v", markers)
	}
}

func TestConvertSegmentsRules(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	duration := 24 * time.Minute
	for _, test := range []struct {
		name    string
		kind    string
		segment introdb.Segment
		keep    bool
	}{
		{"intro without end", "intro", introdb.Segment{StartMs: ms(0)}, false},
		{"credits zero start sentinel", "credits", introdb.Segment{StartMs: ms(0), EndMs: ms(60000)}, false},
		{"credits without start", "credits", introdb.Segment{EndMs: ms(60000)}, false},
		{"end before start", "intro", introdb.Segment{StartMs: ms(9000), EndMs: ms(1000)}, false},
		{"past the file", "intro", introdb.Segment{StartMs: ms(0), EndMs: ms(25 * 60 * 1000)}, false},
		{"negative", "intro", introdb.Segment{StartMs: ms(-1), EndMs: ms(1000)}, false},
		{"preview to the end", "preview", introdb.Segment{StartMs: ms(23 * 60 * 1000)}, true},
		{"recap", "recap", introdb.Segment{EndMs: ms(30000)}, true},
	} {
		got := convertSegments(test.kind, []introdb.Segment{test.segment}, duration)
		if (len(got) == 1) != test.keep {
			t.Errorf("%s: kept %v, want %v", test.name, got, test.keep)
		}
	}
	bad := 1.5
	got := convertSegments("intro", []introdb.Segment{{EndMs: ms(1000), Confidence: &bad}}, duration)
	if len(got) != 1 || got[0].GetConfidence() != 0 {
		t.Fatalf("an out-of-range confidence must count as unrated: %v", got)
	}
}

func TestSubmitMarkerIsRefused(t *testing.T) {
	_, err := (&markerServer{}).SubmitMarker(context.Background(), &pluginv1.SubmitMarkerRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchMarkersNeedsConfiguration(t *testing.T) {
	server := &markerServer{runtime: &runtimeServer{}}
	if _, err := server.FetchMarkers(context.Background(), shokoEpisode("1183965")); err == nil {
		t.Fatal("an unconfigured plugin must report an error")
	}
}
