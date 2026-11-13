package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/identity"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/silocatalog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// watchSyncCapability is the id of the manifest's watch_sync_provider.v1
// capability.
const watchSyncCapability = "shoko"

// Keys of the Silo settings collected when a profile connects. The host sends
// them only with the API-key exchange, so they are kept in the connection's
// encrypted credential.
const (
	siloBaseURLKey   = "silo.base_url"
	siloAPIKeyKey    = "silo.api_key"
	siloProfileIDKey = "silo.profile_id"
)

// watchSyncServer marks episodes watched in Shoko when a profile finishes
// playing them in Silo or when Silo exports the profile's watch history, and,
// when the connection turns it on, lists the
// episodes the Shoko user watched for Silo to import. Each profile connects
// its own Shoko API key, so both act for that profile's Shoko user. Silo's
// watch events name only the Silo item, so the plugin asks Silo, as that
// profile, for the item's files and reads the Shoko episode from the VFS file
// name.
type watchSyncServer struct {
	pluginv1.UnimplementedWatchSyncProviderServer

	runtime *runtimeServer
	// httpClient sends the Shoko and Silo requests. Nil uses each client's
	// default.
	httpClient *http.Client
}

func (s *watchSyncServer) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	if req.GetCapabilityId() != watchSyncCapability {
		return &pluginv1.WatchSyncCredentialResponse{Fault: invalidRequestFault("unknown watch sync capability")}, nil
	}
	apiKey := strings.TrimSpace(req.GetApiKey())
	if apiKey == "" {
		return &pluginv1.WatchSyncCredentialResponse{Fault: invalidRequestFault("a Shoko API key is required")}, nil
	}
	config := req.GetProviderConfig()
	silo := silocatalog.Config{
		BaseURL:   strings.TrimSpace(config.GetValues()[siloBaseURLKey]),
		APIKey:    strings.TrimSpace(config.GetSecretValues()[siloAPIKeyKey]),
		ProfileID: strings.TrimSpace(config.GetValues()[siloProfileIDKey]),
	}
	// The form takes the profile's name or ID; the connection keeps the ID.
	profileID, err := silocatalog.NewClient(silo, s.httpClient).ResolveProfile(ctx)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: siloFault(err)}, nil
	}
	silo.ProfileID = profileID
	client, fault := s.shokoClient(apiKey)
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: shokoFault(err)}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: &pluginv1.WatchSyncCredentials{
			AccessToken: apiKey,
			TokenType:   "apikey",
			SecretAttributes: map[string]string{
				siloBaseURLKey:   silo.BaseURL,
				siloAPIKeyKey:    silo.APIKey,
				siloProfileIDKey: silo.ProfileID,
			},
		},
		Account: shokoAccount(user),
	}, nil
}

// RefreshCredentials always asks for a reconnect: a Shoko API key has nothing
// to refresh, and the plugin never sets an expiry that would make Silo call it.
func (s *watchSyncServer) RefreshCredentials(context.Context, *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	return &pluginv1.WatchSyncCredentialResponse{Fault: &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
		SafeMessage: "Shoko API keys cannot be refreshed; reconnect with a new key",
	}}, nil
}

func (s *watchSyncServer) GetAccount(ctx context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	client, fault := s.shokoClient(req.GetContext().GetCredentials().GetAccessToken())
	if fault != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: fault}, nil
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: shokoFault(err)}, nil
	}
	return &pluginv1.WatchSyncGetAccountResponse{Account: shokoAccount(user)}, nil
}

func (s *watchSyncServer) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	credentials := req.GetContext().GetCredentials()
	attributes := credentials.GetSecretAttributes()
	silo := silocatalog.NewClient(silocatalog.Config{
		BaseURL:   attributes[siloBaseURLKey],
		APIKey:    attributes[siloAPIKeyKey],
		ProfileID: attributes[siloProfileIDKey],
	}, s.httpClient)
	// The Shoko client is built on first use, so a batch with nothing to
	// mark works even while the plugin's Shoko settings are broken.
	var client *shoko.Client
	shokoClient := func() (*shoko.Client, *pluginv1.WatchSyncFault) {
		if client != nil {
			return client, nil
		}
		var fault *pluginv1.WatchSyncFault
		client, fault = s.shokoClient(credentials.GetAccessToken())
		return client, fault
	}
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(req.GetEvents()))
	// An exported history often holds several plays of one episode.
	watched := make(map[int]bool)
	for _, event := range req.GetEvents() {
		result := applyWatchEvent(ctx, silo, shokoClient, watched, event)
		if fault := result.GetFault(); fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
			// A rejected key fails every event. Reporting it for the whole
			// batch makes Silo keep the events and ask for a reconnect.
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
		}
		results = append(results, result)
	}
	return &pluginv1.WatchSyncApplyEventsResponse{Results: results}, nil
}

