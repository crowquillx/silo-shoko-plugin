package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/crowquillx/silo-shoko-plugin/internal/config"
	"github.com/crowquillx/silo-shoko-plugin/internal/identity"
	"github.com/crowquillx/silo-shoko-plugin/internal/imagestore"
	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	mu         sync.RWMutex
	manifest   *pluginv1.PluginManifest
	cfg        config.Connection
	client     *shoko.Client
	reconciler *vfs.Reconciler
	engine     *reconcile.Engine

	crawlMu       sync.Mutex
	crawlCancel   context.CancelFunc
	crawlErr      error
	verifiedUntil time.Time
	// images holds the S3 copies of Shoko images in the S3 image mode.
	images *imagestore.Store

	// Show posters from the Season Posters Shoko plugin, fetched at most once
	// per showPosterTTL for the current client.
	postersMu      sync.Mutex
	posters        map[int]string
	postersClient  *shoko.Client
	postersFetched time.Time
}

// showPosterTTL bounds how long a show poster chosen with the Season Posters
// Shoko plugin can be stale, and how often a server without that plugin is
// asked again.
const showPosterTTL = 10 * time.Minute

// showPoster returns the opaque path of the poster chosen for a group with the
// Season Posters Shoko plugin, or "" when there is none. A failed lookup is
// logged and treated as none, so show metadata never depends on that plugin.
func (s *runtimeServer) showPoster(ctx context.Context, client *shoko.Client, groupID int) string {
	s.postersMu.Lock()
	defer s.postersMu.Unlock()
	now := time.Now()
	if s.postersClient != client || now.Sub(s.postersFetched) >= showPosterTTL {
		lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		posters, err := client.ShowPosters(lookupCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ""
			}
			fmt.Fprintf(os.Stderr, "shokoanime: using group posters, Season Posters lookup failed: %v\n", err)
			posters = nil
		}
		s.posters, s.postersClient, s.postersFetched = posters, client, now
	}
	imageID := s.posters[groupID]
	if !shokoImageUUIDPattern.MatchString(imageID) {
		return ""
	}
	return shokoImageScheme + imageID
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	cfg, err := config.Decode(req.GetConfig())
	if err != nil {
		return nil, err
	}
	client, err := shoko.NewClient(shoko.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	if err != nil {
		return nil, err
	}
	sourceRoots := make([]string, 0, len(cfg.ManagedFolderMap))
	for _, root := range cfg.ManagedFolderMap {
		sourceRoots = append(sourceRoots, root)
	}
	sort.Strings(sourceRoots)
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: cfg.VFSRoot, AllowedSourceRoots: sourceRoots})
	if err != nil {
		return nil, err
	}
	engine, err := reconcile.New(reconcile.Config{
		Client:           client,
		Reconciler:       reconciler,
		ManagedFolderMap: cfg.ManagedFolderMap,
	})
	if err != nil {
		return nil, err
	}
	var images *imagestore.Store
	if cfg.ImageMode == config.ImageS3 {
		images, err = imagestore.New(imagestore.Config{
			Endpoint:        cfg.ImageS3.Endpoint,
			Region:          cfg.ImageS3.Region,
			Bucket:          cfg.ImageS3.Bucket,
			Prefix:          cfg.ImageS3.Prefix,
			AccessKeyID:     cfg.ImageS3.AccessKeyID,
			SecretAccessKey: cfg.ImageS3.SecretAccessKey,
		}, client.Image)
		if err != nil {
			return nil, err
		}
	}
	s.cancelBackgroundCrawl()
	s.mu.Lock()
	s.cfg = cfg
	s.client = client
	s.reconciler = reconciler
	s.engine = engine
	s.images = images
	s.verifiedUntil = time.Time{}
	s.mu.Unlock()
	return &pluginv1.ConfigureResponse{}, nil
}

// Silo gives a scheduled task about ten seconds, far less than a crawl of a real
// library needs, so a run that cannot finish in time keeps going here. Steps
// are bounded so the VFS lock is released regularly for scan-source polls.
const (
	scheduledStepBudget  = 7 * time.Second
	backgroundStepBudget = 20 * time.Second
)

// startBackgroundCrawl finishes a crawl after the scheduled task has returned.
// It does nothing if a crawl is already running.
func (s *runtimeServer) startBackgroundCrawl(engine *reconcile.Engine) {
	s.crawlMu.Lock()
	defer s.crawlMu.Unlock()
	if s.crawlCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.crawlCancel = cancel
	s.crawlErr = nil
	go func() {
		err := runCrawlToCompletion(ctx, engine)
		s.crawlMu.Lock()
		defer s.crawlMu.Unlock()
		// A canceled context means a reconfigure, and a newer engine owns the VFS.
		if ctx.Err() == nil {
			s.crawlCancel = nil
			s.crawlErr = err
		}
		cancel()
	}()
}

