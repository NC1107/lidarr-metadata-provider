package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestFetchRecordsDigest checks a first-boot download records the installed
// digest so a later refresh can compare against it without re-hashing.
func TestFetchRecordsDigest(t *testing.T) {
	data := []byte("dataset v1")
	srv := serveArtifact(t, data, 1)
	dest := filepath.Join(t.TempDir(), "dataset.db")

	if err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, quietLog()); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledDigest(dest, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got != digestOf(data) {
		t.Errorf("recorded digest = %s, want %s", got, digestOf(data))
	}
}

// TestInstalledDigestFallsBackToHashing checks a dataset installed before the
// sidecar existed still yields a digest, by hashing the file once.
func TestInstalledDigestFallsBackToHashing(t *testing.T) {
	data := []byte("pre-existing dataset")
	srv := serveArtifact(t, data, 1)
	dest := filepath.Join(t.TempDir(), "dataset.db")
	if err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, quietLog()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(digestSidecar(dest)); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledDigest(dest, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got != digestOf(data) {
		t.Errorf("hashed digest = %s, want %s", got, digestOf(data))
	}
	// The sidecar should have been rewritten so the next check is cheap.
	if _, err := os.Stat(digestSidecar(dest)); err != nil {
		t.Errorf("sidecar not rewritten: %v", err)
	}
}

// TestRefreshSkipsWhenUnchanged is the property that keeps a huge download off
// the wire on every check: an unchanged remote digest downloads nothing.
func TestRefreshSkipsWhenUnchanged(t *testing.T) {
	data := []byte("dataset v1")
	srv := serveArtifact(t, data, 1)
	dest := filepath.Join(t.TempDir(), "dataset.db")
	if err := Fetch(context.Background(), srv.URL+"/dataset.db", dest, quietLog()); err != nil {
		t.Fatal(err)
	}

	digest, updated, err := Refresh(context.Background(), srv.URL+"/dataset.db", dest, digestOf(data), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Error("refresh downloaded despite an unchanged digest")
	}
	if digest != digestOf(data) {
		t.Errorf("digest = %s, want unchanged %s", digest, digestOf(data))
	}
}

// TestRefreshInstallsNewerDataset checks a changed published digest replaces
// the installed dataset and reports the new digest.
func TestRefreshInstallsNewerDataset(t *testing.T) {
	v1 := []byte("dataset v1")
	v2 := []byte("dataset v2, a newer export")
	srv1 := serveArtifact(t, v1, 1)
	srv2 := serveArtifact(t, v2, 2) // also exercises the multipart path
	dest := filepath.Join(t.TempDir(), "dataset.db")

	if err := Fetch(context.Background(), srv1.URL+"/dataset.db", dest, quietLog()); err != nil {
		t.Fatal(err)
	}

	digest, updated, err := Refresh(context.Background(), srv2.URL+"/dataset.db", dest, digestOf(v1), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("refresh did not install the newer dataset")
	}
	if digest != digestOf(v2) {
		t.Errorf("digest = %s, want %s", digest, digestOf(v2))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(v2) {
		t.Errorf("installed content = %q, want %q", got, v2)
	}
}
