package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

// fakeSource answers from memory so route behaviour can be tested without a
// dataset or a network.
type fakeSource struct {
	artists map[string]*skyhook.ArtistResource
	albums  map[string]*skyhook.AlbumResource
	err     error
}

func (f fakeSource) Name() string { return "fake" }

func (f fakeSource) Artist(_ context.Context, mbid string) (*skyhook.ArtistResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	a, ok := f.artists[mbid]
	if !ok {
		return nil, source.ErrNotFound
	}
	return a, nil
}

func (f fakeSource) Album(_ context.Context, mbid string) (*skyhook.AlbumResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	a, ok := f.albums[mbid]
	if !ok {
		return nil, source.ErrNotFound
	}
	return a, nil
}

func (f fakeSource) SearchArtists(_ context.Context, q string, _ int) ([]skyhook.ArtistResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := []skyhook.ArtistResource{}
	for _, a := range f.artists {
		out = append(out, *a)
	}
	return out, nil
}

func (f fakeSource) SearchAlbums(_ context.Context, q, artist string, _ int) ([]skyhook.AlbumResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []skyhook.AlbumResource{}, nil
}

func str(s string) *string { return &s }

func testServer(t *testing.T, src source.Source) http.Handler {
	t.Helper()
	return New(src, Config{
		Version: "1.2.3",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
}

func sampleSource() fakeSource {
	return fakeSource{
		artists: map[string]*skyhook.ArtistResource{
			"ff3e88b3-7354-4f30-967c-1a61ebc8c642": {
				ID: "ff3e88b3-7354-4f30-967c-1a61ebc8c642", OldIDs: []string{},
				ArtistName: "The La's", SortName: "La's, The", ArtistAliases: []string{},
				Genres: []string{}, Images: []skyhook.ImageResource{},
				Links: []skyhook.LinkResource{}, Type: str("Group"), Status: "ended",
				Albums: []skyhook.ArtistAlbumResource{},
			},
		},
		albums: map[string]*skyhook.AlbumResource{},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestInfoRoute(t *testing.T) {
	rec := get(t, testServer(t, sampleSource()), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var info skyhook.ServerInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.2.3" {
		t.Errorf("version = %q", info.Version)
	}
}

func TestArtistRoute(t *testing.T) {
	h := testServer(t, sampleSource())

	rec := get(t, h, "/artist/ff3e88b3-7354-4f30-967c-1a61ebc8c642")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	diffs, err := skyhook.ContractDiff(rec.Body.Bytes(), &skyhook.ArtistResource{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("response drifted from the contract: %v", diffs)
	}

	// Lidarr distinguishes a missing artist from a broken server, and throws
	// ArtistNotFoundException only on a 404.
	if rec := get(t, h, "/artist/00000000-0000-0000-0000-000000000000"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown artist returned %d, want 404", rec.Code)
	}
}

// A source that is failing must not look like a source with no data, or
// Lidarr treats a transient outage as an emptied library.
func TestLookupFailureIsNotAFourOhFour(t *testing.T) {
	broken := fakeSource{err: io.ErrUnexpectedEOF}
	rec := get(t, testServer(t, broken), "/artist/ff3e88b3-7354-4f30-967c-1a61ebc8c642")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 so Lidarr retries rather than forgetting the artist", rec.Code)
	}
}

func TestSearchRoutes(t *testing.T) {
	h := testServer(t, sampleSource())

	for _, mode := range []string{"artist", "album", "all"} {
		rec := get(t, h, "/search?type="+mode+"&query=las")
		if rec.Code != http.StatusOK {
			t.Errorf("type=%s returned %d", mode, rec.Code)
		}
		var any []json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &any); err != nil {
			t.Errorf("type=%s did not return a list: %v", mode, err)
		}
	}

	if rec := get(t, h, "/search?type=nonsense&query=x"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown type returned %d, want 400", rec.Code)
	}
}

// A static dataset cannot enumerate what changed since a timestamp. Limited
// tells Lidarr to fall back to its normal refresh, which is the honest
// answer; anything else would have it skip work it needs to do.
func TestRecentAlwaysReportsLimited(t *testing.T) {
	h := testServer(t, sampleSource())
	for _, path := range []string{"/recent/artist?since=1752000000", "/recent/album?since=1752000000"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, rec.Code)
		}
		var out skyhook.RecentUpdatesResource
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Limited {
			t.Errorf("%s reported Limited=false", path)
		}
		if out.Items == nil {
			t.Errorf("%s emitted null items, the contract wants []", path)
		}
	}
}

func TestFingerprintStub(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t, sampleSource()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/search/fingerprint", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if body := rec.Body.String(); body != "[]" {
		t.Errorf("body = %q, want an empty list", body)
	}
}

// A browser asking for a favicon is not a metadata request, and counting it
// made an idle healthy server report a 100% error rate.
func TestMetricsIgnoreNonMetadataRequests(t *testing.T) {
	srv := New(sampleSource(), Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := srv.Handler()

	for _, path := range []string{"/favicon.ico", "/ui", "/ui/status"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if snap := srv.metrics.Snapshot(); snap.Requests != 0 || snap.Errors != 0 {
		t.Errorf("browser and console traffic was counted: %d requests, %d errors",
			snap.Requests, snap.Errors)
	}

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/recent/artist?since=1", nil))
	if snap := srv.metrics.Snapshot(); snap.Requests != 1 {
		t.Errorf("a metadata request counted as %d", snap.Requests)
	}
}

// Per-MBID paths must aggregate, or the route table grows one row per lookup.
func TestRouteLabels(t *testing.T) {
	cases := []struct {
		path    string
		label   string
		tracked bool
	}{
		{"/", "/", true},
		{"/artist/abc", "/artist/{mbid}", true},
		{"/album/def", "/album/{mbid}", true},
		{"/recent/artist", "/recent/*", true},
		{"/search", "/search", true},
		{"/favicon.ico", "", false},
		{"/ui/status", "", false},
	}
	for _, c := range cases {
		label, tracked := routeLabel(c.path)
		if tracked != c.tracked || label != c.label {
			t.Errorf("routeLabel(%q) = %q,%v want %q,%v", c.path, label, tracked, c.label, c.tracked)
		}
	}
}
