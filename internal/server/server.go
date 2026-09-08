// Package server serves the Lidarr metadata routes.
//
// Every route Lidarr calls is served at the root of whatever base URL the
// user configures, because Lidarr appends "/{route}" to its MetadataSource
// setting. The "/api/v0.4" prefix exists only inside Lidarr's default cloud
// URL and means nothing to us.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/ratelimit"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

// Config carries everything the server needs that is not a dependency.
type Config struct {
	Version         string
	ReplicationDate string
	// FallbackNames lists the non-dataset sources in the chain, for the info
	// route and the dev UI. Empty means dataset-only.
	FallbackNames []string
	// EnableWebUI mounts the local console at /ui.
	EnableWebUI bool
	// Dataset describes the installed dataset, or its absence. The console
	// always shows dataset age, because a user who searches for a brand new
	// release and finds nothing needs to know whether the dataset is stale
	// before blaming the server.
	Dataset DatasetStatus
	// Compare holds sources the console can query side by side with the
	// server's own answer. These are never used to serve Lidarr, only to show
	// an operator where the sources disagree.
	Compare map[string]source.Source
	// Limiter is exposed to the dev UI so queue state is visible. May be nil.
	Limiter *ratelimit.Limiter
	Logger  *slog.Logger
	// LiveDataset, when set, reports the current dataset on each call, so a
	// dataset hot-swapped by the refresh loop is reflected without a restart.
	// When nil the static Dataset is used.
	LiveDataset func() DatasetStatus
	// Health, when set, is what /healthz runs: a real lookup against whatever
	// is serving, so a process that is up but cannot answer reports unhealthy.
	// When nil, /healthz reports healthy whenever the process answers.
	Health func(ctx context.Context) error
	// FallbackLookups reports how many lookups have reached a network source,
	// for the console. May be nil.
	FallbackLookups func() int64
}

// DatasetStatus describes the local dataset behind the server.
type DatasetStatus struct {
	Present bool   `json:"present"`
	Version string `json:"version"`
	// ExportTimestamp is the MusicBrainz export the dataset was built from,
	// which is what "how fresh is this" actually means.
	ExportTimestamp string     `json:"exportTimestamp"`
	InstalledAt     *time.Time `json:"installedAt"`
	NextCheck       *time.Time `json:"nextCheck"`
	UpdateSchedule  string     `json:"updateSchedule"`
	Artists         int64      `json:"artists"`
	Albums          int64      `json:"albums"`
	Tracks          int64      `json:"tracks"`
}

// Server implements the Lidarr metadata contract over a source chain.
type Server struct {
	src     source.Source
	cfg     Config
	log     *slog.Logger
	metrics *Metrics
}

// New returns a Server reading from src.
func New(src source.Source, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Version == "" {
		cfg.Version = "0.0.0-dev"
	}
	return &Server{src: src, cfg: cfg, log: cfg.Logger, metrics: NewMetrics(cfg.FallbackLookups)}
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleInfo)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /artist/{mbid}", s.handleArtist)
	mux.HandleFunc("GET /album/{mbid}", s.handleAlbum)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /recent/artist", s.handleRecent)
	mux.HandleFunc("GET /recent/album", s.handleRecent)
	mux.HandleFunc("POST /search/fingerprint", s.handleFingerprint)
	// Browsers ask for this unprompted. Answering "nothing here" beats a 404
	// that would otherwise show up in the logs as a failed request.
	// A tab icon, so the console is identifiable next to its sibling.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><text y="13" font-size="13">&#127925;</text></svg>`))
	})

	if s.cfg.EnableWebUI {
		s.mountUI(mux)
	}
	return s.instrument(logRequests(s.log, mux))
}

// instrument records timing for the metadata routes only.
//
// Anything else is deliberately not counted. A browser asking for
// /favicon.ico, or an operator refreshing the console, would otherwise land
// in the same totals as real Lidarr traffic and make a healthy server look
// like it is erroring on every request.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, tracked := routeLabel(r.URL.Path)
		// The container's own healthcheck identifies itself so its every-30s
		// poll does not crowd real Lidarr traffic out of the history.
		if !tracked || r.Header.Get("User-Agent") == healthcheckAgent {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		took := time.Since(start)
		// A 404 is a correct answer to a question about something the dataset
		// does not have; only a 5xx is the server failing.
		s.metrics.Observe(route, took, rec.status >= 500)
		s.metrics.Log(RequestLog{
			At: start.Format("15:04:05"), Route: route, Path: r.URL.Path,
			Query: r.URL.RawQuery, Status: rec.status, Bytes: rec.bytes,
			TookMs: took.Milliseconds(),
		})
	})
}

// healthcheckAgent is the User-Agent the container's HEALTHCHECK sends.
const healthcheckAgent = "healthcheck"

// routeLabel collapses a path to the route it serves, reporting false for
// anything that is not a metadata route. Per-MBID paths aggregate so the
// table shows one row per route rather than one per lookup.
func routeLabel(path string) (string, bool) {
	switch {
	case path == "/":
		return "/", true
	case path == "/search" || path == "/search/fingerprint":
		return path, true
	case strings.HasPrefix(path, "/artist/"):
		return "/artist/{mbid}", true
	case strings.HasPrefix(path, "/album/"):
		return "/album/{mbid}", true
	case strings.HasPrefix(path, "/recent/"):
		return "/recent/*", true
	}
	return "", false
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	replication := s.cfg.ReplicationDate
	if replication == "" {
		replication = exportTime(s.datasetStatus().ExportTimestamp)
	}
	writeJSON(w, http.StatusOK, skyhook.ServerInfo{
		Version:         s.cfg.Version,
		Branch:          "main",
		Commit:          "",
		ReplicationDate: replication,
	})
}

