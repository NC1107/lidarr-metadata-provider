package dataset

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/nc1107/lidarr-metadata-provider/internal/checksum"
)

// digestSidecar names the file that records the digest of the installed
// dataset, written beside it. It lets a refresh compare against the published
// digest without re-hashing a multi-gigabyte file on every check.
func digestSidecar(dest string) string { return dest + ".installed-sha256" }

// recordDigest writes the installed digest beside the dataset. A failure here
// is not fatal: the worst case is a refresh re-hashes or re-downloads once, so
// it is logged rather than propagated.
func recordDigest(dest, digest string, log *slog.Logger) {
	if err := os.WriteFile(digestSidecar(dest), []byte(digest+"\n"), 0o644); err != nil {
		log.Warn("could not record dataset digest", "err", err)
	}
}

// InstalledDigest returns the digest of the dataset currently at dest. It reads
// the sidecar when present, and otherwise hashes the file once and records it,
// so a dataset installed before update checks existed still gets a cheap
// comparison from then on.
func InstalledDigest(dest string, log *slog.Logger) (string, error) {
	if b, err := os.ReadFile(digestSidecar(dest)); err == nil {
		if d := strings.TrimSpace(string(b)); len(d) == 64 {
			return strings.ToLower(d), nil
		}
	}
	log.Info("checksumming existing dataset to enable update checks", "note", "one time only")
	d, err := checksum.File(dest)
	if err != nil {
		return "", err
	}
	recordDigest(dest, d, log)
	return strings.ToLower(d), nil
}

// Refresh re-downloads the dataset only when the published one differs from the
// installed digest, and returns the new digest and whether an update happened.
// The published digest is one small request; the multi-gigabyte transfer only
// runs on a real change. On any error the installed dataset is left untouched
// and current is returned, so a failed check degrades to serving what already
// works rather than to an outage.
func Refresh(ctx context.Context, url, dest, current string, log *slog.Logger) (digest string, updated bool, err error) {
	remote, err := fetchChecksum(ctx, url)
	if err != nil {
		return current, false, err
	}
	if strings.EqualFold(remote, current) {
		return current, false, nil
	}
	log.Info("newer dataset published, updating",
		"have", short(current), "want", short(remote))
	got, err := downloadInstall(ctx, url, dest, log)
	if err != nil {
		return current, false, err
	}
	return got, true, nil
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
