package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveArtifact stands up a server exposing a dataset at /dataset.db with its
// checksum, and optionally as parts with a manifest, mimicking a release.
func serveArtifact(t *testing.T, data []byte, parts int) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	files := map[string][]byte{
		"/dataset.db.sha256": []byte(digest + "  dataset.db\n"),
	}
	if parts <= 1 {
		files["/dataset.db"] = data
	} else {
		size := (len(data) + parts - 1) / parts
		var manifest strings.Builder
		for i := 0; i < parts; i++ {
			start := i * size
			end := min(start+size, len(data))
			if start >= len(data) {
				break
			}
			name := "dataset.db.part" + string(rune('a'+i))
			files["/"+name] = data[start:end]
			manifest.WriteString(name + "\n")
		}
		files["/dataset.db.parts"] = []byte(manifest.String())
	}

	mux := http.NewServeMux()
	for path, body := range files {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Write(body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSingleFile(t *testing.T) {
	data := []byte("a single-file dataset payload")
	srv := serveArtifact(t, data, 1)
	dest := filepath.Join(t.TempDir(), "dataset.db")

	if err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(data) {
		t.Errorf("fetched %q, want %q", got, data)
	}
}

// TestFetchMultipart is the case that matters for a real dataset: too large for
// one release asset, published in parts, and rejoined byte-for-byte.
func TestFetchMultipart(t *testing.T) {
	data := []byte(strings.Repeat("chunky dataset bytes, ", 5000))
	srv := serveArtifact(t, data, 4)
	dest := filepath.Join(t.TempDir(), "dataset.db")

	if err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(data) {
		t.Fatalf("rejoined dataset differs: got %d bytes, want %d", len(got), len(data))
	}
}

// TestFetchMultipartDetectsCorruption proves the whole-file checksum catches a
// tampered part, so a bad reassembly refuses to install rather than serving
// corruption.
func TestFetchMultipartDetectsCorruption(t *testing.T) {
	data := []byte(strings.Repeat("chunky dataset bytes, ", 5000))
	srv := serveArtifactCorrupt(t, data, 4)
	dest := filepath.Join(t.TempDir(), "dataset.db")

	err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected a checksum failure, got none")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("a corrupt download must not be installed at the destination")
	}
}

// serveArtifactCorrupt serves parts whose checksum is for the clean data but
// whose second part has been altered.
func serveArtifactCorrupt(t *testing.T, data []byte, parts int) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	size := (len(data) + parts - 1) / parts

	mux := http.NewServeMux()
	mux.HandleFunc("/dataset.db.sha256", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(digest + "\n"))
	})
	var manifest strings.Builder
	for i := 0; i < parts; i++ {
		start := i * size
		end := min(start+size, len(data))
		chunk := append([]byte(nil), data[start:end]...)
		if i == 1 {
			chunk[0] ^= 0xff // corrupt one byte
		}
		name := "dataset.db.part" + string(rune('a'+i))
		manifest.WriteString(name + "\n")
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(chunk) })
	}
	m := manifest.String()
	mux.HandleFunc("/dataset.db.parts", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(m)) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
