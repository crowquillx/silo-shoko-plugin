package main

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

type refreshLog struct {
	mu    sync.Mutex
	keys  []string
	times []time.Time
	done  chan string
}

func (r *refreshLog) refresh(_ context.Context, key string) {
	r.mu.Lock()
	r.keys = append(r.keys, key)
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
	r.done <- key
}

func (r *refreshLog) wait(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case key := <-r.done:
		return key
	case <-time.After(within):
		t.Fatal("no refresh")
		return ""
	}
}

func (r *refreshLog) none(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case key := <-r.done:
		t.Fatalf("unexpected refresh of %s", key)
	case <-time.After(within):
	}
}

func startQueue(t *testing.T, configure func(*refreshQueue)) (*refreshQueue, *refreshLog) {
	t.Helper()
	log := &refreshLog{done: make(chan string, 16)}
	queue := newRefreshQueue(log.refresh)
	queue.spacing = 0
	configure(queue)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		queue.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return queue, log
}

func TestRefreshQueueWaitsForAShowToSettle(t *testing.T) {
	queue, log := startQueue(t, func(q *refreshQueue) {
		q.settle = 150 * time.Millisecond
		q.maxWait = time.Minute
		q.minInterval = 0
	})
	start := time.Now()
	// Changes trickle in, as image downloads do after an update.
	for range 4 {
		queue.Add("A")
		time.Sleep(50 * time.Millisecond)
	}
	queue.Add("B")
	first, second := log.wait(t, 2*time.Second), log.wait(t, 2*time.Second)
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("refreshed after %s, before the shows settled", elapsed)
	}
	if !reflect.DeepEqual([]string{first, second}, []string{"A", "B"}) {
		t.Fatalf("refreshed %s, %s; want A once, then B once", first, second)
	}
	log.none(t, 300*time.Millisecond)
}

func TestRefreshQueueRefreshesAShowThatKeepsChangingWithinMaxWait(t *testing.T) {
	queue, log := startQueue(t, func(q *refreshQueue) {
		q.settle = 200 * time.Millisecond
		q.maxWait = 300 * time.Millisecond
		q.minInterval = time.Minute
	})
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				queue.Add("A")
			}
		}
	}()
	defer close(stop)
	log.wait(t, 2*time.Second)
}

func TestRefreshQueueDefersAChangeDuringTheCooldown(t *testing.T) {
	queue, log := startQueue(t, func(q *refreshQueue) {
		q.settle = 0
		q.maxWait = 0
		q.minInterval = 400 * time.Millisecond
	})
	queue.Add("A")
	log.wait(t, time.Second)
	queue.Add("A")
	log.none(t, 250*time.Millisecond)
	log.wait(t, time.Second)
	log.mu.Lock()
	defer log.mu.Unlock()
	if gap := log.times[1].Sub(log.times[0]); gap < 400*time.Millisecond {
		t.Fatalf("second refresh came %s after the first, inside the cooldown", gap)
	}
	if queue.Pending() != 0 {
		t.Fatalf("pending after both refreshes = %d", queue.Pending())
	}
}

func TestRefreshQueueSpacesRefreshes(t *testing.T) {
	queue, log := startQueue(t, func(q *refreshQueue) {
		q.settle, q.maxWait, q.minInterval = 0, 0, 0
		q.spacing = 100 * time.Millisecond
	})
	queue.Add("A")
	queue.Add("B")
	queue.Add("C")
	for range 3 {
		log.wait(t, 2*time.Second)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for i := 1; i < len(log.times); i++ {
		if gap := log.times[i].Sub(log.times[i-1]); gap < 90*time.Millisecond {
			t.Fatalf("refreshes %d and %d came %s apart", i-1, i, gap)
		}
	}
}
