// Package enrich fills the two artist fields a MusicBrainz export does not
// carry: a photo and a biography.
//
// MusicBrainz stores neither. What it does store is the relationship that
// points at them, a Wikidata item id, and the pipeline already extracts it.
// From that id Wikidata gives an image on Wikimedia Commons and the title of
// the artist's English Wikipedia article, and Wikipedia gives the article's
// summary. All three sources are open data, joined to MusicBrainz by MBID.
//
// This runs on our build machines, never a user's, and its output is baked
// into the dataset. The join key is queried in bulk (a few requests); only the
// biographies are fetched one article at a time, so those are cached between
// builds and only refetched when an artist is new or its article changed. A
// weekly dump therefore pays the biography cost once, not every time.
package enrich

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Artist is one artist's enrichment, keyed by MusicBrainz artist MBID.
//
// Image is a Wikimedia Commons address the pipeline turns into an image URL.
// Wiki is the English Wikipedia article title, kept because it is both the key
// for fetching Overview and the signal for whether a cached Overview is still
// current: if the article an artist points at changes, the cached summary is
// stale.
type Artist struct {
	MBID     string `json:"mbid"`
	Image    string `json:"image,omitempty"`
	Wiki     string `json:"wiki,omitempty"`
	Overview string `json:"overview,omitempty"`
}

// Load reads an enrichment file, returning an empty set if it does not yet
// exist so the first build starts from nothing rather than an error.
func Load(path string) (map[string]*Artist, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]*Artist{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]*Artist{}
	scanner := bufio.NewScanner(f)
	// Overviews can be long, so a line may exceed bufio's default limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var a Artist
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			return nil, err
		}
		out[a.MBID] = &a
	}
	return out, scanner.Err()
}

// Save writes an enrichment set as one JSON object per line, sorted by MBID so
// a rebuilt file is stable in version control and diffs cleanly. It writes to a
// temporary file and renames, so an interrupted save cannot corrupt the cache
// a future build depends on.
func Save(path string, artists map[string]*Artist) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)

	for _, mbid := range sortedKeys(artists) {
		a := artists[mbid]
		// An entry with neither an image nor an article carries nothing worth
		// storing and would only bloat the cache.
		if a.Image == "" && a.Wiki == "" && a.Overview == "" {
			continue
		}
		line, err := json.Marshal(a)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortedKeys(m map[string]*Artist) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
