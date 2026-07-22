package skyhook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func album(typ string, secondary, statuses []string) ArtistAlbumResource {
	return ArtistAlbumResource{Type: typ, SecondaryTypes: secondary, ReleaseStatuses: statuses}
}

func TestStandardProfileAllows(t *testing.T) {
	official := []string{"Official"}
	cases := []struct {
		name string
		in   ArtistAlbumResource
		want bool
	}{
		{"studio album", album("Album", nil, official), true},
		{"empty secondary types count as studio", album("Album", []string{}, official), true},
		{"wrong primary type", album("EP", nil, official), false},
		{"disallowed secondary type", album("Album", []string{"Live"}, official), false},
		{"bootleg only", album("Album", nil, []string{"Bootleg"}), false},
		{"official among several statuses", album("Album", nil, []string{"Bootleg", "Official"}), true},
		// The dataset failure mode worth a named test: everything correct
		// except an unpopulated ReleaseStatuses makes the album vanish.
		{"empty release statuses", album("Album", nil, []string{}), false},
		{"nil release statuses", album("Album", nil, nil), false},
		{"empty primary type", album("", nil, official), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StandardProfile.Allows(c.in); got != c.want {
				t.Errorf("Allows(%+v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestProfileWithMultipleAllowedTypes(t *testing.T) {
	p := MetadataProfile{
		PrimaryTypes:    []string{"Album", "EP"},
		SecondaryTypes:  []string{"Studio", "Live"},
		ReleaseStatuses: []string{"Official", "Promotion"},
	}
	if !p.Allows(album("EP", []string{"Live"}, []string{"Promotion"})) {
		t.Error("expected a live promotional EP to pass a profile allowing all three")
	}
	if p.Allows(album("Single", nil, []string{"Official"})) {
		t.Error("expected a single to fail a profile that does not allow singles")
	}
}

// Locks in the survivor counts quoted in the docs, so a mapping change that
// silently guts an artist's album list fails here.
func TestStandardProfileSurvivorCountsAgainstFixtures(t *testing.T) {
	cases := []struct {
		file  string
		total int
		kept  int
	}{
		{"artist_b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d_beatles.json", 1019, 18},
		{"artist_070d193a-845c-479f-980e-bef15710653e_prince.json", 637, 36},
		{"artist_24f1766e-9635-4d58-a4d4-9413f9f98a4c_bach.json", 5668, 4487},
		{"artist_b539e453-c4fe-47e3-8a07-8517eac74429_utada-hikaru.json", 105, 11},
		{"artist_ff3e88b3-7354-4f30-967c-1a61ebc8c642_the-las.json", 20, 4},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, c.file))
			if err != nil {
				t.Fatal(err)
			}
			var artist ArtistResource
			if err := json.Unmarshal(raw, &artist); err != nil {
				t.Fatal(err)
			}
			if len(artist.Albums) != c.total {
				t.Errorf("fixture has %d albums, expected %d", len(artist.Albums), c.total)
			}
			if kept := len(StandardProfile.Filter(artist.Albums)); kept != c.kept {
				t.Errorf("Standard profile keeps %d albums, expected %d", kept, c.kept)
			}
		})
	}
}
