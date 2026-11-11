package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/config"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

const (
	testShokoUserKey = "shoko-user-key"
	testSiloKey      = "silo-key"
	testProfileID    = "profile-1"
	testVFSFile      = "/srv/vfs/Show (2014)/Season 01/Show - S01E03 [Shoko Series=7] [AniDB Anime=100] [Shoko Episode=42] [Shoko File=9].mkv"
)

// fakeBackends serves both Silo's catalog API and Shoko's API, and records
// the Shoko episodes marked watched.
type fakeBackends struct {
	t *testing.T

	mu      sync.Mutex
	items   map[string]string // item ID -> JSON body
	shokoUp int               // HTTP status for Shoko writes; 0 means 200
	marked  []string
	siloHit int
	// watched holds the watched-episode list pages by page number.
	watched      map[string]string
	watchedQuery []string
}

func newFakeBackends(t *testing.T) (*fakeBackends, *httptest.Server) {
	backends := &fakeBackends{t: t, items: map[string]string{}, watched: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(backends.serve))
	t.Cleanup(server.Close)
	return backends, server
}

func (b *fakeBackends) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		b.siloHit++
		if r.Header.Get("Authorization") != "Bearer "+testSiloKey || r.Header.Get("X-Profile-Id") != testProfileID {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/profiles" {
			fmt.Fprintf(w, `{"profiles":[{"id":%q}]}`, testProfileID)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/catalog/items/")
		body, ok := b.items[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not_found","message":"item not found"}`)
			return
		}
		fmt.Fprint(w, body)
	case strings.HasPrefix(r.URL.Path, "/api/v3/"):
		if r.Header.Get("apikey") != testShokoUserKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/User/Current" {
			fmt.Fprint(w, `{"ID":5,"Username":"tan","IsAdmin":false}`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/Episode" {
			b.watchedQuery = append(b.watchedQuery, r.URL.RawQuery)
			body, ok := b.watched[r.URL.Query().Get("page")]
			if !ok {
				body = `{"total":0,"list":[]}`
			}
			fmt.Fprint(w, body)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v3/Episode/") && strings.HasSuffix(r.URL.Path, "/Watched/true") {
			if b.shokoUp != 0 {
				w.WriteHeader(b.shokoUp)
				return
			}
			b.marked = append(b.marked, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v3/Episode/"), "/Watched/true"))
			return
		}
		b.t.Errorf("unexpected Shoko request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	default:
		b.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *fakeBackends) markedEpisodes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.marked...)
}

func itemJSON(id string, played bool, files ...string) string {
	versions := make([]string, 0, len(files))
	for _, file := range files {
		versions = append(versions, fmt.Sprintf(`{"file_id":1,"file_name":%q,"file_path":%q}`, filepath.Base(file), file))
	}
	return fmt.Sprintf(`{"content_id":%q,"type":"episode","user_state":{"played":%t},"versions":[%s]}`, id, played, strings.Join(versions, ","))
}

func configuredWatchSync(t *testing.T, baseURL string) *watchSyncServer {
	t.Helper()
	client, err := shoko.NewClient(shoko.Config{BaseURL: baseURL, APIKey: "admin-key"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: filepath.Join(t.TempDir(), "vfs"), AllowedSourceRoots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtimeServer{cfg: config.Connection{BaseURL: baseURL, APIKey: "admin-key"}, client: client, reconciler: reconciler}
	return &watchSyncServer{runtime: rt}
}

func watchContext(baseURL string) *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId: watchSyncCapability,
		Credentials: &pluginv1.WatchSyncCredentials{
			AccessToken: testShokoUserKey,
			SecretAttributes: map[string]string{
				siloBaseURLKey:   baseURL,
				siloAPIKeyKey:    testSiloKey,
				siloProfileIDKey: testProfileID,
			},
		},
	}
}

func stopEvent(itemID string, completed bool, position, duration float64) *pluginv1.WatchSyncEvent {
	completion := 0.0
	if duration > 0 {
		completion = position / duration * 100
	}
	return &pluginv1.WatchSyncEvent{
		EventId:           "scrobble:stop:" + itemID,
		Operation:         pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP,
		Origin:            pluginv1.WatchSyncOrigin_WATCH_SYNC_ORIGIN_PLAYBACK_COMPLETION,
		PositionSeconds:   position,
		DurationSeconds:   duration,
		CompletionPercent: completion,
		Completed:         completed,
		Media: &pluginv1.WatchSyncMedia{
			MediaItemId: itemID,
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		},
	}
}