func runCrawlToCompletion(ctx context.Context, engine *reconcile.Engine) error {
	for {
		outcome, err := engine.Step(ctx, backgroundStepBudget)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "shokoanime: background crawl failed: %v\n", err)
			}
			return err
		}
		if outcome.Complete {
			logReconcileFailures(outcome.Result)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func logReconcileFailures(result vfs.Result) {
	for _, failure := range result.Failures {
		fmt.Fprintf(os.Stderr, "shokoanime: could not link %q: %s\n", failure.LogicalPath, failure.Reason)
	}
	if result.Themes != nil {
		logThemeFailures(*result.Themes)
	}
}

func logThemeFailures(result vfs.ThemeResult) {
	for _, failure := range result.Failures {
		fmt.Fprintf(os.Stderr, "shokoanime: could not sync theme %q: %s\n", failure.LogicalPath, failure.Reason)
	}
}

func (s *runtimeServer) cancelBackgroundCrawl() {
	s.crawlMu.Lock()
	defer s.crawlMu.Unlock()
	if s.crawlCancel != nil {
		s.crawlCancel()
		s.crawlCancel = nil
	}
	s.crawlErr = nil
}

func (s *runtimeServer) backgroundCrawlRunning() bool {
	s.crawlMu.Lock()
	defer s.crawlMu.Unlock()
	return s.crawlCancel != nil
}

// takeBackgroundCrawlError returns a background failure once, so the next
// scheduled run reports it instead of succeeding silently.
func (s *runtimeServer) takeBackgroundCrawlError() error {
	s.crawlMu.Lock()
	defer s.crawlMu.Unlock()
	err := s.crawlErr
	s.crawlErr = nil
	return err
}

// connectionVerifyInterval limits how often title-only searches reach Shoko.
const connectionVerifyInterval = 5 * time.Minute

// verifyConnection checks everything the test connection button can: that the
// URL reaches Shoko, the API key works, and the path map matches the server.
func (s *runtimeServer) verifyConnection(ctx context.Context) error {
	cfg, client, _, err := s.state()
	if err != nil {
		return err
	}
	s.mu.RLock()
	fresh := time.Now().Before(s.verifiedUntil)
	s.mu.RUnlock()
	if fresh {
		return nil
	}
	folders, err := client.ManagedFolders(ctx)
	if err != nil {
		var httpErr *shoko.HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("shokoanime: Shoko rejected the API key (HTTP %d)", httpErr.StatusCode)
		}
		return err
	}
	known := make(map[int]struct{}, len(folders))
	available := make([]string, 0, len(folders))
	for _, folder := range folders {
		known[folder.ID] = struct{}{}
		available = append(available, fmt.Sprintf("%d (%s)", folder.ID, folder.Name))
	}
	for id, root := range cfg.ManagedFolderMap {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("shokoanime: Shoko has no folder %d; available folders: %s", id, strings.Join(available, ", "))
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return fmt.Errorf("shokoanime: mapped path %q for folder %d is not a directory visible to Silo", root, id)
		}
	}
	s.mu.RLock()
	images := s.images
	s.mu.RUnlock()
	if images != nil {
		if err := images.Check(ctx); err != nil {
			return fmt.Errorf("shokoanime: %w", err)
		}
	}
	s.mu.Lock()
	s.verifiedUntil = time.Now().Add(connectionVerifyInterval)
	s.mu.Unlock()
	return nil
}

func (s *runtimeServer) state() (config.Connection, *shoko.Client, *vfs.Reconciler, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.reconciler == nil {
		return config.Connection{}, nil, nil, fmt.Errorf("shokoanime: plugin is not configured")
	}
	return s.cfg, s.client, s.reconciler, nil
}

func (s *runtimeServer) reconcilerState() (*vfs.Reconciler, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.reconciler == nil {
		return nil, fmt.Errorf("shokoanime: plugin is not configured")
	}
	return s.reconciler, nil
}

func (s *runtimeServer) engineState() (*reconcile.Engine, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.engine == nil {
		return nil, fmt.Errorf("shokoanime: plugin is not configured")
	}
	return s.engine, nil
}

func (s *runtimeServer) plan(ctx context.Context) (topology.Plan, *vfs.Reconciler, error) {
	cfg, client, reconciler, err := s.state()
	if err != nil {
		return topology.Plan{}, nil, err
	}
	folderIDs := make([]int, 0, len(cfg.ManagedFolderMap))
	for id := range cfg.ManagedFolderMap {
		folderIDs = append(folderIDs, id)
	}
	sort.Ints(folderIDs)
	snapshot, err := client.Snapshot(ctx, folderIDs)
	if err != nil {
		return topology.Plan{}, nil, err
	}
	plan, err := topology.Build(snapshot, topology.Policy{
		Mode:             topology.ModeAniDB,
		VFSRoot:          cfg.VFSRoot,
		ManagedFolderMap: cfg.ManagedFolderMap,
	})
	if err != nil {
		return topology.Plan{}, nil, err
	}
	return plan, reconciler, nil
}

type metadataServer struct {
	pluginv1.UnimplementedMetadataProviderServer
	pluginv1.UnimplementedImageResolverServer
	runtime *runtimeServer
	// now overrides the clock in tests.
	now func() time.Time
}

