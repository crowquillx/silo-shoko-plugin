package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/signalr"
)

// Handler does the work that Shoko's events call for.
type Handler interface {
	// Connected is called after each successful connection. reconnect is
	// false for the first one. Events sent while the watcher was not
	// connected are lost, so this is where to catch up on them.
	Connected(ctx context.Context, reconnect bool)
	// Apply is called with the merged work of a burst of events. Calls never
	// overlap; events that arrive meanwhile wait for the next call.
	Apply(ctx context.Context, batch Batch)
}

// Config configures a Watcher.
type Config struct {
	BaseURL string
	APIKey  string
	// Quiet is how long events must stop before a batch is applied, so an
	// import of many files becomes one batch.
	Quiet time.Duration
	// MaxDelay bounds how long a batch can wait while events keep arriving.
	MaxDelay time.Duration
	// Logf reports connection problems.
	Logf func(format string, args ...any)
	// Dial overrides signalr.Dial in tests.
	Dial func(context.Context, signalr.Config) (Stream, error)
}

// Stream is an open event connection.
type Stream interface {
	Next(context.Context) (signalr.Invocation, error)
	Close() error
}

const (
	defaultQuiet    = 10 * time.Second
	defaultMaxDelay = time.Minute
	minBackoff      = 2 * time.Second
	maxBackoff      = 2 * time.Minute
	// stableAfter is how long a connection must last for the next failure
	// to start backing off from the beginning again.
	stableAfter = time.Minute
)

// Watcher keeps a connection to Shoko's event hub open and hands debounced
// batches of work to a Handler.
type Watcher struct {
	config  Config
	handler Handler

	mu        sync.Mutex
	pending   Batch
	first     time.Time
	last      time.Time
	wake      chan struct{}
	connected bool
}

// New returns a watcher that is not running yet.
func New(config Config, handler Handler) *Watcher {
	if config.Quiet <= 0 {
		config.Quiet = defaultQuiet
	}
	if config.MaxDelay < config.Quiet {
		config.MaxDelay = max(defaultMaxDelay, config.Quiet)
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	if config.Dial == nil {
		config.Dial = func(ctx context.Context, cfg signalr.Config) (Stream, error) {
			return signalr.Dial(ctx, cfg)
		}
	}
	return &Watcher{config: config, handler: handler, wake: make(chan struct{}, 1)}
}

// Connected reports whether the watcher is connected to Shoko now.
func (w *Watcher) Connected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connected
}

// Run listens until ctx ends, reconnecting with backoff when the connection
// fails.
func (w *Watcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.dispatch(ctx)
	}()
	defer wg.Wait()

	backoff := minBackoff
	everConnected := false
	for ctx.Err() == nil {
		started := time.Now()
		err := w.listen(ctx, everConnected, func() { everConnected = true })
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		}
		w.config.Logf("shokoanime: Shoko event connection lost, retrying in %s: %v", backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (w *Watcher) listen(ctx context.Context, reconnect bool, onConnect func()) error {
	stream, err := w.config.Dial(ctx, signalr.Config{
		BaseURL: w.config.BaseURL,
		HubPath: HubPath,
		Query:   url.Values{"feeds": {Feeds}},
		Header: http.Header{
			"Apikey":        {w.config.APIKey},
			"Authorization": {"Bearer " + w.config.APIKey},
		},
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	w.setConnected(true)
	defer w.setConnected(false)
	onConnect()
	if reconnect {
		w.config.Logf("shokoanime: reconnected to Shoko events")
	} else {
		w.config.Logf("shokoanime: listening to Shoko events")
	}
	w.handler.Connected(ctx, reconnect)
	for {
		invocation, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		if invocation.Target == "feed.joined" {
			// Daily servers say which feeds they joined; a feed missing here
			// is one the server does not have.
			var joined []string
			if len(invocation.Arguments) > 0 && json.Unmarshal(invocation.Arguments[0], &joined) == nil {
				w.config.Logf("shokoanime: receiving Shoko's %s events", strings.Join(joined, ", "))
			}
			continue
		}
		if batch, ok := Translate(invocation.Target, invocation.Arguments); ok {
			w.add(batch)
		}
	}
}

func (w *Watcher) setConnected(value bool) {
	w.mu.Lock()
	w.connected = value
	w.mu.Unlock()
}

func (w *Watcher) add(batch Batch) {
	now := time.Now()
	w.mu.Lock()
	if w.pending.Empty() {
		w.first = now
	}
	w.pending.Merge(batch)
	w.last = now
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// dispatch applies pending work once events have been quiet for Quiet, or
// once the oldest pending event is MaxDelay old.
func (w *Watcher) dispatch(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
		case <-timer.C:
		}
		w.mu.Lock()
		if w.pending.Empty() {
			w.mu.Unlock()
			continue
		}
		due := w.last.Add(w.config.Quiet)
		if limit := w.first.Add(w.config.MaxDelay); limit.Before(due) {
			due = limit
		}
		wait := time.Until(due)
		if wait > 0 {
			w.mu.Unlock()
			timer.Reset(wait)
			continue
		}
		batch := w.pending
		w.pending = Batch{}
		w.mu.Unlock()
		w.handler.Apply(ctx, batch)
		// Events that arrived during Apply are due now or soon.
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}
