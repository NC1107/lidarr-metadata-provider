package ratelimit

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// frozen replaces the limiter's clock with a fixed instant and records the
// delay each caller is asked to sleep, so spacing can be asserted exactly
// without the test sleeping.
func frozen(l *Limiter) *[]time.Duration {
	var mu sync.Mutex
	delays := []time.Duration{}
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	l.now = func() time.Time { return base }
	l.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- base.Add(d)
		return ch
	}
	return &delays
}

func TestWaitSpacesSequentialCallers(t *testing.T) {
	l := New(time.Second)
	delays := frozen(l)

	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	// The first call goes immediately (no sleep recorded); each subsequent
	// call waits one more interval than the last.
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}
	if len(*delays) != len(want) {
		t.Fatalf("got %d delays %v, want %d", len(*delays), *delays, len(want))
	}
	for i, w := range want {
		if (*delays)[i] != w {
			t.Errorf("delay %d = %v, want %v", i, (*delays)[i], w)
		}
	}
}

// The important property: concurrent callers must not collide on a slot.
// Every caller gets a distinct multiple of the interval, so 20 goroutines
// firing at once still leave the wire one request per interval.
func TestWaitSpacesConcurrentCallers(t *testing.T) {
	const n = 20
	l := New(time.Second)
	delays := frozen(l)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background()); err != nil {
				t.Errorf("wait: %v", err)
			}
		}()
	}
	wg.Wait()

	got := append([]time.Duration(nil), *delays...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != n-1 { // one caller goes immediately with no sleep
		t.Fatalf("got %d delays, want %d: %v", len(got), n-1, got)
	}
	for i, d := range got {
		want := time.Duration(i+1) * time.Second
		if d != want {
			t.Fatalf("sorted delay %d = %v, want %v (slots collided)", i, d, want)
		}
	}
}

// Real clock, small interval: elapsed time must cover the gaps.
func TestWaitRespectsWallClock(t *testing.T) {
	const interval = 20 * time.Millisecond
	l := New(interval)

	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed, min := time.Since(start), 3*interval; elapsed < min {
		t.Errorf("4 calls took %v, want at least %v", elapsed, min)
	}
}

func TestWaitHonoursContextCancellation(t *testing.T) {
	l := New(time.Hour)
	if err := l.Wait(context.Background()); err != nil { // consume the free slot
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected a context error while waiting an hour for a slot")
	}
}

// An already-cancelled context must fail even when a slot is free, so a
// caller that has given up never issues a request.
func TestWaitFailsOnPreCancelledContext(t *testing.T) {
	l := New(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected a context error, got nil")
	}
}

func TestBackoffDelaysTheNextSlot(t *testing.T) {
	l := New(time.Second)
	delays := frozen(l)

	l.Backoff(30 * time.Second)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(*delays) != 1 || (*delays)[0] != 30*time.Second {
		t.Fatalf("got delays %v, want [30s]", *delays)
	}
}

// Backoff must never pull an already-later slot closer.
func TestBackoffNeverShortensTheQueue(t *testing.T) {
	l := New(time.Minute)
	delays := frozen(l)

	if err := l.Wait(context.Background()); err != nil { // reserves now+1m
		t.Fatal(err)
	}
	l.Backoff(time.Second) // shorter than the pending gap, must be ignored
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := (*delays)[0]; got != time.Minute {
		t.Fatalf("second caller waited %v, want 1m", got)
	}
}

func TestNewRejectsNonPositiveInterval(t *testing.T) {
	for _, in := range []time.Duration{0, -time.Second} {
		if got := New(in).interval; got != DefaultInterval {
			t.Errorf("New(%v).interval = %v, want %v", in, got, DefaultInterval)
		}
	}
}

func TestStatsReportsQueueState(t *testing.T) {
	l := New(time.Second)
	frozen(l)

	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	s := l.Stats()
	if s.Reserved != 3 {
		t.Errorf("Reserved = %d, want 3", s.Reserved)
	}
	if s.Waiting != 0 {
		t.Errorf("Waiting = %d, want 0", s.Waiting)
	}
	if s.Interval != time.Second {
		t.Errorf("Interval = %v, want 1s", s.Interval)
	}
	// Two callers queued behind the first, for 1s and 2s.
	if want := 3 * time.Second; s.TotalDelay != want {
		t.Errorf("TotalDelay = %v, want %v", s.TotalDelay, want)
	}
}