func applyOne(t *testing.T, srv *watchSyncServer, baseURL string, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	response, err := srv.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: watchContext(baseURL),
		Events:  []*pluginv1.WatchSyncEvent{event},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() != nil {
		t.Fatalf("batch fault = %v", response.GetFault())
	}
	if len(response.GetResults()) != 1 || response.GetResults()[0].GetEventId() != event.GetEventId() {
		t.Fatalf("results = %v", response.GetResults())
	}
	return response.GetResults()[0]
}

func TestManifestAdvertisesShokoWatchSync(t *testing.T) {
	manifest, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range manifest.GetCapabilities() {
		if capability.GetType() != "watch_sync_provider.v1" || capability.GetId() != watchSyncCapability {
			continue
		}
		descriptor := capability.GetWatchSyncProvider()
		if !descriptor.GetScrobblePlayback() {
			t.Fatal("watch sync capability must scrobble playback")
		}
		// Exporting watched history would send every play the profile ever
		// finished to Shoko on connect, not only new completions.
		if descriptor.GetExportWatched() || !descriptor.GetImportWatched() {
			t.Fatalf("watch sync capability must scrobble and import, not export: %v", descriptor)
		}
		// Silo's import switch is on for every new connection, so the
		// plugin's own switch keeps importing off until a profile wants it.
		settings := descriptor.GetConnectionSettings()
		if len(settings) != 1 || settings[0].GetKey() != importWatchedSetting || settings[0].GetDefaultValue().GetBoolValue() {
			t.Fatalf("connection settings = %v, want %q off by default", settings, importWatchedSetting)
		}
		if descriptor.GetMaxBatchSize() != int32(shoko.MaxPageSize) {
			t.Fatalf("max batch size = %d, want Shoko's page size %d", descriptor.GetMaxBatchSize(), shoko.MaxPageSize)
		}
		if !reflect.DeepEqual(descriptor.GetAuthMethods(), []pluginv1.WatchSyncAuthMethod{pluginv1.WatchSyncAuthMethod_WATCH_SYNC_AUTH_METHOD_API_KEY}) {
			t.Fatalf("auth methods = %v", descriptor.GetAuthMethods())
		}
		var fields []string
		for _, schema := range capability.GetConfigSchema() {
			for _, field := range schema.GetAdminForm().GetFields() {
				fields = append(fields, schema.GetKey()+"."+field.GetKey())
			}
		}
		if want := []string{siloBaseURLKey, siloAPIKeyKey, siloProfileIDKey}; !reflect.DeepEqual(fields, want) {
			t.Fatalf("connection fields = %v, want %v", fields, want)
		}
		return
	}
	t.Fatalf("watch_sync_provider.v1 capability missing: %v", manifest.GetCapabilities())
}

func TestApplyEventsMarksFinishedEpisodeWatched(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["item-1"] = itemJSON("item-1", false, testVFSFile)
	srv := configuredWatchSync(t, server.URL)

	result := applyOne(t, srv, server.URL, stopEvent("item-1", true, 1400, 1420))
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v, fault = %v", result.GetStatus(), result.GetFault())
	}
	if got := backends.markedEpisodes(); !reflect.DeepEqual(got, []string{"42"}) {
		t.Fatalf("marked = %v, want [42]", got)
	}
}

func TestApplyEventsMarksEachVersionsEpisodeOnce(t *testing.T) {
	backends, server := newFakeBackends(t)
	other := strings.Replace(testVFSFile, "[Shoko File=9]", "[Shoko File=10]", 1)
	backends.items["item-1"] = itemJSON("item-1", false, testVFSFile, other)
	srv := configuredWatchSync(t, server.URL)

	applyOne(t, srv, server.URL, stopEvent("item-1", true, 1400, 1420))
	if got := backends.markedEpisodes(); !reflect.DeepEqual(got, []string{"42"}) {
		t.Fatalf("marked = %v, want [42]", got)
	}
}

