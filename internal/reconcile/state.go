package reconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

const (
	stateVersion  = 2
	maxStateBytes = 256 << 20
)

var errStaleState = errors.New("reconcile: stale crawl state")

type phase string

const (
	phaseFolders  phase = "folders"
	phaseFiles    phase = "files"
	phaseSeries   phase = "series"
	phaseGroups   phase = "groups"
	phaseEpisodes phase = "episodes"
	phaseReady    phase = "ready"
)

// crawlState is deliberately self-contained. If the plugin process is
// stopped between any two API calls, the next invocation can resume without
// asking Shoko to repeat pages that were already committed here.
type crawlState struct {
	Version        int                         `json:"version"`
	Fingerprint    string                      `json:"fingerprint"`
	Phase          phase                       `json:"phase"`
	FolderIDs      []int                       `json:"folder_ids"`
	FolderIndex    int                         `json:"folder_index"`
	ManagedFolders map[int]shoko.ManagedFolder `json:"managed_folders"`
	NextFilePage   map[int]int                 `json:"next_file_page"`
	Files          []shoko.File                `json:"files"`
	SeriesIDs      []int                       `json:"series_ids"`
	SeriesDone     map[int]bool                `json:"series_done"`
	Series         map[int]shoko.Series        `json:"series"`
	GroupIDs       []int                       `json:"group_ids"`
	GroupDone      map[int]bool                `json:"group_done"`
	Groups         map[int]shoko.Group         `json:"groups"`
	GroupSeries    map[int][]int               `json:"group_series"`
	EpisodeIDs     []int                       `json:"episode_ids"`
	EpisodeDone    map[int]bool                `json:"episode_done"`
	Episodes       map[int]shoko.Episode       `json:"episodes"`
	UpdatedAt      time.Time                   `json:"updated_at"`
}

func loadState(path string) (*crawlState, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reconcile: inspect crawl state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("reconcile: crawl state is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reconcile: open crawl state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reconcile: read crawl state: %w", err)
	}
	if len(data) > maxStateBytes {
		return nil, errors.New("reconcile: crawl state exceeds 256 MiB")
	}
	var state crawlState
	if err := json.Unmarshal(bytes.TrimSpace(data), &state); err != nil {
		return nil, fmt.Errorf("reconcile: decode crawl state: %w", err)
	}
	if state.Version != stateVersion {
		return nil, fmt.Errorf("%w: version %d", errStaleState, state.Version)
	}
	if state.ManagedFolders == nil {
		state.ManagedFolders = make(map[int]shoko.ManagedFolder)
	}
	if state.NextFilePage == nil {
		state.NextFilePage = make(map[int]int)
	}
	if state.SeriesDone == nil {
		state.SeriesDone = make(map[int]bool)
	}
	if state.Series == nil {
		state.Series = make(map[int]shoko.Series)
	}
	if state.GroupDone == nil {
		state.GroupDone = make(map[int]bool)
	}
	if state.Groups == nil {
		state.Groups = make(map[int]shoko.Group)
	}
	if state.GroupSeries == nil {
		state.GroupSeries = make(map[int][]int)
	}
	if state.EpisodeDone == nil {
		state.EpisodeDone = make(map[int]bool)
	}
	if state.Episodes == nil {
		state.Episodes = make(map[int]shoko.Episode)
	}
	return &state, nil
}

func saveState(path string, state *crawlState) error {
	if state == nil {
		return errors.New("reconcile: crawl state is nil")
	}
	state.Version = stateVersion
	state.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("reconcile: encode crawl state: %w", err)
	}
	directory := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(directory, ".shokoanime-crawl-state-*")
	if err != nil {
		return fmt.Errorf("reconcile: create crawl state temp file: %w", err)
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("reconcile: protect crawl state: %w", err)
	}
	if _, err := tmpFile.Write(append(data, '\n')); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("reconcile: write crawl state: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("reconcile: sync crawl state: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("reconcile: close crawl state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("reconcile: install crawl state: %w", err)
	}
	return nil
}

func clearState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reconcile: remove completed crawl state: %w", err)
	}
	return nil
}