func (s *metadataServer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *metadataServer) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	token := requestToken(providerIDs, providerIDs["_filepath"], req.GetQuery())
	if !token.Valid() {
		// Silo's connection test is a title search with no file path or IDs. Scans
		// always send a file path, so they never wait on Shoko for non-VFS files.
		if len(providerIDs) == 0 {
			if err := s.runtime.verifyConnection(ctx); err != nil {
				return nil, err
			}
		}
		return &pluginv1.SearchMetadataResponse{}, nil
	}

	itemType := req.GetItemType()
	if itemType == "episode" && token.EpisodeID > 0 {
		episode, err := client.Episode(ctx, token.EpisodeID)
		if err != nil {
			return nil, err
		}
		return &pluginv1.SearchMetadataResponse{Results: []*pluginv1.ProviderSearchResult{episodeSearchResult(episode, itemType)}}, nil
	}
	if token.GroupID > 0 {
		group, err := client.Group(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		year, err := groupYear(ctx, client, group, token.SeriesID)
		if err != nil {
			return nil, err
		}
		return &pluginv1.SearchMetadataResponse{Results: []*pluginv1.ProviderSearchResult{groupSearchResult(group, itemType, year)}}, nil
	}
	seriesID := token.SeriesID
	if seriesID == 0 && token.EpisodeID > 0 {
		episode, err := client.Episode(ctx, token.EpisodeID)
		if err != nil {
			return nil, err
		}
		seriesID = episode.IDs.ParentSeries
	}
	if seriesID == 0 {
		return &pluginv1.SearchMetadataResponse{}, nil
	}
	series, err := client.Series(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	return &pluginv1.SearchMetadataResponse{Results: []*pluginv1.ProviderSearchResult{seriesSearchResult(series, itemType)}}, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	token := requestToken(providerIDs, req.GetProviderId(), req.GetFilePath(), providerIDs["_filepath"])
	if req.GetItemType() == "episode" {
		episodeID := token.EpisodeID
		if episodeID == 0 {
			episodeID = positiveID(providerIDs["shoko_episode"])
		}
		if episodeID == 0 {
			episodeID = typedPrimaryID(req.GetProviderId(), "episode")
		}
		if episodeID == 0 {
			return &pluginv1.GetMetadataResponse{}, nil
		}
		episode, err := client.Episode(ctx, episodeID)
		if err != nil {
			return nil, err
		}
		return &pluginv1.GetMetadataResponse{Item: episodeMetadata(episode, req.GetItemType())}, nil
	}
	if token.GroupID > 0 {
		group, err := client.Group(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		members, err := client.GroupSeries(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		year, err := yearFromMembers(ctx, client, group, members, token.SeriesID)
		if err != nil {
			return nil, err
		}
		item := groupMetadata(group, req.GetItemType(), year)
		if poster := s.runtime.showPoster(ctx, client, group.IDs.ID); poster != "" {
			item.PosterPath = poster
		}
		if err := addShowDetails(ctx, client, item, members, group.IDs.MainSeries, s.clock()); err != nil {
			return nil, err
		}
		setTitleLanguage(item, req.GetLanguage(), mainSeries(members, group.IDs.MainSeries))
		return &pluginv1.GetMetadataResponse{Item: item}, nil
	}
	seriesID := token.SeriesID
	if seriesID == 0 {
		seriesID = positiveID(providerIDs["shoko_series"])
	}
	if seriesID == 0 {
		seriesID = typedPrimaryID(req.GetProviderId(), "series")
	}
	if seriesID == 0 && token.EpisodeID > 0 {
		episode, err := client.Episode(ctx, token.EpisodeID)
		if err != nil {
			return nil, err
		}
		seriesID = episode.IDs.ParentSeries
	}
	if seriesID == 0 {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	series, err := client.Series(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	item := seriesMetadata(series, req.GetItemType())
	if err := addShowDetails(ctx, client, item, []shoko.Series{series}, series.IDs.ID, s.clock()); err != nil {
		return nil, err
	}
	setTitleLanguage(item, req.GetLanguage(), series)
	return &pluginv1.GetMetadataResponse{Item: item}, nil
}

func (s *metadataServer) GetSeasons(ctx context.Context, req *pluginv1.GetSeasonsRequest) (*pluginv1.GetSeasonsResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	token := requestToken(providerIDs, req.GetSeriesProviderId())
	if token.GroupID > 0 {
		group, err := client.Group(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		members, err := client.GroupSeries(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		episodesBySeries, err := groupEpisodes(ctx, client.Episodes, members)
		if err != nil {
			return nil, err
		}
		return groupSeasons(group, members, episodesBySeries), nil
	}
	seriesID := token.SeriesID
	if seriesID == 0 {
		seriesID = positiveID(providerIDs["shoko_series"])
	}
	if seriesID == 0 {
		return &pluginv1.GetSeasonsResponse{}, nil
	}
	series, err := client.Series(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	episodes, err := client.Episodes(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	seasons := make(map[int]struct{})
	for _, episode := range episodes {
		season, _ := topology.EpisodeNumbers(episode)
		seasons[season] = struct{}{}
	}
	numbers := make([]int, 0, len(seasons))
	for number := range seasons {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	response := &pluginv1.GetSeasonsResponse{Seasons: make([]*pluginv1.SeasonRecord, 0, len(numbers))}
	for _, number := range numbers {
		seasonIDs := map[string]string{
			"shoko_series": strconv.Itoa(seriesID),
			"shoko_season": strconv.Itoa(number),
		}
		if series.IDs.AniDB > 0 {
			seasonIDs["anidb"] = strconv.Itoa(series.IDs.AniDB)
		}
		record := &pluginv1.SeasonRecord{
			SeasonNumber: int32(number),
			Title:        fmt.Sprintf("Season %02d", number),
			PosterPath:   selectedPosterPath(series.Images),
			ProviderId:   fmt.Sprintf("series:%d:season:%d", seriesID, number),
			ProviderIds:  stringStruct(seasonIDs),
		}
		if number > 0 {
			record.Overview = sanitizeDescription(series.Description)
			record.AirDate = seriesAirDate(series)
		}
		response.Seasons = append(response.Seasons, record)
	}
	return response, nil
}

func (s *metadataServer) GetEpisodes(ctx context.Context, req *pluginv1.GetEpisodesRequest) (*pluginv1.GetEpisodesResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	token := requestToken(providerIDs, req.GetSeriesProviderId())
	if token.GroupID > 0 {
		members, err := client.GroupSeries(ctx, token.GroupID)
		if err != nil {
			return nil, err
		}
		episodesBySeries, err := groupEpisodes(ctx, client.MetadataEpisodes, members)
		if err != nil {
			return nil, err
		}
		layout := topology.NewGroupLayout(members)
		response := &pluginv1.GetEpisodesResponse{Episodes: make([]*pluginv1.EpisodeRecord, 0)}
		for _, series := range layout.OrderedMembers {
			for _, episode := range episodesBySeries[series.IDs.ID] {
				season, number := layout.Position(series, episode)
				if int(req.GetSeasonNumber()) != season {
					continue
				}
				response.Episodes = append(response.Episodes, episodeRecord(season, number, episode))
			}
		}
		return response, nil
	}
	seriesID := token.SeriesID
	if seriesID == 0 {
		seriesID = positiveID(providerIDs["shoko_series"])
	}
	if seriesID == 0 {
		return &pluginv1.GetEpisodesResponse{}, nil
	}
	episodes, err := client.MetadataEpisodes(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	response := &pluginv1.GetEpisodesResponse{Episodes: make([]*pluginv1.EpisodeRecord, 0)}
	for _, episode := range episodes {
		season, number := topology.EpisodeNumbers(episode)
		if int(req.GetSeasonNumber()) != season {
			continue
		}
		response.Episodes = append(response.Episodes, episodeRecord(season, number, episode))
	}
	return response, nil
}

func (s *metadataServer) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	groupID := positiveID(providerIDs["shoko_group"])
	seriesID := positiveID(providerIDs["shoko_series"])
	if groupID == 0 && seriesID == 0 {
		groupID = typedPrimaryID(req.GetProviderId(), "group")
		seriesID = typedPrimaryID(req.GetProviderId(), "series")
		if seriesID == 0 {
			seriesID = typedSeasonOwnerID(req.GetProviderId(), "series")
		}
		if groupID == 0 {
			groupID = typedSeasonOwnerID(req.GetProviderId(), "group")
		}
	}
	// Grouped seasons carry both the parent group and their member series ID.
	// The member series owns the season artwork, so it must win whenever both
	// IDs are present.
	if seriesID > 0 {
		series, err := client.Series(ctx, seriesID)
		if err != nil {
			return nil, err
		}
		return imageRecords(series.Images), nil
	}
	if groupID > 0 {
		group, err := client.Group(ctx, groupID)
		if err != nil {
			return nil, err
		}
		records := imageRecords(group.Images)
		// The specials season shares the group's images; only the show itself
		// takes a show poster chosen with Season Posters.
		if req.GetItemType() == "season" || strings.Contains(req.GetProviderId(), ":season:") {
			return records, nil
		}
		return withShowPoster(records, s.runtime.showPoster(ctx, client, groupID)), nil
	}
	return &pluginv1.GetImagesResponse{}, nil
}

// ResolveImageURL resolves one scheme-stripped image/<uuid> path to a URL Silo
// can download: Shoko's own, the public image URL, or a link to the S3 copy,
// depending on the image mode. Invalid paths resolve to an empty URL, and
// malformed input is never reflected into an error or URL.
func (s *metadataServer) ResolveImageURL(ctx context.Context, req *pluginv1.ResolveImageURLRequest) (*pluginv1.ResolveImageURLResponse, error) {
	cfg, _, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	return &pluginv1.ResolveImageURLResponse{Url: s.runtime.imageURL(ctx, cfg, req.GetPath())}, nil
}

// ResolveImageURLs resolves a batch of scheme-stripped image paths. Paths that
// do not strictly match image/<uuid> map to an empty URL.
func (s *metadataServer) ResolveImageURLs(ctx context.Context, req *pluginv1.ResolveImageURLsRequest) (*pluginv1.ResolveImageURLsResponse, error) {
	cfg, _, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	paths := req.GetPaths()
	resolved := make([]string, len(paths))
	// S3 copies are made on first use, so a batch is resolved a few at a time.
	limit := make(chan struct{}, imageCopyConcurrency)
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		limit <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-limit }()
			resolved[i] = s.runtime.imageURL(ctx, cfg, path)
		}()
	}
	wg.Wait()
	urls := make(map[string]string, len(paths))
	for i, path := range paths {
		urls[path] = resolved[i]
	}
	return &pluginv1.ResolveImageURLsResponse{Urls: urls}, nil
}

// imageCopyConcurrency bounds how many images one batch copies to S3 at once.
const imageCopyConcurrency = 4

// imageURL resolves one image path for the configured image mode. When the S3
// copy fails, Shoko's own URL is returned, so clients that can reach Shoko
// still show the image.
func (s *runtimeServer) imageURL(ctx context.Context, cfg config.Connection, barePath string) string {
	imageID, ok := shokoImageID(barePath)
	if !ok {
		return ""
	}
	if cfg.ImageMode == config.ImageS3 {
		s.mu.RLock()
		images := s.images
		s.mu.RUnlock()
		if images != nil {
			link, err := images.URL(ctx, imageID)
			if err == nil {
				return link
			}
			fmt.Fprintf(os.Stderr, "shokoanime: could not copy image %s to S3: %v\n", imageID, err)
		}
	}
	return strings.TrimRight(cfg.ImageURLBase(), "/") + shokoImageEndpointPrefix + imageID
}

// imageRecords converts an entity's image set into resolver-addressed image
// records. Only images that are available and not disabled are advertised;
// the rest cannot be served by Shoko. Record URLs use the opaque
// shoko://image/<uuid> scheme and are resolved by the image_resolver.v1
// capability.
func imageRecords(images shoko.Images) *pluginv1.GetImagesResponse {
	response := &pluginv1.GetImagesResponse{}
	add := func(kind string, candidates []shoko.Image) {
		for i := range candidates {
			image := &candidates[i]
			if !image.Available || image.Disabled {
				continue
			}
			url := imagePath(*image)
			if url == "" {
				continue
			}
			record := &pluginv1.ImageRecord{Kind: kind, Url: url}
			if image.LanguageCode != nil {
				record.Language = *image.LanguageCode
			}
			if image.Width != nil {
				record.Width = int32(*image.Width)
			}
			if image.Height != nil {
				record.Height = int32(*image.Height)
			}
			response.Images = append(response.Images, record)
		}
	}
	add("poster", images.Posters)
	add("backdrop", images.Backdrops)
	add("banner", images.Banners)
	add("logo", images.Logos)
	add("disc", images.Discs)
	return response
}

// withShowPoster puts a show poster chosen with the Season Posters Shoko plugin
// first among a group's images, without listing it twice.
func withShowPoster(response *pluginv1.GetImagesResponse, poster string) *pluginv1.GetImagesResponse {
	if poster == "" {
		return response
	}
	images := []*pluginv1.ImageRecord{{Kind: "poster", Url: poster}}
	for _, image := range response.GetImages() {
		if image.GetUrl() != poster {
			images = append(images, image)
		}
	}
	response.Images = images
	return response
}

type scheduledTaskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	runtime *runtimeServer
	// stepBudget overrides scheduledStepBudget in tests.
	stepBudget time.Duration
}

func (s *scheduledTaskServer) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	if !isReconcileTaskKey(req.GetTaskKey()) {
		return nil, fmt.Errorf("shokoanime: unsupported scheduled task %q", req.GetTaskKey())
	}
	dryRun := false
	if req.GetInput() != nil {
		if value, ok := req.GetInput().AsMap()["dry_run"].(bool); ok {
			dryRun = value
		}
	}
	if !dryRun {
		if err := s.runtime.takeBackgroundCrawlError(); err != nil {
			return nil, err
		}
	}
	var plan topology.Plan
	var result vfs.Result
	var err error
	complete := true
	phase := ""
	filesFetched, seriesFetched, episodesFetched := 0, 0, 0
	if dryRun {
		var reconciler *vfs.Reconciler
		plan, reconciler, err = s.runtime.plan(ctx)
		if err != nil {
			return nil, err
		}
		result, err = reconciler.Reconcile(ctx, plan, true)
		if err != nil {
			return nil, err
		}
	} else {
		engine, err := s.runtime.engineState()
		if err != nil {
			return nil, err
		}
		var outcome reconcile.Outcome
		if s.runtime.backgroundCrawlRunning() {
			phase = "background"
		} else {
			budget := s.stepBudget
			if budget == 0 {
				budget = scheduledStepBudget
			}
			outcome, err = engine.Step(ctx, budget)
			if err != nil {
				return nil, err
			}
			phase = outcome.Phase
		}
		complete = outcome.Complete
		filesFetched, seriesFetched, episodesFetched = outcome.FilesFetched, outcome.SeriesFetched, outcome.EpisodesFetched
		plan, result = outcome.Plan, outcome.Result
		if complete {
			logReconcileFailures(result)
		} else {
			s.runtime.startBackgroundCrawl(engine)
		}
	}
	output, err := taskOutput(dryRun, complete, phase, filesFetched, seriesFetched, episodesFetched, plan, result)
	if err != nil {
		return nil, err
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

const (
	maxTaskOutputActions  = 100
	maxTaskOutputFailures = 100
)

func themeOutput(themes *vfs.ThemeResult) map[string]any {
	if themes == nil {
		return nil
	}
	return map[string]any{
		"linked":        themes.Linked,
		"written_back":  themes.WrittenBack,
		"removed":       themes.Removed,
		"failure_count": len(themes.Failures),
	}
}

func taskMessage(dryRun, complete bool, filesFetched, entries int, result vfs.Result) string {
	switch {
	case !complete:
		return fmt.Sprintf("Still crawling Shoko in the background (%d files so far). Links are written when the crawl finishes.", filesFetched)
	case dryRun:
		return fmt.Sprintf("Dry run: %d links planned, nothing written.", entries)
	case len(result.Failures) > 0:
		return fmt.Sprintf("Reconciled %d links, but %d could not be changed; see failures.", entries, len(result.Failures))
	}
	return fmt.Sprintf("Reconciled %d links.", entries)
}

func taskOutput(dryRun, complete bool, phase string, filesFetched, seriesFetched, episodesFetched int, plan topology.Plan, result vfs.Result) (*structpb.Struct, error) {
	actionLimit := len(result.Actions)
	if actionLimit > maxTaskOutputActions {
		actionLimit = maxTaskOutputActions
	}
	actions := make([]any, 0, actionLimit)
	for _, action := range result.Actions[:actionLimit] {
		actions = append(actions, map[string]any{
			"kind":         action.Kind,
			"logical_path": action.LogicalPath,
			"target_path":  action.TargetPath,
			"reason":       action.Reason,
		})
	}
	failureLimit := min(len(result.Failures), maxTaskOutputFailures)
	failures := make([]any, 0, failureLimit)
	for _, failure := range result.Failures[:failureLimit] {
		failures = append(failures, map[string]any{
			"logical_path": failure.LogicalPath,
			"target_path":  failure.TargetPath,
			"reason":       failure.Reason,
		})
	}
	return structpb.NewStruct(map[string]any{
		"message":           taskMessage(dryRun, complete, filesFetched, len(plan.Entries), result),
		"dry_run":           dryRun,
		"complete":          complete,
		"queued":            !complete,
		"phase":             phase,
		"files_fetched":     filesFetched,
		"series_fetched":    seriesFetched,
		"episodes_fetched":  episodesFetched,
		"planned_entries":   len(plan.Entries),
		"action_count":      len(result.Actions),
		"actions_returned":  len(actions),
		"actions_truncated": len(result.Actions) - len(actions),
		"actions":           actions,
		"failure_count":     len(result.Failures),
		"failures":          failures,
		"diagnostic_count":  len(plan.Diagnostics),
		"themes":            themeOutput(result.Themes),
		"diagnostics":       diagnostics(plan.Diagnostics),
	})
}

// isReconcileTaskKey accepts both the capability-local key used by direct SDK
// callers and the installation-qualified key used by Silo's task registry.
// Silo invokes plugin tasks as plugin:<installation-id>:<capability-id>.
func isReconcileTaskKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "reconcile" {
		return true
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "plugin" || parts[2] != "reconcile" {
		return false
	}
	installationID, err := strconv.Atoi(parts[1])
	return err == nil && installationID > 0
}

// scanSourceServer exposes the VFS change journal through Silo's pull-based
// scan_source.v1 contract. Silo owns polling, marker persistence, and scan
// enqueueing; this capability completes any durable crawl left by a scheduled
// task and replays only paths committed by a successful non-dry-run reconcile.
type scanSourceServer struct {
	pluginv1.UnimplementedScanSourceServer
	runtime *runtimeServer
}

func (s *scanSourceServer) PollChanges(ctx context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error) {
	if s == nil || s.runtime == nil {
		return nil, fmt.Errorf("shokoanime: scan source is not configured")
	}
	reconciler, err := s.runtime.reconcilerState()
	if err != nil {
		return nil, err
	}
	marker := strings.TrimSpace(req.GetMarker())
	s.runtime.mu.RLock()
	engine := s.runtime.engine
	s.runtime.mu.RUnlock()
	// Crawl work never runs inside a poll. Writing a large library's links can
	// take longer than the host's poll deadline, and a poll that times out
	// leaves the source without a marker. A pending crawl is handed to the
	// background crawl instead.
	if engine != nil && !s.runtime.backgroundCrawlRunning() {
		pending, err := engine.Pending(ctx)
		if err != nil {
			return nil, err
		}
		if pending {
			s.runtime.startBackgroundCrawl(engine)
		}
	}
	// A running crawl holds the VFS lock for long stretches and journals nothing
	// until it commits, so answer straight away and keep the marker.
	if s.runtime.backgroundCrawlRunning() {
		return &pluginv1.PollChangesResponse{NextMarker: marker}, nil
	}
	// Pick up themes written into the VFS since the last poll, so they reach the
	// media folders without waiting for the next reconcile.
	if themes, err := reconciler.SyncThemes(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shokoanime: theme sync failed: %v\n", err)
	} else {
		logThemeFailures(themes)
	}
	if marker == "" {
		return firstPoll(ctx, reconciler)
	}
	paths, nextMarker, err := reconciler.PollChanges(ctx, marker)
	if err != nil {
		return nil, err
	}
	return &pluginv1.PollChangesResponse{
		SourcePaths: paths,
		NextMarker:  nextMarker,
	}, nil
}

// firstPoll starts the source at the journal's tail. If the VFS already has
// links, the host may never have scanned them: the library could have been
// created before the crawl finished, or an earlier first poll may have failed.
// The whole VFS is then scanned once; files the host already has are unchanged
// and cheap to scan.
//
// Silo scans the parent folder of each reported path and refuses a subtree
// change for a library root, so the journal file, which sits directly in the
// root, is reported to get a scan of the whole library.
func firstPoll(ctx context.Context, reconciler *vfs.Reconciler) (*pluginv1.PollChangesResponse, error) {
	marker, err := reconciler.CurrentMarker(ctx)
	if err != nil {
		return nil, err
	}
	response := &pluginv1.PollChangesResponse{NextMarker: marker}
	if marker != "0" {
		response.SourcePaths = []string{vfs.ChangeJournalPath(reconciler.Root())}
	}
	return response, nil
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable: %w", err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	if len(manifest.GetSupportedPlatforms()) == 0 {
		manifest.SupportedPlatforms = []*pluginv1.SupportedPlatform{{Os: goruntime.GOOS, Arch: goruntime.GOARCH}}
	}
	return manifest, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := runBootstrap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "shokoanime bootstrap: %v\n", err)
			os.Exit(1)
		}
		return
	}
	manifest, err := loadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shokoanime: %v\n", err)
		os.Exit(1)
	}
	rt := &runtimeServer{manifest: manifest}
	metadata := &metadataServer{runtime: rt}
	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:          rt,
			MetadataProvider: metadata,
			ImageResolver:    metadata,
			ScheduledTask:    &scheduledTaskServer{runtime: rt},
			ScanSource:       &scanSourceServer{runtime: rt},
		},
	})
}

