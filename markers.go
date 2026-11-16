package main

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-shoko-plugin/internal/introdb"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// markerAlgorithm names where the plugin's markers come from in Silo.
const markerAlgorithm = "introdb:v4:shoko"

// releaseTolerance is how far a release TheIntroDB timed may differ in length
// from the file before its credits and previews stop applying. Intros and
// recaps are timed from the start, so they apply either way.
const releaseTolerance = 5 * time.Minute

// TMDB episode lookups in Shoko are cached: found episodes for longer, since a
// TMDB episode's place in its show rarely changes, and misses briefly, so a
// newly linked show is picked up soon.
const (
	tmdbEpisodeTTL     = 12 * time.Hour
	tmdbEpisodeMissTTL = time.Hour
	maxTMDBEpisodes    = 20000
)

var imdbIDPattern = regexp.MustCompile(`^tt[0-9]{7,8}$`)

// markerServer looks markers up in TheIntroDB. Silo numbers a Shoko VFS
// episode by its place in the Shoko group, which is not TMDB's or TVDB's
// numbering, so an episode Shoko knows by its TMDB ID is looked up by that
// TMDB episode's own show, season and number. Other episodes are looked up as
// Silo numbers them, but only when Silo says that numbering is TVDB's or
// TMDB's.
type markerServer struct {
	pluginv1.UnimplementedMarkerProviderServer
	runtime *runtimeServer

	mu       sync.Mutex
	episodes map[tmdbEpisodeKey]tmdbEpisodeEntry
	now      func() time.Time
}

type tmdbEpisodeKey struct {
	client *shoko.Client
	id     int
}

type tmdbEpisodeEntry struct {
	episode shoko.TMDBEpisode
	found   bool
	expires time.Time
}

func (s *markerServer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *markerServer) FetchMarkers(ctx context.Context, req *pluginv1.FetchMarkersRequest) (*pluginv1.FetchMarkersResponse, error) {
	cfg, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	target, ok, err := s.target(ctx, client, req)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pluginv1.FetchMarkersResponse{}, nil
	}
	duration := secondsToDuration(req.GetDurationSeconds())
	media, err := s.runtime.introDB(cfg.IntroDBAPIKey).Lookup(ctx, target, duration.Milliseconds())
	if err != nil {
		return nil, markerError(err)
	}
	return &pluginv1.FetchMarkersResponse{Markers: markerSegments(media, duration)}, nil
}

// SubmitMarker is never called: the manifest declares the provider fetch-only.
func (s *markerServer) SubmitMarker(context.Context, *pluginv1.SubmitMarkerRequest) (*pluginv1.SubmitMarkerResponse, error) {
	return nil, status.Error(codes.Unimplemented, "shokoanime: markers are not shared with TheIntroDB")
}

func (s *markerServer) GetMarkerProviderStats(context.Context, *pluginv1.GetMarkerProviderStatsRequest) (*pluginv1.MarkerProviderStatsResponse, error) {
	return &pluginv1.MarkerProviderStatsResponse{}, nil
}

// target chooses how to ask TheIntroDB for the item. ok is false when the item
// cannot be addressed without risking another episode's markers.
func (s *markerServer) target(ctx context.Context, client *shoko.Client, req *pluginv1.FetchMarkersRequest) (introdb.Target, bool, error) {
	series := req.GetExternalIds()
	switch strings.ToLower(strings.TrimSpace(req.GetItemType())) {
	case "movie":
		// TheIntroDB keys titles by TVDB and converts the other IDs itself.
		for _, candidate := range []struct{ param, id string }{
			{"tvdb_id", numericID(series.GetTvdbId())},
			{"tmdb_id", numericID(series.GetTmdbId())},
			{"imdb_id", imdbID(series.GetImdbId())},
		} {
			if candidate.id != "" {
				return introdb.Target{Param: candidate.param, ID: candidate.id}, true, nil
			}
		}
		return introdb.Target{}, false, nil
	case "episode":
	default:
		return introdb.Target{}, false, nil
	}

	numbering := strings.ToLower(strings.TrimSpace(req.GetEpisodeNumbering()))
	siloNumbersByProvider := numbering == "tvdb" || numbering == "tmdb"
	if id, err := strconv.Atoi(numericID(req.GetEpisodeExternalIds().GetTmdbId())); err == nil && id > 0 {
		episode, found, err := s.tmdbEpisode(ctx, client, id)
		// Silo's own numbers are right when they are TVDB's or TMDB's, so a
		// Shoko outage only holds back episodes that need Shoko's numbering.
		if err != nil && !siloNumbersByProvider {
			return introdb.Target{}, false, err
		}
		if err == nil && found {
			return episodeTarget("tmdb_id", strconv.Itoa(episode.ShowID), episode.SeasonNumber, episode.EpisodeNumber)
		}
	}
	season, number := int(req.GetSeasonNumber()), int(req.GetEpisodeNumber())
	switch numbering {
	case "tvdb":
		return episodeTarget("tvdb_id", numericID(series.GetTvdbId()), season, number)
	case "tmdb":
		return episodeTarget("tmdb_id", numericID(series.GetTmdbId()), season, number)
	}
	// Silo's numbers follow neither provider, as in a Shoko VFS library, so
	// they would name some other episode.
	return introdb.Target{}, false, nil
}

func episodeTarget(param, id string, season, number int) (introdb.Target, bool, error) {
	if id == "" || season < 0 || season > 1000 || number < 1 || number > 10000 {
		return introdb.Target{}, false, nil
	}
	return introdb.Target{Param: param, ID: id, Episode: true, Season: season, Number: number}, true, nil
}

