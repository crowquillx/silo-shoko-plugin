package shoko

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiPrefix          = "/api/v3"
	maxResponseBytes   = 32 << 20
	pageSize           = 1000
	defaultConcurrency = 8

	// Daily servers serve folders from /ManagedFolder and stable servers from
	// /ImportFolder. The two routes return the same shape.
	managedFolderRoute = "/ManagedFolder"
	importFolderRoute  = "/ImportFolder"
)

// HTTPError reports a non-2xx response from Shoko. Detail is already redacted.
type HTTPError struct {
	Endpoint   string
	StatusCode int
	Detail     string
}

func (e *HTTPError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("shoko: %s returned HTTP %d", e.Endpoint, e.StatusCode)
	}
	return fmt.Sprintf("shoko: %s returned HTTP %d: %s", e.Endpoint, e.StatusCode, e.Detail)
}

// Config configures a Shoko v3 client. The API key is only used for the
// request header and is never included in returned errors. The VFS and
// metadata paths only read from Shoko; MarkEpisodeWatched is the client's only
// write, used by the watch-sync capability with a profile's own API key.
type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type Client struct {
	httpClient  *http.Client
	baseURL     *url.URL
	apiKey      string
	concurrency int

	folderMu    sync.RWMutex
	folderRoute string
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("shoko: api key is required")
	}
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, errors.New("shoko: base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("shoko: parse base URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("shoko: base URL must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("shoko: base URL must not contain credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	}
	return &Client{httpClient: cfg.HTTPClient, baseURL: u, apiKey: strings.TrimSpace(cfg.APIKey), concurrency: defaultConcurrency}, nil
}

func (c *Client) resolve(endpoint string, query url.Values) string {
	return c.resolvePath(apiPrefix+endpoint, query)
}

// resolvePath resolves a server path that is not under the v3 API, such as a
// plugin's own route.
func (c *Client) resolvePath(path string, query url.Values) string {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	u.RawQuery = ""
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, dst any) error {
	return c.fetch(ctx, c.resolve(endpoint, query), endpoint, dst)
}

// fetch sends an authenticated GET to target and decodes the JSON response.
// endpoint names the request in errors.
func (c *Client) fetch(ctx context.Context, target, endpoint string, dst any) error {
	return c.do(ctx, http.MethodGet, target, endpoint, dst)
}

// do sends an authenticated request with no body to target and decodes the
// JSON response into dst, if dst is not nil. endpoint names the request in
// errors.
func (c *Client) do(ctx context.Context, method, target, endpoint string, dst any) error {
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return fmt.Errorf("shoko: create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("apikey", c.apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("shoko: request failed: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("shoko: read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return errors.New("shoko: response exceeds 32 MiB limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		detail = strings.ReplaceAll(detail, c.apiKey, "[redacted]")
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return &HTTPError{Endpoint: endpoint, StatusCode: response.StatusCode, Detail: detail}
	}
	if dst == nil || len(bytes.TrimSpace(body)) == 0 || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("shoko: decode %s response: %w", endpoint, err)
	}
	return nil
}

// showPostersPath is the show poster list of the Season Posters Shoko plugin
// (https://github.com/crowquillx/shoko-season-posters).
const showPostersPath = "/api/plugin/season-posters/show-posters"

// ShowPosters returns the show posters chosen with the Season Posters Shoko
// plugin, as Shoko image IDs keyed by group ID. Shoko gives a group only its
// main series' poster, so this is the only way for a show to have its own. A
// server without the plugin answers 404, which yields an empty map and no
// error.
func (c *Client) ShowPosters(ctx context.Context) (map[int]string, error) {
	var result struct {
		Shows []struct {
			GroupID *int   `json:"groupId"`
			ImageID string `json:"imageId"`
		} `json:"shows"`
	}
	err := c.fetch(ctx, c.resolvePath(showPostersPath, nil), showPostersPath, &result)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return map[int]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	posters := make(map[int]string, len(result.Shows))
	for _, show := range result.Shows {
		if show.GroupID != nil && *show.GroupID > 0 && show.ImageID != "" {
			posters[*show.GroupID] = show.ImageID
		}
	}
	return posters, nil
}

// CurrentUser returns the Shoko user the client's API key belongs to.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var result User
	err := c.get(ctx, "/User/Current", nil, &result)
	return result, err
}

