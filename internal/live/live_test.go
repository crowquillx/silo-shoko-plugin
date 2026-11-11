package live

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
	"github.com/crowquillx/silo-shoko-plugin/internal/signalr"
)

func translate(t *testing.T, target, argument string) (Batch, bool) {
	t.Helper()
	if argument == "" {
		return Translate(target, nil)
	}
	return Translate(target, []json.RawMessage{json.RawMessage(argument)})
}

func TestTranslate(t *testing.T) {
	files := func(ids ...int) Batch { return Batch{VFS: reconcile.Changes{Files: ids}} }
	tests := []struct {
		name     string
		target   string
		argument string
		want     Batch
		ok       bool
	}{
		{"daily release", "release:saved", `{"FileID":5}`, files(5), true},
		{"daily unmatch", "release:removed", `{"FileID":4,"Release":{}}`, files(4), true},
		{"daily unmatch of a deleted file", "release:removed", `{"FileID":null}`, Batch{}, false},
		{"earlier daily unmatch", "release:deleted", `{"FileID":4}`, files(4), true},
		{"daily delete", "file:deleted", `{"FileID":6,"ManagedFolderID":1,"RelativePath":"a.mkv"}`, files(6), true},
		{"daily ignored flag", "file:updated", `{"FileID":3,"Changes":["Ignored"],"IsIgnored":true}`, files(3), true},
		{"stable match", "ShokoEvent:FileMatched", `{"FileID":7,"CrossRefs":[]}`, files(7), true},
		{"stable no match", "ShokoEvent:FileNotMatched", `{"FileID":2,"HasCrossReferences":false}`, files(2), true},
		{"stable rename", "ShokoEvent:FileRenamed", `{"FileID":8,"FileName":"b.mkv"}`, files(8), true},
		{"file without ID", "release:saved", `{}`, Batch{}, false},
		{"file hashed", "file:hashed", `{"FileID":5}`, Batch{}, false},
		{
			"daily series update", "metadata:series.updated", `{"Reason":"Updated","Source":"AniDB","SeriesID":"100","ShokoSeriesIDs":[3]}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}}, Refresh: Refresh{Series: []int{3}}}, true,
		},
		{
			"daily preferred image", "metadata:series.imageupdated", `{"Reason":"ImageUpdated","Source":"TMDB","SeriesID":"12","ShokoSeriesIDs":[3]}`,
			Batch{Refresh: Refresh{Series: []int{3}}}, true,
		},
		{
			"daily image without a reason", "metadata:episode.imageadded", `{"ShokoSeriesIDs":[3],"ShokoEpisodeIDs":[30]}`,
			Batch{Refresh: Refresh{Series: []int{3}, Episodes: []int{30}}}, true,
		},
		{
			"daily combined reasons as flags", "metadata:series.18", `{"Reason":18,"ShokoSeriesIDs":[3]}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}}, Refresh: Refresh{Series: []int{3}}}, true,
		},
		{
			"combined reasons by name", "metadata:series.updated", `{"Reason":"Updated, ImageAdded","ShokoSeriesIDs":[3]}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}}, Refresh: Refresh{Series: []int{3}}}, true,
		},
		{
			"stable episode update", "ShokoEvent:EpisodeUpdated", `{"Reason":"Updated","ShokoSeriesIDs":[3],"ShokoEpisodeIDs":[30,31]}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}}, Refresh: Refresh{Series: []int{3}, Episodes: []int{30, 31}}}, true,
		},
		{
			"stable preferred image", "ShokoEvent:SeriesUpdated", `{"Reason":"ImageAdded","ShokoSeriesIDs":[3]}`,
			Batch{Refresh: Refresh{Series: []int{3}}}, true,
		},
		{
			"removal updates the VFS only", "metadata:episode.removed", `{"Reason":"Removed","ShokoSeriesIDs":[3],"ShokoEpisodeIDs":[30]}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}}}, true,
		},
		{
			"TMDB link change", "metadata:links.changed", `{"Reason":"Linked","ShokoSeriesIDs":[3,4],"Changes":[]}`,
			Batch{Refresh: Refresh{Series: []int{3, 4}}}, true,
		},
		{
			"group renamed", "group:group.updated", `{"Reason":"Updated","GroupID":9}`,
			Batch{VFS: reconcile.Changes{Groups: []int{9}}, Refresh: Refresh{Groups: []int{9}}}, true,
		},
		{"group artwork", "group:group.imageupdated", `{"Reason":"ImageUpdated","GroupID":9}`, Batch{Refresh: Refresh{Groups: []int{9}}}, true},
		{"group removed", "group:group.removed", `{"Reason":"Removed","GroupID":9}`, Batch{VFS: reconcile.Changes{Groups: []int{9}}}, true},
		{
			"series moved", "group:series.moved", `{"SeriesID":3,"OldGroupID":9,"NewGroupID":10}`,
			Batch{VFS: reconcile.Changes{Series: []int{3}, Groups: []int{9, 10}}, Refresh: Refresh{Groups: []int{9, 10}}}, true,
		},
		{"groups recreated", "group:recreated", ``, Batch{Recrawl: true}, true},
		{"custom tags", "metadata:series.customTagsChanged", `{"ShokoSeriesIDs":[3]}`, Batch{}, false},
		{"hidden episode", "metadata:episode.hiddenChanged", `{"ShokoSeriesIDs":[3]}`, Batch{}, false},
		{"metadata for nothing in Shoko", "metadata:series.updated", `{"Reason":"Updated","ShokoSeriesIDs":[]}`, Batch{}, false},
		{"unknown event", "ShokoEvent:Something", `{"FileID":5}`, Batch{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := translate(t, tt.target, tt.argument)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Translate = %#v, %v; want %#v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := Translate("release:saved", nil); ok {
		t.Fatal("Translate without arguments reported work")
	}
}

// fakeStream replays invocations, then blocks until closed or fails.
type fakeStream struct {
	invocations chan signalr.Invocation
	fail        chan error
	closed      chan struct{}
	once        sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{invocations: make(chan signalr.Invocation, 16), fail: make(chan error, 1), closed: make(chan struct{})}
}

func (s *fakeStream) Next(ctx context.Context) (signalr.Invocation, error) {
	select {
	case invocation := <-s.invocations:
		return invocation, nil
	case err := <-s.fail:
		return signalr.Invocation{}, err
	case <-s.closed:
		return signalr.Invocation{}, errors.New("closed")
	case <-ctx.Done():
		return signalr.Invocation{}, ctx.Err()
	}
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeStream) send(target, argument string) {
	s.invocations <- signalr.Invocation{Target: target, Arguments: []json.RawMessage{json.RawMessage(argument)}}
}

type recordingHandler struct {
	mu        sync.Mutex
	connected []bool
	batches   []Batch
	applied   chan struct{}
}

func (h *recordingHandler) Connected(_ context.Context, reconnect bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected = append(h.connected, reconnect)
}

func (h *recordingHandler) Apply(_ context.Context, batch Batch) {
	h.mu.Lock()
	h.batches = append(h.batches, batch)
	h.mu.Unlock()
	h.applied <- struct{}{}
}

func TestWatcherMergesABurstIntoOneBatchAndReconnects(t *testing.T) {
	streams := make(chan *fakeStream, 2)
	first, second := newFakeStream(), newFakeStream()
	streams <- first
	streams <- second
	handler := &recordingHandler{applied: make(chan struct{}, 4)}
	watcher := New(Config{
		Quiet:    50 * time.Millisecond,
		MaxDelay: time.Second,
		Dial: func(context.Context, signalr.Config) (Stream, error) {
			return <-streams, nil
		},
	}, handler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()

	first.send("release:saved", `{"FileID":1}`)
	first.send("release:saved", `{"FileID":2}`)
	first.send("release:saved", `{"FileID":1}`)
	first.send("metadata:series.imageadded", `{"Reason":"ImageAdded","ShokoSeriesIDs":[9]}`)
	select {
	case <-handler.applied:
	case <-time.After(5 * time.Second):
		t.Fatal("no batch applied")
	}
	handler.mu.Lock()
	got := handler.batches[0]
	handler.mu.Unlock()
	want := Batch{VFS: reconcile.Changes{Files: []int{1, 2}}, Refresh: Refresh{Series: []int{9}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch = %#v, want %#v", got, want)
	}
	if !watcher.Connected() {
		t.Fatal("watcher does not report being connected")
	}

	// The connection drops; the watcher reconnects and says so.
	first.fail <- errors.New("dropped")
	second.send("file:deleted", `{"FileID":3}`)
	select {
	case <-handler.applied:
	case <-time.After(10 * time.Second):
		t.Fatal("no batch after reconnecting")
	}
	cancel()
	<-done
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if !reflect.DeepEqual(handler.connected, []bool{false, true}) {
		t.Fatalf("Connected calls = %v", handler.connected)
	}
	if !reflect.DeepEqual(handler.batches[1].VFS.Files, []int{3}) {
		t.Fatalf("second batch = %#v", handler.batches[1])
	}
}

func TestWatcherAppliesWithinMaxDelayWhileEventsContinue(t *testing.T) {
	stream := newFakeStream()
	handler := &recordingHandler{applied: make(chan struct{}, 4)}
	watcher := New(Config{
		Quiet:    200 * time.Millisecond,
		MaxDelay: 300 * time.Millisecond,
		Dial: func(context.Context, signalr.Config) (Stream, error) {
			return stream, nil
		},
	}, handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	stop := make(chan struct{})
	go func() {
		for id := 1; ; id++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				stream.send("release:saved", `{"FileID":`+itoa(id)+`}`)
			}
		}
	}()
	defer close(stop)
	select {
	case <-handler.applied:
	case <-time.After(2 * time.Second):
		t.Fatal("a steady stream of events held the batch back")
	}
}

func itoa(value int) string {
	data, _ := json.Marshal(value)
	return string(data)
}
