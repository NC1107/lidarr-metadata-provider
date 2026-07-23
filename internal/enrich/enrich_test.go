package enrich

import (
	"path/filepath"
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

func TestCommonsFilePathForcesHTTPS(t *testing.T) {
	in := "http://commons.wikimedia.org/wiki/Special:FilePath/Radiohead.jpg"
	want := "https://commons.wikimedia.org/wiki/Special:FilePath/Radiohead.jpg"
	if got := commonsFilePath(in); got != want {
		t.Errorf("commonsFilePath(%q) = %q, want %q", in, got, want)
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

func TestLoadMissingFileIsEmpty(t *testing.T) {
	out, err := Load(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("loading a missing file should not error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("missing file yielded %d entries, want 0", len(out))
	}
}
