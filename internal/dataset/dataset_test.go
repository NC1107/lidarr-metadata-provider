package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

func str(s string) *string { return &s }

func sampleArtist() *skyhook.ArtistResource {
	return &skyhook.ArtistResource{
		ID:             "ff3e88b3-7354-4f30-967c-1a61ebc8c642",
		OldIDs:         []string{"00000000-dead-beef-0000-000000000000"},
		ArtistName:     "The La's",
		SortName:       "La's, The",
		ArtistAliases:  []string{"The Las"},
		Disambiguation: "UK band",
		Type:           str("Group"),
		Status:         "ended",
		Genres:         []string{},
		Images:         []skyhook.ImageResource{},
		Links:          []skyhook.LinkResource{},
		Rating:         skyhook.RatingResource{Count: 2, Value: nil},
		Albums: []skyhook.ArtistAlbumResource{{
			ID: "f57d03ff-b0a5-3b73-a14c-a5ed5f8cd956", OldIDs: []string{},
			Title: "The La's", Type: "Album", SecondaryTypes: []string{},
			ReleaseStatuses: []string{"Official"}, ReleaseDate: str("1990-10-01"),
		}},
	}
}

func sampleAlbum() *skyhook.AlbumResource {
	return &skyhook.AlbumResource{
		ID: "f57d03ff-b0a5-3b73-a14c-a5ed5f8cd956", OldIDs: []string{},
		Title: "The La's", Aliases: []string{}, Type: "Album",
		SecondaryTypes: []string{}, ReleaseStatuses: []string{"Official"},
		ReleaseDate: str("1990-10-01"), ArtistID: "ff3e88b3-7354-4f30-967c-1a61ebc8c642",
		Artists: []skyhook.AlbumArtistResource{}, Genres: []string{},
		Images: []skyhook.ImageResource{}, Links: []skyhook.LinkResource{},
		Rating: skyhook.RatingResource{},
		Releases: []skyhook.ReleaseResource{{
			ID: "afdd5049-e01e-4741-a971-f96bd449e179", OldIDs: []string{},
			Title: "The La's", Country: []string{"GB"}, Label: []string{"Go! Discs"},
			Status: "Official", Media: []skyhook.MediumResource{}, TrackCount: 2,
			Tracks: []skyhook.TrackResource{
				{ID: "t1", OldIDs: []string{}, OldRecordingIDs: []string{}, TrackName: "Son of a Gun", TrackNumber: "1"},
				{ID: "t2", OldIDs: []string{}, OldRecordingIDs: []string{}, TrackName: "There She Goes", TrackNumber: "2"},
			},
		}},
	}
}

func build(t *testing.T) *Reader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dataset.db")

	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddArtist(sampleArtist()); err != nil {
		t.Fatal(err)
	}
	if err := w.AddAlbum(sampleAlbum()); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish("20260718-002132", 187552); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// The payload a build stores must be byte-for-byte the payload the server
// serves, or the contract tests prove nothing about what users receive.
func TestPayloadSurvivesRoundTrip(t *testing.T) {
	r := build(t)

	got, err := r.Artist(context.Background(), "ff3e88b3-7354-4f30-967c-1a61ebc8c642")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(sampleArtist())
	have, _ := json.Marshal(got)
	if string(want) != string(have) {
		t.Errorf("artist payload changed in storage:\n stored %s\n loaded %s", want, have)
	}

	album, err := r.Album(context.Background(), "f57d03ff-b0a5-3b73-a14c-a5ed5f8cd956")
	if err != nil {
		t.Fatal(err)
	}
	wantAlbum, _ := json.Marshal(sampleAlbum())
	haveAlbum, _ := json.Marshal(album)
	if string(wantAlbum) != string(haveAlbum) {
		t.Errorf("album payload changed in storage:\n stored %s\n loaded %s", wantAlbum, haveAlbum)
	}
}

func TestStoredPayloadStillMatchesTheContract(t *testing.T) {
	r := build(t)
	got, err := r.Artist(context.Background(), "ff3e88b3-7354-4f30-967c-1a61ebc8c642")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	diffs, err := skyhook.ContractDiff(raw, &skyhook.ArtistResource{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("stored payload drifted from the contract: %v", diffs)
	}
}

// Lidarr keeps MBIDs for the life of a library, so an id retired by a
// MusicBrainz merge has to keep resolving.
func TestRetiredMBIDResolvesToItsReplacement(t *testing.T) {
	r := build(t)
	got, err := r.Artist(context.Background(), "00000000-dead-beef-0000-000000000000")
	if err != nil {
		t.Fatalf("retired MBID did not resolve: %v", err)
	}
	if got.ID != "ff3e88b3-7354-4f30-967c-1a61ebc8c642" {
		t.Errorf("resolved to %s", got.ID)
	}
}

func TestMissingMBIDReportsNotFound(t *testing.T) {
	r := build(t)
	_, err := r.Artist(context.Background(), "99999999-0000-0000-0000-000000000000")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("expected source.ErrNotFound so the chain falls through, got %v", err)
	}
}

