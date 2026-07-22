package dataset

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/checksum"
)

// Fetch downloads a dataset to dest when it is not already there.
//
// This is what makes the container a single docker run: a user never handles
// a MusicBrainz dump, only the finished artifact. Nothing is downloaded when
// dest already exists, so a restart is instant and an offline machine keeps
// working forever.
//
// The download is staged beside dest and verified before it is moved into
// place, so a truncated transfer leaves the previous dataset serving rather
// than replacing it with something unreadable.
func Fetch(ctx context.Context, url, dest string, log *slog.Logger) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// Staged in the destination directory because a rename is only atomic
	// within one filesystem, and a temp dir may be on another.
	staged := dest + ".incoming"

	log.Info("downloading dataset", "url", url, "to", dest)
	start := time.Now()

	size, err := download(ctx, url, staged, log)
	if err != nil {
		os.Remove(staged)
		return fmt.Errorf("dataset: downloading %s: %w", url, err)
	}

	want, err := fetchChecksum(ctx, url)
	if err != nil {
		os.Remove(staged)
		return fmt.Errorf("dataset: fetching checksum: %w", err)
	}

	if err := checksum.Install(staged, dest, want); err != nil {
		return fmt.Errorf("dataset: %w", err)
	}
	log.Info("dataset ready",
		"size", fmt.Sprintf("%.2f GB", float64(size)/(1<<30)),
		"took", time.Since(start).Round(time.Second).String())
	return nil
}

// checksumSuffix names the digest published beside a dataset artifact.
const checksumSuffix = ".sha256"

func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+checksumSuffix, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s for %s", resp.Status, url+checksumSuffix)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	// Accepts either a bare digest or sha256sum's "digest  filename" form.
	digest, _, _ := strings.Cut(strings.TrimSpace(string(body)), " ")
	if len(digest) != 64 {
		return "", fmt.Errorf("expected a sha256 digest, got %q", digest)
	}
	return strings.ToLower(digest), nil
}

func download(ctx context.Context, url, dest string, log *slog.Logger) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// A multi-gigabyte download over a slow line looks identical to a hang
	// without this.
	written, err := io.Copy(f, &progressReader{
		r: resp.Body, total: resp.ContentLength, log: log, last: time.Now(),
	})
	if err != nil {
		return written, err
	}
	return written, f.Sync()
}

type progressReader struct {
	r     io.Reader
	total int64
	read  int64
	log   *slog.Logger
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if time.Since(p.last) >= 15*time.Second {
		p.last = time.Now()
		if p.total > 0 {
			p.log.Info("downloading dataset",
				"progress", fmt.Sprintf("%.0f%%", float64(p.read)/float64(p.total)*100),
				"of", fmt.Sprintf("%.2f GB", float64(p.total)/(1<<30)))
		} else {
			p.log.Info("downloading dataset",
				"received", fmt.Sprintf("%.2f GB", float64(p.read)/(1<<30)))
		}
	}
	return n, err
}
