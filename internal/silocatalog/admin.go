package silocatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
)

// AdminConfig names a Silo server and an API key of an administrator, which
// the plugin uses to ask Silo to refresh shows whose Shoko metadata or artwork
// changed. Silo gives plugins no other way to start a refresh.
type AdminConfig struct {
	BaseURL string
	APIKey  string
}

// Enabled reports whether both settings are present.
func (c AdminConfig) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.APIKey) != ""
}

// Validate checks the settings without contacting Silo.
func (c AdminConfig) Validate() error {
	return Config{BaseURL: c.BaseURL, APIKey: c.APIKey, ProfileID: "-"}.Validate()
}

// Admin asks one Silo server to refresh items.
type Admin struct {
	config AdminConfig
	http   *http.Client

	// profileMu guards profile, the administrator's profile the catalog
	// endpoints are read as. Silo requires an X-Profile-Id header on them.
	profileMu sync.Mutex
	profile   string
}

// NewAdmin returns an admin client. A nil httpClient uses a default with a
// request timeout that does not follow redirects, so the API key is never sent
// to another host.
func NewAdmin(config AdminConfig, httpClient *http.Client) *Admin {
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.APIKey = strings.TrimSpace(config.APIKey)
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Admin{config: config, http: httpClient}
}

// Library is one Silo library and the folders it scans.
type Library struct {
	ID    string   `json:"id"`
	Paths []string `json:"paths"`
}

// Libraries lists Silo's libraries.
func (a *Admin) Libraries(ctx context.Context) ([]Library, error) {
	var response struct {
		Items []Library `json:"items"`
	}
	if _, err := a.do(ctx, http.MethodGet, "/api/v2/libraries", nil, &response); err != nil {
		return nil, err
	}
	return response.Items, nil
}

