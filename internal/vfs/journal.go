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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	changeJournalVersion = 1
	changeJournalName    = ".shokoanime-change-journal.jsonl"
	crawlStateName       = ".shokoanime-crawl-state.json"
	snapshotName         = ".shokoanime-snapshot.json"
	liveStatusName       = ".shokoanime-live-status.json"
	maxJournalLineBytes  = 1 << 20
)

// The journal is trimmed once it holds more than journalTrimAbove entries:
// those older than journalKeep go, which a host that polls at all has long
// since read. A host that was away for longer is sent a scan of the whole
// library, as on its first poll. They are variables for tests.
var (
	journalTrimAbove = 10000
	journalKeep      = 72 * time.Hour
)

// journalStamp identifies one written version of the journal file.
type journalStamp struct {
	modified int64
	size     int64
}

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

// SnapshotPath returns where the Shoko data behind the last committed
// reconcile is kept, so a change Shoko reports can be applied without
// crawling the whole library again. It is protected by the VFS lock.
func SnapshotPath(root string) string {
	return filepath.Join(filepath.Clean(root), snapshotName)
}

// LiveStatusPath returns where the plugin reports how live updates are going,
// for an administrator to read without running the reconcile task.
func LiveStatusPath(root string) string {
	return filepath.Join(filepath.Clean(root), liveStatusName)
}

// ErrUnusableMarker means a marker is not a position in this journal: it is
// not a number, or it is ahead of the journal's tail. The changes since it
// cannot be worked out, so the caller has to resynchronize to CurrentMarker.
var ErrUnusableMarker = errors.New("vfs: unusable change marker")

type journalState struct {
	entries []ChangeJournalEntry
	last    uint64
}

// PollChanges returns changed absolute VFS paths after marker. An empty marker
// intentionally starts at the current journal tail, matching scan_source.v1's
// first-poll semantics. A non-empty marker is replayable across process
// restarts because entries are stored under the configured VFS root. A marker
// that is not a position in this journal fails with ErrUnusableMarker.
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
	if len(state.entries) > 0 && parsed+1 < state.entries[0].Sequence {
		// Entries after the marker were trimmed, so some changes are unknown.
		return nil, "", fmt.Errorf("%w: marker %d is older than the journal's first entry %d", ErrUnusableMarker, parsed, state.entries[0].Sequence)
	}
	if parsed > state.last {
		// The journal has never reached this position, so it is not the journal
		// the marker came from: the VFS root was wiped or recreated and sequences
		// started again. Echoing the marker back would skip every change until
		// the new journal passed it.
		return nil, "", fmt.Errorf("%w: marker %d is ahead of the journal's tail %d", ErrUnusableMarker, parsed, state.last)
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
		return 0, fmt.Errorf("%w: %q is not a journal position", ErrUnusableMarker, marker)
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
	// The entries are appended to a copy, since state may be the cached one.
	entries := state.entries[:len(state.entries):len(state.entries)]
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
		entries = append(entries, entry)
	}
	if len(entries) > journalTrimAbove {
		if kept := recentEntries(entries, now.Add(-journalKeep)); len(kept) < len(entries) {
			return r.rewriteJournal(kept)
		}
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
	r.rememberJournal(journalState{entries: entries, last: sequence})
	return nil
}

// recentEntries returns the entries reconciled since cutoff. The newest entry
// is always kept, since the next sequence number is read from it.
func recentEntries(entries []ChangeJournalEntry, cutoff time.Time) []ChangeJournalEntry {
	first := sort.Search(len(entries), func(i int) bool { return !entries[i].ReconciledAt.Before(cutoff) })
	return entries[min(first, len(entries)-1):]
}

// rewriteJournal replaces the journal with entries.
func (r *Reconciler) rewriteJournal(entries []ChangeJournalEntry) error {
	var data bytes.Buffer
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("vfs: encode change journal entry: %w", err)
		}
		data.Write(encoded)
		data.WriteByte('\n')
	}
	tmpFile, err := os.CreateTemp(r.config.Root, ".shokoanime-journal-*")
	if err != nil {
		return fmt.Errorf("vfs: create change journal temp file: %w", err)
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("vfs: protect change journal: %w", err)
	}
	if _, err := tmpFile.Write(data.Bytes()); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("vfs: write change journal: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("vfs: sync change journal: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("vfs: close change journal: %w", err)
	}
	if err := os.Rename(tmp, r.journalPath); err != nil {
		return fmt.Errorf("vfs: install change journal: %w", err)
	}
	r.rememberJournal(journalState{entries: entries, last: entries[len(entries)-1].Sequence})
	return nil
}

// rememberJournal keeps the journal as just written, so polls between
// reconciles do not read the file again.
func (r *Reconciler) rememberJournal(state journalState) {
	r.journal, r.journalStamp = journalState{}, journalStamp{}
	if info, err := os.Lstat(r.journalPath); err == nil {
		r.journal = state
		r.journalStamp = journalStamp{modified: info.ModTime().UnixNano(), size: info.Size()}
	}
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

// readJournal returns the journal, from memory when the file is the one last
// read or written. The result is shared and must not be changed.
func (r *Reconciler) readJournal() (journalState, error) {
	info, err := os.Lstat(r.journalPath)
	if err == nil {
		stamp := journalStamp{modified: info.ModTime().UnixNano(), size: info.Size()}
		if info.Mode().IsRegular() && r.journalStamp == stamp && stamp != (journalStamp{}) {
			return r.journal, nil
		}
		state, err := r.loadJournal()
		if err != nil {
			return journalState{}, err
		}
		r.journal, r.journalStamp = state, stamp
		return state, nil
	}
	return r.loadJournal()
}

func (r *Reconciler) loadJournal() (journalState, error) {
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
