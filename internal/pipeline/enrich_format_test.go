package pipeline

import "testing"

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
	}
	for in, want := range cases {
		if got := linkType(in); got != want {
			t.Errorf("linkType(%q) = %q, want %q", in, got, want)
		}
	}
}