// MarkEpisodeWatched marks an episode watched for the Shoko user the client's
// API key belongs to. Shoko also marks the episode's files watched, which is
// its default for this endpoint.
func (c *Client) MarkEpisodeWatched(ctx context.Context, episodeID int) error {
	if episodeID < 1 {
		return fmt.Errorf("shoko: invalid episode ID %d", episodeID)
	}
	endpoint := "/Episode/" + strconv.Itoa(episodeID) + "/Watched/true"
	return c.do(ctx, http.MethodPost, c.resolve(endpoint, nil), endpoint, nil)
}

// MaxPageSize is the largest page Shoko serves.
const MaxPageSize = pageSize

// WatchedEpisodesPage returns one page of the episodes the API key's user has
// watched, across the whole server, and the total number of them. Pages start
// at 1 and hold at most MaxPageSize episodes.
func (c *Client) WatchedEpisodesPage(ctx context.Context, page, size int) ([]Episode, int, error) {
	if page < 1 {
		return nil, 0, errors.New("shoko: page must be positive")
	}
	size = min(max(size, 1), MaxPageSize)
	var result listResult[Episode]
	query := url.Values{
		"includeWatched": {"only"},
		"pageSize":       {strconv.Itoa(size)},
		"page":           {strconv.Itoa(page)},
	}
	if err := c.get(ctx, "/Episode", query, &result); err != nil {
		return nil, 0, err
	}
	return result.List, result.Total, nil
}

func (c *Client) Version(ctx context.Context) (VersionSet, error) {
	var result VersionSet
	err := c.get(ctx, "/Init/Version", nil, &result)
	return result, err
}

// ManagedFolders lists the server's folders. See withFolderRoute for how the
// server's folder API is detected.
func (c *Client) ManagedFolders(ctx context.Context) ([]ManagedFolder, error) {
	var result []ManagedFolder
	err := c.withFolderRoute(func(route string) error {
		result = nil
		return c.get(ctx, route, nil, &result)
	})
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return nil, errors.New("shoko: neither /ManagedFolder nor /ImportFolder exists; check that the server URL points at Shoko Server")
	}
	return result, err
}

// withFolderRoute calls fn with the server's folder route. The route that worked
// last is tried first and the other is tried on a 404, so no version check is
// needed and upgrading Shoko from stable to daily needs no reconfiguration.
func (c *Client) withFolderRoute(fn func(route string) error) error {
	routes := []string{managedFolderRoute, importFolderRoute}
	if c.currentFolderRoute() == importFolderRoute {
		routes = []string{importFolderRoute, managedFolderRoute}
	}
	var err error
	for _, route := range routes {
		err = fn(route)
		if err == nil {
			c.setFolderRoute(route)
			return nil
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
			return err
		}
	}
	return err
}

func (c *Client) currentFolderRoute() string {
	c.folderMu.RLock()
	defer c.folderMu.RUnlock()
	return c.folderRoute
}

func (c *Client) setFolderRoute(route string) {
	c.folderMu.Lock()
	defer c.folderMu.Unlock()
	c.folderRoute = route
}

func (c *Client) FilesForManagedFolder(ctx context.Context, folderID int) ([]File, error) {
	if folderID < 1 {
		return nil, errors.New("shoko: managed folder ID must be positive")
	}
	var files []File
	for page := 1; ; page++ {
		pageFiles, total, err := c.FilesForManagedFolderPage(ctx, folderID, page)
		if err != nil {
			return nil, err
		}
		files = append(files, pageFiles...)
		if len(pageFiles) == 0 || (total > 0 && len(files) >= total) || len(pageFiles) < pageSize {
			return files, nil
		}
	}
}

// FilesForManagedFolderPage fetches one page so a caller can persist a
// resumable crawl cursor instead of holding the complete production library in
// memory until every page has been fetched.
func (c *Client) FilesForManagedFolderPage(ctx context.Context, folderID, page int) ([]File, int, error) {
	if folderID < 1 {
		return nil, 0, errors.New("shoko: managed folder ID must be positive")
	}
	if page < 1 {
		return nil, 0, errors.New("shoko: managed folder page must be positive")
	}
	query := url.Values{
		"page":     {strconv.Itoa(page)},
		"pageSize": {strconv.Itoa(pageSize)},
		"include":  {"XRefs"},
	}
	var raw json.RawMessage
	if err := c.withFolderRoute(func(route string) error {
		raw = nil
		return c.get(ctx, route+"/"+strconv.Itoa(folderID)+"/File", query, &raw)
	}); err != nil {
		return nil, 0, err
	}
	pageFiles, total, err := decodePage[File](raw)
	if err != nil {
		return nil, 0, fmt.Errorf("shoko: decode managed folder %d page %d: %w", folderID, page, err)
	}
	return pageFiles, total, nil
}

