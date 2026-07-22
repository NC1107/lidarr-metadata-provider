package mbdump

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildArchive writes a bzip2 tarball shaped like a MusicBrainz export.
// bzip2 compression is shelled out because the standard library only reads
// it; the binary is present anywhere a dump could realistically be handled.
func buildArchive(t *testing.T, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Provenance first, then tables, mirroring real archive order so the
	// early-exit paths get exercised the way they will be in production.
	order := []string{"TIMESTAMP", "SCHEMA_SEQUENCE", "REPLICATION_SEQUENCE"}
	for name := range files {
		if !contains(order, name) {
			order = append(order, name)
		}
	}
	for _, name := range order {
		body, ok := files[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "mbdump.tar.bz2")
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = &buf
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("bzip2 unavailable: %v", err)
	}
	if err := writeFile(path, out); err != nil {
		t.Fatal(err)
	}
	return path
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func validArchive(t *testing.T) string {
	return buildArchive(t, map[string]string{
		"TIMESTAMP":            "2026-07-18 00:21:33.104259+00\n",
		"SCHEMA_SEQUENCE":      "31\n",
		"REPLICATION_SEQUENCE": "187552\n",
		"mbdump/artist": strings.Join([]string{
			"1\tb10bbbfc-cf9e-42e0-be17-e2c3e1d2600d\tThe Beatles\tBeatles, The\t\\N\tUK rock band",
			"2\tff3e88b3-7354-4f30-967c-1a61ebc8c642\tThe La's\tLa's, The\t\\N\t",
		}, "\n") + "\n",
		"mbdump/release_group": "1\t794a5fcd-4098-3519-b4b6-e66707f4cbc3\tAbbey Road\t1\n",
	})
}

func TestInfoReadsProvenance(t *testing.T) {
	a, err := Open(validArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	info, err := a.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.SchemaSequence != 31 {
		t.Errorf("SchemaSequence = %d, want 31", info.SchemaSequence)
	}
	if info.ReplicationSequence != 187552 {
		t.Errorf("ReplicationSequence = %d, want 187552", info.ReplicationSequence)
	}
	if !strings.HasPrefix(info.Timestamp, "2026-07-18") {
		t.Errorf("Timestamp = %q", info.Timestamp)
	}
}

// A schema bump must stop the build rather than produce a subtly wrong
// dataset from shifted columns.
func TestInfoRejectsUnknownSchema(t *testing.T) {
	path := buildArchive(t, map[string]string{
		"TIMESTAMP":            "2027-01-01 00:00:00+00\n",
		"SCHEMA_SEQUENCE":      "32\n",
		"REPLICATION_SEQUENCE": "200000\n",
		"mbdump/artist":        "1\tmbid\tName\tName\t\\N\t\n",
	})
	a, _ := Open(path)
	_, err := a.Info()
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("expected ErrSchemaMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("error should name the archive's schema: %v", err)
	}

	a.AllowSchemaMismatch = true
	if _, err := a.Info(); err != nil {
		t.Errorf("AllowSchemaMismatch should permit reading: %v", err)
	}
}

func TestReadTablesDispatchesRows(t *testing.T) {
	a, _ := Open(validArchive(t))

	var artists [][]string
	var groups int
	err := a.ReadTables(map[string]RowFunc{
		"artist": func(row []Field) error {
			// Fields are reused between calls, so copy what is kept.
			artists = append(artists, []string{row[2].Value, row[4].Or("(null)"), row[5].Value})
			return nil
		},
		"release_group": func(row []Field) error {
			groups++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(artists) != 2 {
		t.Fatalf("got %d artists, want 2", len(artists))
	}
	if artists[0][0] != "The Beatles" {
		t.Errorf("artist name = %q", artists[0][0])
	}
	if artists[0][1] != "(null)" {
		t.Errorf("expected a NULL column, got %q", artists[0][1])
	}
	// The distinction COPY would lose without Field.IsNull: row 2's last
	// column is an empty string, row 1's is real text.
	if artists[1][2] != "" {
		t.Errorf("expected an empty string, got %q", artists[1][2])
	}
	if groups != 1 {
		t.Errorf("got %d release groups, want 1", groups)
	}
}

// A table the caller asked for but the archive lacks must be an error, since
// silently skipping it yields an incomplete dataset that still looks valid.
func TestReadTablesReportsMissingTables(t *testing.T) {
	a, _ := Open(validArchive(t))
	err := a.ReadTables(map[string]RowFunc{
		"artist":         func([]Field) error { return nil },
		"does_not_exist": func([]Field) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "does_not_exist") {
		t.Fatalf("expected a missing-table error, got %v", err)
	}
}

func TestReadTablesPropagatesHandlerErrors(t *testing.T) {
	a, _ := Open(validArchive(t))
	sentinel := errors.New("boom")
	err := a.ReadTables(map[string]RowFunc{
		"artist": func([]Field) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the handler error to surface, got %v", err)
	}
	if !strings.Contains(err.Error(), "artist line 1") {
		t.Errorf("error should locate the row: %v", err)
	}
}

func TestTablesLists(t *testing.T) {
	a, _ := Open(validArchive(t))
	tables, err := a.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 || !contains(tables, "artist") || !contains(tables, "release_group") {
		t.Errorf("Tables() = %v", tables)
	}
}

func TestOpenRejectsMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.tar.bz2")); err == nil {
		t.Fatal("expected an error opening a nonexistent archive")
	}
}

func writeFile(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
}
