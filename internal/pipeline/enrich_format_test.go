package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTitleCaseReproducesEveryFixtureGenre drives the casing rule from the
// golden captures rather than a hand-picked list: every genre string the live
// service has ever returned must be reproduced by titleCase(strings.ToLower(g)).
// This is the guard that was missing when the space-only boundary bug shipped,
// and it auto-covers any genre a future fixture capture adds.
func TestTitleCaseReproducesEveryFixtureGenre(t *testing.T) {
	files, err := filepath.Glob("../../fixtures/v0.4/*.json")
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures found under ../../fixtures/v0.4")
	}
	genres := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			continue // not every fixture is entity JSON
		}
		collectFixtureGenres(v, genres)
	}
	if len(genres) == 0 {
		t.Fatal("extracted no genres from fixtures")
	}
	for g := range genres {
		if got := titleCase(strings.ToLower(g)); got != g {
			t.Errorf("titleCase(lower(%q)) = %q, want %q (upstream ground truth)", g, got, g)
		}
	}
}

// collectFixtureGenres walks arbitrary decoded fixture JSON and records every
// string in any "genres" array, at any nesting depth.
func collectFixtureGenres(v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "genres") {
				if arr, ok := val.([]any); ok {
					for _, e := range arr {
						if s, ok := e.(string); ok && s != "" {
							out[s] = true
						}
					}
				}
			}
			collectFixtureGenres(val, out)
		}
	case []any:
		for _, e := range x {
			collectFixtureGenres(e, out)
		}
	}
}

// TestTitleCaseMatchesUpstream pins genre casing to what the golden fixtures
// show, including the hyphen and ampersand boundaries that were wrong before.
func TestTitleCaseMatchesUpstream(t *testing.T) {
	cases := map[string]string{
		"r&b":               "R&B",
		"contemporary r&b":  "Contemporary R&B",
		"j-pop":             "J-Pop",
		"dance-pop":         "Dance-Pop",
		"singer-songwriter": "Singer-Songwriter",
		"lo-fi":             "Lo-Fi",
		"pop rock":          "Pop Rock",
		"pop":               "Pop",
		// Upstream flattens acronyms to plain per-word casing rather than
		// special-casing them: the live service returns "Idm", not "IDM"
		// (captured in the Radiohead fixtures). Reproducing that naive
		// behavior is correct; a canonical-acronym table would diverge.
		"idm": "Idm",
	}
	for in, want := range cases {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLinkTypeMatchesUpstream pins link typing to the compound-suffix behavior
// the fixtures prove: only co.uk and co.jp are stripped.
func TestLinkTypeMatchesUpstream(t *testing.T) {
	cases := map[string]string{
		"https://www.discogs.com/artist/1":     "discogs",
		"https://bbc.co.uk/music":              "bbc",
		"https://amazon.co.uk/x":               "amazon",
		"https://music.amazon.co.jp/x":         "amazon",
		"https://cdjapan.co.jp/x":              "cdjapan",
		"https://nla.gov.au/nla.party-1212427": "gov",
		"https://music.bugs.co.kr/x":           "co",
		"https://genie.co.kr/x":                "co",
		"https://ci.nii.ac.jp/x":               "ac",
		"https://id.ndl.go.jp/x":               "go",
		"https://andremehmari.com.br/x":        "com",
		"https://id.loc.gov/x":                 "loc",
		"http://user:pass@example.com/x":       "example",
		"https://example.com./x":               "example",
		// A path can carry an '@' (social handles); it is not userinfo, so the
		// type stays the service, not the handle.
		"https://www.tiktok.com/@thebeatles":  "tiktok",
		"https://www.threads.com/@thebeatles": "threads",
		"https://www.instagram.com/rihanna":   "instagram",
	}
	for in, want := range cases {
		if got := linkType(in); got != want {
			t.Errorf("linkType(%q) = %q, want %q", in, got, want)
		}
	}
}
