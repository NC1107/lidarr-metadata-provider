package dataset

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
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

// Concurrent albums credited to one artist share a decoded shape, which only
// stays safe while nothing writes to it.
func TestConcurrentAlbumsShareAnEmbeddedArtistSafely(t *testing.T) {
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
