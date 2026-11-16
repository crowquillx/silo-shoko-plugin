// Package introdb looks up playback markers in TheIntroDB's v4 API
// (https://theintrodb.org). It only reads: the plugin never submits markers.
package introdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is TheIntroDB's v4 API.
const DefaultBaseURL = "https://api.theintrodb.org/v4"

const (
	userAgent       = "silo-shoko-plugin (+https://github.com/crowquillx/silo-shoko-plugin)"
	requestTimeout  = 15 * time.Second
	maxResponseBody = 1 << 20
	maxServerErrors = 2
	// TheIntroDB allows 30 lookups per 10 seconds without a key and 120 with
	// one. Spacing requests a little below that keeps a library sync from
	// running into its burst limit.
	anonymousInterval = 400 * time.Millisecond
	keyInterval       = 100 * time.Millisecond
	// A Cloudflare block is not about one title, so every lookup pauses.
	blockedCooldown = 5 * time.Minute
	// TheIntroDB reports the daily reset as 0 once the day's budget is spent.
	exhaustedCooldown = time.Hour
	maxRetryAfter     = 25 * time.Hour

	completeTTL   = 24 * time.Hour
	incompleteTTL = 15 * time.Minute
	maxCached     = 2048
)

// Target names a title the way TheIntroDB addresses it: exactly one of its
// TVDB, TMDB or IMDb IDs. For an episode, Season and Episode are numbered in
// that provider's order, which is why the caller must choose the provider
// whose numbering it has.
type Target struct {
	Param   string // "tvdb_id", "tmdb_id" or "imdb_id"
	ID      string
	Episode bool
	Season  int
	Number  int
}

// Segment is one occurrence of a marker kind. A nil Start means the segment
// starts with the file, a nil End that it runs to the end of the release.
type Segment struct {
	StartMs         *int64   `json:"start_ms"`
	EndMs           *int64   `json:"end_ms"`
	Confidence      *float64 `json:"confidence"`
	SubmissionCount *int     `json:"submission_count"`
}

// Media is TheIntroDB's answer for one title. GroupDurationMs is the length of
// the release the timestamps were taken from, when TheIntroDB knows it.
type Media struct {
	GroupDurationMs *int64    `json:"group_duration_ms"`
	Intro           []Segment `json:"intro"`
	Recap           []Segment `json:"recap"`
	Credits         []Segment `json:"credits"`
	Preview         []Segment `json:"preview"`
}

// RetryError asks the caller to stop looking anything up for RetryAfter.
type RetryError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *RetryError) Error() string { return e.Message }

// Client is safe for concurrent use. Lookups are spaced to stay under
// TheIntroDB's rate limit, cached, and shared between concurrent callers.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	now     func() time.Time

	paceMu   sync.Mutex
	nextSlot time.Time

	cacheMu  sync.Mutex
	cache    map[string]cacheEntry
	inFlight map[string]*flight
	blocked  time.Time
}

type cacheEntry struct {
	media   *Media
	expires time.Time
}

type flight struct {
	done  chan struct{}
	media *Media
	err   error
}

// New returns a client for baseURL, or DefaultBaseURL when it is empty. An
// empty apiKey looks markers up anonymously, which TheIntroDB allows with a
// smaller daily budget.
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   strings.TrimSpace(apiKey),
		http:     httpClient,
		now:      time.Now,
		cache:    make(map[string]cacheEntry),
		inFlight: make(map[string]*flight),
	}
}

// APIKey returns the key the client was built with.
func (c *Client) APIKey() string { return c.apiKey }