// runBootstrap is a one-shot operator escape hatch for a first production
// crawl. It uses the same durable engine as Silo, performs only Shoko GETs,
// and writes only the configured VFS root. It is intentionally outside the
// plugin SDK serve path so it cannot be mistaken for a Silo task timeout.
func runBootstrap(args []string) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to a JSON connection config")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*configPath) == "" {
		return errors.New("--config is required")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.DecodeJSON(data)
	if err != nil {
		return err
	}
	client, err := shoko.NewClient(shoko.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	if err != nil {
		return err
	}
	sourceRoots := make([]string, 0, len(cfg.ManagedFolderMap))
	for _, root := range cfg.ManagedFolderMap {
		sourceRoots = append(sourceRoots, root)
	}
	sort.Strings(sourceRoots)
	reconciler, err := vfs.NewReconciler(vfs.Config{Root: cfg.VFSRoot, AllowedSourceRoots: sourceRoots})
	if err != nil {
		return err
	}
	engine, err := reconcile.New(reconcile.Config{Client: client, Reconciler: reconciler, ManagedFolderMap: cfg.ManagedFolderMap})
	if err != nil {
		return err
	}
	outcome, err := engine.Step(context.Background(), 0)
	if err != nil {
		return err
	}
	logReconcileFailures(outcome.Result)
	encoded, err := json.Marshal(map[string]any{
		"complete":         outcome.Complete,
		"phase":            outcome.Phase,
		"files_fetched":    outcome.FilesFetched,
		"series_fetched":   outcome.SeriesFetched,
		"episodes_fetched": outcome.EpisodesFetched,
		"action_count":     len(outcome.Result.Actions),
		"failure_count":    len(outcome.Result.Failures),
	})
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func stringMap(value *structpb.Struct) map[string]string {
	result := make(map[string]string)
	if value == nil {
		return result
	}
	for key, raw := range value.AsMap() {
		if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
			result[key] = strings.TrimSpace(text)
		}
	}
	return result
}

