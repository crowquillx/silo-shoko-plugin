// Package silocatalog reads one profile's view of a Silo catalog item: whether
// the profile has played it and which files back it. The watch-sync
// capability uses it to find the Shoko episode behind a finished play, since
// Silo's watch events carry only the Silo item ID. Its only write is Admin's
// request that Silo refresh an item.
package silocatalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout   = 10 * time.Second
	maxResponseBytes = 8 << 20
)

// Kind classifies an Error so callers can map it to a watch-sync fault.
type Kind int

const (
	Temporary Kind = iota
	InvalidConfig
	InvalidCredential
	PermissionDenied
	RateLimited
	Permanent
)

// Error carries only fixed messages. Silo response bodies, URLs and transport
// errors may contain credentials, so none of them reach the message.
type Error struct {
	Kind    Kind
	message string
}

func (e *Error) Error() string { return e.message }

// Config names the Silo server, the API key of the account that owns the
// profile, and the profile whose state is read.
type Config struct {
	BaseURL   string
	APIKey    string
	ProfileID string
}

// Validate checks the settings without contacting Silo.
func (c Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return &Error{InvalidConfig, "the Silo server URL must be an HTTP or HTTPS URL without credentials, query, or fragment"}
	}
	if strings.TrimSpace(c.APIKey) == "" || strings.ContainsAny(c.APIKey, "\r\n") {
		return &Error{InvalidConfig, "a Silo API key is required"}
	}
	if strings.TrimSpace(c.ProfileID) == "" || strings.ContainsAny(c.ProfileID, "\r\n") {
		return &Error{InvalidConfig, "a Silo profile ID is required"}
	}
	return nil
}

// Client reads from one Silo server as one profile.
type Client struct {
	config Config
	http   *http.Client
}

// NewClient returns a client for config. A nil httpClient uses a default with
// a request timeout that does not follow redirects, so the API key is never
// sent to another host.
func NewClient(config Config, httpClient *http.Client) *Client {
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.ProfileID = strings.TrimSpace(config.ProfileID)
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{config: config, http: httpClient}
}

// ValidateProfile checks that the API key works and can act as the profile.
func (c *Client) ValidateProfile(ctx context.Context) error {
	if err := c.config.Validate(); err != nil {
		return err
	}
	var response struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		problem
	}
	status, err := c.get(ctx, "/api/v2/profiles", &response)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		// Silo answers an unknown X-Profile-Id with its own not_found
		// problem; any other 404 means the URL does not lead to Silo's API.
		if response.isNotFound() && strings.Contains(strings.ToLower(response.Detail), "profile") {
			return &Error{InvalidConfig, "the Silo profile ID was not found for this API key's account; check the profile ID"}
		}
		return &Error{Permanent, "Silo's profile list was not found; check the Silo server URL"}
	}
	for _, profile := range response.Items {
		if profile.ID == c.config.ProfileID {
			return nil
		}
	}
	return &Error{PermissionDenied, "the Silo API key cannot access the selected profile; reconnect with the right account and profile"}
}

// Item is what the plugin needs to know about one catalog item.
type Item struct {
	ContentID string
	// Played reports whether the profile has the item marked played.
	Played bool
	// Files lists each file version's path, or its base name when Silo
	// leaves the path out.
	Files []string
}

// Item returns the profile's view of a catalog item. found is false when Silo
// has no such item for the profile.
func (c *Client) Item(ctx context.Context, mediaItemID string) (item Item, found bool, err error) {
	if err := c.config.Validate(); err != nil {
		return Item{}, false, err
	}
	mediaItemID = strings.TrimSpace(mediaItemID)
	if mediaItemID == "" || mediaItemID == "." || mediaItemID == ".." {
		return Item{}, false, &Error{InvalidConfig, "the watch event has no Silo item ID"}
	}
	var detail struct {
		ContentID string `json:"content_id"`
		UserState *struct {
			Played bool `json:"played"`
		} `json:"user_state"`
		Versions []struct {
			FileName string `json:"file_name"`
			FilePath string `json:"file_path"`
		} `json:"versions"`
	}
	status, err := c.get(ctx, "/api/v2/catalog/items/"+url.PathEscape(mediaItemID), &detail)
	if err != nil {
		return Item{}, false, err
	}
	if status == http.StatusNotFound {
		return Item{}, false, nil
	}
	if detail.ContentID != mediaItemID {
		return Item{}, false, &Error{Permanent, "Silo returned a different item than the one asked for"}
	}
	item = Item{ContentID: detail.ContentID, Played: detail.UserState != nil && detail.UserState.Played}
	for _, version := range detail.Versions {
		file := version.FilePath
		if file == "" {
			file = version.FileName
		}
		if file = strings.TrimSpace(file); file != "" {
			item.Files = append(item.Files, file)
		}
	}
	return item, true, nil
}

// problem is the part of Silo's RFC 9457 problem response the plugin reads to
// tell Silo's own errors apart. It is never shown.
type problem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

func (p problem) isNotFound() bool {
	return p.Type == "not_found" || strings.HasSuffix(p.Type, "/not_found")
}

func (c *Client) get(ctx context.Context, apiPath string, target any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.BaseURL+apiPath, nil)
	if err != nil {
		return 0, &Error{InvalidConfig, "the Silo server URL is invalid"}
	}
	request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	request.Header.Set("X-Profile-Id", c.config.ProfileID)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return 0, &Error{Temporary, "could not reach Silo; try again"}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// A 404 body is read only to tell Silo's own errors apart. It is
		// never shown, so an unreadable one is not an error.
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = json.Unmarshal(body, target)
		return response.StatusCode, nil
	case http.StatusGone:
		return response.StatusCode, &Error{Permanent, "this Silo server does not serve the API the plugin uses; update the plugin"}
	case http.StatusUnauthorized:
		return response.StatusCode, &Error{InvalidCredential, "Silo rejected the Silo API key; reconnect with a valid key"}
	case http.StatusForbidden:
		return response.StatusCode, &Error{PermissionDenied, "Silo denied access to the selected profile"}
	case http.StatusTooManyRequests:
		return response.StatusCode, &Error{RateLimited, "Silo's request limit was reached; try again"}
	default:
		if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
			return response.StatusCode, &Error{Temporary, "Silo is temporarily unavailable"}
		}
		return response.StatusCode, &Error{Permanent, "Silo rejected the request; check the Silo server URL and profile"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes || json.Unmarshal(body, target) != nil {
		return response.StatusCode, &Error{Temporary, "Silo returned an unreadable response"}
	}
	return response.StatusCode, nil
}
