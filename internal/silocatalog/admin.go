package silocatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
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

// Refresh queues a metadata refresh of an item. For a show, Silo also
// refreshes its seasons, episodes and artwork. found is false when Silo has no
// such item.
func (a *Admin) Refresh(ctx context.Context, contentID string) (found bool, err error) {
	status, err := a.do(ctx, http.MethodPost, "/api/v1/admin/items/"+url.PathEscape(contentID)+"/refresh-metadata", map[string]string{"mode": "quick"}, nil)
	if err != nil {
		return false, err
	}
	return status != http.StatusNotFound, nil
}

func (a *Admin) do(ctx context.Context, method, apiPath string, body any, target any) (int, error) {
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
