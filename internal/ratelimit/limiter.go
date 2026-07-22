// Package ratelimit provides the process-wide gate in front of the
// MusicBrainz web service.
//
// MusicBrainz measures the request rate per source IP and, once it is
// exceeded, declines 100% of requests from that IP until the rate drops -
// not just the excess. Exceeding it repeatedly gets an application blocked.
// Their documented ceiling is 1 request/second, so every call to the service
// from this process must pass through one shared Limiter. A per-client or
// per-request limiter would let concurrent lookups exceed the cap while each
// individually looked well behaved.
//
// Slots are reserved rather than merely checked: Wait atomically claims the
// next free instant and returns once it arrives, so N concurrent callers are
// spaced out instead of stampeding. Reservation makes the queue FIFO by
// arrival, and a caller that gives up while waiting leaves its slot unused,
// which errs toward being slower rather than faster.
package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is the spacing between requests. MusicBrainz documents 1
// request/second; the extra 10ms absorbs clock jitter and keeps us on the
// safe side of a limit whose penalty is a total block.
const DefaultInterval = 1010 * time.Millisecond

// Limiter serializes outbound requests so they are never issued closer
// together than a fixed interval. The zero value is not usable; call New.
type Limiter struct {
	interval time.Duration

	mu   sync.Mutex
	next time.Time // earliest instant the next request may be issued

	waiting  atomic.Int64
	reserved atomic.Int64
	delayed  atomic.Int64 // cumulative nanoseconds callers spent waiting

	// Injected for tests so they need not sleep in real time.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

// New returns a Limiter spacing requests at least interval apart. A
// non-positive interval falls back to DefaultInterval, so a missing config
// value cannot silently disable rate limiting.
func New(interval time.Duration) *Limiter {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Limiter{
		interval: interval,
		now:      time.Now,
		after:    time.After,
	}
}

// Wait blocks until the caller's reserved slot arrives, or until ctx is done.
// On a context error the slot stays consumed, which throttles us slightly
// more than necessary rather than risking a burst.
func (l *Limiter) Wait(ctx context.Context) error {
	l.waiting.Add(1)
	defer l.waiting.Add(-1)

	l.mu.Lock()
	now := l.now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(l.interval)
	l.mu.Unlock()

	l.reserved.Add(1)
	delay := at.Sub(now)
	if delay > 0 {
		l.delayed.Add(int64(delay))
	}

	if delay <= 0 {
		// Still honour an already-cancelled context.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.after(delay):
		return nil
	}
}

// Backoff pushes the next available slot at least d into the future. Call it
// when the server signals it is unhappy - an HTTP 503 or a Retry-After - so
// the whole process slows down together instead of each caller retrying into
// the same wall.
func (l *Limiter) Backoff(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.next) {
		l.next = until
	}
}

// Stats is a snapshot of limiter activity, for the dev UI and logs.
type Stats struct {
	Interval    time.Duration `json:"interval"`
	Waiting     int64         `json:"waiting"`      // callers queued right now
	Reserved    int64         `json:"reserved"`     // slots handed out since start
	TotalDelay  time.Duration `json:"totalDelay"`   // time callers spent queued
	NextSlotIn  time.Duration `json:"nextSlotIn"`   // until the next free slot
	NextSlotStr string        `json:"nextSlotText"` // human form of NextSlotIn
}

// Stats returns a snapshot of current limiter state.
func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	next := l.next
	l.mu.Unlock()

	in := next.Sub(l.now())
	if in < 0 {
		in = 0
	}
	return Stats{
		Interval:    l.interval,
		Waiting:     l.waiting.Load(),
		Reserved:    l.reserved.Load(),
		TotalDelay:  time.Duration(l.delayed.Load()),
		NextSlotIn:  in,
		NextSlotStr: in.Round(time.Millisecond).String(),
	}
}