// File fetches one file with its episode cross-references, in the same shape
// as a managed folder's file list.
func (c *Client) File(ctx context.Context, fileID int) (File, error) {
	if fileID < 1 {
		return File{}, errors.New("shoko: file ID must be positive")
	}
	var result File
	err := c.get(ctx, "/File/"+strconv.Itoa(fileID), url.Values{"include": {"XRefs"}}, &result)
	return result, err
}

// IsNotFound reports whether err is Shoko answering 404, which for an ID
// lookup means the item no longer exists.
func IsNotFound(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

func (c *Client) Series(ctx context.Context, seriesID int) (Series, error) {
	if seriesID < 1 {
		return Series{}, errors.New("shoko: series ID must be positive")
	}
	var result Series
	err := c.get(ctx, "/Series/"+strconv.Itoa(seriesID), url.Values{"includeDataFrom": {"AniDB"}}, &result)
	return result, err
}
func (c *Client) Group(ctx context.Context, groupID int) (Group, error) {
	if groupID < 1 {
		return Group{}, errors.New("shoko: group ID must be positive")
	}
	var result Group
	err := c.get(ctx, "/Group/"+strconv.Itoa(groupID), nil, &result)
	return result, err
}

func (c *Client) GroupSeries(ctx context.Context, groupID int) ([]Series, error) {
	if groupID < 1 {
		return nil, errors.New("shoko: group ID must be positive")
	}
	var result []Series
	query := url.Values{
		"recursive":       {"false"},
		"includeDataFrom": {"AniDB"},
	}
	err := c.get(ctx, "/Group/"+strconv.Itoa(groupID)+"/Series", query, &result)
	return result, err
}

func (c *Client) Episode(ctx context.Context, episodeID int) (Episode, error) {
	if episodeID < 1 {
		return Episode{}, errors.New("shoko: episode ID must be positive")
	}
	var result Episode
	err := c.get(ctx, "/Episode/"+strconv.Itoa(episodeID), url.Values{"includeDataFrom": {"AniDB"}}, &result)
	return result, err
}

func (c *Client) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	if seriesID < 1 {
		return nil, errors.New("shoko: series ID must be positive")
	}
	var episodes []Episode
	for page := 1; ; page++ {
		pageEpisodes, total, err := c.EpisodesPage(ctx, seriesID, page)
		if err != nil {
			return nil, err
		}
		episodes = append(episodes, pageEpisodes...)
		if len(pageEpisodes) == 0 || (total > 0 && len(episodes) >= total) || len(pageEpisodes) < pageSize {
			return episodes, nil
		}
	}
}

// MetadataEpisodes lists a series' episodes with their TMDB data, which holds
// the TMDB rating. The crawl does not need it and uses Episodes.
func (c *Client) MetadataEpisodes(ctx context.Context, seriesID int) ([]Episode, error) {
	if seriesID < 1 {
		return nil, errors.New("shoko: series ID must be positive")
	}
	var episodes []Episode
	for page := 1; ; page++ {
		pageEpisodes, total, err := c.episodesPage(ctx, seriesID, page, "AniDB,TMDB")
		if err != nil {
			return nil, err
		}
		episodes = append(episodes, pageEpisodes...)
		if len(pageEpisodes) == 0 || (total > 0 && len(episodes) >= total) || len(pageEpisodes) < pageSize {
			return episodes, nil
		}
	}
}

// genreTagFilter asks Shoko for only the tags it classifies as genres: the
// Genre bit with Invert set keeps that category and drops every other one.
const genreTagFilter = 1<<31 | 1<<7

// SeriesGenres returns the series' verified tags that Shoko counts as genres.
func (c *Client) SeriesGenres(ctx context.Context, seriesID int) ([]Tag, error) {
	if seriesID < 1 {
		return nil, errors.New("shoko: series ID must be positive")
	}
	var result []Tag
	query := url.Values{"filter": {strconv.Itoa(genreTagFilter)}, "excludeDescriptions": {"true"}}
	err := c.get(ctx, "/Series/"+strconv.Itoa(seriesID)+"/Tags", query, &result)
	return result, err
}

// SeriesCast returns the series' cast, staff and studio credits.
func (c *Client) SeriesCast(ctx context.Context, seriesID int) ([]Role, error) {
	if seriesID < 1 {
		return nil, errors.New("shoko: series ID must be positive")
	}
	var result []Role
	err := c.get(ctx, "/Series/"+strconv.Itoa(seriesID)+"/Cast", nil, &result)
	return result, err
}