func stringStruct(values map[string]string) *structpb.Struct {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		if strings.TrimSpace(value) != "" {
			converted[key] = value
		}
	}
	if len(converted) == 0 {
		return nil
	}
	result, err := structpb.NewStruct(converted)
	if err != nil {
		return nil
	}
	return result
}

func positiveID(value string) int {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id < 1 {
		return 0
	}
	return id
}
func typedPrimaryID(value, kind string) int {
	value = strings.TrimSpace(value)
	prefix := kind + ":"
	if !strings.HasPrefix(value, prefix) {
		return 0
	}
	return positiveID(strings.TrimPrefix(value, prefix))
}

// typedSeasonOwnerID parses the season provider IDs emitted by GetSeasons.
// Keeping this separate from typedPrimaryID preserves the latter's strict
// rejection of arbitrary suffixes.
func typedSeasonOwnerID(value, kind string) int {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 4 || parts[0] != kind || parts[2] != "season" {
		return 0
	}
	ownerID := positiveID(parts[1])
	season, err := strconv.Atoi(parts[3])
	if ownerID == 0 || err != nil || season < 0 {
		return 0
	}
	return ownerID
}

func requestToken(providerIDs map[string]string, paths ...string) identity.Token {
	for _, path := range paths {
		if id := typedPrimaryID(path, "group"); id > 0 {
			return identity.Token{GroupID: id}
		}
		if id := typedPrimaryID(path, "series"); id > 0 {
			return identity.Token{SeriesID: id}
		}
		if id := typedPrimaryID(path, "episode"); id > 0 {
			return identity.Token{EpisodeID: id}
		}
		token := identity.Parse(path)
		if token.Valid() {
			return token
		}
	}
	return identity.Token{
		GroupID:   positiveID(providerIDs["shoko_group"]),
		SeriesID:  positiveID(providerIDs["shoko_series"]),
		EpisodeID: positiveID(providerIDs["shoko_episode"]),
		FileID:    positiveID(providerIDs["shoko_file"]),
	}
}