func TestApplyEventsIgnoresUnfinishedPlays(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["item-1"] = itemJSON("item-1", true, testVFSFile)
	srv := configuredWatchSync(t, server.URL)

	for name, event := range map[string]*pluginv1.WatchSyncEvent{
		"stopped partway": stopEvent("item-1", false, 300, 1420),
		"start": {
			EventId:   "scrobble:start",
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
			Media:     &pluginv1.WatchSyncMedia{MediaItemId: "item-1", MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE},
		},
		"pause": {
			EventId:   "scrobble:pause",
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE,
			Media:     &pluginv1.WatchSyncMedia{MediaItemId: "item-1", MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE},
		},
		"manual mark": {
			EventId:   "history-1",
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
			Media:     &pluginv1.WatchSyncMedia{MediaItemId: "item-1", MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE},
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := applyOne(t, srv, server.URL, event)
			if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
				t.Fatalf("status = %v, fault = %v", result.GetStatus(), result.GetFault())
			}
		})
	}
	if got := backends.markedEpisodes(); len(got) != 0 {
		t.Fatalf("marked = %v, want none", got)
	}
	if backends.siloHit != 0 {
		t.Fatalf("Silo was asked %d times for plays that were not finished", backends.siloHit)
	}
}

// Some players report a finished play as a stop at position zero. Silo's
// saved played state decides whether it counts.
func TestApplyEventsZeroStopUsesSiloPlayedState(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["played"] = itemJSON("played", true, testVFSFile)
	backends.items["unplayed"] = itemJSON("unplayed", false, strings.Replace(testVFSFile, "Episode=42", "Episode=43", 1))
	srv := configuredWatchSync(t, server.URL)

	if result := applyOne(t, srv, server.URL, stopEvent("unplayed", false, 0, 1420)); result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("unplayed status = %v, fault = %v", result.GetStatus(), result.GetFault())
	}
	if result := applyOne(t, srv, server.URL, stopEvent("played", false, 0, 1420)); result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("played status = %v, fault = %v", result.GetStatus(), result.GetFault())
	}
	if got := backends.markedEpisodes(); !reflect.DeepEqual(got, []string{"42"}) {
		t.Fatalf("marked = %v, want [42]", got)
	}
}

func TestApplyEventsSkipsItemsOutsideTheShokoVFS(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["movie"] = itemJSON("movie", false, "/media/movies/Some Movie (2020)/Some Movie (2020).mkv")
	srv := configuredWatchSync(t, server.URL)

	for _, id := range []string{"movie", "deleted"} {
		result := applyOne(t, srv, server.URL, stopEvent(id, true, 5000, 5010))
		if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
			t.Fatalf("%s: status = %v, fault = %v", id, result.GetStatus(), result.GetFault())
		}
	}
	if got := backends.markedEpisodes(); len(got) != 0 {
		t.Fatalf("marked = %v, want none", got)
	}
}

func TestApplyEventsReportsRejectedShokoKeyForTheBatch(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["item-1"] = itemJSON("item-1", false, testVFSFile)
	srv := configuredWatchSync(t, server.URL)
	ctx := watchContext(server.URL)
	ctx.Credentials.AccessToken = "revoked"

	response, err := srv.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: ctx,
		Events:  []*pluginv1.WatchSyncEvent{stopEvent("item-1", true, 1400, 1420)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v, results = %v", response.GetFault(), response.GetResults())
	}
	if strings.Contains(response.GetFault().GetSafeMessage(), "revoked") {
		t.Fatalf("fault leaks the key: %q", response.GetFault().GetSafeMessage())
	}
}

func TestApplyEventsRetriesWhenShokoIsDown(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["item-1"] = itemJSON("item-1", false, testVFSFile)
	backends.shokoUp = http.StatusServiceUnavailable
	srv := configuredWatchSync(t, server.URL)

	result := applyOne(t, srv, server.URL, stopEvent("item-1", true, 1400, 1420))
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY ||
		result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Fatalf("status = %v, fault = %v", result.GetStatus(), result.GetFault())
	}
}

func TestApplyEventsRetriesWhileThePluginIsNotConfigured(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.items["item-1"] = itemJSON("item-1", false, testVFSFile)
	srv := &watchSyncServer{runtime: &runtimeServer{}}

	result := applyOne(t, srv, server.URL, stopEvent("item-1", true, 1400, 1420))
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("status = %v, fault = %v", result.GetStatus(), result.GetFault())
	}
}