// SeriesTMDBShows returns the TMDB shows linked to a series, with their
// content ratings.
func (c *Client) SeriesTMDBShows(ctx context.Context, seriesID int) ([]TMDBShow, error) {
	if seriesID < 1 {
		return nil, errors.New("shoko: series ID must be positive")
	}
	var result []TMDBShow
	err := c.get(ctx, "/Series/"+strconv.Itoa(seriesID)+"/TMDB/Show", url.Values{"include": {"ContentRatings"}}, &result)
	return result, err
}

// EpisodesPage fetches one series episode page for resumable graph crawls.
func (c *Client) EpisodesPage(ctx context.Context, seriesID, page int) ([]Episode, int, error) {
	return c.episodesPage(ctx, seriesID, page, "AniDB")
}

func (c *Client) episodesPage(ctx context.Context, seriesID, page int, dataSources string) ([]Episode, int, error) {
	if seriesID < 1 {
		return nil, 0, errors.New("shoko: series ID must be positive")
	}
	if page < 1 {
		return nil, 0, errors.New("shoko: series page must be positive")
	}
	query := url.Values{
		"page":            {strconv.Itoa(page)},
		"pageSize":        {strconv.Itoa(pageSize)},
		"includeDataFrom": {dataSources},
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/Series/"+strconv.Itoa(seriesID)+"/Episode", query, &raw); err != nil {
		return nil, 0, err
	}
	pageEpisodes, total, err := decodePage[Episode](raw)
	if err != nil {
		return nil, 0, fmt.Errorf("shoko: decode series %d page %d: %w", seriesID, page, err)
	}
	return pageEpisodes, total, nil
}

