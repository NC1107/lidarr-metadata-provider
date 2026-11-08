package dataset

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

func compress(t *testing.T, raw []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(raw, nil)
}

// Embedded artists are read by stopping at the album list, so any field added
// after it would silently vanish from every album response.
func TestAlbumsIsTheLastArtistField(t *testing.T) {
	typ := reflect.TypeOf(skyhook.ArtistResource{})
	if tag := typ.Field(typ.NumField() - 1).Tag.Get("json"); tag != "Albums" {
		t.Errorf("last ArtistResource field has json tag %q, want Albums", tag)
	}
}

func TestEmbeddedArtistReadMatchesFullDecode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "artist_*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no artist fixtures: %v", err)
	}
	r := build(t)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			var artist skyhook.ArtistResource
			if err := json.Unmarshal(readFixture(t, filepath.Base(file)), &artist); err != nil {
				t.Fatal(err)
			}
			stored, err := json.Marshal(artist)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.decodeAlbumArtist(compress(t, stored))
			if err != nil {
				t.Fatal(err)
			}
			gotRaw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			want := jsonTree(t, stored).(map[string]any)
			delete(want, "Albums")
			if keys := differingKeys(jsonTree(t, gotRaw), want); len(keys) > 0 {
				t.Errorf("embedded artist differs from the full decode in %v", keys)
			}
		})
	}
}

// Various Artists decompresses to gigabytes of decoded albums, and a compilation
// embeds it on every request. The malformed tail proves the albums are never
// parsed.
func TestEmbeddedArtistReadStopsBeforeAlbums(t *testing.T) {
	r := build(t)
	payload := `{"id":"89ad4ac3-39f7-470e-963a-56509c546377","artistname":"Various Artists","Albums":[{"not valid json`
	got, err := r.decodeAlbumArtist(compress(t, []byte(payload)))
	if err != nil {
		t.Fatalf("read past the album list: %v", err)
	}
	if got.ArtistName != "Various Artists" {
		t.Errorf("artist name = %q", got.ArtistName)
	}
}

func writeDataset(t *testing.T, album *skyhook.AlbumResource) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dataset.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddArtist(sampleArtist()); err != nil {
		t.Fatal(err)
	}
	if err := w.AddAlbum(album); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish("20260718-002132", 187552); err != nil {
		t.Fatal(err)
	}
	return path
}

func openDataset(t *testing.T, path string) *Reader {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Production datasets are compressed with a trained dictionary, and a frame
// the streaming decoder cannot read would otherwise pass every other test.
func TestEmbeddedArtistReadsThroughTrainedDictionary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	w, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	b := NewDictBuilder(w, 4, 600)
	if err := b.AddArtist(sampleArtist()); err != nil {
		t.Fatal(err)
	}
	for i := range 1500 {
		a := sampleArtist()
		a.ID = fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)
		a.OldIDs = []string{}
		if err := b.AddArtist(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AddAlbum(sampleAlbum()); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if len(w.Dictionary()) == 0 {
		t.Skip("no zstd binary, dictionary not trained")
	}
	if err := w.Finish("stamp", 1); err != nil {
		t.Fatal(err)
	}

	album, err := openDataset(t, path).Album(context.Background(), sampleAlbum().ID)
	if err != nil {
		t.Fatal(err)
	}
	got := album.Artists[0]
	if got.Overview == nil || len(got.Images) != 1 {
		t.Errorf("embedded artist was not filled through the dictionary: %+v", got)
	}
}

func TestEmbeddedArtistKeepsTheIDTheAlbumCredits(t *testing.T) {
	const retired = "00000000-dead-beef-0000-000000000000"
	album := sampleAlbum()
	album.ArtistID = retired
	album.Artists[0].ID = retired
	album.Releases[0].Tracks[0].ArtistID = retired

	got, err := openDataset(t, writeDataset(t, album)).Album(context.Background(), album.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Artists) != 1 || got.Artists[0].ID != retired {
		t.Fatalf("embedded artists = %+v, want one credited as %s", got.Artists, retired)
	}
	if got.Artists[0].Overview == nil {
		t.Error("embedded artist was not filled from its replacement")
	}
	if tid := got.Releases[0].Tracks[0].ArtistID; tid != retired {
		t.Errorf("track artist = %q, want %q", tid, retired)
	}
}

// Serving the stub after a failed read would erase the artist's biography in
// Lidarr, whereas a failed request is retried.
func TestAlbumFailsWhenAnEmbeddedArtistCannotBeRead(t *testing.T) {
	path := writeDataset(t, sampleAlbum())
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artist SET payload = x'00' WHERE mbid = ?`, sampleArtist().ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = openDataset(t, path).Album(context.Background(), sampleAlbum().ID)
	if err == nil {
		t.Fatal("album served with an unreadable embedded artist")
	}
	if errors.Is(err, source.ErrNotFound) {
		t.Errorf("a failed read was reported as not found: %v", err)
	}
}

func TestConcurrentAlbumsKeepTheirEmbeddedArtists(t *testing.T) {
	r := build(t)
	const mbid = "f57d03ff-b0a5-3b73-a14c-a5ed5f8cd956"
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				album, err := r.Album(context.Background(), mbid)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := json.Marshal(album); err != nil {
					t.Error(err)
					return
				}
				if album.Artists[0].Overview == nil {
					t.Error("embedded artist lost its overview")
					return
				}
			}
		}()
	}
	wg.Wait()
}