func TestExchangeAPIKeyChecksShokoAndSilo(t *testing.T) {
	_, server := newFakeBackends(t)
	srv := configuredWatchSync(t, server.URL)
	request := func(shokoKey, siloKey string) *pluginv1.WatchSyncExchangeAPIKeyRequest {
		return &pluginv1.WatchSyncExchangeAPIKeyRequest{
			CapabilityId: watchSyncCapability,
			ApiKey:       shokoKey,
			ProviderConfig: &pluginv1.WatchSyncProviderConfig{
				Values:       map[string]string{siloBaseURLKey: server.URL, siloProfileIDKey: testProfileID},
				SecretValues: map[string]string{siloAPIKeyKey: siloKey},
			},
		}
	}

	response, err := srv.ExchangeAPIKey(context.Background(), request(" "+testShokoUserKey+" ", testSiloKey))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if response.GetCredentials().GetAccessToken() != testShokoUserKey {
		t.Fatalf("access token = %q", response.GetCredentials().GetAccessToken())
	}
	wantAttributes := map[string]string{siloBaseURLKey: server.URL, siloAPIKeyKey: testSiloKey, siloProfileIDKey: testProfileID}
	if got := response.GetCredentials().GetSecretAttributes(); !reflect.DeepEqual(got, wantAttributes) {
		t.Fatalf("secret attributes = %v", got)
	}
	if account := response.GetAccount(); account.GetExternalSubject() != "5" || account.GetUsername() != "tan" {
		t.Fatalf("account = %v", account)
	}

	for name, req := range map[string]*pluginv1.WatchSyncExchangeAPIKeyRequest{
		"bad Shoko key": request("wrong", testSiloKey),
		"bad Silo key":  request(testShokoUserKey, "wrong"),
	} {
		response, err := srv.ExchangeAPIKey(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL || response.GetCredentials() != nil {
			t.Fatalf("%s: response = %v", name, response)
		}
	}
}

func TestShokoEpisodeIDsReadsOnlyTheFileName(t *testing.T) {
	got := shokoEpisodeIDs([]string{
		`/vfs/Odd [Shoko Episode=1] dir/Season 01/Show - S01E02 [Shoko Series=7] [Shoko Episode=12] [Shoko File=3].mkv`,
		`D:\vfs\Show\Show - S01E01 [Shoko Series=7] [Shoko Episode=11] [Shoko File=2].mkv`,
		`Show - S01E02 [Shoko Series=7] [Shoko Episode=12] [Shoko File=4].mkv`,
		`/media/elsewhere/Show - S01E03.mkv`,
	})
	if want := []int{11, 12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("episodes = %v, want %v", got, want)
	}
}

func listContext(baseURL string, importOn bool) *pluginv1.WatchSyncAuthenticatedContext {
	ctx := watchContext(baseURL)
	ctx.ConnectionSettings = map[string]string{importWatchedSetting: fmt.Sprint(importOn)}
	return ctx
}

func TestListRemoteStateIsOffByDefault(t *testing.T) {
	backends, server := newFakeBackends(t)
	srv := configuredWatchSync(t, server.URL)
	for name, ctx := range map[string]*pluginv1.WatchSyncAuthenticatedContext{
		"turned off":   listContext(server.URL, false),
		"setting sent": watchContext(server.URL),
	} {
		response, err := srv.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: ctx, PageSize: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.GetItems()) != 0 || response.GetNextPageToken() != "" || response.GetFault() != nil || response.GetCompleteSnapshot() {
			t.Fatalf("%s: response = %v", name, response)
		}
	}
	if len(backends.watchedQuery) != 0 {
		t.Fatalf("Shoko was asked for watched episodes while importing is off: %v", backends.watchedQuery)
	}
}

