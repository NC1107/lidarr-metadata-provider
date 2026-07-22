// Package checksum verifies downloaded files and installs them only once
// they are known good.
//
// Two things this project downloads must never be trusted on arrival: the
// MusicBrainz export a build reads, and the dataset artifact a running
// container fetches. A truncated or corrupted download that gets installed
// anyway is the worst failure mode available here, because the server would
// come up and serve confidently wrong metadata into somebody's library.
//
// The rule is therefore verify first, install second, and never overwrite a
// working file with an unverified one.
package checksum

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrMismatch means the file's digest did not match what was expected.
var ErrMismatch = errors.New("checksum: digest mismatch")

// ParseSums reads a sha256sum-style manifest, as published alongside every
// MusicBrainz export. Both the plain and binary ("*name") forms appear in the
// wild, so both are accepted.
func ParseSums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		digest, name, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if name == "" || len(digest) != sha256.Size*2 {
			continue
		}
		sums[name] = strings.ToLower(digest)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, errors.New("checksum: no digests found in manifest")
	}
	return sums, nil
}

// File returns the hex-encoded SHA-256 of the file at path.
func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify checks that the file at path has the expected digest.
func Verify(path, want string) error {
	got, err := File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w for %s: got %s, expected %s", ErrMismatch, filepath.Base(path), got, want)
	}
	return nil
}

// Install verifies staged against want and only then moves it over dest.
//
// The order is the whole point: a failed verification leaves dest untouched,
// so a bad download degrades to still running the previous dataset rather
// than to running a corrupt one. The staged file is removed on failure so a
// retry does not resume from a known-bad copy.
//
// Rename is atomic only within a filesystem, so staged should be written
// beside dest rather than in a temp directory that might live elsewhere.
func Install(staged, dest, want string) error {
	if err := Verify(staged, want); err != nil {
		os.Remove(staged)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, dest); err != nil {
		return fmt.Errorf("checksum: installing %s: %w", filepath.Base(dest), err)
	}
	return nil
}

// VerifyAgainstManifest looks the file up by base name in a manifest and
// verifies it. A file absent from the manifest is an error rather than a
// pass, since silently accepting an unlisted file defeats the check.
func VerifyAgainstManifest(path string, sums map[string]string) error {
	name := filepath.Base(path)
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("checksum: %s is not listed in the manifest", name)
	}
	return Verify(path, want)
}
