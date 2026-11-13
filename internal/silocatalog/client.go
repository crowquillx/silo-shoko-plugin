// Package silocatalog reads one profile's view of a Silo catalog item: whether
// the profile has played it and which files back it. The watch-sync
// capability uses it to find the Shoko episode behind a finished play, since
// Silo's watch events carry only the Silo item ID. Its only write is Admin's
// request that Silo refresh an item.
package silocatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
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

// Error carries only fixed messages, apart from the account's own profile
// names and IDs, which ResolveProfile lists to help pick one. Silo response
// bodies, URLs and transport errors may contain credentials, so none of them
// reach the message.
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
		return &Error{InvalidConfig, "a Silo profile name or ID is required"}
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

// Profile is one profile of the Silo account an API key belongs to.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ResolveProfile finds the profile the config names, by ID or, ignoring
// case, by name, and checks that the API key can act as it. It returns the
// profile's ID and makes the client act as it. Silo's profile IDs are opaque
// UUIDs that its web app never shows, so people know their profile by name.
func (c *Client) ResolveProfile(ctx context.Context) (string, error) {
	if err := c.config.Validate(); err != nil {
		return "", err
	}
	// Listing without X-Profile-Id returns every profile of the account,
	// whatever the config names.
	profiles, err := c.profiles(ctx, false)
	if err != nil {
		return "", err
	}
	id, err := matchProfile(profiles, c.config.ProfileID)
	if err != nil {
		return "", err
	}
	c.config.ProfileID = id
	// Acting as the profile can still fail, such as for a PIN-locked one.
	if _, err := c.profiles(ctx, true); err != nil {
		return "", err
	}
	return id, nil
}

func (c *Client) profiles(ctx context.Context, asProfile bool) ([]Profile, error) {
	var response struct {
		Items []Profile `json:"items"`
		problem
	}
	status, err := c.request(ctx, "/api/v2/profiles", asProfile, &response)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// Silo answers an unknown X-Profile-Id with its own not_found
		// problem; any other 404 means the URL does not lead to Silo's API.
		if asProfile && response.isNotFound() && strings.Contains(strings.ToLower(response.Detail), "profile") {
			return nil, &Error{InvalidConfig, "the Silo profile was not found for this API key's account; check the profile"}
		}
		return nil, &Error{Permanent, "Silo's profile list was not found; check the Silo server URL"}
	}
	return response.Items, nil
}

// matchProfile returns the ID of the profile that wanted names: an exact ID
// match wins, then a single profile whose name matches ignoring case.
func matchProfile(profiles []Profile, wanted string) (string, error) {
	for _, profile := range profiles {
		if profile.ID == wanted {
			return profile.ID, nil
		}
	}
	var named []Profile
	for _, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(profile.Name), wanted) {
			named = append(named, profile)
		}
	}
	switch len(named) {
	case 1:
		return named[0].ID, nil
	case 0:
		if len(profiles) == 0 {
			return "", &Error{InvalidConfig, "this Silo API key's account has no profiles"}
		}
		names := make([]string, 0, len(profiles))
		for _, profile := range profiles {
			names = append(names, displayName(profile.Name))
		}
		return "", &Error{InvalidConfig, fmt.Sprintf("no profile %q on this Silo API key's account; its profiles are: %s", displayName(wanted), listed(names))}
	default:
		entries := make([]string, 0, len(named))
		for _, profile := range named {
			entries = append(entries, displayName(profile.Name)+" ("+displayName(profile.ID)+")")
		}
		return "", &Error{InvalidConfig, fmt.Sprintf("several profiles are named %q; enter the ID of the one you are connecting: %s", displayName(wanted), listed(entries))}
	}
}

// maxListedProfiles bounds how many profiles an error message names.
const maxListedProfiles = 12

func listed(entries []string) string {
	if len(entries) > maxListedProfiles {
		return strings.Join(entries[:maxListedProfiles], ", ") + fmt.Sprintf(" and %d more", len(entries)-maxListedProfiles)
	}
	return strings.Join(entries, ", ")
}

// displayName makes a profile name or ID safe to show in a one-line message.
func displayName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	if runes := []rune(name); len(runes) > 64 {
		name = string(runes[:64]) + "…"
	}
	if name == "" {
		return "(unnamed)"
	}
	return name
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
	return c.request(ctx, apiPath, true, target)
}

// request sends a GET, acting as the profile when asProfile is set.
func (c *Client) request(ctx context.Context, apiPath string, asProfile bool, target any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.BaseURL+apiPath, nil)
	if err != nil {
		return 0, &Error{InvalidConfig, "the Silo server URL is invalid"}
	}
	request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	if asProfile {
		request.Header.Set("X-Profile-Id", c.config.ProfileID)
	}
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
