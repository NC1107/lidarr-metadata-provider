// Package server serves the Lidarr metadata routes.
//
// Every route Lidarr calls is served at the root of whatever base URL the
// user configures, because Lidarr appends "/{route}" to its MetadataSource
// setting. The "/api/v0.4" prefix exists only inside Lidarr's default cloud
// URL and means nothing to us.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// EnableWebUI mounts the local testing UI at /ui.
	EnableWebUI bool
	// Limiter is exposed to the dev UI so queue state is visible. May be nil.
	Limiter *ratelimit.Limiter
	Logger  *slog.Logger
}

// Server implements the Lidarr metadata contract over a source chain.
type Server struct {
	src source.Source
	cfg Config
	log *slog.Logger
}

// New returns a Server reading from src.
func New(src source.Source, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Version == "" {
		cfg.Version = "0.0.0-dev"
	}
	return &Server{src: src, cfg: cfg, log: cfg.Logger}
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleInfo)
	mux.HandleFunc("GET /artist/{mbid}", s.handleArtist)
	mux.HandleFunc("GET /album/{mbid}", s.handleAlbum)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /recent/artist", s.handleRecent)
	mux.HandleFunc("GET /recent/album", s.handleRecent)
	mux.HandleFunc("POST /search/fingerprint", s.handleFingerprint)

	if s.cfg.EnableWebUI {
		s.mountUI(mux)
	}
	return logRequests(s.log, mux)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, skyhook.ServerInfo{
		Version:         s.cfg.Version,
		Branch:          "main",
		Commit:          "",
		ReplicationDate: s.cfg.ReplicationDate,
	})
}

func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	artist, err := s.src.Artist(r.Context(), r.PathValue("mbid"))
	if err != nil {
		s.writeLookupError(w, r, "artist", err)
		return
	}
	writeJSON(w, http.StatusOK, artist)
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	album, err := s.src.Album(r.Context(), r.PathValue("mbid"))
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

	out := make([]skyhook.EntityResource, 0, len(artists)+len(albums))
	for i := range artists {
		out = append(out, skyhook.EntityResource{Score: scoreFor(i), Artist: &artists[i]})
	}
	for i := range albums {
		out = append(out, skyhook.EntityResource{Score: scoreFor(i), Album: &albums[i]})
	}
	return out, nil
}

// scoreFor approximates the upstream score, which counts down from 100 by
// rank. Lidarr only uses it for ordering.
func scoreFor(rank int) int {
	if score := 100 - rank; score > 0 {
		return score
	}
	return 1
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
