package dataset

import (
	"context"
	"path/filepath"
	"testing"
)

func buildStamped(t *testing.T, stamp string) *Reader {
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
	if err := w.Finish(stamp, 1); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestLiveSwapReplacesServedDataset checks a swap points both lookups and the
// reported status at the new reader, which is what lets an update land without
// a restart or a dropped request.
func TestLiveSwapReplacesServedDataset(t *testing.T) {
	first := buildStamped(t, "20260718-000000")
	second := buildStamped(t, "20260801-000000")

	live := NewLive(first)
	if got := live.Info().ExportStamp; got != "20260718-000000" {
		t.Fatalf("initial export = %q, want the first dataset", got)
	}
	if _, err := live.Artist(context.Background(), sampleArtist().ID); err != nil {
		t.Fatalf("artist lookup before swap: %v", err)
	}

	live.Swap(second)

	if got := live.Info().ExportStamp; got != "20260801-000000" {
		t.Errorf("after swap export = %q, want the second dataset", got)
	}
	if _, err := live.Artist(context.Background(), sampleArtist().ID); err != nil {
		t.Errorf("artist lookup after swap: %v", err)
	}
}