// importWatchedSetting is the connection setting that turns on importing
// Shoko's watched episodes. Silo's own import switch is on for every new
// connection, so this one, off by default, keeps the import opt-in.
const importWatchedSetting = "import_watched"

// ListRemoteState lists the episodes the connection's Shoko user has watched,
// for Silo to import, when the connection has importing turned on. Silo
// matches each one to its episode by the TVDB, TMDB or IMDb episode ID the
// plugin's metadata gave that episode, so episodes without one are skipped.
// Every sync reads the whole list: Shoko cannot list only recent watches.
func (s *watchSyncServer) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	auth := req.GetContext()
	if auth.GetConnectionSettings()[importWatchedSetting] != "true" || !requestsWatchedState(req.GetStateKinds()) {
		return &pluginv1.WatchSyncListRemoteStateResponse{}, nil
	}
	page := 1
	if token := strings.TrimSpace(req.GetPageToken()); token != "" {
		parsed, err := strconv.Atoi(token)
		if err != nil || parsed < 2 {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: invalidRequestFault("invalid page token")}, nil
		}
		page = parsed
	}
	size := int(req.GetPageSize())
	if size < 1 || size > shoko.MaxPageSize {
		size = shoko.MaxPageSize
	}
	client, fault := s.shokoClient(auth.GetCredentials().GetAccessToken())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	episodes, total, err := client.WatchedEpisodesPage(ctx, page, size)
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: shokoFault(err)}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{}
	unlinked, undated := 0, 0
	for _, episode := range episodes {
		state, reason := remoteWatchedState(episode)
		switch reason {
		case skipUnlinked:
			unlinked++
		case skipUndated:
			undated++
		default:
			response.Items = append(response.Items, state)
		}
	}
	if unlinked > 0 {
		response.Warnings = append(response.Warnings, fmt.Sprintf("skipped %d watched Shoko episodes with no TVDB, TMDB or IMDb episode link, which Silo cannot match", unlinked))
	}
	if undated > 0 {
		response.Warnings = append(response.Warnings, fmt.Sprintf("skipped %d watched Shoko episodes with no watched date", undated))
	}
	if len(episodes) > 0 && page*size < total {
		response.NextPageToken = strconv.Itoa(page + 1)
	}
	return response, nil
}

func requestsWatchedState(kinds []pluginv1.WatchSyncRemoteStateKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, kind := range kinds {
		if kind == pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED {
			return true
		}
	}
	return false
}

type skipReason int

const (
	notSkipped skipReason = iota
	skipUnlinked
	skipUndated
)

// remoteWatchedState describes a watched Shoko episode for Silo's import. Its
// external IDs are the ones episodeIDs gives the episode's Silo item, so the
// two match. The season and episode numbers are left out on purpose: Silo
// would read them as numbers in the TVDB or TMDB show, which Shoko's are not.
func remoteWatchedState(episode shoko.Episode) (*pluginv1.WatchSyncRemoteState, skipReason) {
	ids := episodeIDs(episode)
	external := map[string]string{}
	for _, namespace := range []string{"tvdb", "tmdb", "imdb"} {
		if id := ids[namespace]; id != "" {
			external[namespace] = id
		}
	}
	if len(external) == 0 || episode.IDs.ID < 1 {
		return nil, skipUnlinked
	}
	watchedAt, ok := episode.WatchedAt()
	if !ok {
		return nil, skipUndated
	}
	return &pluginv1.WatchSyncRemoteState{
		ProviderItemKey: "episode:" + strconv.Itoa(episode.IDs.ID),
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:       episodeTitle(episode),
			ExternalIds: external,
		},
		Watched: &pluginv1.WatchSyncRemoteWatchedState{
			PlayCount:     int32(max(episode.WatchCount, 1)),
			LastWatchedAt: timestamppb.New(watchedAt),
		},
	}, notSkipped
}