func TestListRemoteStateListsWatchedEpisodes(t *testing.T) {
	backends, server := newFakeBackends(t)
	backends.watched["1"] = `{"total":4,"list":[
		{"IDs":{"ID":42,"ParentSeries":7,"AniDB":900,"TvDB":[5001],"TMDB":{"Episode":[7001,7002]},"IMDB":[]},"Name":"Episode 3","Watched":"2026-11-01T20:15:00Z","WatchCount":2},
		{"IDs":{"ID":43,"ParentSeries":7,"AniDB":901,"TvDB":[],"TMDB":{"Episode":[]}},"Name":"Unlinked","Watched":"2026-11-02T20:15:00Z","WatchCount":1}
	]}`
	backends.watched["2"] = `{"total":4,"list":[
		{"IDs":{"ID":44,"TMDB":{"Episode":[7003]}},"Name":"No date","Watched":null},
		{"IDs":{"ID":45,"IMDB":["tt0000045"]},"Name":"Local time","Watched":"2026-11-03T08:00:00.123","WatchCount":0}
	]}`
	srv := configuredWatchSync(t, server.URL)
	list := func(token string) *pluginv1.WatchSyncListRemoteStateResponse {
		t.Helper()
		response, err := srv.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context:    listContext(server.URL, true),
			PageSize:   2,
			PageToken:  token,
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault() != nil {
			t.Fatalf("fault = %v", response.GetFault())
		}
		return response
	}

	first := list("")
	if first.GetNextPageToken() != "2" || first.GetNextCursor() != "" {
		t.Fatalf("first page token = %q, cursor = %q", first.GetNextPageToken(), first.GetNextCursor())
	}
	if len(first.GetItems()) != 1 {
		t.Fatalf("first page items = %v", first.GetItems())
	}
	item := first.GetItems()[0]
	if item.GetProviderItemKey() != "episode:42" {
		t.Fatalf("provider item key = %q", item.GetProviderItemKey())
	}
	// The same IDs episodeIDs gives the Silo episode, so Silo's matcher finds
	// it, and no season or episode number for it to misread.
	if want := map[string]string{"tvdb": "5001", "tmdb": "7001"}; !reflect.DeepEqual(item.GetMedia().GetExternalIds(), want) {
		t.Fatalf("external IDs = %v, want %v", item.GetMedia().GetExternalIds(), want)
	}
	if item.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE ||
		item.GetMedia().GetSeasonNumber() != 0 || item.GetMedia().GetEpisodeNumber() != 0 || len(item.GetMedia().GetSeriesExternalIds()) != 0 {
		t.Fatalf("media = %v", item.GetMedia())
	}
	if item.GetWatched().GetPlayCount() != 2 || item.GetWatched().GetLastWatchedAt().AsTime().Format("2006-01-02T15:04:05Z") != "2026-11-01T20:15:00Z" {
		t.Fatalf("watched = %v", item.GetWatched())
	}
	if len(first.GetWarnings()) != 1 || !strings.Contains(first.GetWarnings()[0], "1 watched Shoko episodes with no TVDB") {
		t.Fatalf("first page warnings = %v", first.GetWarnings())
	}

	second := list("2")
	if second.GetNextPageToken() != "" {
		t.Fatalf("last page token = %q", second.GetNextPageToken())
	}
	if len(second.GetItems()) != 1 || second.GetItems()[0].GetProviderItemKey() != "episode:45" ||
		second.GetItems()[0].GetWatched().GetPlayCount() != 1 ||
		second.GetItems()[0].GetWatched().GetLastWatchedAt().AsTime().Format("2006-01-02T15:04:05.000Z") != "2026-11-03T08:00:00.123Z" {
		t.Fatalf("second page items = %v", second.GetItems())
	}
	if len(second.GetWarnings()) != 1 || !strings.Contains(second.GetWarnings()[0], "no watched date") {
		t.Fatalf("second page warnings = %v", second.GetWarnings())
	}
	if want := []string{"includeWatched=only&page=1&pageSize=2", "includeWatched=only&page=2&pageSize=2"}; !reflect.DeepEqual(backends.watchedQuery, want) {
		t.Fatalf("Shoko queries = %v, want %v", backends.watchedQuery, want)
	}
}

func TestListRemoteStateFaults(t *testing.T) {
	_, server := newFakeBackends(t)
	srv := configuredWatchSync(t, server.URL)

	response, err := srv.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: listContext(server.URL, true), PageToken: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("bad token: fault = %v", response.GetFault())
	}

	ctx := listContext(server.URL, true)
	ctx.Credentials.AccessToken = "revoked"
	response, err = srv.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("revoked key: fault = %v", response.GetFault())
	}

	response, err = srv.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    listContext(server.URL, true),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS},
	})
	if err != nil || len(response.GetItems()) != 0 || response.GetFault() != nil {
		t.Fatalf("progress request: response = %v, err = %v", response, err)
	}
}