func TestSearch(t *testing.T) {
	r := build(t)

	artists, err := r.SearchArtists(context.Background(), "la's", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 1 || artists[0].ArtistName != "The La's" {
		t.Errorf("artist search returned %d results: %+v", len(artists), artists)
	}

	albums, err := r.SearchAlbums(context.Background(), "the la's", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 {
		t.Errorf("album search returned %d results", len(albums))
	}
}

// Punctuation is FTS5 syntax. A user searching for a real band name must get
// a search rather than a query error.
func TestSearchHandlesPunctuationWithoutErroring(t *testing.T) {
	r := build(t)
	for _, q := range []string{"AC/DC", "Where Are We Now?", `"quoted"`, "*", "AND", ""} {
		if _, err := r.SearchArtists(context.Background(), q, 10); err != nil {
			t.Errorf("search %q errored: %v", q, err)
		}
	}
}

func TestInfoRecordsProvenance(t *testing.T) {
	r := build(t)
	info := r.Info()

	if info.ExportStamp != "20260718-002132" {
		t.Errorf("ExportStamp = %q", info.ExportStamp)
	}
	if info.ReplicationSequence != 187552 {
		t.Errorf("ReplicationSequence = %d", info.ReplicationSequence)
	}
	if info.Artists != 1 || info.Albums != 1 {
		t.Errorf("counts = %d artists, %d albums", info.Artists, info.Albums)
	}
	if info.Tracks != 2 {
		t.Errorf("Tracks = %d, want 2 counted from the release", info.Tracks)
	}
	if info.BuiltAt == "" {
		t.Error("BuiltAt not recorded")
	}
}

func TestOpenRejectsANonDataset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error opening a file that is not a dataset")
	}
}