func completeAirDate(series shoko.Series) (time.Time, bool) {
	if series.AniDB == nil {
		return time.Time{}, false
	}
	date, err := time.Parse("2006-01-02", series.AniDB.AirDate)
	return date, err == nil
}

func groupYear(ctx context.Context, client *shoko.Client, group shoko.Group, currentSeriesID int) (int, error) {
	members, err := client.GroupSeries(ctx, group.IDs.ID)
	if err != nil {
		return 0, err
	}
	return yearFromMembers(ctx, client, group, members, currentSeriesID)
}

// yearFromMembers returns the main series' year, else the earliest member's,
// else the current series' year.
func yearFromMembers(ctx context.Context, client *shoko.Client, group shoko.Group, members []shoko.Series, currentSeriesID int) (int, error) {
	for _, member := range members {
		if member.IDs.ID == group.IDs.MainSeries {
			if year := member.AniDB.AirYear(); year > 0 {
				return year, nil
			}
		}
	}
	var earliest time.Time
	for _, member := range members {
		date, ok := completeAirDate(member)
		if ok && (earliest.IsZero() || date.Before(earliest)) {
			earliest = date
		}
	}
	if !earliest.IsZero() {
		return earliest.Year(), nil
	}
	if currentSeriesID > 0 {
		series, err := client.Series(ctx, currentSeriesID)
		if err != nil {
			return 0, err
		}
		return series.AniDB.AirYear(), nil
	}
	return 0, nil
}

