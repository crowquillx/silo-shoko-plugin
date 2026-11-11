package main

import (
	"context"
	"sync"
	"time"
)

// refreshQueue schedules Silo refreshes so each show is refreshed once for a
// run of changes rather than once per change. Shoko reports one update as
// several events spread over minutes: the metadata first, then each image as
// it finishes downloading. A refresh waits until a show has been quiet for
// settle, but never longer than maxWait after its first change. A show is not
// refreshed again within minInterval of its last refresh; a change in that
// time waits for it instead of being dropped. Refreshes are spaced apart so a
// burst of changed shows does not flood Silo's job queue.
type refreshQueue struct {
	settle      time.Duration
	maxWait     time.Duration
	minInterval time.Duration
	spacing     time.Duration
	refresh     func(ctx context.Context, key string)

	mu    sync.Mutex
	shows map[string]*queuedRefresh
	wake  chan struct{}
}

type queuedRefresh struct {
	pending       bool
	first, last   time.Time
	lastRefreshed time.Time
}

const (
	refreshSettle      = time.Minute
	refreshMaxWait     = 10 * time.Minute
	refreshMinInterval = 5 * time.Minute
	refreshSpacing     = 2 * time.Second
)

func newRefreshQueue(refresh func(context.Context, string)) *refreshQueue {
	return &refreshQueue{
		settle:      refreshSettle,
		maxWait:     refreshMaxWait,
		minInterval: refreshMinInterval,
		spacing:     refreshSpacing,
		refresh:     refresh,
		shows:       make(map[string]*queuedRefresh),
		wake:        make(chan struct{}, 1),
	}
}

// Add asks for key to be refreshed.
func (q *refreshQueue) Add(key string) {
	now := time.Now()
	q.mu.Lock()
	show := q.shows[key]
	if show == nil {
		show = &queuedRefresh{}
		q.shows[key] = show
	}
	if !show.pending {
		show.pending = true
		show.first = now
	}
	show.last = now
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// due returns when show may be refreshed.
func (q *refreshQueue) due(show *queuedRefresh) time.Time {
	due := show.last.Add(q.settle)
	if limit := show.first.Add(q.maxWait); limit.Before(due) {
		due = limit
	}
	if !show.lastRefreshed.IsZero() {
		if earliest := show.lastRefreshed.Add(q.minInterval); earliest.After(due) {
			due = earliest
		}
	}
	return due
}

// next returns the key of the show that is due first, and when.
func (q *refreshQueue) next() (string, time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var (
		key   string
		when  time.Time
		found bool
	)
	now := time.Now()
	for candidate, show := range q.shows {
		if !show.pending {
			// Forget shows whose cooldown is over.
			if now.Sub(show.lastRefreshed) > q.minInterval {
				delete(q.shows, candidate)
			}
			continue
		}
		due := q.due(show)
		if !found || due.Before(when) || (due.Equal(when) && candidate < key) {
			key, when, found = candidate, due, true
		}
	}
	return key, when, found
}

// Run refreshes shows as they fall due until ctx ends.
func (q *refreshQueue) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		key, when, found := q.next()
		if found && !time.Now().Before(when) {
			q.mu.Lock()
			show := q.shows[key]
			show.pending = false
			show.lastRefreshed = time.Now()
			q.mu.Unlock()
			q.refresh(ctx, key)
			if !sleep(ctx, q.spacing) {
				return
			}
			continue
		}
		wait := time.Hour
		if found {
			wait = time.Until(when)
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-timer.C:
		}
	}
}

// Pending reports how many shows wait for a refresh.
func (q *refreshQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	for _, show := range q.shows {
		if show.pending {
			count++
		}
	}
	return count
}

func sleep(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