// Compression is what keeps the artifact small enough to ship, so a
// regression that silently stored raw JSON is worth catching.
func TestPayloadsAreStoredCompressed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	artist := sampleArtist()
	if err := w.AddArtist(artist); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish("stamp", 1); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var stored []byte
	row := r.db.QueryRow(`SELECT payload FROM artist WHERE mbid = ?`, artist.ID)
	if err := row.Scan(&stored); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(artist)
	if len(stored) >= len(raw) {
		t.Errorf("stored %d bytes for %d bytes of JSON, compression is not happening", len(stored), len(raw))
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AC/DC", "acdc"},
		{"P!nk", "pnk"},
		{"The La's", "the las"},
		{"Simon & Garfunkel", "simon and garfunkel"},
		{"Simon and Garfunkel", "simon and garfunkel"},
		{"  Sigur   Rós  ", "sigur rós"},
		{"!!!", ""},
		{"Yes", "yes"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The ranking failure that matters: someone typing an exact name gets that
// artist, not a longer one that happens to contain it.
func TestSearchPrefersAnExactNameOverALongerMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(mbid, name string, albums int) *skyhook.ArtistResource {
		a := &skyhook.ArtistResource{
			ID: mbid, OldIDs: []string{}, ArtistName: name, SortName: name,
			ArtistAliases: []string{}, Genres: []string{},
			Images: []skyhook.ImageResource{}, Links: []skyhook.LinkResource{},
			Albums: make([]skyhook.ArtistAlbumResource, albums),
		}
		return a
	}
	// The longer name is written first so a stable ordering cannot be what
	// makes this pass.
	for _, a := range []*skyhook.ArtistResource{
		mk("11111111-0000-0000-0000-000000000001", "Yes Yes Yes", 0),
		mk("22222222-0000-0000-0000-000000000002", "Yes", 40),
		mk("33333333-0000-0000-0000-000000000003", "Yes", 0),
	} {
		if err := w.AddArtist(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish("stamp", 1); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	got, err := r.SearchArtists(context.Background(), "yes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no results")
	}
	if got[0].ArtistName != "Yes" {
		t.Errorf("top result %q, want the exact match", got[0].ArtistName)
	}
	// Between two artists named Yes, the one with a catalogue is the one
	// people mean.
	if got[0].ID != "22222222-0000-0000-0000-000000000002" {
		t.Errorf("top result %s, want the artist with albums", got[0].ID)
	}
}

// "&" and "and" are the same word to someone typing a band name.
func TestSearchMatchesAmpersandSpelledAsAnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddArtist(&skyhook.ArtistResource{
		ID: "44444444-0000-0000-0000-000000000004", OldIDs: []string{},
		ArtistName: "King Gizzard & the Lizard Wizard", SortName: "King Gizzard",
		ArtistAliases: []string{}, Genres: []string{},
		Images: []skyhook.ImageResource{}, Links: []skyhook.LinkResource{},
		Albums: []skyhook.ArtistAlbumResource{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish("stamp", 1); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, q := range []string{
		"king gizzard and the lizard wizard",
		"king gizzard & the lizard wizard",
	} {
		got, err := r.SearchArtists(context.Background(), q, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) == 0 {
			t.Errorf("search %q found nothing", q)
		}
	}
}

// Lidarr sends the artist for manual import, where the file already says who
// made the record. Ignoring it returns every album sharing a title, and
// "Greatest Hits" is a title thousands of artists share.
func TestSearchAlbumsNarrowsByArtist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	album := func(mbid, title, artist string) *skyhook.AlbumResource {
		return &skyhook.AlbumResource{
			ID: mbid, OldIDs: []string{}, Title: title, Aliases: []string{},
			SecondaryTypes: []string{}, ReleaseStatuses: []string{"Official"},
			Genres: []string{}, Images: []skyhook.ImageResource{},
			Links: []skyhook.LinkResource{}, Releases: []skyhook.ReleaseResource{},
			Artists: []skyhook.AlbumArtistResource{{ArtistName: artist}},
		}
	}
	for _, a := range []*skyhook.AlbumResource{
		album("aaaaaaaa-0000-0000-0000-000000000001", "Greatest Hits", "Queen"),
		album("bbbbbbbb-0000-0000-0000-000000000002", "Greatest Hits", "ABBA"),
		album("cccccccc-0000-0000-0000-000000000003", "Greatest Hits", "Simon & Garfunkel"),
	} {
		if err := w.AddAlbum(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish("stamp", 1); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	all, err := r.SearchAlbums(ctx, "greatest hits", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered search returned %d, want all 3", len(all))
	}

	only, err := r.SearchAlbums(ctx, "greatest hits", "ABBA", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Artists[0].ArtistName != "ABBA" {
		t.Fatalf("artist filter returned %d results: %+v", len(only), only)
	}

	// A file tagged with "and" must still find the album credited with "&".
	amp, err := r.SearchAlbums(ctx, "greatest hits", "Simon and Garfunkel", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(amp) != 1 {
		t.Errorf("searching with 'and' found %d, want the album credited with '&'", len(amp))
	}

	none, err := r.SearchAlbums(ctx, "greatest hits", "Nobody At All", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("unknown artist returned %d results, want none", len(none))
	}
}

// The parallel writer must produce exactly what the serial one does, since it
// is the only thing that writes a shipped dataset.
func TestParallelWriterMatchesSerial(t *testing.T) {
	build := func(parallel bool) map[string]string {
		path := filepath.Join(t.TempDir(), "d.db")
		w, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		artists := []*skyhook.ArtistResource{}
		for i := 0; i < 200; i++ {
			a := sampleArtist()
			a.ID = fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)
			a.ArtistName = fmt.Sprintf("Artist %d", i)
			a.OldIDs = []string{}
			artists = append(artists, a)
		}

		if parallel {
			p := NewParallel(w, 4)
			for _, a := range artists {
				if err := p.AddArtist(a); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, a := range artists {
				if err := w.AddArtist(a); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Finish("stamp", 1); err != nil {
			t.Fatal(err)
		}

		r, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()

		out := map[string]string{}
		for _, a := range artists {
			got, err := r.Artist(context.Background(), a.ID)
			if err != nil {
				t.Fatalf("%s: %v", a.ID, err)
			}
			raw, _ := json.Marshal(got)
			out[a.ID] = string(raw)
		}
		return out
	}

	serial, parallel := build(false), build(true)
	if len(serial) != len(parallel) {
		t.Fatalf("serial wrote %d, parallel wrote %d", len(serial), len(parallel))
	}
	for id, want := range serial {
		if parallel[id] != want {
			t.Fatalf("payload for %s differs between serial and parallel writes", id)
		}
	}
}