func groupEpisodes(ctx context.Context, list func(context.Context, int) ([]shoko.Episode, error), members []shoko.Series) (map[int][]shoko.Episode, error) {
	episodesBySeries := make(map[int][]shoko.Episode, len(members))
	for _, series := range members {
		episodes, err := list(ctx, series.IDs.ID)
		if err != nil {
			return nil, err
		}
		episodesBySeries[series.IDs.ID] = episodes
	}
	return episodesBySeries, nil
}

func groupSeasons(group shoko.Group, members []shoko.Series, episodesBySeries map[int][]shoko.Episode) *pluginv1.GetSeasonsResponse {
	layout := topology.NewGroupLayout(members)
	response := &pluginv1.GetSeasonsResponse{Seasons: make([]*pluginv1.SeasonRecord, 0, len(layout.OrderedMembers)+1)}
	groupID := strconv.Itoa(group.IDs.ID)
	groupPoster := selectedPosterPath(group.Images)
	for _, series := range layout.OrderedMembers {
		season := layout.SeasonNumber(series.IDs.ID)
		ids := map[string]string{"shoko_group": groupID, "shoko_series": strconv.Itoa(series.IDs.ID)}
		if series.IDs.AniDB > 0 {
			ids["anidb"] = strconv.Itoa(series.IDs.AniDB)
		}
		poster := selectedPosterPath(series.Images)
		if poster == "" {
			poster = groupPoster
		}
		response.Seasons = append(response.Seasons, &pluginv1.SeasonRecord{
			SeasonNumber: int32(season),
			Title:        series.Name,
			Overview:     sanitizeDescription(series.Description),
			AirDate:      seriesAirDate(series),
			PosterPath:   poster,
			ProviderId:   fmt.Sprintf("series:%d:season:%d", series.IDs.ID, season),
			ProviderIds:  stringStruct(ids),
		})
	}
	// Silo only asks for the episodes of seasons listed here, so season zero is
	// listed when the group has specials.
	for _, series := range layout.OrderedMembers {
		for _, episode := range episodesBySeries[series.IDs.ID] {
			if season, _ := layout.Position(series, episode); season == 0 {
				response.Seasons = append(response.Seasons, &pluginv1.SeasonRecord{
					SeasonNumber: 0,
					Title:        "Specials",
					PosterPath:   groupPoster,
					ProviderId:   fmt.Sprintf("group:%d:season:0", group.IDs.ID),
					ProviderIds:  stringStruct(map[string]string{"shoko_group": groupID}),
				})
				return response
			}
		}
	}
	return response
}

// Shoko artwork is addressed by opaque shoko://image/ paths: <uuid> on daily
// servers and <source>/<type>/<id> on stable servers, which have no UUIDs. The
// paths carry no host, credentials, or query, so a path alone can never leak
// the configured API key; hosts resolve them through the image_resolver.v1
// capability.
const (
	shokoImageScheme         = "shoko://image/"
	shokoImageBarePrefix     = "image/"
	shokoImageEndpointPrefix = "/api/v3/Image/"
)

var (
	shokoImageUUIDPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	shokoStableImagePattern = regexp.MustCompile(`^[A-Za-z]+/[A-Za-z]+/[1-9][0-9]*$`)
)

// imagePath returns the opaque shoko://image/ path for a Shoko image. Images
// that cannot be addressed either way yield an empty path.
func imagePath(image shoko.Image) string {
	if shokoImageUUIDPattern.MatchString(image.UID) {
		return shokoImageScheme + image.UID
	}
	if image.UID == "" {
		stablePath := fmt.Sprintf("%s/%s/%d", image.Source, image.Type, image.ID)
		if shokoStableImagePattern.MatchString(stablePath) {
			return shokoImageScheme + stablePath
		}
	}
	return ""
}

// selectImage returns the preferred available image, falling back to the
// first available image. Disabled and unavailable images are never selected.
func selectImage(images []shoko.Image) *shoko.Image {
	var fallback *shoko.Image
	for i := range images {
		image := &images[i]
		if !image.Available || image.Disabled {
			continue
		}
		if fallback == nil {
			fallback = image
		}
		if image.Preferred {
			return image
		}
	}
	return fallback
}

// applyArtwork fills the opaque poster, backdrop, and logo paths on a
// metadata item from an entity's image set.
func selectedPosterPath(images shoko.Images) string {
	if poster := selectImage(images.Posters); poster != nil {
		return imagePath(*poster)
	}
	return ""
}

func applyArtwork(item *pluginv1.MetadataItem, images shoko.Images) {
	item.PosterPath = selectedPosterPath(images)
	if backdrop := selectImage(images.Backdrops); backdrop != nil {
		item.BackdropPath = imagePath(*backdrop)
	}
	if logo := selectImage(images.Logos); logo != nil {
		item.LogoPath = imagePath(*logo)
	}
}

// shokoImageID strictly extracts the Shoko image ID from a scheme-stripped
// image path supplied by Silo. Anything that is not exactly a path imagePath
// produces is rejected.
func shokoImageID(barePath string) (string, bool) {
	if !strings.HasPrefix(barePath, shokoImageBarePrefix) {
		return "", false
	}
	imageID := strings.TrimPrefix(barePath, shokoImageBarePrefix)
	if !shokoImageUUIDPattern.MatchString(imageID) && !shokoStableImagePattern.MatchString(imageID) {
		return "", false
	}
	return imageID, true
}