// LibrariesUnder returns the IDs of the libraries that scan root, or a folder
// inside or above it.
func LibrariesUnder(libraries []Library, root string) []string {
	root = filepath.Clean(root)
	ids := make([]string, 0)
	for _, library := range libraries {
		for _, path := range library.Paths {
			path = filepath.Clean(path)
			if path == root || within(path, root) || within(root, path) {
				ids = append(ids, library.ID)
				break
			}
		}
	}
	return ids
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ShowContentID returns the ID of the show whose files carry marker, such as
// "[Shoko Group=12]", in a library. Silo matches its search against each
// show folder's path and a sample file path, and the plugin puts the marker in
// every file name. found is false when Silo has not matched such a show.
func (a *Admin) ShowContentID(ctx context.Context, libraryID, marker string) (contentID string, found bool, err error) {
	query := url.Values{
		"library_id": {libraryID},
		"q":          {marker},
		"limit":      {"10"},
	}
	var response struct {
		Items []struct {
			ContentID      string `json:"content_id"`
			RootPath       string `json:"root_path"`
			SampleFilePath string `json:"sample_file_path"`
		} `json:"items"`
	}
	status, err := a.do(ctx, http.MethodGet, "/api/v2/libraries/roots?"+query.Encode(), nil, &response)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	for _, item := range response.Items {
		if item.ContentID == "" {
			continue
		}
		if strings.Contains(item.SampleFilePath, marker) || strings.Contains(item.RootPath, marker) {
			return item.ContentID, true, nil
		}
	}
	return "", false, nil
}

// ShowByFolder finds the show in a library whose folder is folder, by
// searching the catalog for each of titles and checking each series found.
// Silo lists a show among a library's roots only after a full library scan,
// so a show added by an autoscan is found this way instead. found is false
// when no show has that folder.
func (a *Admin) ShowByFolder(ctx context.Context, libraryID, folder string, titles []string) (contentID string, found bool, err error) {
	folder = filepath.Clean(folder)
	checked := make(map[string]bool)
	for _, title := range titles {
		if strings.TrimSpace(title) == "" {
			continue
		}
		query := url.Values{
			"library_id": {libraryID},
			"q":          {title},
			"type":       {"series"},
			"limit":      {"20"},
		}
		var response struct {
			Items []struct {
				ContentID string `json:"content_id"`
				Type      string `json:"type"`
			} `json:"items"`
		}
		if _, err := a.do(ctx, http.MethodGet, "/api/v2/catalog?"+query.Encode(), nil, &response); err != nil {
			return "", false, err
		}
		for _, item := range response.Items {
			if item.ContentID == "" || item.Type != "series" || checked[item.ContentID] {
				continue
			}
			checked[item.ContentID] = true
			var detail struct {
				FolderPaths []string `json:"folder_paths"`
			}
			status, err := a.do(ctx, http.MethodGet, "/api/v2/catalog/items/"+url.PathEscape(item.ContentID), nil, &detail)
			if err != nil {
				return "", false, err
			}
			if status == http.StatusNotFound {
				continue
			}
			for _, path := range detail.FolderPaths {
				if filepath.Clean(path) == folder {
					return item.ContentID, true, nil
				}
			}
		}
	}
	return "", false, nil
}

// Refresh queues a metadata refresh of an item. For a show, Silo also
// refreshes its seasons, episodes and artwork. found is false when Silo has no
// such item.
func (a *Admin) Refresh(ctx context.Context, contentID string) (found bool, err error) {
	status, err := a.do(ctx, http.MethodPost, "/api/v2/admin/items/"+url.PathEscape(contentID)+"/refresh-metadata", map[string]string{"mode": "quick"}, nil)
	if err != nil {
		return false, err
	}
	return status != http.StatusNotFound, nil
}

// ReleaseEpisodeIDs clears the TMDB, TVDB and IMDb IDs on every episode of a
// show that Silo still lists. Silo keeps each of those IDs on one episode
// only, so a show that went away when Shoko regrouped its series would
// otherwise keep them, and the show that now holds the files could not store
// them: its episodes would keep Silo's "Episode N" placeholders until Silo
// deleted the old show. Only the IDs change; titles, watch state and the show
// itself stay as they are. found is false when Silo has no such show.
func (a *Admin) ReleaseEpisodeIDs(ctx context.Context, seriesID string) (released int, found bool, err error) {
	var seasons struct {
		Items []struct {
			SeasonNumber int `json:"season_number"`
		} `json:"items"`
	}
	status, err := a.do(ctx, http.MethodGet, "/api/v2/catalog/series/"+url.PathEscape(seriesID)+"/seasons", nil, &seasons)
	if err != nil {
		return 0, false, err
	}
	if status == http.StatusNotFound {
		return 0, false, nil
	}
	cleared := ""
	for _, season := range seasons.Items {
		var episodes struct {
			Items []struct {
				ContentID string `json:"content_id"`
				ImdbID    string `json:"imdb_id"`
				TmdbID    string `json:"tmdb_id"`
				TvdbID    string `json:"tvdb_id"`
			} `json:"items"`
		}
		status, err := a.do(ctx, http.MethodGet, fmt.Sprintf("/api/v2/catalog/series/%s/seasons/%d/episodes", url.PathEscape(seriesID), season.SeasonNumber), nil, &episodes)
		if err != nil {
			return released, true, err
		}
		if status == http.StatusNotFound {
			continue
		}
		for _, episode := range episodes.Items {
			if episode.ContentID == "" || (episode.ImdbID == "" && episode.TmdbID == "" && episode.TvdbID == "") {
				continue
			}
			// An admin edit changes only the fields it names and locks nothing,
			// so the episode can take IDs again if its show comes back.
			body := map[string]*string{"imdb_id": &cleared, "tmdb_id": &cleared, "tvdb_id": &cleared}
			status, err := a.do(ctx, http.MethodPatch, "/api/v2/admin/items/"+url.PathEscape(episode.ContentID)+"/metadata", body, nil)
			if err != nil {
				return released, true, err
			}
			if status != http.StatusNotFound {
				released++
			}
		}
	}
	return released, true, nil
}

// catalogProfile returns the profile to read Silo's catalog as: the API key
// owner's first profile that is not a child's, has no PIN and is not limited
// to some libraries, or else their first profile that is not a child's. Silo's
// catalog endpoints require a profile and show only what it may see. It is
// looked up once; "" means Silo listed no usable profile, and requests go
// without one.
func (a *Admin) catalogProfile(ctx context.Context) (string, error) {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	if a.profile != "" {
		return a.profile, nil
	}
	var response struct {
		Items []struct {
			ID                string   `json:"id"`
			IsChild           bool     `json:"is_child"`
			HasPIN            bool     `json:"has_pin"`
			AllowedLibraryIDs []string `json:"allowed_library_ids"`
		} `json:"items"`
	}
	status, err := a.do(ctx, http.MethodGet, "/api/v2/profiles", nil, &response)
	if err != nil || status == http.StatusNotFound {
		return "", err
	}
	fallback := ""
	for _, profile := range response.Items {
		if profile.ID == "" || profile.IsChild {
			continue
		}
		if !profile.HasPIN && len(profile.AllowedLibraryIDs) == 0 {
			a.profile = profile.ID
			return a.profile, nil
		}
		if fallback == "" {
			fallback = profile.ID
		}
	}
	a.profile = fallback
	return a.profile, nil
}

func (a *Admin) do(ctx context.Context, method, apiPath string, body any, target any) (int, error) {
	profile := ""
	if strings.HasPrefix(apiPath, "/api/v2/catalog") {
		var err error
		if profile, err = a.catalogProfile(ctx); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, &Error{Permanent, "could not encode the Silo request"}
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.config.BaseURL+apiPath, reader)
	if err != nil {
		return 0, &Error{InvalidConfig, "the Silo server URL is invalid"}
	}
	request.Header.Set("Authorization", "Bearer "+a.config.APIKey)
	request.Header.Set("Accept", "application/json")
	if profile != "" {
		request.Header.Set("X-Profile-Id", profile)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.http.Do(request)
	if err != nil {
		return 0, &Error{Temporary, "could not reach Silo"}
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return response.StatusCode, nil
	case response.StatusCode == http.StatusUnauthorized:
		return response.StatusCode, &Error{InvalidCredential, "Silo rejected the Silo admin API key"}
	case response.StatusCode == http.StatusForbidden:
		return response.StatusCode, &Error{PermissionDenied, "the Silo API key is not an administrator's, or is limited to some scopes"}
	case response.StatusCode == http.StatusTooManyRequests:
		return response.StatusCode, &Error{RateLimited, "Silo's request limit was reached"}
	case response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout:
		return response.StatusCode, &Error{Temporary, "Silo is temporarily unavailable"}
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return response.StatusCode, &Error{Permanent, "Silo rejected the request; check the Silo server URL"}
	}
	if target == nil {
		return response.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes || json.Unmarshal(data, target) != nil {
		return response.StatusCode, &Error{Temporary, "Silo returned an unreadable response"}
	}
	return response.StatusCode, nil
}
