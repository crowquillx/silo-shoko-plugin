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
	s.mu.Lock()
	s.cfg = cfg
	s.client = client
	s.reconciler = reconciler
	s.engine = engine
	s.mu.Unlock()
	return &pluginv1.ConfigureResponse{}, nil
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
}

func (s *metadataServer) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	_, client, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	providerIDs := stringMap(req.GetProviderIds())
	token := requestToken(providerIDs, providerIDs["_filepath"], req.GetQuery())
	if !token.Valid() {
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
		year, err := groupYear(ctx, client, group, token.SeriesID)
		if err != nil {
			return nil, err
		}
		return &pluginv1.GetMetadataResponse{Item: groupMetadata(group, req.GetItemType(), year)}, nil
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
	return &pluginv1.GetMetadataResponse{Item: seriesMetadata(series, req.GetItemType())}, nil
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
		return groupSeasons(group, members), nil
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
		season, _ := episodeNumbers(episode)
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
		response.Seasons = append(response.Seasons, &pluginv1.SeasonRecord{
			SeasonNumber: int32(number),
			Title:        fmt.Sprintf("Season %02d", number),
			PosterPath:   selectedPosterPath(series.Images),
			ProviderId:   fmt.Sprintf("series:%d:season:%d", seriesID, number),
			ProviderIds:  stringStruct(seasonIDs),
		})
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
		episodesBySeries := make(map[int][]shoko.Episode, len(members))
		for _, series := range members {
			episodes, err := client.Episodes(ctx, series.IDs.ID)
			if err != nil {
				return nil, err
			}
			episodesBySeries[series.IDs.ID] = episodes
		}
		layout := topology.NewGroupLayout(members, episodesBySeries)
		response := &pluginv1.GetEpisodesResponse{Episodes: make([]*pluginv1.EpisodeRecord, 0)}
		for _, series := range layout.OrderedMembers {
			for _, episode := range episodesBySeries[series.IDs.ID] {
				season, number := layout.Position(series, episode)
				if int(req.GetSeasonNumber()) != season {
					continue
				}
				response.Episodes = append(response.Episodes, &pluginv1.EpisodeRecord{
					SeasonNumber:  int32(season),
					EpisodeNumber: int32(number),
					Title:         episode.Name,
					Overview:      episode.Description,
					ProviderId:    fmt.Sprintf("episode:%d", episode.IDs.ID),
					ProviderIds:   idsStruct(episodeIDs(episode)),
				})
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
	episodes, err := client.Episodes(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	response := &pluginv1.GetEpisodesResponse{Episodes: make([]*pluginv1.EpisodeRecord, 0)}
	for _, episode := range episodes {
		season, number := episodeNumbers(episode)
		if int(req.GetSeasonNumber()) != season {
			continue
		}
		response.Episodes = append(response.Episodes, &pluginv1.EpisodeRecord{
			SeasonNumber:  int32(season),
			EpisodeNumber: int32(number),
			Title:         episode.Name,
			Overview:      episode.Description,
			ProviderId:    fmt.Sprintf("episode:%d", episode.IDs.ID),
			ProviderIds:   idsStruct(episodeIDs(episode)),
		})
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
		return imageRecords(group.Images), nil
	}
	return &pluginv1.GetImagesResponse{}, nil
}

// ResolveImageURL resolves one scheme-stripped image/<uuid> path to the
// authenticated Shoko image endpoint. Silo routes shoko:// paths to this
// capability after removing the scheme. Invalid paths resolve to an empty URL,
// and malformed input is never reflected into an error or URL.
func (s *metadataServer) ResolveImageURL(_ context.Context, req *pluginv1.ResolveImageURLRequest) (*pluginv1.ResolveImageURLResponse, error) {
	cfg, _, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	return &pluginv1.ResolveImageURLResponse{Url: resolveShokoImagePath(req.GetPath(), cfg.BaseURL)}, nil
}

// ResolveImageURLs resolves a batch of scheme-stripped image paths. Paths that
// do not strictly match image/<uuid> map to an empty URL.
func (s *metadataServer) ResolveImageURLs(_ context.Context, req *pluginv1.ResolveImageURLsRequest) (*pluginv1.ResolveImageURLsResponse, error) {
	cfg, _, _, err := s.runtime.state()
	if err != nil {
		return nil, err
	}
	urls := make(map[string]string, len(req.GetPaths()))
	for _, path := range req.GetPaths() {
		urls[path] = resolveShokoImagePath(path, cfg.BaseURL)
	}
	return &pluginv1.ResolveImageURLsResponse{Urls: urls}, nil
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

type scheduledTaskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	runtime *runtimeServer
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
		outcome, err := engine.Step(ctx, 7*time.Second)
		if err != nil {
			return nil, err
		}
		complete = outcome.Complete
		phase = outcome.Phase
		filesFetched, seriesFetched, episodesFetched = outcome.FilesFetched, outcome.SeriesFetched, outcome.EpisodesFetched
		plan, result = outcome.Plan, outcome.Result
	}
	output, err := taskOutput(dryRun, complete, phase, filesFetched, seriesFetched, episodesFetched, plan, result)
	if err != nil {
		return nil, err
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

const maxTaskOutputActions = 100

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
	return structpb.NewStruct(map[string]any{
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
	if marker == "" {
		marker, err = reconciler.CurrentMarker(ctx)
		if err != nil {
			return nil, err
		}
	}
	s.runtime.mu.RLock()
	engine := s.runtime.engine
	s.runtime.mu.RUnlock()
	if engine != nil {
		pending, err := engine.Pending(ctx)
		if err != nil {
			return nil, err
		}
		if pending {
			if _, err := engine.Step(ctx, 4*time.Minute+30*time.Second); err != nil {
				return nil, err
			}
		}
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
	encoded, err := json.Marshal(map[string]any{
		"complete":         outcome.Complete,
		"phase":            outcome.Phase,
		"files_fetched":    outcome.FilesFetched,
		"series_fetched":   outcome.SeriesFetched,
		"episodes_fetched": outcome.EpisodesFetched,
		"action_count":     len(outcome.Result.Actions),
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

func isTVSeries(series shoko.Series) bool {
	return series.AniDB != nil && strings.EqualFold(series.AniDB.Type, "TV")
}

func isMovieSeries(series shoko.Series) bool {
	return series.AniDB != nil && strings.EqualFold(series.AniDB.Type, "Movie")
}

func groupYear(ctx context.Context, client *shoko.Client, group shoko.Group, currentSeriesID int) (int, error) {
	if group.IDs.MainSeries > 0 {
		series, err := client.Series(ctx, group.IDs.MainSeries)
		if err != nil {
			return 0, err
		}
		if year := series.AniDB.AirYear(); year > 0 {
			return year, nil
		}
	}
	members, err := client.GroupSeries(ctx, group.IDs.ID)
	if err != nil {
		return 0, err
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

func groupSeasons(group shoko.Group, members []shoko.Series) *pluginv1.GetSeasonsResponse {
	layout := topology.NewGroupLayout(members, nil)
	response := &pluginv1.GetSeasonsResponse{Seasons: make([]*pluginv1.SeasonRecord, 0, len(layout.OrderedMembers))}
	records := make(map[int]*pluginv1.SeasonRecord)
	for _, series := range layout.OrderedMembers {
		season := layout.SeasonNumber(series.IDs.ID)
		posterPath := selectedPosterPath(series.Images)
		if existing := records[season]; existing != nil {
			if existing.PosterPath == "" {
				existing.PosterPath = posterPath
			}
			continue
		}
		ids := map[string]string{"shoko_group": strconv.Itoa(group.IDs.ID)}
		providerID := fmt.Sprintf("group:%d:season:%d", group.IDs.ID, season)
		title := fmt.Sprintf("Season %02d", season)
		if season > 0 && isTVSeries(series) {
			ids["shoko_series"] = strconv.Itoa(series.IDs.ID)
			providerID = fmt.Sprintf("series:%d:season:%d", series.IDs.ID, season)
			title = series.Name
			if series.IDs.AniDB > 0 {
				ids["anidb"] = strconv.Itoa(series.IDs.AniDB)
			}
		} else if isMovieSeries(series) && !layout.HasTV() {
			title = group.Name
		}
		record := &pluginv1.SeasonRecord{
			SeasonNumber: int32(season),
			Title:        title,
			PosterPath:   posterPath,
			ProviderId:   providerID,
			ProviderIds:  stringStruct(ids),
		}
		records[season] = record
		response.Seasons = append(response.Seasons, record)
	}
	groupPosterPath := selectedPosterPath(group.Images)
	for _, record := range response.Seasons {
		if record.PosterPath == "" {
			record.PosterPath = groupPosterPath
		}
	}
	return response
}

// Shoko artwork is addressed by opaque shoko://image/<uuid> paths. The paths
// carry no host, credentials, or query, so a path alone can never leak the
// configured API key; hosts resolve them through the image_resolver.v1
// capability.
const (
	shokoImageScheme         = "shoko://image/"
	shokoImageBarePrefix     = "image/"
	shokoImageEndpointPrefix = "/api/v3/Image/"
)

var shokoImageUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// imagePath returns the opaque shoko://image/<uuid> path for a Shoko image.
// Images without a canonical UUID cannot be addressed and yield an empty
// path.
func imagePath(image shoko.Image) string {
	if !shokoImageUUIDPattern.MatchString(image.UID) {
		return ""
	}
	return shokoImageScheme + image.UID
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

// resolveShokoImagePath strictly resolves the scheme-stripped image/<uuid>
// path supplied by Silo against the configured Shoko base URL. Anything that
// is not exactly that shape resolves to an empty string. The base URL is
// validated at configure time and can carry no credentials, query, or
// fragment.
func resolveShokoImagePath(barePath, baseURL string) string {
	if !strings.HasPrefix(barePath, shokoImageBarePrefix) {
		return ""
	}
	uuid := strings.TrimPrefix(barePath, shokoImageBarePrefix)
	if !shokoImageUUIDPattern.MatchString(uuid) {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + shokoImageEndpointPrefix + uuid
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
		Overview:      series.Description,
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
		Overview:      group.Description,
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
		Title:         episode.Name,
		OriginalTitle: episode.Name,
		Overview:      episode.Description,
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
		Overview:             series.Description,
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
		Overview:             group.Description,
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
		Title:         episode.Name,
		OriginalTitle: episode.Name,
		Overview:      episode.Description,
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

func episodeNumbers(episode shoko.Episode) (int, int) {
	if episode.AniDB == nil {
		return 1, 1
	}
	if strings.EqualFold(episode.AniDB.Type, "Episode") {
		if episode.AniDB.EpisodeNumber > 0 {
			return 1, episode.AniDB.EpisodeNumber
		}
		return 1, 1
	}
	if episode.AniDB.EpisodeNumber > 0 {
		return 0, episode.AniDB.EpisodeNumber
	}
	return 0, 1
}

func diagnostics(values []topology.Diagnostic) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"file_id":  value.FileID,
			"message":  value.Message,
			"severity": value.Severity,
		})
	}
	return result
}