// tmdbEpisode returns where a TMDB episode sits in its show, from Shoko.
func (s *markerServer) tmdbEpisode(ctx context.Context, client *shoko.Client, id int) (shoko.TMDBEpisode, bool, error) {
	key := tmdbEpisodeKey{client: client, id: id}
	s.mu.Lock()
	if entry, ok := s.episodes[key]; ok && s.clock().Before(entry.expires) {
		s.mu.Unlock()
		return entry.episode, entry.found, nil
	}
	s.mu.Unlock()

	episode, found, err := client.TMDBEpisode(ctx, id)
	if err != nil {
		return shoko.TMDBEpisode{}, false, err
	}
	ttl := tmdbEpisodeMissTTL
	if found {
		ttl = tmdbEpisodeTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.episodes == nil || len(s.episodes) >= maxTMDBEpisodes {
		next := make(map[tmdbEpisodeKey]tmdbEpisodeEntry)
		for k, entry := range s.episodes {
			if now.Before(entry.expires) && k.client == client && len(next) < maxTMDBEpisodes/2 {
				next[k] = entry
			}
		}
		s.episodes = next
	}
	s.episodes[key] = tmdbEpisodeEntry{episode: episode, found: found, expires: now.Add(ttl)}
	return episode, found, nil
}

// markerSegments converts TheIntroDB's answer for a file of the given
// duration. Credits and previews are kept only when TheIntroDB timed them on a
// release about as long as the file, since they sit near its end.
func markerSegments(media *introdb.Media, duration time.Duration) []*pluginv1.MarkerSegment {
	if media == nil {
		return nil
	}
	endsApply := true
	if media.GroupDurationMs != nil && *media.GroupDurationMs > 0 && duration > 0 {
		diff := time.Duration(*media.GroupDurationMs)*time.Millisecond - duration
		endsApply = diff.Abs() <= releaseTolerance
	}
	var out []*pluginv1.MarkerSegment
	out = append(out, convertSegments("intro", media.Intro, duration)...)
	out = append(out, convertSegments("recap", media.Recap, duration)...)
	if endsApply {
		out = append(out, convertSegments("credits", media.Credits, duration)...)
		out = append(out, convertSegments("preview", media.Preview, duration)...)
	}
	return out
}

func convertSegments(kind string, segments []introdb.Segment, duration time.Duration) []*pluginv1.MarkerSegment {
	fromStart := kind == "intro" || kind == "recap"
	var out []*pluginv1.MarkerSegment
	for _, segment := range segments {
		if !validMs(segment.StartMs) || !validMs(segment.EndMs) {
			continue
		}
		var start, end time.Duration
		if segment.StartMs != nil {
			start = time.Duration(*segment.StartMs) * time.Millisecond
		}
		if fromStart {
			// An intro or recap needs an end; it may start with the file.
			if segment.EndMs == nil {
				continue
			}
		} else if segment.StartMs == nil || start == 0 {
			// A credits or preview segment starting at zero is TheIntroDB's
			// way of saying there is none.
			continue
		}
		end = duration
		if segment.EndMs != nil {
			end = time.Duration(*segment.EndMs) * time.Millisecond
		}
		// A segment that runs to the end of a slightly longer release still
		// ends with this file. Silo stores whole-second durations.
		if !fromStart && duration > 0 && start < duration && end > duration && end-duration <= releaseTolerance {
			end = duration
		}
		if end <= start || (duration > 0 && (start >= duration || end > duration)) {
			continue
		}
		startSeconds := start.Seconds()
		marker := &pluginv1.MarkerSegment{Segment: kind, StartSeconds: &startSeconds, Algorithm: markerAlgorithm}
		if segment.EndMs != nil || duration > 0 {
			endSeconds := end.Seconds()
			marker.EndSeconds = &endSeconds
		}
		if segment.Confidence != nil && *segment.Confidence >= 0 && *segment.Confidence <= 1 {
			marker.Confidence = *segment.Confidence
		}
		if segment.SubmissionCount != nil && *segment.SubmissionCount > 0 {
			marker.SubmissionCount = int32(min(*segment.SubmissionCount, math.MaxInt32))
		}
		out = append(out, marker)
	}
	return out
}

func validMs(value *int64) bool {
	return value == nil || (*value >= 0 && *value <= int64(math.MaxInt64/int64(time.Millisecond)))
}

func secondsToDuration(seconds float64) time.Duration {
	if math.IsNaN(seconds) || seconds <= 0 || seconds > 24*60*60 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func numericID(value string) string {
	value = strings.TrimSpace(value)
	if id, err := strconv.ParseInt(value, 10, 64); err != nil || id < 1 {
		return ""
	}
	return value
}

func imdbID(value string) string {
	value = strings.TrimSpace(value)
	if !imdbIDPattern.MatchString(value) {
		return ""
	}
	return value
}

// markerError tells Silo to pause the provider when TheIntroDB asks for it.
func markerError(err error) error {
	var retry *introdb.RetryError
	if !errors.As(err, &retry) {
		return err
	}
	st := status.New(codes.ResourceExhausted, retry.Error())
	if retry.RetryAfter <= 0 {
		return st.Err()
	}
	if detailed, detailErr := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retry.RetryAfter)}); detailErr == nil {
		return detailed.Err()
	}
	return st.Err()
}

// introDB returns the TheIntroDB client for the configured API key, reusing it
// while the key is unchanged so its cache and pacing carry over.
func (s *runtimeServer) introDB(apiKey string) *introdb.Client {
	s.introMu.Lock()
	defer s.introMu.Unlock()
	if s.intro == nil || s.intro.APIKey() != strings.TrimSpace(apiKey) {
		s.intro = introdb.New(s.introBaseURL, apiKey, nil)
	}
	return s.intro
}
