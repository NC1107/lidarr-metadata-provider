package server

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

//go:embed ui.html
var uiPage []byte

// upstreamBase is the live service the UI compares against. Comparison is the
// point of the UI: seeing our result next to the one Lidarr would have got
// from the cloud is faster than diffing JSON by hand.
const upstreamBase = "https://api.lidarr.audio/api/v0.4"

func (s *Server) mountUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiPage)
	})
	mux.HandleFunc("GET /ui/query", s.handleUIQuery)
	mux.HandleFunc("GET /ui/status", s.handleUIStatus)
}

type uiResult struct {
	Source   string          `json:"source"`
	Took     string          `json:"took"`
	Error    string          `json:"error,omitempty"`
	Summary  []uiItem        `json:"summary"`
	Raw      json.RawMessage `json:"raw"`
	Contract []string        `json:"contract"`
}

// uiItem is the flattened view of one result, so the page can render a
// readable card without knowing the shape of every route.
type uiItem struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	ID          string `json:"id"`
	Type        string `json:"type"`
	Detail      string `json:"detail"`
	AlbumsTotal int    `json:"albumsTotal"`
	AlbumsKept  int    `json:"albumsKept"`
	HasAlbums   bool   `json:"hasAlbums"`
}

func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"chain":           s.cfg.FallbackNames,
		"version":         s.cfg.Version,
		"replicationDate": s.cfg.ReplicationDate,
	}
	if s.cfg.Limiter != nil {
		out["limiter"] = s.cfg.Limiter.Stats()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUIQuery answers a UI search against our own chain, the live upstream,
// or both.
func (s *Server) handleUIQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("mode")
	query := strings.ToLower(strings.TrimSpace(q.Get("query")))
	artist := strings.ToLower(strings.TrimSpace(q.Get("artist")))

	out := map[string]any{}
	if q.Get("compare") != "0" {
		out["upstream"] = s.queryUpstream(r, mode, query, artist)
	}
	out["ours"] = s.queryOurs(r, mode, query, artist)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) queryOurs(r *http.Request, mode, query, artist string) uiResult {
	start := time.Now()
	res := uiResult{Source: "ours"}

	var payload any
	var err error
	switch mode {
	case "artist":
		payload, err = s.src.SearchArtists(r.Context(), query, 0)
	case "album":
		payload, err = s.src.SearchAlbums(r.Context(), query, artist, 0)
	case "all":
		payload, err = s.searchAll(r, query)
	case "artist-id":
		payload, err = s.src.Artist(r.Context(), query)
	case "album-id":
		payload, err = s.src.Album(r.Context(), query)
	default:
		err = errUnknownMode
	}

	res.Took = time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	raw, _ := json.Marshal(payload)
	res.Raw = raw
	res.Summary = summarize(mode, raw)
	res.Contract = checkContract(mode, raw)
	return res
}

func (s *Server) queryUpstream(r *http.Request, mode, query, artist string) uiResult {
	start := time.Now()
	res := uiResult{Source: "upstream"}

	var endpoint string
	switch mode {
	case "artist", "all":
		v := url.Values{"type": {mode}, "query": {query}}
		endpoint = upstreamBase + "/search?" + v.Encode()
	case "album":
		v := url.Values{"type": {"album"}, "query": {query}, "artist": {artist}, "includeTracks": {"1"}}
		endpoint = upstreamBase + "/search?" + v.Encode()
	case "artist-id":
		endpoint = upstreamBase + "/artist/" + url.PathEscape(query)
	case "album-id":
		endpoint = upstreamBase + "/album/" + url.PathEscape(query)
	default:
		res.Error = errUnknownMode.Error()
		res.Raw = json.RawMessage("null")
		return res
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	req.Header.Set("User-Agent", "lidarr-metadata-provider-devui/0.1")

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	res.Took = time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.Error = "HTTP " + resp.Status
	}
	if !json.Valid(body) {
		res.Error = "upstream returned non-JSON"
		res.Raw = json.RawMessage("null")
		return res
	}
	res.Raw = body
	res.Summary = summarize(mode, body)
	res.Contract = checkContract(mode, body)
	return res
}

var errUnknownMode = &modeError{}

