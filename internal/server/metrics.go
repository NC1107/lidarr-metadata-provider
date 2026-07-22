package server

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics tracks what an operator would check to decide whether this server
// is behaving: how much it is being asked, how fast it answers, how often it
// fails, and how much traffic is falling through to the network.
//
// The fallback counter is the one worth watching over time. A rising share of
// fallback lookups means the dataset is missing things people are asking for,
// which is the signal to take an update.
type Metrics struct {
	started time.Time

	requests atomic.Int64
	errors   atomic.Int64
	fallback atomic.Int64

	mu       sync.Mutex
	byRoute  map[string]*routeStats
	recentMs []float64
}

type routeStats struct {
	count   int64
	errors  int64
	totalMs float64
	maxMs   float64
}

// recentWindow bounds the sample kept for percentiles. Enough to be
// meaningful, small enough that a long-running server does not grow without
// limit.
const recentWindow = 512

func NewMetrics() *Metrics {
	return &Metrics{started: time.Now(), byRoute: map[string]*routeStats{}}
}

// Observe records one served request.
func (m *Metrics) Observe(route string, took time.Duration, failed bool) {
	m.requests.Add(1)
	if failed {
		m.errors.Add(1)
	}
	ms := float64(took.Microseconds()) / 1000

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.byRoute[route]
	if !ok {
		s = &routeStats{}
		m.byRoute[route] = s
	}
	s.count++
	s.totalMs += ms
	if failed {
		s.errors++
	}
	if ms > s.maxMs {
		s.maxMs = ms
	}

	m.recentMs = append(m.recentMs, ms)
	if len(m.recentMs) > recentWindow {
		m.recentMs = m.recentMs[len(m.recentMs)-recentWindow:]
	}
}

// ObserveFallback records a lookup that had to leave the machine.
func (m *Metrics) ObserveFallback() { m.fallback.Add(1) }

// RouteSnapshot is per-route timing, for the status view.
type RouteSnapshot struct {
	Route     string  `json:"route"`
	Count     int64   `json:"count"`
	Errors    int64   `json:"errors"`
	AverageMs float64 `json:"averageMs"`
	SlowestMs float64 `json:"slowestMs"`
}

// Snapshot is the whole picture at one instant.
type Snapshot struct {
	UptimeSeconds   float64         `json:"uptimeSeconds"`
	Requests        int64           `json:"requests"`
	Errors          int64           `json:"errors"`
	FallbackLookups int64           `json:"fallbackLookups"`
	MedianMs        float64         `json:"medianMs"`
	P95Ms           float64         `json:"p95Ms"`
	Routes          []RouteSnapshot `json:"routes"`
}

func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := Snapshot{
		UptimeSeconds:   time.Since(m.started).Seconds(),
		Requests:        m.requests.Load(),
		Errors:          m.errors.Load(),
		FallbackLookups: m.fallback.Load(),
		Routes:          make([]RouteSnapshot, 0, len(m.byRoute)),
	}

	if len(m.recentMs) > 0 {
		sorted := append([]float64(nil), m.recentMs...)
		sort.Float64s(sorted)
		out.MedianMs = percentile(sorted, 0.50)
		out.P95Ms = percentile(sorted, 0.95)
	}

	for route, s := range m.byRoute {
		avg := 0.0
		if s.count > 0 {
			avg = s.totalMs / float64(s.count)
		}
		out.Routes = append(out.Routes, RouteSnapshot{
			Route: route, Count: s.count, Errors: s.errors,
			AverageMs: avg, SlowestMs: s.maxMs,
		})
	}
	sort.Slice(out.Routes, func(i, j int) bool { return out.Routes[i].Count > out.Routes[j].Count })
	return out
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}
