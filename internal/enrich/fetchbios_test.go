package enrich

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchBiosConcurrentCheckpoint exercises the worker pool and the
// checkpoint together under -race. The checkpoint reads every artist's Overview
// while workers are resolving others; this passes only because a single
// collector owns all mutation. It also asserts the biographies actually land.
func TestFetchBiosConcurrentCheckpoint(t *testing.T) {
	// A fake extracts API that returns an extract per requested title.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		titles := strings.Split(r.URL.Query().Get("titles"), "|")
		var resp extractsResponse
		resp.Query.Pages = map[string]struct {
			Title   string `json:"title"`
			Extract string `json:"extract"`
		}{}
		for i, tt := range titles {
			resp.Query.Pages[fmt.Sprint(i)] = struct {
				Title   string `json:"title"`
				Extract string `json:"extract"`
			}{Title: tt, Extract: "Bio of " + tt + ".\nSecond paragraph."}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	old := extractsEndpoint
	extractsEndpoint = srv.URL
	defer func() { extractsEndpoint = old }()

	// Enough distinct articles to fill many batches so workers overlap the
	// periodic checkpoint.
	artists := map[string]*Artist{}
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("mbid-%04d", i)
		artists[id] = &Artist{MBID: id, Wiki: fmt.Sprintf("Article_%04d", i)}
	}

	// The checkpoint reads every artist, exactly as enrich.Save does, so -race
	// sees any concurrent write.
	checkpoint := func() {
		total := 0
		for _, a := range artists {
			total += len(a.Overview)
		}
		_ = total
	}

	if err := FetchBios(srv.Client(), "test/1.0", artists, 16, nil, checkpoint); err != nil {
		t.Fatal(err)
	}

	got := 0
	for _, a := range artists {
		if a.Overview != "" {
			got++
		}
	}
	if got != len(artists) {
		t.Errorf("resolved %d/%d biographies", got, len(artists))
	}
	// Spot-check the content: leadParagraph keeps the first line.
	if a := artists["mbid-0000"]; a.Overview != "Bio of Article_0000." {
		t.Errorf("mbid-0000 overview = %q, want %q", a.Overview, "Bio of Article_0000.")
	}
}