type modeError struct{}

func (*modeError) Error() string {
	return `mode must be one of "artist", "album", "all", "artist-id", "album-id"`
}

// checkContract runs the same differ the fixture tests use, so the UI shows
// contract drift on live data as well as on goldens.
func checkContract(mode string, raw json.RawMessage) []string {
	var target any
	switch mode {
	case "artist":
		target = &[]skyhook.ArtistResource{}
	case "album":
		target = &[]skyhook.AlbumResource{}
	case "all":
		target = &[]skyhook.EntityResource{}
	case "artist-id":
		target = &skyhook.ArtistResource{}
	case "album-id":
		target = &skyhook.AlbumResource{}
	default:
		return nil
	}
	diffs, err := skyhook.ContractDiff(raw, target)
	if err != nil {
		return []string{err.Error()}
	}
	const max = 12
	if len(diffs) > max {
		return append(diffs[:max], "...")
	}
	if diffs == nil {
		return []string{}
	}
	return diffs
}

// summarize flattens a response into display rows. It decodes leniently
// because the point is to show whatever came back, including from upstream,
// whose payloads we do not control.
func summarize(mode string, raw json.RawMessage) []uiItem {
	switch mode {
	case "artist":
		var artists []skyhook.ArtistResource
		json.Unmarshal(raw, &artists)
		return artistItems(artists)
	case "artist-id":
		var artist skyhook.ArtistResource
		json.Unmarshal(raw, &artist)
		return artistItems([]skyhook.ArtistResource{artist})
	case "album":
		var albums []skyhook.AlbumResource
		json.Unmarshal(raw, &albums)
		return albumItems(albums)
	case "album-id":
		var a skyhook.AlbumResource
		json.Unmarshal(raw, &a)
		return albumItems([]skyhook.AlbumResource{a})
	case "all":
		var entities []skyhook.EntityResource
		json.Unmarshal(raw, &entities)
		out := []uiItem{}
		for _, e := range entities {
			switch {
			case e.Artist != nil:
				out = append(out, artistItems([]skyhook.ArtistResource{*e.Artist})...)
			case e.Album != nil:
				out = append(out, albumItems([]skyhook.AlbumResource{*e.Album})...)
			}
		}
		return out
	}
	return nil
}

func artistItems(artists []skyhook.ArtistResource) []uiItem {
	out := make([]uiItem, 0, len(artists))
	for _, a := range artists {
		kept := len(skyhook.StandardProfile.Filter(a.Albums))
		detail := "no albums in payload"
		if len(a.Albums) > 0 {
			detail = "Lidarr would show " + itoa(kept) + " of " + itoa(len(a.Albums))
		}
		out = append(out, uiItem{
			Kind:        "artist",
			Title:       a.ArtistName,
			Subtitle:    a.Disambiguation,
			ID:          a.ID,
			Type:        deref(a.Type),
			Detail:      detail,
			AlbumsTotal: len(a.Albums),
			AlbumsKept:  kept,
			HasAlbums:   len(a.Albums) > 0,
		})
	}
	return out
}

func albumItems(albums []skyhook.AlbumResource) []uiItem {
	out := make([]uiItem, 0, len(albums))
	for _, al := range albums {
		names := make([]string, 0, len(al.Artists))
		for _, ar := range al.Artists {
			names = append(names, ar.ArtistName)
		}
		typ := al.Type
		if len(al.SecondaryTypes) > 0 {
			typ += " / " + strings.Join(al.SecondaryTypes, ", ")
		}
		detail := "no release statuses"
		if len(al.ReleaseStatuses) > 0 {
			detail = strings.Join(al.ReleaseStatuses, ", ")
		}
		if n := len(al.Releases); n > 0 {
			detail += " - " + itoa(n) + " release(s)"
		}
		out = append(out, uiItem{
			Kind:     "album",
			Title:    al.Title,
			Subtitle: strings.Join(names, ", "),
			ID:       al.ID,
			Type:     typ,
			Detail:   detail + dateSuffix(al.ReleaseDate),
		})
	}
	return out
}

func dateSuffix(d *string) string {
	if d == nil || *d == "" {
		return ""
	}
	return " - " + *d
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
