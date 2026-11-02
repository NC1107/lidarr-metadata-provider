package dataset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

const fixtureDir = "../../fixtures/v0.4"

// The captured album payloads embed each artist as a stub: overview null and
// no images. Lidarr's album refresh writes that stub over the artist metadata
// it stored from /artist, so serving it verbatim erases every refreshed
// artist's biography and photo. This server therefore departs from the
// captures in exactly one place: an embedded artist the dataset holds is the
// artist payload without its album list. Everything else in the album, and
// any embedded artist the dataset lacks, must still match the capture.
func TestAlbumDepartsFromCaptureOnlyInEmbeddedArtists(t *testing.T) {
	cases := []struct {
		album   string
		artists []string
	}{
		// One credited artist, present in the dataset.
		{"album_ac7bcd07-b598-4f3a-94bf-d2531f34979c.json",
			[]string{"artist_b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d_beatles.json"}},
		// Two credited artists: Bach is in the dataset, Glenn Gould is not
		// and keeps the captured stub.
		{"album_31048ac9-53bf-305a-9f4e-38ad458ee966_goldberg-state-of-wonder.json",
			[]string{"artist_24f1766e-9635-4d58-a4d4-9413f9f98a4c_bach.json"}},
	}
	for _, c := range cases {
		t.Run(c.album, func(t *testing.T) {
			albumRaw := readFixture(t, c.album)
			var album skyhook.AlbumResource
			if err := json.Unmarshal(albumRaw, &album); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(t.TempDir(), "dataset.db")
			w, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			held := map[string]map[string]any{}
			for _, name := range c.artists {
				raw := readFixture(t, name)
				var artist skyhook.ArtistResource
				if err := json.Unmarshal(raw, &artist); err != nil {
					t.Fatal(err)
				}
				if err := w.AddArtist(&artist); err != nil {
					t.Fatal(err)
				}
				tree := jsonTree(t, raw).(map[string]any)
				delete(tree, "Albums")
				held[artist.ID] = tree
			}
			if err := w.AddAlbum(&album); err != nil {
				t.Fatal(err)
			}
			if err := w.Finish("20260722-000000", 1); err != nil {
				t.Fatal(err)
			}
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			served, err := r.Album(context.Background(), album.ID)
			if err != nil {
				t.Fatal(err)
			}
			servedRaw, err := json.Marshal(served)
			if err != nil {
				t.Fatal(err)
			}

			want := jsonTree(t, albumRaw).(map[string]any)
			have := jsonTree(t, servedRaw).(map[string]any)
			wantArtists := want["artists"].([]any)
			haveArtists := have["artists"].([]any)
			if len(haveArtists) != len(wantArtists) {
				t.Fatalf("served %d embedded artists, capture has %d", len(haveArtists), len(wantArtists))
			}
			for i, wa := range wantArtists {
				id := wa.(map[string]any)["id"].(string)
				expected := wa
				if full, ok := held[id]; ok {
					expected = full
				}
				if keys := differingKeys(haveArtists[i], expected); len(keys) > 0 {
					t.Errorf("embedded artist %s differs in %v", id, keys)
				}
			}
			delete(want, "artists")
			delete(have, "artists")
			if !reflect.DeepEqual(have, want) {
				t.Error("album payload outside the embedded artists differs from the capture")
			}
		})
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func jsonTree(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// differingKeys names the top-level keys whose values differ between two
// JSON objects, so a failure says which fields drifted instead of dumping
// both payloads.
func differingKeys(have, want any) []string {
	h, _ := have.(map[string]any)
	w, _ := want.(map[string]any)
	var out []string
	for k := range w {
		if !reflect.DeepEqual(h[k], w[k]) {
			out = append(out, k)
		}
	}
	for k := range h {
		if _, ok := w[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
