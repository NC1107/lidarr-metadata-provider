package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// datasetBytes builds a real, openable dataset carrying the given export stamp
// and returns its bytes, so download tests exercise the same validation an
// installed file gets rather than a placeholder payload that would fail it.
func datasetBytes(t *testing.T, stamp string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "built.db")
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestFetchRecordsDigest checks a first-boot download records the installed
// digest so a later refresh can compare against it without re-hashing.
func TestFetchRecordsDigest(t *testing.T) {
	data := datasetBytes(t, "20260718-000000")
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
	data := datasetBytes(t, "20260718-000000")
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
	data := datasetBytes(t, "20260718-000000")
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
	v1 := datasetBytes(t, "20260718-000000")
	v2 := datasetBytes(t, "20260801-000000")
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

// TestRefreshRefusesUnreadableDataset is the guarantee behind "you stay on the
// version that already worked": a published file that matches its checksum
// but cannot be opened by this build is never installed.
func TestRefreshRefusesUnreadableDataset(t *testing.T) {
	v1 := datasetBytes(t, "20260718-000000")
	garbage := []byte("this is not a dataset, but its checksum is correct")
	srv1 := serveArtifact(t, v1, 1)
	srv2 := serveArtifact(t, garbage, 1)
	dest := filepath.Join(t.TempDir(), "dataset.db")

	if err := Fetch(context.Background(), srv1.URL+"/dataset.db", dest, quietLog()); err != nil {
		t.Fatal(err)
	}
	digest, updated, err := Refresh(context.Background(), srv2.URL+"/dataset.db", dest, digestOf(v1), quietLog())
	if err == nil {
		t.Fatal("expected the unreadable dataset to be refused")
	}
	if updated || digest != digestOf(v1) {
		t.Errorf("refresh reported updated=%v digest=%s; want no update and the old digest", updated, digest)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(v1) {
		t.Error("the installed dataset was replaced by an unreadable one")
	}
	if _, err := os.Stat(dest + ".incoming"); !os.IsNotExist(err) {
		t.Error("the staged download was left behind")
	}
	if r, err := Open(dest); err != nil {
		t.Errorf("installed dataset no longer opens: %v", err)
	} else {
		r.Close()
	}
}

// TestReaderSurvivesRenameUnderLoad is the property the live update depends
// on: once a Reader is open, replacing the file underneath it by rename must
// not change what that Reader serves, even while it is busy and the pool is
// churning. database/sql reopens connections by path, so without a pinned
// pool the old Reader would start answering from the new file.
func TestReaderSurvivesRenameUnderLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dataset.db")
	if err := os.WriteFile(path, datasetBytes(t, "20260718-000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(dir, "dataset.db.incoming")
	if err := os.WriteFile(next, datasetBytes(t, "20260801-000000"), 0o644); err != nil {
		t.Fatal(err)
	}

	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	const workers, rounds = 16, 40
	var wrongFile, failures atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				if _, err := old.Artist(context.Background(), sampleArtist().ID); err != nil {
					failures.Add(1)
					continue
				}
				var stamp string
				if err := old.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, MetaExportStamp).Scan(&stamp); err != nil {
					failures.Add(1)
				} else if stamp != "20260718-000000" {
					wrongFile.Add(1)
				}
			}
		}()
	}
	close(start)
	// Rename while the workers are mid-flight.
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if n := wrongFile.Load(); n != 0 {
		t.Errorf("%d lookups on the old reader were answered from the new file", n)
	}
	if n := failures.Load(); n != 0 {
		t.Errorf("%d lookups failed during the rename", n)
	}
	if got := old.Info().ExportStamp; got != "20260718-000000" {
		t.Errorf("old reader now reports export %s", got)
	}
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if got := fresh.Info().ExportStamp; got != "20260801-000000" {
		t.Errorf("new reader reports export %s, want the renamed-in file", got)
	}
}

// TestValidateRejectsNonDataset checks the pre-install validation refuses a
// file that is not a dataset with a readable reason.
func TestValidateRejectsNonDataset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(path); err == nil {
		t.Fatal("expected validation to fail for a non-dataset file")
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(empty); err == nil {
		t.Fatal("expected validation to fail for an empty file")
	}
}
