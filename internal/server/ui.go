package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

//go:embed ui.html
var uiPage []byte

// officialBase is Lidarr's cloud metadata service, which the console queries
// side by side with this server. Comparing against the thing users are
// switching away from is the only way to answer "is this safe to switch to".
const officialBase = "https://api.lidarr.audio/api/v0.4"

func (s *Server) mountUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiPage)
	})
	mux.HandleFunc("GET /ui/query", s.handleUIQuery)
	mux.HandleFunc("GET /ui/status", s.handleUIStatus)
}

// sideResult is one half of a comparison: what a single server returned and
// what it cost.
type sideResult struct {
	Label  string `json:"label"`
	Origin string `json:"origin"`
	Took   int64  `json:"tookMs"`
	Bytes  int    `json:"bytes"`
	Count  int    `json:"count"`
	Error  string `json:"error,omitempty"`

	// FormatOK reports whether the response carried exactly the keys, casing
	// and types Lidarr's deserializer expects. It says nothing about whether
	// the values are right, which is why the console words the two
	// separately.
	FormatOK     bool     `json:"formatOk"`
	FormatIssues []string `json:"formatIssues"`

	Items []uiItem        `json:"items"`
	Raw   json.RawMessage `json:"raw"`
}

// uiItem is one flattened result row.
type uiItem struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Detail   string `json:"detail"`

	// Albums and Visible describe how many albums Lidarr would display under
	// its default profile. Named explicitly because the number is otherwise
	// alarming: most albums are hidden by profile settings, not missing.
	Albums  int  `json:"albums"`
	Visible int  `json:"visible"`
	Hidden  bool `json:"hasAlbums"`

	// Severe marks a result that would misbehave inside Lidarr regardless of
	// how correct its JSON looks, currently an album carrying no release
	// statuses.
	Severe       bool   `json:"severe"`
	SevereReason string `json:"severeReason,omitempty"`

	// Image is the artwork URL a source returned, if any. MusicBrainz carries
	// no artwork at all, so this is one of the clearest ways to see what
	// build-time enrichment would still have to add.
	Image string `json:"image,omitempty"`
}

// verdict is the one-line answer the console leads with.
type verdict struct {
	Status string `json:"status"`
	Text   string `json:"text"`
	Speed  string `json:"speed,omitempty"`
	Size   string `json:"size,omitempty"`
}

func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"version":  s.cfg.Version,
		"dataset":  s.cfg.Dataset,
		"metrics":  s.metrics.Snapshot(),
		"fallback": s.fallbackStatus(),
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) fallbackStatus() map[string]any {
	out := map[string]any{
		"enabled": len(s.cfg.FallbackNames) > 0,
		"sources": s.cfg.FallbackNames,
	}
	if s.cfg.Limiter != nil {
		stats := s.cfg.Limiter.Stats()
		out["pacedEvery"] = stats.Interval.String()
		out["lookups"] = stats.Reserved
		out["queued"] = stats.Waiting
	}
	return out
}

func (s *Server) handleUIQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("mode")
	query := strings.ToLower(strings.TrimSpace(q.Get("query")))
	artist := strings.ToLower(strings.TrimSpace(q.Get("artist")))
	// Comparison sources are opt in. Each one costs a round trip to somebody
	// else's service, so nothing is queried unless it was asked for.
	against := map[string]bool{}
	for _, name := range strings.Split(q.Get("against"), ",") {
		if name != "" {
			against[name] = true
		}
	}

	out := map[string]any{"local": s.queryLocal(r, mode, query, artist)}
	local := out["local"].(sideResult)

	if against["musicbrainz"] {
		if src, ok := s.cfg.Compare["musicbrainz"]; ok {
			out["musicbrainz"] = s.querySource(r, src, "MusicBrainz",
				"musicbrainz.org, queried live", mode, query, artist)
		}
	}
	if against["official"] {
		official := s.queryOfficial(r, mode, query, artist)
		out["official"] = official
		out["verdict"] = compare(local, official)
	}
	writeJSON(w, http.StatusOK, out)
}

