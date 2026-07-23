package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/ratelimit"
)

// extractsEndpoint is a var, not a const, only so a test can point the fetch at
// a local server; production always uses the real MediaWiki API.
var extractsEndpoint = "https://en.wikipedia.org/w/api.php"

// One extracts request returns the lead section of up to twenty articles, so
// the whole biography set is a few thousand requests rather than one per
// artist. That is what keeps the fetch under Wikimedia's rate limit: an earlier
// design fetched each article separately and, without that twentyfold
// reduction, tripped the throttle after its burst allowance and stalled.
const (
	extractsBatch    = 20
	extractsInterval = 60 * time.Millisecond // about 16 batches a second
	// maxBackoff caps how far a single throttled response can push the whole
	// fleet's next request. Without it, one 429 carrying a large Retry-After
	// stalls every worker for that entire duration, and the limiter never
	// learns the server has recovered.
	maxBackoff = 30 * time.Second
)

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
// no biography yet. Articles are fetched in batches of twenty through the
// MediaWiki extracts API, spread over a pool of workers, and the lead paragraph
// of each is kept as the biography.
//
// checkpoint is called periodically so the caller can persist partial results:
// the fetch takes several minutes and losing it to an interruption would mean
// starting over.
func FetchBios(client *http.Client, userAgent string, artists map[string]*Artist, workers int, logf func(string, ...any), checkpoint func()) error {
	if workers <= 0 {
		workers = 8
	}

	// Group artists by the title sent to the API. Two artists can point at the
	// same article, and one request covers both.
	bySend := map[string][]*Artist{}
	var order []string
	for _, a := range artists {
		if a.Wiki == "" || a.Overview != "" {
			continue
		}
		title := sendTitle(a.Wiki)
		if _, ok := bySend[title]; !ok {
			order = append(order, title)
		}
		bySend[title] = append(bySend[title], a)
	}
	if len(order) == 0 {
		return nil
	}

	var batches [][]string
	for i := 0; i < len(order); i += extractsBatch {
		batches = append(batches, order[i:min(i+extractsBatch, len(order))])
	}
	if logf != nil {
		logf("  fetching biographies for %d articles in %d batches", len(order), len(batches))
	}

	limiter := ratelimit.New(extractsInterval)
	ctx := context.Background()
	jobs := make(chan []string)
	// Workers only fetch; a single collector applies every result to the artist
	// map. That keeps all mutation of Artist.Overview and all reads of it by the
	// checkpoint on one goroutine, so the biography written to the cache can
	// never be a torn read of a string a worker was mutating at that instant.
	results := make(chan batchResult, workers)
	var missedBatches int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for titles := range jobs {
				extracts, err := fetchExtracts(ctx, client, limiter, userAgent, titles)
				if err != nil {
					atomic.AddInt64(&missedBatches, 1)
				}
				results <- batchResult{extracts: extracts}
			}
		}()
	}
	go func() {
		for _, b := range batches {
			jobs <- b
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	done, gotBios := 0, 0
	for r := range results {
		for title, text := range r.extracts {
			for _, a := range bySend[title] {
				a.Overview = text
				gotBios++
			}
		}
		done++
		if done%200 == 0 {
			if logf != nil {
				logf("  biographies: %d/%d articles, %d with text (%d batches unreachable)",
					done*extractsBatch, len(order), gotBios, atomic.LoadInt64(&missedBatches))
			}
			if checkpoint != nil {
				checkpoint()
			}
		}
	}
	if logf != nil {
		logf("  biographies: %d articles resolved to text", gotBios)
	}
	return nil
}

// batchResult carries one batch's fetched extracts from a worker to the single
// collector that applies them.
type batchResult struct {
	extracts map[string]string
}

// sendTitle turns a stored article title into the form the API is queried with.
// The stored value is percent-encoded as Wikipedia's URL had it; decoding it
// yields the raw title, and the query encoding is applied once when the request
// is built, so "Sigur_R%C3%B3s" is not double-encoded into a title that does
// not exist.
func sendTitle(stored string) string {
	if decoded, err := url.PathUnescape(stored); err == nil {
		return decoded
	}
	return stored
}

// extractsResponse is the part of an extracts result this reads. Pages are
// keyed by page id; titles are normalised and redirected separately, so the
// title on a page is the final resolved one.
type extractsResponse struct {
	Query struct {
		Normalized []titleMapping `json:"normalized"`
		Redirects  []titleMapping `json:"redirects"`
		Pages      map[string]struct {
			Title   string `json:"title"`
			Extract string `json:"extract"`
		} `json:"pages"`
	} `json:"query"`
}

type titleMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// fetchExtracts fetches the lead extracts for a batch of titles, returning each
// requested title mapped to its biography. A non-nil error is a retryable
// throttle or network failure. The whole fleet slows through the shared limiter
// on a throttle, capped so a hostile Retry-After cannot stall the run.
func fetchExtracts(ctx context.Context, client *http.Client, limiter *ratelimit.Limiter, userAgent string, titles []string) (map[string]string, error) {
	params := url.Values{
		"action":      {"query"},
		"format":      {"json"},
		"prop":        {"extracts"},
		"exintro":     {"1"},
		"explaintext": {"1"},
		"exlimit":     {"20"},
		"redirects":   {"1"},
		"titles":      {strings.Join(titles, "|")},
	}
	endpoint := extractsEndpoint + "?" + params.Encode()

	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		parsed, retryAfter, err := tryExtracts(ctx, client, endpoint, userAgent)
		if err == nil {
			return resolveExtracts(titles, parsed), nil
		}
		lastErr = err
		if retryAfter <= 0 || retryAfter > maxBackoff {
			retryAfter = min(time.Duration(attempt+1)*time.Second, maxBackoff)
		}
		limiter.Backoff(retryAfter)
	}
	return nil, lastErr
}