// applyWatchEvent marks the Shoko episodes behind a finished play watched.
// Every other event is acknowledged without a change.
func applyWatchEvent(
	ctx context.Context,
	silo *silocatalog.Client,
	shokoClient func() (*shoko.Client, *pluginv1.WatchSyncFault),
	watched map[int]bool,
	event *pluginv1.WatchSyncEvent,
) *pluginv1.WatchSyncApplyResult {
	result := &pluginv1.WatchSyncApplyResult{
		EventId: event.GetEventId(),
		Status:  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE,
	}
	switch event.GetMedia().GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
	default:
		return result
	}
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		return applyFinishedPlay(ctx, silo, shokoClient, watched, event, result)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		return applyExportedPlay(ctx, silo, shokoClient, watched, event, result)
	}
	return result
}

// applyFinishedPlay marks the item's Shoko episodes watched when a play in
// Silo finishes. Each finished play counts, so a rewatch is recorded too.
func applyFinishedPlay(
	ctx context.Context,
	silo *silocatalog.Client,
	shokoClient func() (*shoko.Client, *pluginv1.WatchSyncFault),
	watched map[int]bool,
	event *pluginv1.WatchSyncEvent,
	result *pluginv1.WatchSyncApplyResult,
) *pluginv1.WatchSyncApplyResult {
	// Some players report a finished play as a stop at position zero. Silo
	// still marks the item played, so such a stop counts when the profile's
	// saved state says the item is played.
	zeroStop := event.GetPositionSeconds() == 0 && event.GetCompletionPercent() == 0 && event.GetDurationSeconds() > 0
	if !event.GetCompleted() && !zeroStop {
		return result
	}
	item, found, err := silo.Item(ctx, event.GetMedia().GetMediaItemId())
	if err != nil {
		return faultResult(result, siloFault(err))
	}
	if !found || (!event.GetCompleted() && !item.Played) {
		return result
	}
	episodes := shokoEpisodeIDs(item.Files)
	if len(episodes) == 0 {
		// Not a Shoko VFS item, such as a play from another library.
		return result
	}
	return markEpisodes(ctx, shokoClient, watched, episodes, false, result)
}

// applyExportedPlay marks the item's Shoko episodes watched for a play in the
// profile's Silo history, which Silo sends once per play when it exports the
// history. Shoko cannot take the play's date and would record it as watched
// now, so an episode Shoko already has watched keeps its own date.
func applyExportedPlay(
	ctx context.Context,
	silo *silocatalog.Client,
	shokoClient func() (*shoko.Client, *pluginv1.WatchSyncFault),
	watched map[int]bool,
	event *pluginv1.WatchSyncEvent,
	result *pluginv1.WatchSyncApplyResult,
) *pluginv1.WatchSyncApplyResult {
	item, found, err := silo.Item(ctx, event.GetMedia().GetMediaItemId())
	if err != nil {
		return faultResult(result, siloFault(err))
	}
	if !found {
		return faultResult(result, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			SafeMessage: "the item is no longer in Silo",
		})
	}
	episodes := shokoEpisodeIDs(item.Files)
	if len(episodes) == 0 {
		// Reported as not found rather than sent, so the sync's count of
		// sent plays covers only the plays Shoko knows.
		return faultResult(result, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			SafeMessage: "the item is not from the Shoko VFS",
		})
	}
	return markEpisodes(ctx, shokoClient, watched, episodes, true, result)
}

// markEpisodes marks episodes watched in Shoko. watched remembers, for one
// batch, the episodes known to be watched; with skipWatched, those and any
// Shoko already has watched are left alone.
func markEpisodes(
	ctx context.Context,
	shokoClient func() (*shoko.Client, *pluginv1.WatchSyncFault),
	watched map[int]bool,
	episodes []int,
	skipWatched bool,
	result *pluginv1.WatchSyncApplyResult,
) *pluginv1.WatchSyncApplyResult {
	client, fault := shokoClient()
	if fault != nil {
		return faultResult(result, fault)
	}
	marked := false
	for _, episodeID := range episodes {
		if skipWatched {
			known, ok := watched[episodeID]
			if !ok {
				var err error
				known, err = client.EpisodeWatched(ctx, episodeID)
				if err != nil {
					return faultResult(result, episodeFault(episodeID, err, "read"))
				}
				watched[episodeID] = known
			}
			if known {
				continue
			}
		}
		if err := client.MarkEpisodeWatched(ctx, episodeID); err != nil {
			return faultResult(result, episodeFault(episodeID, err, "mark"))
		}
		watched[episodeID] = true
		marked = true
		fmt.Fprintf(os.Stderr, "shokoanime: marked Shoko episode %d watched\n", episodeID)
	}
	if marked {
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	}
	return result
}