// exportTime turns a MusicBrainz export stamp such as 20260718-002132 into
// RFC 3339, which is the form the cloud service reports its replication date
// in. An unparseable stamp is returned as is rather than dropped.
func exportTime(stamp string) string {
	if t, err := time.Parse("20060102-150405", stamp); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return stamp
}

// handleHealth answers the container healthcheck and any orchestrator probe.
// It runs a real lookup when one is configured, so "healthy" means "can
// answer Lidarr", not merely "process exists".
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.cfg.Health(ctx); err != nil {
			s.log.Error("health probe failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// mbidPattern is the shape of a MusicBrainz identifier. Anything else cannot
// exist in the dataset or in MusicBrainz, so it is answered 404 here rather
// than forwarded to a network source that would reject it with a 400.
var mbidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	mbid := strings.TrimSpace(r.PathValue("mbid"))
	if !mbidPattern.MatchString(mbid) {
		s.writeLookupError(w, r, "artist", source.ErrNotFound)
		return
	}
	artist, err := s.src.Artist(r.Context(), mbid)
	if err != nil {
		s.writeLookupError(w, r, "artist", err)
		return
	}
	writeJSON(w, http.StatusOK, artist)
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	mbid := strings.TrimSpace(r.PathValue("mbid"))
	if !mbidPattern.MatchString(mbid) {
		s.writeLookupError(w, r, "album", source.ErrNotFound)
		return
	}
	album, err := s.src.Album(r.Context(), mbid)
	if err != nil {
		s.writeLookupError(w, r, "album", err)
		return
	}
	writeJSON(w, http.StatusOK, album)
}

// handleSearch serves the one route Lidarr uses for all three search modes.
// Lidarr sends the query already lowercased and trimmed; we do it again
// because the dev UI and curl do not.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.ToLower(strings.TrimSpace(q.Get("query")))
	artist := strings.ToLower(strings.TrimSpace(q.Get("artist")))

	switch q.Get("type") {
	case "artist":
		got, err := s.src.SearchArtists(r.Context(), query, 0)
		if err != nil {
			s.writeLookupError(w, r, "search artist", err)
			return
		}
		writeJSON(w, http.StatusOK, got)

	case "album":
		got, err := s.src.SearchAlbums(r.Context(), query, artist, 0)
		if err != nil {
			s.writeLookupError(w, r, "search album", err)
			return
		}
		writeJSON(w, http.StatusOK, got)

	case "all":
		got, err := s.searchAll(r, query)
		if err != nil {
			s.writeLookupError(w, r, "search all", err)
			return
		}
		writeJSON(w, http.StatusOK, got)

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `type must be one of "artist", "album" or "all"`,
		})
	}
}

// searchAll powers Lidarr's top search bar, which expects artists and albums
// interleaved as scored entities.
func (s *Server) searchAll(r *http.Request, query string) ([]skyhook.EntityResource, error) {
	artists, err := s.src.SearchArtists(r.Context(), query, 0)
	if err != nil {
		return nil, err
	}
	albums, err := s.src.SearchAlbums(r.Context(), query, "", 0)
	if err != nil {
		return nil, err
	}

	// Score by how well each result's name matches the query, not by its
	// position within its own type. Otherwise a query that is clearly an album
	// title ("good girl gone bad") is buried under two dozen artists that
	// merely share a word, since Lidarr shows the highest scoring entities
	// first and both lists previously started at the same score.
	norm := skyhook.Normalize(query)
	out := make([]skyhook.EntityResource, 0, len(artists)+len(albums))
	for i := range artists {
		out = append(out, skyhook.EntityResource{
			Score: matchScore(norm, artists[i].ArtistName, i), Artist: &artists[i],
		})
	}
	for i := range albums {
		out = append(out, skyhook.EntityResource{
			Score: matchScore(norm, albums[i].Title, i), Album: &albums[i],
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// matchScore ranks a result by how closely its name matches the query, so an
// exact title outranks a fuzzy one regardless of whether it is an artist or an
// album. rank breaks ties within an equally good match, preserving each
// source's own ordering.
func matchScore(normQuery, name string, rank int) int {
	n := skyhook.Normalize(name)
	switch {
	case n == normQuery:
		return 1000 - rank
	case strings.HasPrefix(n, normQuery) || strings.HasPrefix(normQuery, n):
		return 500 - rank
	case strings.Contains(n, normQuery):
		return 250 - rank
	default:
		return 100 - rank
	}
}

// handleRecent answers both recent routes. A dataset that only moves when a
// new artifact is published cannot enumerate what changed since an arbitrary
// timestamp, and Lidarr treats Limited as "fall back to a normal refresh",
// which is exactly right here.
func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
			since = time.Unix(secs, 0)
		}
	}
	writeJSON(w, http.StatusOK, skyhook.RecentUpdatesResource{
		Since:   since.UTC().Format(time.RFC3339),
		Count:   0,
		Limited: true,
		Items:   []string{},
	})
}

// handleFingerprint stubs acoustic fingerprint search, which degrades to
// "no matches" in Lidarr rather than failing an import.
func (s *Server) handleFingerprint(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func (s *Server) writeLookupError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, source.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": what + " not found"})
		return
	}
	if errors.Is(err, ratelimit.ErrOverloaded) {
		// The queue in front of MusicBrainz is full. Lidarr retries transient
		// failures, so tell it when rather than failing the lookup outright.
		s.log.Warn("lookup refused, fallback queue is full", "what", what, "path", r.URL.Path)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many live lookups queued, try again shortly"})
		return
	}
	s.log.Error("lookup failed", "what", what, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"bytes", rec.bytes,
			"took", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
