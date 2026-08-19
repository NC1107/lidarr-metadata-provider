package enrich

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

var prefixRe = regexp.MustCompile(`STRSTARTS\(\?mbid, "([0-9a-f])"\)`)

// fastHarvest points the harvest at a stand-in server and strips the pacing and
// backoff so a test runs instantly, restoring the globals afterwards.
func fastHarvest(t *testing.T, url string) {
	t.Helper()
	oe, oa, ob, op := wdqsEndpoint, harvestMaxAttempts, harvestBackoffUnit, harvestPace
	wdqsEndpoint, harvestMaxAttempts, harvestBackoffUnit, harvestPace = url, 2, 0, 0
	t.Cleanup(func() {
		wdqsEndpoint, harvestMaxAttempts, harvestBackoffUnit, harvestPace = oe, oa, ob, op
	})
}

func sparqlOneBinding(prefix string) string {
	return `{"results":{"bindings":[{"mbid":{"value":"` + prefix + `0000000-0000-0000-0000-000000000000"}}]}}`
}

// TestHarvestToleratesAFailedSlice is the fix for the build outages: one
// throttled slice must not abort the whole harvest. The other fifteen slices
// still come back and only the failed prefix is missing, for the cache to cover.
func TestHarvestToleratesAFailedSlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := prefixRe.FindStringSubmatch(r.URL.Query().Get("query"))
		prefix := ""
		if len(m) == 2 {
			prefix = m[1]
		}
		if prefix == "3" {
			w.Write([]byte(`{"results":`)) // truncated -> "unexpected end of JSON input"
			return
		}
		w.Write([]byte(sparqlOneBinding(prefix)))
	}))
	defer srv.Close()
	fastHarvest(t, srv.URL)

	out, err := Harvest(srv.Client(), "test", nil)
	if err != nil {
		t.Fatalf("a single failed slice should not error: %v", err)
	}
	if len(out) != 15 {
		t.Errorf("harvested %d artists, want 15 (one of sixteen slices failed)", len(out))
	}
	for mbid := range out {
		if mbid[0] == '3' {
			t.Errorf("the failed slice leaked %q into the result", mbid)
		}
	}
}

// TestHarvestErrorsWhenEverySliceFails checks a total outage still errors, so
// the caller falls back to the cache deliberately rather than publishing empty
// enrichment.
func TestHarvestErrorsWhenEverySliceFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`garbage`))
	}))
	defer srv.Close()
	fastHarvest(t, srv.URL)

	if _, err := Harvest(srv.Client(), "test", nil); err == nil {
		t.Error("a total outage should return an error")
	}
}

// TestCarryMissingPreservesCachedArtists checks a cached artist absent from the
// fresh harvest is carried forward and a present one is left untouched.
func TestCarryMissingPreservesCachedArtists(t *testing.T) {
	fresh := map[string]*Artist{"a": {MBID: "a", Image: "img-a"}}
	cached := map[string]*Artist{
		"a": {MBID: "a", Image: "old-a"},
		"b": {MBID: "b", Image: "img-b", Overview: "bio-b"},
	}
	if n := CarryMissing(fresh, cached); n != 1 {
		t.Fatalf("carried %d, want 1", n)
	}
	if fresh["a"].Image != "img-a" {
		t.Errorf("a present artist was overwritten from the cache: %q", fresh["a"].Image)
	}
	if fresh["b"] == nil || fresh["b"].Overview != "bio-b" {
		t.Errorf("a cached-only artist was not carried: %+v", fresh["b"])
	}
}
