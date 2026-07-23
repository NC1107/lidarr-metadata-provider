package enrich

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// thumbWidth is the artist image size stored. A fixed width keeps the URL
// clean, which matters: Lidarr derives the cached filename from the URL and a
// query string like ?width=500 ends up in the filename, breaking display.
const thumbWidth = 500

const wdqsEndpoint = "https://query.wikidata.org/sparql"

// Harvest pulls, for every Wikidata item that records a MusicBrainz artist id,
// its image and its English Wikipedia article title.
//
// The whole set is a few hundred thousand items, too many for one query under
// the query service's time limit, so it is fetched in sixteen slices split on
// the first hex digit of the MBID. Each slice is well within the limit and the
// union is exhaustive because every MBID starts with a hex digit.
//
// The join runs the other way from what the pipeline knows: Wikidata is asked
// for everything carrying property P434 (MusicBrainz artist id) rather than
// asked about each of our artists in turn, which would be millions of
// requests. The result is matched back to our artists by MBID.
func Harvest(client *http.Client, userAgent string, logf func(string, ...any)) (map[string]*Artist, error) {
	out := map[string]*Artist{}
	const hex = "0123456789abcdef"
	for _, prefix := range hex {
		rows, err := harvestPrefixRetry(client, userAgent, string(prefix))
		if err != nil {
			return nil, fmt.Errorf("harvesting MBIDs starting %q: %w", string(prefix), err)
		}
		for mbid, a := range rows {
			out[mbid] = a
		}
		if logf != nil {
			logf("  wikidata %q: %d items, %d total", string(prefix), len(rows), len(out))
		}
		// The query service is a shared free resource; do not machine-gun it.
		time.Sleep(time.Second)
	}
	return out, nil
}

// harvestPrefixRetry wraps harvestPrefix with bounded backoff. The query
// service is a best-effort shared resource that occasionally times out or
// throttles; without this a single hiccup on any of the sixteen slices aborts a
// multi-hour unattended build, the same way the Wikipedia fetch already guards
// itself.
func harvestPrefixRetry(client *http.Client, userAgent, prefix string) (map[string]*Artist, error) {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rows, err := harvestPrefix(client, userAgent, prefix)
		if err == nil {
			return rows, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
	}
	return nil, lastErr
}

// sparqlResults is the shape of a SPARQL JSON result set, narrowed to the
// bindings this harvest reads.
type sparqlResults struct {
	Results struct {
		Bindings []map[string]struct {
			Value string `json:"value"`
		} `json:"bindings"`
	} `json:"results"`
}

func harvestPrefix(client *http.Client, userAgent, prefix string) (map[string]*Artist, error) {
	query := fmt.Sprintf(`SELECT ?mbid ?img ?article WHERE {
  ?item wdt:P434 ?mbid .
  FILTER(STRSTARTS(?mbid, %q))
  OPTIONAL { ?item wdt:P18 ?img }
  OPTIONAL { ?article schema:about ?item ; schema:isPartOf <https://en.wikipedia.org/> }
}`, prefix)

	req, err := http.NewRequest(http.MethodGet, wdqsEndpoint+"?"+url.Values{"query": {query}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, truncate(string(body), 200))
	}

	var parsed sparqlResults
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	// An item can carry several images; take the lexically first so a rebuild
	// is deterministic rather than dependent on result order.
	images := map[string][]string{}
	out := map[string]*Artist{}
	for _, b := range parsed.Results.Bindings {
		mbid := strings.ToLower(b["mbid"].Value)
		if mbid == "" {
			continue
		}
		a := out[mbid]
		if a == nil {
			a = &Artist{MBID: mbid}
			out[mbid] = a
		}
		// A P18 statement can be an unknown-value placeholder, which the query
		// service returns as a blank-node URL rather than a file. Keep only real
		// Commons files.
		if img := b["img"].Value; strings.Contains(img, "commons.wikimedia.org/") {
			images[mbid] = append(images[mbid], img)
		}
		if art := b["article"].Value; art != "" && a.Wiki == "" {
			a.Wiki = wikiTitle(art)
		}
	}
	for mbid, imgs := range images {
		sort.Strings(imgs)
		out[mbid].Image = commonsThumb(imgs[0])
	}
	return out, nil
}

// wikiTitle extracts the article title from a Wikipedia URL, which is the key
// the summary API is addressed by. The stored title keeps percent-encoding as
// Wikipedia gives it, so it can be placed straight into a summary request.
func wikiTitle(article string) string {
	if i := strings.LastIndex(article, "/wiki/"); i >= 0 {
		return article[i+len("/wiki/"):]
	}
	if i := strings.LastIndexByte(article, '/'); i >= 0 {
		return article[i+1:]
	}
	return article
}

// commonsThumb turns a Wikidata P18 value into a direct Wikimedia thumbnail
// URL of a fixed width, ending in a clean image extension with no query string.
//
// A thumbnail's address on Commons is deterministic: the file lives under a
// two-level directory named by the md5 of its filename, and the thumbnail sits
// beside it under thumb/ with a "<width>px-" prefix. Building the address here
// rather than linking Special:FilePath?width= avoids the query string, which
// Lidarr folds into the cached filename and then cannot serve, and avoids a
// redirect on every image load. Spaces are underscores in these paths; the rest
// of the filename keeps the encoding Wikidata already gave it, which is the same
// encoding Commons uses, so no re-encoding can drift.
func commonsThumb(raw string) string {
	const marker = "/Special:FilePath/"
	i := strings.Index(raw, marker)
	if i < 0 {
		// Not the expected form; fall back to the https file address.
		return strings.Replace(raw, "http://commons.wikimedia.org/", "https://commons.wikimedia.org/", 1)
	}
	encoded := raw[i+len(marker):]
	if q := strings.IndexByte(encoded, '?'); q >= 0 {
		encoded = encoded[:q]
	}
	// Commons paths use underscores for spaces where a URL would use %20.
	seg := strings.ReplaceAll(encoded, "%20", "_")

	name, err := url.PathUnescape(seg)
	if err != nil {
		return strings.Replace(raw, "http://commons.wikimedia.org/", "https://commons.wikimedia.org/", 1)
	}
	sum := md5.Sum([]byte(name))
	h := hex.EncodeToString(sum[:])

	prefix := fmt.Sprintf("%dpx-%s", thumbWidth, seg)
	// A few formats are rendered to a different thumbnail type.
	switch strings.ToLower(name[strings.LastIndexByte(name, '.')+1:]) {
	case "svg":
		prefix = fmt.Sprintf("%dpx-%s.png", thumbWidth, seg)
	case "tif", "tiff":
		prefix = fmt.Sprintf("lossy-page1-%dpx-%s.jpg", thumbWidth, seg)
	}
	return fmt.Sprintf("https://upload.wikimedia.org/wikipedia/commons/thumb/%s/%s/%s/%s",
		h[0:1], h[0:2], seg, prefix)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
