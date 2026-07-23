package enrich

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

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
		rows, err := harvestPrefix(client, userAgent, string(prefix))
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
		out[mbid].Image = commonsFilePath(imgs[0])
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

// commonsFilePath normalises a Wikidata P18 value to an https Special:FilePath
// address. That endpoint redirects to the current file and accepts a width
// parameter, so the pipeline can request a thumbnail without storing image
// bytes or knowing Commons's internal hashing.
func commonsFilePath(raw string) string {
	return strings.Replace(raw, "http://commons.wikimedia.org/", "https://commons.wikimedia.org/", 1)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