// Lookup returns TheIntroDB's markers for target, or nil when TheIntroDB has
// none. durationMs, when positive, lets TheIntroDB pick the release closest to
// the file's length.
func (c *Client) Lookup(ctx context.Context, target Target, durationMs int64) (*Media, error) {
	query := url.Values{}
	query.Set(target.Param, target.ID)
	if target.Episode {
		query.Set("season", strconv.Itoa(target.Season))
		query.Set("episode", strconv.Itoa(target.Number))
	}
	if durationMs > 0 {
		query.Set("duration_ms", strconv.FormatInt(durationMs, 10))
	}
	query.Set("details", "true")
	key := query.Encode()

	c.cacheMu.Lock()
	if wait := c.blocked.Sub(c.now()); wait > 0 {
		c.cacheMu.Unlock()
		return nil, &RetryError{RetryAfter: wait, Message: fmt.Sprintf("introdb: lookups paused for %s", wait.Round(time.Second))}
	}
	if entry, ok := c.cache[key]; ok && c.now().Before(entry.expires) {
		c.cacheMu.Unlock()
		return entry.media, nil
	}
	if current, ok := c.inFlight[key]; ok {
		c.cacheMu.Unlock()
		select {
		case <-current.done:
			return current.media, current.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	current := &flight{done: make(chan struct{})}
	c.inFlight[key] = current
	c.cacheMu.Unlock()

	// The shared request must not end because the first caller gave up.
	go func() {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout*2)
		defer cancel()
		media, err := c.fetch(fetchCtx, c.baseURL+"/media?"+key)
		c.cacheMu.Lock()
		delete(c.inFlight, key)
		if err == nil {
			c.store(key, media)
		}
		var retry *RetryError
		if errors.As(err, &retry) && retry.RetryAfter > 0 {
			c.blocked = c.now().Add(retry.RetryAfter)
		}
		c.cacheMu.Unlock()
		current.media, current.err = media, err
		close(current.done)
	}()
	select {
	case <-current.done:
		return current.media, current.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// store caches a response, keeping one without both an intro and credits only
// briefly so newly contributed markers are found soon. The caller holds
// cacheMu.
func (c *Client) store(key string, media *Media) {
	if len(c.cache) >= maxCached {
		now := c.now()
		for k, entry := range c.cache {
			if !now.Before(entry.expires) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= maxCached {
			c.cache = make(map[string]cacheEntry)
		}
	}
	ttl := incompleteTTL
	if media != nil && len(media.Intro) > 0 && len(media.Credits) > 0 {
		ttl = completeTTL
	}
	c.cache[key] = cacheEntry{media: media, expires: c.now().Add(ttl)}
}

func (c *Client) fetch(ctx context.Context, target string) (*Media, error) {
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("introdb: create request: %w", err)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", userAgent)
		if c.apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		response, err := c.http.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("introdb: request failed: %w", stripURL(err))
		}
		media, retry, err := c.read(response)
		if !retry || attempt >= maxServerErrors {
			return media, err
		}
		select {
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// read decodes one response. retry reports a server error worth trying again.
func (c *Client) read(response *http.Response) (*Media, bool, error) {
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
		_ = response.Body.Close()
	}()
	jsonBody := isJSON(response.Header.Get("Content-Type"))
	switch {
	case response.StatusCode == http.StatusOK:
		var media Media
		if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBody)).Decode(&media); err != nil {
			return nil, false, fmt.Errorf("introdb: decode response: %w", err)
		}
		return &media, false, nil
	case response.StatusCode == http.StatusNotFound && jsonBody:
		// TheIntroDB answers a title it has nothing for with a JSON 404. Any
		// other 404 means the endpoint moved, which must not read as no markers.
		return nil, false, nil
	case response.StatusCode == http.StatusTooManyRequests:
		after := retryAfter(response.Header, c.now())
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, maxResponseBody)).Decode(&body)
		if body.Code == "specific_media_usage_limit_exceeded" {
			// Only this title is limited for the day; other lookups go on.
			return nil, false, fmt.Errorf("introdb: daily lookup limit for this title reached; retry after %s", after)
		}
		return nil, false, &RetryError{RetryAfter: after, Message: fmt.Sprintf("introdb: rate or usage limit reached; retry after %s", after)}
	case response.StatusCode == http.StatusForbidden && !jsonBody &&
		(response.Header.Get("CF-Ray") != "" || strings.Contains(strings.ToLower(response.Header.Get("Server")), "cloudflare")):
		return nil, false, &RetryError{RetryAfter: blockedCooldown, Message: fmt.Sprintf("introdb: Cloudflare blocked the request; retry after %s", blockedCooldown)}
	case response.StatusCode >= 500:
		return nil, true, fmt.Errorf("introdb: HTTP %d", response.StatusCode)
	default:
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, false, fmt.Errorf("introdb: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
}

// pace waits for the next request slot.
func (c *Client) pace(ctx context.Context) error {
	interval := anonymousInterval
	if c.apiKey != "" {
		interval = keyInterval
	}
	c.paceMu.Lock()
	now := c.now()
	slot := c.nextSlot
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot = slot.Add(interval)
	c.paceMu.Unlock()
	wait := slot.Sub(now)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfter reads how long TheIntroDB asks clients to wait, from Retry-After
// or, for a spent budget, its rate and usage headers.
func retryAfter(headers http.Header, now time.Time) time.Duration {
	after := seconds(headers.Get("Retry-After"))
	if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		after = max(after, date.Sub(now))
	}
	for _, prefix := range []string{"X-UsageLimit-", "X-RateLimit-"} {
		remaining, err := strconv.ParseInt(strings.TrimSpace(headers.Get(prefix+"Remaining")), 10, 64)
		if err == nil && remaining > 0 {
			continue
		}
		reset := seconds(headers.Get(prefix + "Reset"))
		if reset <= 0 && prefix == "X-UsageLimit-" && err == nil {
			reset = exhaustedCooldown
		}
		after = max(after, reset)
	}
	if after <= 0 {
		after = 10 * time.Second
	}
	return min(after, maxRetryAfter)
}

func seconds(value string) time.Duration {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return time.Duration(parsed) * time.Second
}

func isJSON(contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// stripURL drops the request URL from a transport error. Lookups carry no
// secret in the URL, but the error is stored by Silo per item and the URL adds
// nothing to it.
func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