// querySource runs the same lookup against one comparison source.
func (s *Server) querySource(r *http.Request, src source.Source, label, origin, mode, query, artist string) sideResult {
	res := sideResult{Label: label, Origin: origin}
	start := time.Now()

	var payload any
	var err error
	switch mode {
	case "artist":
		payload, err = src.SearchArtists(r.Context(), query, 0)
	case "album":
		payload, err = src.SearchAlbums(r.Context(), query, artist, 0)
	case "all":
		var artists []skyhook.ArtistResource
		if artists, err = src.SearchArtists(r.Context(), query, 0); err == nil {
			entities := make([]skyhook.EntityResource, 0, len(artists))
			for i := range artists {
				entities = append(entities, skyhook.EntityResource{Score: scoreFor(i), Artist: &artists[i]})
			}
			payload = entities
		}
	case "artist-id":
		payload, err = src.Artist(r.Context(), query)
	case "album-id":
		payload, err = src.Album(r.Context(), query)
	default:
		err = fmt.Errorf("pick a search type")
	}
	res.Took = time.Since(start).Milliseconds()

	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	raw, _ := json.Marshal(payload)
	res.fill(mode, raw)
	return res
}

func (s *Server) queryLocal(r *http.Request, mode, query, artist string) sideResult {
	res := sideResult{Label: s.sourceLabel(), Origin: s.originLabel()}
	start := time.Now()

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
		err = fmt.Errorf("pick a search type")
	}
	res.Took = time.Since(start).Milliseconds()

	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	raw, _ := json.Marshal(payload)
	res.fill(mode, raw)
	return res
}

// sourceLabel names what answered, so the pane heading itself carries the
// disclosure rather than making the reader hunt for it. With both a dataset
// and fallback configured the answer can come from either, so the heading
// stays neutral and the origin line explains.
func (s *Server) sourceLabel() string {
	switch {
	case s.cfg.Dataset.Present:
		return "This server"
	case len(s.cfg.FallbackNames) > 0:
		return "This server (no database, using MusicBrainz)"
	default:
		return "This server"
	}
}

// originLabel says where an answer actually came from, which is the question
// the console exists to make unambiguous.
func (s *Server) originLabel() string {
	switch {
	case s.cfg.Dataset.Present && len(s.cfg.FallbackNames) > 0:
		return "Local database, with live MusicBrainz lookups when something is missing"
	case s.cfg.Dataset.Present:
		return "Local database"
	case len(s.cfg.FallbackNames) > 0:
		return "Live MusicBrainz lookup, no local database installed yet"
	default:
		return "No data source configured"
	}
}

func (s *Server) queryOfficial(r *http.Request, mode, query, artist string) sideResult {
	res := sideResult{Label: "Official Lidarr service", Origin: "api.lidarr.audio, the cloud service Lidarr uses by default"}
	start := time.Now()

	endpoint, err := officialURL(mode, query, artist)
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	req.Header.Set("User-Agent", "LidarrMetadataProvider-console/0.1")

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		res.Took = time.Since(start).Milliseconds()
		res.Error = "could not reach the official service: " + err.Error()
		res.Raw = json.RawMessage("null")
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	res.Took = time.Since(start).Milliseconds()

	switch {
	case err != nil:
		res.Error = err.Error()
		res.Raw = json.RawMessage("null")
	case resp.StatusCode != http.StatusOK:
		res.Error = "official service returned HTTP " + resp.Status
		res.Raw = json.RawMessage("null")
	case !json.Valid(body):
		res.Error = "official service returned something that is not JSON"
		res.Raw = json.RawMessage("null")
	default:
		res.fill(mode, body)
	}
	return res
}

func officialURL(mode, query, artist string) (string, error) {
	switch mode {
	case "artist", "all":
		v := url.Values{"type": {mode}, "query": {query}}
		return officialBase + "/search?" + v.Encode(), nil
	case "album":
		v := url.Values{"type": {"album"}, "query": {query}, "artist": {artist}, "includeTracks": {"1"}}
		return officialBase + "/search?" + v.Encode(), nil
	case "artist-id":
		return officialBase + "/artist/" + url.PathEscape(query), nil
	case "album-id":
		return officialBase + "/album/" + url.PathEscape(query), nil
	}
	return "", fmt.Errorf("pick a search type")
}

// fill derives everything the console shows from a raw response body.
func (r *sideResult) fill(mode string, raw json.RawMessage) {
	r.Bytes = len(raw)
	r.Raw = raw
	r.Items = summarize(mode, raw)
	r.Count = len(r.Items)
	r.FormatIssues = checkFormat(mode, raw)
	r.FormatOK = len(r.FormatIssues) == 0
}

