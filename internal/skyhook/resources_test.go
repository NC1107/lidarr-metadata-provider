package skyhook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../fixtures/v0.4"

// TestFixtureRoundTrip proves the structs and the golden fixtures agree on
// the full contract: ContractDiff fails on keys upstream emits that the
// structs lack, keys the structs would add, and casing, type or value drift.
func TestFixtureRoundTrip(t *testing.T) {
	cases := []struct {
		glob string
		new  func() any
	}{
		{"artist_*.json", func() any { return &ArtistResource{} }},
		{"album_*.json", func() any { return &AlbumResource{} }},
		{"search-type-artist_*.json", func() any { return &[]ArtistResource{} }},
		{"search-artist_radiohead.json", func() any { return &[]ArtistResource{} }},
		{"search-type-album_*.json", func() any { return &[]AlbumResource{} }},
		{"search-type-all_*.json", func() any { return &[]EntityResource{} }},
		{"recent-*.json", func() any { return &RecentUpdatesResource{} }},
		{"root.json", func() any { return &ServerInfo{} }},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		paths, err := filepath.Glob(filepath.Join(fixtureDir, c.glob))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) == 0 {
			t.Errorf("no fixtures match %s", c.glob)
		}
		for _, path := range paths {
			seen[filepath.Base(path)] = true
			t.Run(filepath.Base(path), func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				diffs, err := ContractDiff(raw, c.new())
				if err != nil {
					t.Fatal(err)
				}
				if len(diffs) > 20 {
					diffs = append(diffs[:20], fmt.Sprintf("... and %d more", len(diffs)-20))
				}
				if len(diffs) > 0 {
					t.Errorf("re-marshalled JSON differs from fixture:\n%s", strings.Join(diffs, "\n"))
				}
			})
		}
	}

	// Every fixture in the directory must be covered by some case; an
	// unmatched file means an untested part of the contract.
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !seen[e.Name()] {
			t.Errorf("fixture %s is not covered by any round-trip case", e.Name())
		}
	}
}
