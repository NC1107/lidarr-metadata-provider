package enrich

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWikiTitleExtractsArticle(t *testing.T) {
	cases := map[string]string{
		"https://en.wikipedia.org/wiki/The_Beatles":    "The_Beatles",
		"https://en.wikipedia.org/wiki/AC/DC":          "AC/DC",
		"https://en.wikipedia.org/wiki/Sigur_R%C3%B3s": "Sigur_R%C3%B3s",
		"Radiohead": "Radiohead",
	}
	for in, want := range cases {
		if got := wikiTitle(in); got != want {
			t.Errorf("wikiTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCommonsThumb checks the P18 value becomes a direct thumbnail URL ending
// in a clean image extension, with spaces as underscores and the md5-based
// two-level directory. The expected URLs were confirmed to return the image.
func TestCommonsThumb(t *testing.T) {
	cases := map[string]string{
		"http://commons.wikimedia.org/wiki/Special:FilePath/RadioheadO2211125%20composite.jpg": "https://upload.wikimedia.org/wikipedia/commons/thumb/a/a1/RadioheadO2211125_composite.jpg/500px-RadioheadO2211125_composite.jpg",
		"http://commons.wikimedia.org/wiki/Special:FilePath/Beatles%20Trenter%201963.jpg":      "https://upload.wikimedia.org/wikipedia/commons/thumb/4/46/Beatles_Trenter_1963.jpg/500px-Beatles_Trenter_1963.jpg",
	}
	for in, want := range cases {
		if got := commonsThumb(in); got != want {
			t.Errorf("commonsThumb(%q)\n = %q\nwant %q", in, got, want)
		}
	}
	// A special char (parenthesis) keeps Wikidata's encoding.
	got := commonsThumb("http://commons.wikimedia.org/wiki/Special:FilePath/Peter%20Steen%20%28skuespiller%29.jpg")
	if !strings.Contains(got, "/Peter_Steen_%28skuespiller%29.jpg/500px-Peter_Steen_%28skuespiller%29.jpg") {
		t.Errorf("parenthesised name encoded wrong: %q", got)
	}
	// An svg renders to a png thumbnail.
	svg := commonsThumb("http://commons.wikimedia.org/wiki/Special:FilePath/Some%20Logo.svg")
	if !strings.HasSuffix(svg, ".svg.png") {
		t.Errorf("svg thumbnail should end .svg.png: %q", svg)
	}
}

// TestCarryOverviewsReusesUnchangedArticles is the mechanism that makes a
// rebuild cheap: a biography is carried forward only when the artist still
// points at the same article, and refetched when the article changed.
func TestCarryOverviewsReusesUnchangedArticles(t *testing.T) {
	cached := map[string]*Artist{
		"same":    {MBID: "same", Wiki: "Article_A", Overview: "kept"},
		"changed": {MBID: "changed", Wiki: "Old_Article", Overview: "stale"},
		"gone":    {MBID: "gone", Wiki: "Article_C", Overview: "orphan"},
	}
	fresh := map[string]*Artist{
		"same":    {MBID: "same", Wiki: "Article_A"},
		"changed": {MBID: "changed", Wiki: "New_Article"},
		"new":     {MBID: "new", Wiki: "Article_D"},
	}

	carried := CarryOverviews(fresh, cached)
	if carried != 1 {
		t.Fatalf("carried %d, want 1", carried)
	}
	if fresh["same"].Overview != "kept" {
		t.Errorf("unchanged article lost its cached overview: %q", fresh["same"].Overview)
	}
	if fresh["changed"].Overview != "" {
		t.Errorf("changed article kept a stale overview: %q", fresh["changed"].Overview)
	}
	if fresh["new"].Overview != "" {
		t.Errorf("new artist should have no overview yet: %q", fresh["new"].Overview)
	}
}

// TestLoadSaveRoundTrip checks the cache survives a write and read, and that
// entries carrying nothing worth storing are dropped so the file stays lean.
func TestLoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artists.jsonl")
	in := map[string]*Artist{
		"a":     {MBID: "a", Image: "img", Wiki: "W", Overview: "bio"},
		"b":     {MBID: "b", Wiki: "OnlyArticle"},
		"empty": {MBID: "empty"},
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["empty"]; ok {
		t.Error("an entry with no image, article or biography should not be stored")
	}
	if a := out["a"]; a == nil || a.Image != "img" || a.Overview != "bio" || a.Wiki != "W" {
		t.Errorf("round-tripped entry a = %+v", a)
	}
	if b := out["b"]; b == nil || b.Wiki != "OnlyArticle" {
		t.Errorf("round-tripped entry b = %+v", b)
	}
}

func TestSendTitleDecodesEncoding(t *testing.T) {
	cases := map[string]string{
		"The_Beatles":    "The_Beatles",
		"Sigur_R%C3%B3s": "Sigur_Rós",
		"AC/DC":          "AC/DC",
	}
	for in, want := range cases {
		if got := sendTitle(in); got != want {
			t.Errorf("sendTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeadParagraphKeepsFirstParagraph(t *testing.T) {
	extract := "Radiohead are an English rock band. They formed in 1985.\nTheir second paragraph.\nA third."
	got := leadParagraph(extract)
	want := "Radiohead are an English rock band. They formed in 1985."
	if got != want {
		t.Errorf("leadParagraph = %q, want %q", got, want)
	}
	if leadParagraph("") != "" {
		t.Error("empty extract should stay empty")
	}
}

// TestResolveExtractsFollowsNormalizationAndRedirects checks a requested title
// finds its page after the API normalises underscores and follows a redirect,
// which is how most real titles resolve.
func TestResolveExtractsFollowsNormalizationAndRedirects(t *testing.T) {
	resp := &extractsResponse{}
	resp.Query.Normalized = []titleMapping{{From: "The_Beatles", To: "The Beatles"}}
	resp.Query.Redirects = []titleMapping{{From: "The Beatles", To: "Beatles"}}
	resp.Query.Pages = map[string]struct {
		Title   string `json:"title"`
		Extract string `json:"extract"`
	}{
		"1": {Title: "Beatles", Extract: "The Beatles were an English rock band.\nMore."},
		"2": {Title: "Radiohead", Extract: "Radiohead are an English rock band.\nMore."},
	}

	out := resolveExtracts([]string{"The_Beatles", "Radiohead", "Nonexistent"}, resp)
	if got := out["The_Beatles"]; got != "The Beatles were an English rock band." {
		t.Errorf("redirected title resolved to %q", got)
	}
	if got := out["Radiohead"]; got != "Radiohead are an English rock band." {
		t.Errorf("direct title resolved to %q", got)
	}
	if _, ok := out["Nonexistent"]; ok {
		t.Error("a title with no page should not appear in the result")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	out, err := Load(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("loading a missing file should not error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("missing file yielded %d entries, want 0", len(out))
	}
}