// compare turns two results into the single answer the console leads with:
// did this server find the same thing, and what did it cost either side.
func compare(local, official sideResult) verdict {
	switch {
	case local.Error != "" && official.Error != "":
		return verdict{Status: "error", Text: "Neither server answered"}
	case local.Error != "":
		return verdict{Status: "bad", Text: "This server failed, the official one answered"}
	case official.Error != "":
		return verdict{Status: "unknown", Text: "The official service did not answer, so there is nothing to compare against"}
	case local.Count == 0 && official.Count == 0:
		return verdict{Status: "unknown", Text: "Neither server found anything"}
	case local.Count == 0:
		return verdict{Status: "bad", Text: "This server found nothing, the official one did"}
	case official.Count == 0:
		return verdict{Status: "good", Text: "This server found it, the official one did not"}
	}

	v := verdict{Speed: speedText(local.Took, official.Took), Size: sizeText(local.Bytes, official.Bytes)}
	// Top result is what Lidarr shows first and what a user picks, so
	// agreement there matters more than the length of the list.
	if local.Items[0].ID == official.Items[0].ID {
		v.Status = "good"
		v.Text = "Same top result as the official service"
		return v
	}
	v.Status = "warn"
	v.Text = fmt.Sprintf("Different top result: %q here, %q officially",
		local.Items[0].Title, official.Items[0].Title)
	return v
}

func speedText(local, official int64) string {
	switch {
	case local <= 0 || official <= 0:
		return ""
	case local < official:
		return fmt.Sprintf("%.1fx faster", float64(official)/float64(local))
	case official < local:
		return fmt.Sprintf("%.1fx slower", float64(local)/float64(official))
	}
	return "same speed"
}

func sizeText(local, official int) string {
	if local <= 0 || official <= 0 {
		return ""
	}
	diff := float64(local-official) / float64(official) * 100
	switch {
	case diff > 5:
		return fmt.Sprintf("%.0f%% larger", diff)
	case diff < -5:
		return fmt.Sprintf("%.0f%% smaller", -diff)
	}
	return "about the same size"
}

// checkFormat runs the contract differ, which catches keys, casing and types
// drifting from what Lidarr's deserializer expects.
func checkFormat(mode string, raw json.RawMessage) []string {
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
		return []string{}
	}
	diffs, err := skyhook.ContractDiff(raw, target)
	if err != nil {
		return []string{err.Error()}
	}
	const max = 8
	if len(diffs) > max {
		return append(diffs[:max], fmt.Sprintf("and %d more", len(diffs)-max))
	}
	if diffs == nil {
		return []string{}
	}
	return diffs
}

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
		item := uiItem{
			Kind: "artist", Title: a.ArtistName, Subtitle: a.Disambiguation,
			ID: a.ID, Type: deref(a.Type),
			Albums: len(a.Albums), Hidden: len(a.Albums) > 0,
		}
		item.Visible = len(skyhook.StandardProfile.Filter(a.Albums))
		item.Image = firstImage(a.Images)

		// An album with no release statuses is invisible to every profile,
		// so it is worth calling out separately from ordinary filtering.
		blank := 0
		for _, al := range a.Albums {
			if len(al.ReleaseStatuses) == 0 {
				blank++
			}
		}
		if blank > 0 {
			item.Severe = true
			item.SevereReason = fmt.Sprintf("%s no release status, so Lidarr cannot show %s at all",
				plural(blank, "album has", "albums have"), map[bool]string{true: "it", false: "them"}[blank == 1])
		}
		out = append(out, item)
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
			typ += ", " + strings.Join(al.SecondaryTypes, ", ")
		}

		item := uiItem{
			Kind: "album", Title: al.Title, Subtitle: strings.Join(names, ", "),
			ID: al.ID, Type: typ, Detail: releaseDetail(al), Image: firstImage(al.Images),
		}
		if len(al.ReleaseStatuses) == 0 {
			item.Severe = true
			item.SevereReason = "No release status, so Lidarr cannot show this album at all"
		}
		out = append(out, item)
	}
	return out
}

func releaseDetail(al skyhook.AlbumResource) string {
	parts := []string{}
	if al.ReleaseDate != nil && *al.ReleaseDate != "" {
		parts = append(parts, *al.ReleaseDate)
	}
	if n := len(al.Releases); n > 0 {
		parts = append(parts, plural(n, "edition", "editions"))
	}
	return strings.Join(parts, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// plural renders a count with the right noun form, so the console never shows
// the "1 album(s)" shape that reads as unfinished copy.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// firstImage prefers a cover over any other artwork, which is what a person
// scanning a list expects to see.
func firstImage(images []skyhook.ImageResource) string {
	for _, img := range images {
		if strings.EqualFold(img.CoverType, "Cover") && img.URL != "" {
			return img.URL
		}
	}
	for _, img := range images {
		if img.URL != "" {
			return img.URL
		}
	}
	return ""
}
