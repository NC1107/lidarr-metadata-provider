package enrich

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const wikipediaSummary = "https://en.wikipedia.org/api/rest_v1/page/summary/"

// CarryOverviews copies biographies from a previous run into the fresh harvest,
// for every artist whose Wikipedia article is unchanged. This is what makes a
// rebuild cheap: only artists that are new, or now point at a different
// article, are left needing a fetch. It reports how many were carried.
func CarryOverviews(fresh, cached map[string]*Artist) int {
	carried := 0
	for mbid, a := range fresh {
		if a.Wiki == "" || a.Overview != "" {
			continue
		}
		if old, ok := cached[mbid]; ok && old.Wiki == a.Wiki && old.Overview != "" {
			a.Overview = old.Overview
			carried++
		}
	}
	return carried
}

// FetchBios fills the Overview of every artist that has a Wikipedia article but
// no biography yet, fetching the article's summary. Work is spread over a pool
// of workers because each fetch is a network round trip; the summary endpoint
// is a static cache that tolerates concurrency, unlike MusicBrainz.
//
// checkpoint is called periodically with the run's progress so the caller can
// persist partial results: a biography fetch over the whole set takes a while,
// and losing it to an interruption would mean starting over.
func FetchBios(client *http.Client, userAgent string, artists map[string]*Artist, workers int, logf func(string, ...any), checkpoint func()) error {
	if workers <= 0 {
		workers = 16
	}

	pending := make([]*Artist, 0)
	for _, a := range artists {
		if a.Wiki != "" && a.Overview == "" {
			pending = append(pending, a)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if logf != nil {
		logf("  fetching %d biographies with %d workers", len(pending), workers)
	}

	jobs := make(chan *Artist)
	var done int64
	var mu sync.Mutex // guards checkpoint, which reads the shared map
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				if text, err := fetchSummary(client, userAgent, a.Wiki); err == nil {
					a.Overview = text
				}
				n := atomic.AddInt64(&done, 1)
				if n%5000 == 0 {
					if logf != nil {
						logf("  biographies: %d/%d", n, len(pending))
					}
					if checkpoint != nil {
						mu.Lock()
						checkpoint()
						mu.Unlock()
					}
				}
			}
		}()
	}
	for _, a := range pending {
		jobs <- a
	}
	close(jobs)
	wg.Wait()
	return nil
}

// summaryResponse is the part of a REST summary that matters here.
type summaryResponse struct {
	Type    string `json:"type"`
	Extract string `json:"extract"`
}

// fetchSummary returns an article's plain-text summary, or empty for anything
// that is not a real biography: a missing page, or a disambiguation page that
// resolves to a list rather than an artist.
func fetchSummary(client *http.Client, userAgent, title string) (string, error) {
	// The stored title is percent-encoded as Wikipedia's URL had it; decode it
	// and re-escape as a single path segment so a title containing a slash
	// (AC/DC) addresses one article rather than a nested path.
	raw := title
	if decoded, err := url.PathUnescape(title); err == nil {
		raw = decoded
	}
	req, err := http.NewRequest(http.MethodGet, wikipediaSummary+url.PathEscape(raw), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A 404 means the article was moved or deleted since Wikidata's
		// sitelink; that is a miss, not a build failure.
		io.Copy(io.Discard, resp.Body)
		return "", nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed summaryResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.Type == "disambiguation" {
		return "", nil
	}
	return parsed.Extract, nil
}

// DefaultClient is an HTTP client sized for the enrichment fetches: a generous
// timeout because Commons and the query service can be slow, and connection
// reuse across the many summary requests.
func DefaultClient() *http.Client {
	return &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}
