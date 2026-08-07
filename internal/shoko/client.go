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
)

// Config configures a read-only Shoko v3 client. The API key is only used for
// the request header and is never included in returned errors.
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
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + apiPrefix + endpoint
	u.RawQuery = ""
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, dst any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(endpoint, query), nil)
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
		if detail == "" {
			return fmt.Errorf("shoko: %s returned HTTP %d", endpoint, response.StatusCode)
		}
		return fmt.Errorf("shoko: %s returned HTTP %d: %s", endpoint, response.StatusCode, detail)
	}
	if dst == nil || len(bytes.TrimSpace(body)) == 0 || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("shoko: decode %s response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) Version(ctx context.Context) (VersionSet, error) {
	var result VersionSet
	err := c.get(ctx, "/Init/Version", nil, &result)
	return result, err
}

func (c *Client) ManagedFolders(ctx context.Context) ([]ManagedFolder, error) {
	var result []ManagedFolder
	err := c.get(ctx, "/ManagedFolder", nil, &result)
	return result, err
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
	if err := c.get(ctx, "/ManagedFolder/"+strconv.Itoa(folderID)+"/File", query, &raw); err != nil {
		return nil, 0, err
	}
	pageFiles, total, err := decodePage[File](raw)
	if err != nil {
		return nil, 0, fmt.Errorf("shoko: decode managed folder %d page %d: %w", folderID, page, err)
	}
	return pageFiles, total, nil
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

// EpisodesPage fetches one series episode page for resumable graph crawls.
func (c *Client) EpisodesPage(ctx context.Context, seriesID, page int) ([]Episode, int, error) {
	if seriesID < 1 {
		return nil, 0, errors.New("shoko: series ID must be positive")
	}
	if page < 1 {
		return nil, 0, errors.New("shoko: series page must be positive")
	}
	query := url.Values{
		"page":            {strconv.Itoa(page)},
		"pageSize":        {strconv.Itoa(pageSize)},
		"includeDataFrom": {"AniDB"},
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
		sort.Ints(seriesIDs)
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