func tryExtracts(ctx context.Context, client *http.Client, endpoint, userAgent string) (*extractsResponse, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return nil, parseRetryAfter(resp.Header.Get("Retry-After")), errThrottled
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, 0, errThrottled
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	var parsed extractsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, err
	}
	return &parsed, 0, nil
}

// resolveExtracts maps each requested title to its biography, following the
// normalisation and redirect the API reports so a request for "The_Beatles"
// finds the page titled "The Beatles".
func resolveExtracts(sent []string, resp *extractsResponse) map[string]string {
	normalized := map[string]string{}
	for _, n := range resp.Query.Normalized {
		normalized[n.From] = n.To
	}
	redirected := map[string]string{}
	for _, r := range resp.Query.Redirects {
		redirected[r.From] = r.To
	}
	byTitle := map[string]string{}
	for _, p := range resp.Query.Pages {
		if bio := leadParagraph(p.Extract); bio != "" {
			byTitle[p.Title] = bio
		}
	}

	out := map[string]string{}
	for _, s := range sent {
		title := s
		if to, ok := normalized[title]; ok {
			title = to
		}
		// Redirects can chain; resolve a few hops rather than assume one.
		for hop := 0; hop < 4; hop++ {
			to, ok := redirected[title]
			if !ok {
				break
			}
			title = to
		}
		if bio, ok := byTitle[title]; ok {
			out[s] = bio
		}
	}
	return out
}

// leadParagraph keeps an extract's first paragraph, which is the concise
// summary of the article, and drops the rest of the lead section. A very long
// paragraph is trimmed at a sentence boundary so a biography stays a blurb
// rather than an essay.
func leadParagraph(extract string) string {
	if extract == "" {
		return ""
	}
	para := strings.TrimSpace(extract)
	if i := strings.IndexByte(para, '\n'); i >= 0 {
		para = strings.TrimSpace(para[:i])
	}
	const maxLen = 1500
	if len(para) > maxLen {
		cut := strings.LastIndex(para[:maxLen], ". ")
		if cut > 0 {
			para = para[:cut+1]
		} else {
			para = para[:maxLen]
		}
	}
	return para
}

// errThrottled marks a response worth retrying.
var errThrottled = errThrottledType{}

type errThrottledType struct{}

func (errThrottledType) Error() string { return "throttled or server error" }

// parseRetryAfter reads a Retry-After header expressed in seconds. The date
// form is ignored; the caller's fixed backoff covers it.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
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