// episodeFault maps a failed read or write of one Shoko episode to a fault.
func episodeFault(episodeID int, err error, action string) *pluginv1.WatchSyncFault {
	fault := shokoFault(err)
	var httpErr *shoko.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		fault.SafeMessage = fmt.Sprintf("Shoko has no episode %d; reconcile the Shoko VFS", episodeID)
	}
	fmt.Fprintf(os.Stderr, "shokoanime: could not %s Shoko episode %d: %s\n", action, episodeID, fault.GetSafeMessage())
	return fault
}

func shokoEpisodeIDs(files []string) []int {
	seen := map[int]struct{}{}
	var ids []int
	for _, file := range files {
		name := file[strings.LastIndexAny(file, `/\`)+1:]
		id := identity.Parse(name).EpisodeID
		if id < 1 {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// shokoClient returns a client for the plugin's Shoko server that acts as the
// Shoko user apiKey belongs to.
func (s *watchSyncServer) shokoClient(apiKey string) (*shoko.Client, *pluginv1.WatchSyncFault) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			SafeMessage: "the connection has no Shoko API key; reconnect",
		}
	}
	cfg, _, _, err := s.runtime.state()
	if err != nil {
		return nil, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			SafeMessage: "the ShokoAnime plugin's Shoko connection is not set up; an administrator must configure it",
		}
	}
	client, err := shoko.NewClient(shoko.Config{BaseURL: cfg.BaseURL, APIKey: apiKey, HTTPClient: s.httpClient})
	if err != nil {
		return nil, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			SafeMessage: "the ShokoAnime plugin's Shoko Server URL is invalid",
		}
	}
	return client, nil
}

func shokoAccount(user shoko.User) *pluginv1.WatchSyncAccount {
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: strconv.Itoa(user.ID),
		Username:        user.Username,
		DisplayName:     user.Username,
	}
}

// shokoFault maps a Shoko client error to a fault with a fixed message:
// Shoko's response text is never shown.
func shokoFault(err error) *pluginv1.WatchSyncFault {
	fault := &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: "could not reach Shoko; try again"}
	var httpErr *shoko.HTTPError
	if !errors.As(err, &httpErr) {
		return fault
	}
	switch status := httpErr.StatusCode; {
	case status == http.StatusUnauthorized:
		fault.Code, fault.SafeMessage = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Shoko rejected the API key; reconnect with a valid key"
	case status == http.StatusForbidden:
		fault.Code, fault.SafeMessage = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED, "Shoko denied the request for this API key's user"
	case status == http.StatusTooManyRequests:
		fault.Code, fault.SafeMessage = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED, "Shoko's request limit was reached; try again"
	case status >= 500 || status == http.StatusRequestTimeout:
		fault.SafeMessage = fmt.Sprintf("Shoko is temporarily unavailable (HTTP %d)", status)
	default:
		fault.Code, fault.SafeMessage = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, fmt.Sprintf("Shoko rejected the request (HTTP %d)", status)
	}
	return fault
}

// siloFault maps a Silo catalog error to a fault. Its messages are fixed.
func siloFault(err error) *pluginv1.WatchSyncFault {
	fault := &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: "could not read the item from Silo; try again"}
	var siloErr *silocatalog.Error
	if !errors.As(err, &siloErr) {
		return fault
	}
	fault.SafeMessage = siloErr.Error()
	switch siloErr.Kind {
	case silocatalog.InvalidConfig:
		fault.Code = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST
	case silocatalog.InvalidCredential:
		fault.Code = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL
	case silocatalog.PermissionDenied:
		fault.Code = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED
	case silocatalog.RateLimited:
		fault.Code = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED
	case silocatalog.Permanent:
		fault.Code = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT
	}
	return fault
}

func invalidRequestFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, SafeMessage: message}
}

// faultResult sets fault on result. A fault Silo can recover from asks for a
// retry; the rest reject the event.
func faultResult(result *pluginv1.WatchSyncApplyResult, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	result.Fault = fault
	switch fault.GetCode() {
	case pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT:
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
	default:
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
	}
	return result
}
