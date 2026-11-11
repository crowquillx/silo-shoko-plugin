package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/vfs"
)

// liveStatus records what live updates have done, for the status file and the
// reconcile task's output: Silo keeps plugin logs out of its own log view, so
// these are where an administrator can see whether the plugin follows Shoko.
type liveStatus struct {
	mu sync.Mutex
	// dirty is set when something changed since the status file was written.
	dirty bool

	startedAt      time.Time
	connected      bool
	connectedAt    time.Time
	disconnectedAt time.Time
	lastError      string
	connections    int

	events      int
	lastEventAt time.Time
	lastEvent   string

	batches     int
	lastBatchAt time.Time

	applies        int
	lastApplyAt    time.Time
	lastApplyTook  time.Duration
	lastApplyLinks int
	lastApplyError string

	refreshes        int
	lastRefreshAt    time.Time
	lastRefreshShow  string
	lastRefreshError string
}

func (s *liveStatus) update(fn func(*liveStatus)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
	s.dirty = true
}

// takeDirty reports whether the status changed since the last call.
func (s *liveStatus) takeDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := s.dirty
	s.dirty = false
	return dirty
}

// report returns the status for the task output.
func (s *liveStatus) report(pending int) map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at := func(value time.Time) any {
		if value.IsZero() {
			return nil
		}
		return value.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"enabled":            true,
		"started_at":         at(s.startedAt),
		"connected":          s.connected,
		"connected_at":       at(s.connectedAt),
		"disconnected_at":    at(s.disconnectedAt),
		"connections":        s.connections,
		"last_error":         s.lastError,
		"events":             s.events,
		"last_event":         s.lastEvent,
		"last_event_at":      at(s.lastEventAt),
		"batches":            s.batches,
		"last_batch_at":      at(s.lastBatchAt),
		"applies":            s.applies,
		"last_apply_at":      at(s.lastApplyAt),
		"last_apply_seconds": s.lastApplyTook.Seconds(),
		"last_apply_links":   s.lastApplyLinks,
		"last_apply_error":   s.lastApplyError,
		"refreshes":          s.refreshes,
		"refreshes_pending":  pending,
		"last_refresh_at":    at(s.lastRefreshAt),
		"last_refresh_show":  s.lastRefreshShow,
		"last_refresh_error": s.lastRefreshError,
	}
}

// statusWriteInterval bounds how often the status file is rewritten.
const statusWriteInterval = 15 * time.Second

// writeStatusFiles keeps the live status in the VFS root, where it can be read
// without running the reconcile task, which starts a full crawl.
func (l *liveUpdates) writeStatusFiles(ctx context.Context) {
	ticker := time.NewTicker(statusWriteInterval)
	defer ticker.Stop()
	for {
		if l.status.takeDirty() {
			report := l.report()
			report["updated_at"] = time.Now().UTC().Format(time.RFC3339)
			if err := writeJSONFile(vfs.LiveStatusPath(l.reconciler.Root()), report); err != nil && ctx.Err() == nil {
				logf("shokoanime: could not write the live status: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// writeJSONFile replaces path with value as JSON, atomically.
func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".shokoanime-status-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
