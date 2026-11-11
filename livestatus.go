package main

import (
	"sync"
	"time"
)

// liveStatus records what live updates have done, for the reconcile task's
// output: Silo keeps plugin logs out of its own log view, so the task output
// is where an administrator can see whether the plugin follows Shoko.
type liveStatus struct {
	mu sync.Mutex

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
