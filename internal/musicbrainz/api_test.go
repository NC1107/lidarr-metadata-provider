package musicbrainz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/ratelimit"
)

// TestArtistDiscographyExcludesGuestReleaseGroups is the fallback counterpart
// to the pipeline guard: a browsed release group must land in the artist's
// discography only when the artist leads its credit. A guest credit that slips
// in would make Lidarr treat the album's foreign lead as a missing parent and
// auto-add, monitor, and search it on refresh, cascading across the whole
// collaboration graph.
func TestArtistDiscographyExcludesGuestReleaseGroups(t *testing.T) {
	const guest = "11111111-1111-1111-1111-111111111111"
	const other = "22222222-2222-2222-2222-222222222222"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/artist/"):
			w.Write([]byte(`{"id":"` + guest + `","name":"Guest","sort-name":"Guest"}`))
		case r.URL.Path == "/release":
			// One release led by the artist, one led by someone else with the
			// artist only a secondary credit.
			w.Write([]byte(`{"releases":[
				{"id":"rel-1","status":"Official","release-group":{"id":"rg-self","title":"Solo","artist-credit":[{"artist":{"id":"` + guest + `"}}]}},
				{"id":"rel-2","status":"Official","release-group":{"id":"rg-guest","title":"Their Album","artist-credit":[{"artist":{"id":"` + other + `"}},{"artist":{"id":"` + guest + `"}}]}}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:   srv.URL,
		UserAgent: "LidarrMetadataProvider/test ( test@example.com )",
		HTTP:      srv.Client(),
		Limiter:   ratelimit.New(time.Millisecond),
	}

	artist, err := c.Artist(context.Background(), guest)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, al := range artist.Albums {
		titles[al.Title] = true
	}
	if !titles["Solo"] {
		t.Errorf("artist-led release group missing from discography; got %+v", artist.Albums)
	}
	if titles["Their Album"] {
		t.Errorf("guest release group leaked into discography; on refresh Lidarr would auto-add its foreign lead")
	}
}
