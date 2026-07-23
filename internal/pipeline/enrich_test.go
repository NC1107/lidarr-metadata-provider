package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// buildAll runs a full build over the album fixture with the given enrichment,
// returning both the artists and albums so a test can assert across them.
func buildAll(t *testing.T, artistEnrich map[string]ArtistEnrichment) (map[string]*skyhook.ArtistResource, map[string]*skyhook.AlbumResource) {
	t.Helper()
	tables := albumTables()

	core, err := mbdump.Open(fakeExport(t, albumCoreTables, tables))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := mbdump.Open(fakeExport(t, derivedTables, tables))
	if err != nil {
		t.Fatal(err)
	}

	artists := map[string]*skyhook.ArtistResource{}
	albums := map[string]*skyhook.AlbumResource{}
	err = BuildAll(core, derived, nil, filepath.Join(t.TempDir(), "staging.db"), artistEnrich, Emitter{
		Artist: func(a *skyhook.ArtistResource) error { artists[a.ID] = a; return nil },
		Album:  func(a *skyhook.AlbumResource) error { albums[a.ID] = a; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return artists, albums
}

// TestEnrichmentPopulatesArtistImageAndBiography checks that the image and
// biography gathered outside the export reach the full artist payload, and only
// it: the skeletal album-artist shape stays bare so album payloads do not
// balloon.
func TestEnrichmentPopulatesArtistImageAndBiography(t *testing.T) {
	const bio = "The La's were an English rock band from Liverpool."
	enrich := map[string]ArtistEnrichment{
		artistGID: {
			Image:    "https://commons.wikimedia.org/wiki/Special:FilePath/The%20La%27s.jpg",
			Overview: bio,
		},
	}
	artists, albums := buildAll(t, enrich)

	a := artists[artistGID]
	if a == nil {
		t.Fatal("the enriched artist was not built")
	}
	if len(a.Images) != 1 {
		t.Fatalf("enriched artist got %d images, want 1", len(a.Images))
	}
	img := a.Images[0]
	if img.CoverType != "Poster" {
		t.Errorf("image CoverType = %q, want Poster", img.CoverType)
	}
	want := "https://commons.wikimedia.org/wiki/Special:FilePath/The%20La%27s.jpg?width=500"
	if img.URL != want || img.RemoteURL != want {
		t.Errorf("image URL = %q / %q, want %q", img.URL, img.RemoteURL, want)
	}
	if a.Overview == nil || *a.Overview != bio {
		t.Errorf("Overview = %v, want %q", a.Overview, bio)
	}

	// The same artist embedded inside its album must not carry the image or
	// biography, matching how upstream keeps album payloads small.
	album := albums[albumGID]
	if album == nil || len(album.Artists) == 0 {
		t.Fatal("the album or its artist list was not built")
	}
	embedded := album.Artists[0]
	if len(embedded.Images) != 0 {
		t.Errorf("embedded album-artist carried %d images, want 0", len(embedded.Images))
	}
	if embedded.Overview != nil {
		t.Errorf("embedded album-artist carried an overview, want none")
	}
}

// TestUnenrichedArtistHasEmptyImagesNotNull guards the JSON contract: an artist
// with no enrichment must serialise "images": [] rather than null.
func TestUnenrichedArtistHasEmptyImagesNotNull(t *testing.T) {
	artists, _ := buildAll(t, nil)
	a := artists[artistGID]
	if a == nil {
		t.Fatal("artist was not built")
	}
	if a.Images == nil {
		t.Error("Images is nil, must be an empty slice so it serialises as []")
	}
	if len(a.Images) != 0 {
		t.Errorf("unenriched artist got %d images, want 0", len(a.Images))
	}
	if a.Overview != nil {
		t.Errorf("unenriched artist got an overview, want none")
	}
}

// TestAlbumRatingFromReleaseGroupMeta checks the album rating is read from the
// derived archive and scaled from MusicBrainz's 0-100 to the contract's 0-10.
func TestAlbumRatingFromReleaseGroupMeta(t *testing.T) {
	_, albums := buildAll(t, nil)
	album := albums[albumGID]
	if album == nil {
		t.Fatal("album was not built")
	}
	if album.Rating.Value == nil || *album.Rating.Value != 8.0 || album.Rating.Count != 12 {
		t.Errorf("album Rating = %+v, want 8.0 over 12 votes", album.Rating)
	}

	// An album with no rating row reports a zero-count, null-value rating
	// rather than a fabricated score.
	live := albums[liveGID]
	if live == nil {
		t.Fatal("second album was not built")
	}
	if live.Rating.Value != nil || live.Rating.Count != 0 {
		t.Errorf("unrated album Rating = %+v, want empty", live.Rating)
	}
}
