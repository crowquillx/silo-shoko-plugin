package vfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	changeJournalVersion = 1
	changeJournalName    = ".shokoanime-change-journal.jsonl"
	crawlStateName       = ".shokoanime-crawl-state.json"
	maxJournalLineBytes  = 1 << 20
)

// ChangeJournalEntry is one successfully reconciled VFS path. Sequence is an
// opaque-to-the-host cursor value; the plugin only requires that callers send
// back the marker returned by PollChanges.
type ChangeJournalEntry struct {
	Version      int       `json:"version"`
	Sequence     uint64    `json:"sequence"`
	Path         string    `json:"path"`
	ReconciledAt time.Time `json:"reconciled_at"`
}

// ChangeJournalPath returns the persistent journal location for a VFS root.
// It is exported so diagnostics and integration tests can identify the
// plugin-owned state without duplicating the filename convention.
func ChangeJournalPath(root string) string {
	return filepath.Join(filepath.Clean(root), changeJournalName)
}

// CrawlStatePath returns the persistent resumable-crawl state location for a
// VFS root. It is intentionally hidden and is protected by the same VFS lock
// as the manifest and change journal.
func CrawlStatePath(root string) string {
	return filepath.Join(filepath.Clean(root), crawlStateName)
}

type journalState struct {
	entries []ChangeJournalEntry
	last    uint64
}

// PollChanges returns changed absolute VFS paths after marker. An empty marker
// intentionally starts at the current journal tail, matching scan_source.v1's
// first-poll semantics. A non-empty marker is replayable across process
// restarts because entries are stored under the configured VFS root.
func (r *Reconciler) PollChanges(ctx context.Context, marker string) ([]string, string, error) {
	var paths []string
	var next string
	err := r.WithExclusive(ctx, func(locked *Reconciler) error {
		var err error
		paths, next, err = locked.PollChangesLocked(ctx, marker)
		return err
	})
	return paths, next, err
}

// PollChangesLocked reads the journal while the caller holds WithExclusive.
func (r *Reconciler) PollChangesLocked(ctx context.Context, marker string) ([]string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	state, err := r.readJournal()
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(marker) == "" {
		return nil, formatMarker(state.last), nil
	}
	parsed, err := parseMarker(marker)
	if err != nil {
		return nil, "", err
	}
	if parsed > state.last {
		// Preserve a future marker verbatim rather than regressing it. This is
		// useful if a host restores a source row and its cursor independently
		// of the plugin's VFS state.
		return nil, strings.TrimSpace(marker), nil
	}

	paths := make([]string, 0)
	seen := make(map[string]struct{})
	for _, entry := range state.entries {
		if entry.Sequence <= parsed {
			continue
		}
		if _, exists := seen[entry.Path]; exists {
			continue
		}
		seen[entry.Path] = struct{}{}
		paths = append(paths, entry.Path)
	}
	return paths, formatMarker(state.last), nil
}

// CurrentMarker returns the current journal tail without consuming it.
func (r *Reconciler) CurrentMarker(ctx context.Context) (string, error) {
	var marker string
	err := r.WithExclusive(ctx, func(locked *Reconciler) error {
		var err error
		marker, err = locked.CurrentMarkerLocked(ctx)
		return err
	})
	return marker, err
}

// CurrentMarkerLocked returns the journal tail while the caller holds
// WithExclusive.
func (r *Reconciler) CurrentMarkerLocked(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	state, err := r.readJournal()
	if err != nil {
		return "", err
	}
	return formatMarker(state.last), nil
}

// Root returns the cleaned configured VFS root.
func (r *Reconciler) Root() string {
	if r == nil {
		return ""
	}
	return r.config.Root
}

// EnsureRootLocked creates and validates the configured output root while the
// caller holds WithExclusive. Resumable crawls use it before persisting their
// state; dry-run callers should continue to use Reconcile instead.
func (r *Reconciler) EnsureRootLocked() error {
	if r == nil {
		return ErrUnsafeRoot
	}
	return r.ensureRoot(true)
}

// EnsureRoot creates and validates the configured output root. It is intended
// for real crawl callers before they acquire the persistent lock; dry-run
// callers must not invoke it.
func (r *Reconciler) EnsureRoot() error {
	if r == nil {
		return ErrUnsafeRoot
	}
	return r.ensureRoot(true)
}

func parseMarker(marker string) (uint64, error) {
	trimmed := strings.TrimSpace(marker)
	value, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("vfs: invalid change marker %q: %w", marker, err)
	}
	return value, nil
}

func formatMarker(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func (r *Reconciler) appendJournal(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	state, err := r.readJournal()
	if err != nil {
		return err
	}
	unique := uniquePaths(paths)
	if len(unique) == 0 {
		return nil
	}

	var data bytes.Buffer
	sequence := state.last
	now := time.Now().UTC()
	for _, path := range unique {
		sequence++
		entry := ChangeJournalEntry{
			Version:      changeJournalVersion,
			Sequence:     sequence,
			Path:         path,
			ReconciledAt: now,
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("vfs: encode change journal entry: %w", err)
		}
		data.Write(encoded)
		data.WriteByte('\n')
	}

	if info, err := os.Lstat(r.journalPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("vfs: change journal is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("vfs: inspect change journal: %w", err)
	}
	file, err := os.OpenFile(r.journalPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("vfs: open change journal: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("vfs: protect change journal: %w", err)
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("vfs: append change journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("vfs: sync change journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("vfs: close change journal: %w", err)
	}
	return nil
}

func uniquePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		path = filepath.Clean(path)
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func (r *Reconciler) readJournal() (journalState, error) {
	if info, err := os.Lstat(r.journalPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return journalState{}, errors.New("vfs: change journal is not a regular file")
		}
	} else if os.IsNotExist(err) {
		return journalState{}, nil
	} else {
		return journalState{}, fmt.Errorf("vfs: inspect change journal: %w", err)
	}

	file, err := os.Open(r.journalPath)
	if err != nil {
		return journalState{}, fmt.Errorf("vfs: read change journal: %w", err)
	}
	defer file.Close()

	state := journalState{entries: make([]ChangeJournalEntry, 0)}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxJournalLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry ChangeJournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return journalState{}, fmt.Errorf("vfs: decode change journal: %w", err)
		}
		if entry.Version != changeJournalVersion || entry.Sequence == 0 || entry.Sequence <= state.last {
			return journalState{}, fmt.Errorf("vfs: invalid change journal sequence %d", entry.Sequence)
		}
		if !filepath.IsAbs(entry.Path) || !strictUnder(entry.Path, r.config.Root) {
			return journalState{}, fmt.Errorf("vfs: change journal path %q is outside the VFS root", entry.Path)
		}
		state.entries = append(state.entries, entry)
		state.last = entry.Sequence
	}
	if err := scanner.Err(); err != nil {
		return journalState{}, fmt.Errorf("vfs: read change journal: %w", err)
	}
	return state, nil
}
