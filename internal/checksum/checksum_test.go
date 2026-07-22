package checksum

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The digest of "hello\n", used so the expectations here are checkable by
// hand with sha256sum.
const helloDigest = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSumsAcceptsBothManifestForms(t *testing.T) {
	// MetaBrainz publishes the binary form with an asterisk; plain sha256sum
	// output has two spaces.
	manifest := `
# a comment
5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03 *mbdump.tar.bz2
5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  mbdump-derived.tar.bz2
garbage line with no digest
`
	sums, err := ParseSums(strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("parsed %d digests, want 2: %v", len(sums), sums)
	}
	if sums["mbdump.tar.bz2"] != helloDigest {
		t.Errorf("binary form parsed as %q", sums["mbdump.tar.bz2"])
	}
	if sums["mbdump-derived.tar.bz2"] != helloDigest {
		t.Errorf("plain form parsed as %q", sums["mbdump-derived.tar.bz2"])
	}
}

func TestParseSumsRejectsAnEmptyManifest(t *testing.T) {
	if _, err := ParseSums(strings.NewReader("# nothing here\n")); err == nil {
		t.Fatal("expected an error for a manifest with no digests")
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "good", "hello\n")

	if err := Verify(path, helloDigest); err != nil {
		t.Errorf("matching digest rejected: %v", err)
	}
	if err := Verify(path, strings.ToUpper(helloDigest)); err != nil {
		t.Errorf("digest comparison should be case insensitive: %v", err)
	}
	err := Verify(path, strings.Repeat("0", 64))
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("expected ErrMismatch, got %v", err)
	}
}

// The property that matters: a bad download must leave the previous file in
// place, so a failed update degrades to the old dataset rather than a corrupt
// one.
func TestInstallLeavesTheExistingFileOnMismatch(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, dir, "dataset.db", "the good dataset")
	staged := write(t, dir, "dataset.db.incoming", "truncated garbage")

	err := Install(staged, dest, helloDigest)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch, got %v", err)
	}

	body, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "the good dataset" {
		t.Errorf("destination was modified by a failed install: %q", body)
	}
	// A known-bad staged file must not survive to be retried or resumed.
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staged file should be removed after a failed verification")
	}
}

func TestInstallReplacesOnMatch(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, dir, "dataset.db", "the old dataset")
	staged := write(t, dir, "dataset.db.incoming", "hello\n")

	if err := Install(staged, dest, helloDigest); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello\n" {
		t.Errorf("destination = %q, want the newly installed content", body)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staged file should be gone after a successful install")
	}
}

func TestInstallCreatesTheDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	staged := write(t, dir, "incoming", "hello\n")
	dest := filepath.Join(dir, "nested", "deeper", "dataset.db")

	if err := Install(staged, dest, helloDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("destination not created: %v", err)
	}
}

// An unlisted file must fail rather than pass, or the check accomplishes
// nothing for a file an attacker or a bad mirror added.
func TestVerifyAgainstManifestRejectsUnlistedFiles(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "surprise.tar.bz2", "hello\n")

	err := VerifyAgainstManifest(path, map[string]string{"expected.tar.bz2": helloDigest})
	if err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("expected an unlisted-file error, got %v", err)
	}
}

func TestVerifyAgainstManifestMatchesByBaseName(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "mbdump.tar.bz2", "hello\n")

	if err := VerifyAgainstManifest(path, map[string]string{"mbdump.tar.bz2": helloDigest}); err != nil {
		t.Errorf("verification failed: %v", err)
	}
}