// resolveShokoImagePath resolves an image path against a Shoko base URL, which
// is validated at configure time and can carry no credentials, query, or
// fragment. Invalid paths resolve to an empty string.
func resolveShokoImagePath(barePath, baseURL string) string {
	imageID, ok := shokoImageID(barePath)
	if !ok {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + shokoImageEndpointPrefix + imageID
}

func seriesSearchResult(series shoko.Series, itemType string) *pluginv1.ProviderSearchResult {
	if itemType == "" {
		itemType = "series"
	}
	result := &pluginv1.ProviderSearchResult{
		ProviderId:    fmt.Sprintf("series:%d", series.IDs.ID),
		ItemType:      itemType,
		Title:         series.Name,
		OriginalTitle: series.Name,
		Year:          int32(series.AniDB.AirYear()),
		Overview:      sanitizeDescription(series.Description),
		ProviderIds:   idsStruct(seriesIDs(series)),
	}
	if poster := selectImage(series.Images.Posters); poster != nil {
		result.ImageUrl = imagePath(*poster)
	}
	return result
}

func groupSearchResult(group shoko.Group, itemType string, year int) *pluginv1.ProviderSearchResult {
	if itemType == "" {
		itemType = "series"
	}
	return &pluginv1.ProviderSearchResult{
		ProviderId:    fmt.Sprintf("group:%d", group.IDs.ID),
		ItemType:      itemType,
		Title:         group.Name,
		OriginalTitle: group.Name,
		Year:          int32(year),
		Overview:      sanitizeDescription(group.Description),
		ProviderIds:   idsStruct(groupIDs(group)),
	}
}

func episodeSearchResult(episode shoko.Episode, itemType string) *pluginv1.ProviderSearchResult {
	if itemType == "" {
		itemType = "episode"
	}
	return &pluginv1.ProviderSearchResult{
		ProviderId:    fmt.Sprintf("episode:%d", episode.IDs.ID),
		ItemType:      itemType,
		Title:         episodeTitle(episode),
		OriginalTitle: episode.Name,
		Overview:      sanitizeDescription(episode.Description),
		ProviderIds:   idsStruct(episodeIDs(episode)),
	}
}

func seriesMetadata(series shoko.Series, itemType string) *pluginv1.MetadataItem {
	if itemType == "" {
		itemType = "series"
	}
	item := &pluginv1.MetadataItem{
		ProviderId:           fmt.Sprintf("series:%d", series.IDs.ID),
		ItemType:             itemType,
		Title:                series.Name,
		OriginalTitle:        series.Name,
		Year:                 int32(series.AniDB.AirYear()),
		Overview:             sanitizeDescription(series.Description),
		ProviderIds:          idsStruct(seriesIDs(series)),
		TitleAliasesComplete: false,
	}
	applyArtwork(item, series.Images)
	return item
}

func groupMetadata(group shoko.Group, itemType string, year int) *pluginv1.MetadataItem {
	if itemType == "" {
		itemType = "series"
	}
	item := &pluginv1.MetadataItem{
		ProviderId:           fmt.Sprintf("group:%d", group.IDs.ID),
		ItemType:             itemType,
		Title:                group.Name,
		OriginalTitle:        group.Name,
		Year:                 int32(year),
		Overview:             sanitizeDescription(group.Description),
		ProviderIds:          idsStruct(groupIDs(group)),
		TitleAliasesComplete: false,
	}
	applyArtwork(item, group.Images)
	return item
}

func episodeMetadata(episode shoko.Episode, itemType string) *pluginv1.MetadataItem {
	if itemType == "" {
		itemType = "episode"
	}
	return &pluginv1.MetadataItem{
		ProviderId:    fmt.Sprintf("episode:%d", episode.IDs.ID),
		ItemType:      itemType,
		Title:         episodeTitle(episode),
		OriginalTitle: episode.Name,
		Overview:      sanitizeDescription(episode.Description),
		ProviderIds:   idsStruct(episodeIDs(episode)),
	}
}

func groupIDs(group shoko.Group) map[string]string {
	return map[string]string{"shoko_group": strconv.Itoa(group.IDs.ID)}
}

func seriesIDs(series shoko.Series) map[string]string {
	ids := map[string]string{"shoko_series": strconv.Itoa(series.IDs.ID)}
	if series.IDs.AniDB > 0 {
		ids["anidb"] = strconv.Itoa(series.IDs.AniDB)
	}
	if len(series.IDs.TvDB) > 0 {
		ids["tvdb"] = strconv.Itoa(series.IDs.TvDB[0])
	}
	if len(series.IDs.IMDB) > 0 {
		ids["imdb"] = series.IDs.IMDB[0]
	}
	if len(series.IDs.TMDB.Show) > 0 {
		ids["tmdb"] = strconv.Itoa(series.IDs.TMDB.Show[0])
	}
	if len(series.IDs.TMDB.Movie) > 0 && ids["tmdb"] == "" {
		ids["tmdb"] = strconv.Itoa(series.IDs.TMDB.Movie[0])
	}
	return ids
}

func episodeIDs(episode shoko.Episode) map[string]string {
	ids := map[string]string{"shoko_episode": strconv.Itoa(episode.IDs.ID)}
	if episode.IDs.ParentSeries > 0 {
		ids["shoko_series"] = strconv.Itoa(episode.IDs.ParentSeries)
	}
	if episode.IDs.AniDB > 0 {
		ids["anidb"] = strconv.Itoa(episode.IDs.AniDB)
	}
	if len(episode.IDs.TvDB) > 0 {
		ids["tvdb"] = strconv.Itoa(episode.IDs.TvDB[0])
	}
	if len(episode.IDs.IMDB) > 0 {
		ids["imdb"] = episode.IDs.IMDB[0]
	}
	if len(episode.IDs.TMDB.Episode) > 0 {
		ids["tmdb"] = strconv.Itoa(episode.IDs.TMDB.Episode[0])
	}
	return ids
}

func idsStruct(ids map[string]string) *structpb.Struct {
	return stringStruct(ids)
}

func diagnostics(values []topology.Diagnostic) []any {
	limit := min(len(values), maxTaskOutputFailures)
	result := make([]any, 0, limit)
	for _, value := range values[:limit] {
		result = append(result, map[string]any{
			"file_id":  value.FileID,
			"message":  value.Message,
			"severity": value.Severity,
		})
	}
	return result
}