// Snapshot fetches the managed folders, their files, and the series/episode
// graph referenced by those files. It is intentionally authoritative: callers
// can cache the result, but should periodically rebuild it from scratch.
func (c *Client) Snapshot(ctx context.Context, folderIDs []int) (Snapshot, error) {
	folders, err := c.ManagedFolders(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	folderSet := make(map[int]struct{}, len(folderIDs))
	for _, id := range folderIDs {
		if id > 0 {
			folderSet[id] = struct{}{}
		}
	}
	result := Snapshot{
		ManagedFolders: make(map[int]ManagedFolder),
		Groups:         make(map[int]Group),
		GroupSeries:    make(map[int][]int),
		Series:         make(map[int]Series),
		Episodes:       make(map[int]Episode),
	}
	for _, folder := range folders {
		if len(folderSet) > 0 {
			if _, ok := folderSet[folder.ID]; !ok {
				continue
			}
		}
		result.ManagedFolders[folder.ID] = folder
		files, err := c.FilesForManagedFolder(ctx, folder.ID)
		if err != nil {
			return Snapshot{}, err
		}
		result.Files = append(result.Files, files...)
	}

	groups, groupSeries, series, episodes, err := c.FetchGraph(ctx, result.Files)
	if err != nil {
		return Snapshot{}, err
	}
	result.Groups = groups
	result.GroupSeries = groupSeries
	result.Series = series
	result.Episodes = episodes
	return result, nil
}

// FetchGraph resolves the series/episode graph referenced by files. Requests
// for independent series are bounded and parallel, which keeps a complete
// production crawl inside scan_source.v1's longer host deadline without
// creating unbounded load on Shoko.
func (c *Client) FetchGraph(ctx context.Context, files []File) (map[int]Group, map[int][]int, map[int]Series, map[int]Episode, error) {
	seriesIDs := make(map[int]struct{})
	episodeIDs := make(map[int]struct{})
	for _, file := range files {
		for _, reference := range file.SeriesIDs {
			if reference.SeriesID.ID != nil && *reference.SeriesID.ID > 0 {
				seriesIDs[*reference.SeriesID.ID] = struct{}{}
			}
			for _, episodeReference := range reference.EpisodeIDs {
				if episodeReference.ID != nil && *episodeReference.ID > 0 {
					episodeIDs[*episodeReference.ID] = struct{}{}
				}
			}
		}
	}

	seriesResult := make(map[int]Series, len(seriesIDs))
	episodeResult := make(map[int]Episode)
	var resultMu sync.Mutex
	if err := c.parallel(ctx, sortedIDs(seriesIDs), func(id int) error {
		series, err := c.Series(ctx, id)
		if err != nil {
			return err
		}
		episodes, err := c.Episodes(ctx, id)
		if err != nil {
			return err
		}
		resultMu.Lock()
		seriesResult[id] = series
		for _, episode := range episodes {
			if episode.IDs.ID > 0 {
				episodeResult[episode.IDs.ID] = episode
			}
		}
		resultMu.Unlock()
		return nil
	}); err != nil {
		return nil, nil, nil, nil, err
	}

	missingEpisodes := make([]int, 0, len(episodeIDs))
	for id := range episodeIDs {
		if _, exists := episodeResult[id]; !exists {
			missingEpisodes = append(missingEpisodes, id)
		}
	}
	sort.Ints(missingEpisodes)
	if err := c.parallel(ctx, missingEpisodes, func(id int) error {
		episode, err := c.Episode(ctx, id)
		if err != nil {
			return err
		}
		resultMu.Lock()
		episodeResult[id] = episode
		resultMu.Unlock()
		return nil
	}); err != nil {
		return nil, nil, nil, nil, err
	}

	parentIDs := make(map[int]struct{})
	for _, episode := range episodeResult {
		if episode.IDs.ParentSeries > 0 {
			if _, exists := seriesResult[episode.IDs.ParentSeries]; !exists {
				parentIDs[episode.IDs.ParentSeries] = struct{}{}
			}
		}
	}
	if err := c.parallel(ctx, sortedIDs(parentIDs), func(id int) error {
		series, err := c.Series(ctx, id)
		if err != nil {
			return err
		}
		resultMu.Lock()
		seriesResult[id] = series
		resultMu.Unlock()
		return nil
	}); err != nil {
		return nil, nil, nil, nil, err
	}

	groupIDs := make(map[int]struct{})
	for _, series := range seriesResult {
		if series.IDs.ParentGroup > 0 {
			groupIDs[series.IDs.ParentGroup] = struct{}{}
		}
	}
	groupResult := make(map[int]Group, len(groupIDs))
	groupSeriesResult := make(map[int][]int, len(groupIDs))
	if err := c.parallel(ctx, sortedIDs(groupIDs), func(id int) error {
		group, err := c.Group(ctx, id)
		if err != nil {
			return err
		}
		members, err := c.GroupSeries(ctx, id)
		if err != nil {
			return err
		}
		// Keep Shoko's order: it decides the group's season numbers.
		seen := make(map[int]struct{}, len(members))
		seriesIDs := make([]int, 0, len(members))
		resultMu.Lock()
		groupResult[id] = group
		for _, member := range members {
			memberID := member.IDs.ID
			if memberID < 1 {
				continue
			}
			if _, exists := seen[memberID]; !exists {
				seen[memberID] = struct{}{}
				seriesIDs = append(seriesIDs, memberID)
			}
			if _, exists := seriesResult[memberID]; !exists {
				seriesResult[memberID] = member
			}
		}
		groupSeriesResult[id] = seriesIDs
		resultMu.Unlock()
		return nil
	}); err != nil {
		return nil, nil, nil, nil, err
	}
	return groupResult, groupSeriesResult, seriesResult, episodeResult, nil
}

func (c *Client) parallel(ctx context.Context, ids []int, fn func(int) error) error {
	if len(ids) == 0 {
		return nil
	}
	workers := c.concurrency
	if workers < 1 {
		workers = defaultConcurrency
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(ids))
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case id, ok := <-jobs:
					if !ok {
						return
					}
					if err := fn(id); err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						cancel()
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	errMu.Lock()
	defer errMu.Unlock()
	return firstErr
}

func sortedIDs(values map[int]struct{}) []int {
	ids := make([]int, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func decodePage[T any](raw json.RawMessage) ([]T, int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, 0, nil
	}
	if trimmed[0] == '[' {
		var list []T
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, 0, err
		}
		return list, len(list), nil
	}
	var page listResult[T]
	if err := json.Unmarshal(trimmed, &page); err != nil {
		return nil, 0, err
	}
	return page.List, page.Total, nil
}

// maxImageBytes bounds one downloaded image. Shoko's largest artwork, such as
// a 4K backdrop, is a few MiB.
const maxImageBytes = 32 << 20

// Image downloads one image by its Shoko ID, as image paths name it: a UUID on
// daily servers, or Source/Type/ID on stable ones. It returns the bytes and the
// content type Shoko reported.
func (c *Client) Image(ctx context.Context, imageID string) ([]byte, string, error) {
	endpoint := "/Image/" + imageID
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(endpoint, nil), nil)
	if err != nil {
		return nil, "", fmt.Errorf("shoko: create request: %w", err)
	}
	request.Header.Set("apikey", c.apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("shoko: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", &HTTPError{Endpoint: endpoint, StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("shoko: read image: %w", err)
	}
	if len(body) > maxImageBytes {
		return nil, "", errors.New("shoko: image exceeds 32 MiB limit")
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("shoko: %s returned no data", endpoint)
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		contentType = http.DetectContentType(body)
	}
	return body, contentType, nil
}
