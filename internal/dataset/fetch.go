package dataset

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/checksum"
)

// fetchClient bounds the transfer without a total deadline (the dataset is
// multi-gigabyte, so a whole-request timeout is wrong): a connection that fails
// to connect, hand back headers, or stay alive is cut. A transfer that stops
// making progress is cut by downloadInto's stall timer and retried.
var fetchClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

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
	_, err := downloadInstall(ctx, url, dest, log)
	return err
}

// downloadInstall downloads the dataset at url and installs it at dest,
// replacing whatever is already there, and records the installed digest in a
// sidecar so a later refresh can tell whether the published dataset changed
// without hashing the whole file. It returns that digest.
func downloadInstall(ctx context.Context, url, dest string, log *slog.Logger) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	// Staged in the destination directory because a rename is only atomic
	// within one filesystem, and a temp dir may be on another.
	staged := dest + ".incoming"

	log.Info("downloading dataset", "url", url, "to", dest)
	start := time.Now()

	// A full dataset is larger than a GitHub release asset may be, so it is
	// published as parts listed in a manifest beside it. When that manifest is
	// present the parts are fetched and joined; when it is absent the url is a
	// single file, which keeps older releases and any other host working.
	parts, multipart, err := fetchManifest(ctx, url+partsSuffix)
	if err != nil {
		return "", fmt.Errorf("dataset: reading part manifest: %w", err)
	}

	var size int64
	if multipart {
		log.Info("dataset is published in parts", "count", len(parts))
		size, err = downloadParts(ctx, url, parts, staged, log)
	} else {
		size, err = download(ctx, url, staged, log)
	}
	if err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("dataset: downloading %s: %w", url, err)
	}

	want, err := fetchChecksum(ctx, url)
	if err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("dataset: fetching checksum: %w", err)
	}

	// Verify, then prove the file opens and serves, and only then move it into
	// place. A download that matches its checksum but cannot be read by this
	// build (a schema this version does not know, most likely) must never
	// replace a dataset that works.
	if err := checksum.Verify(staged, want); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("dataset: %w", err)
	}
	if err := Validate(staged); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("dataset: downloaded file failed validation, keeping the current dataset: %w", err)
	}
	if err := os.Rename(staged, dest); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("dataset: installing %s: %w", filepath.Base(dest), err)
	}
	recordDigest(dest, want, log)
	log.Info("dataset ready",
		"size", fmt.Sprintf("%.2f GB", float64(size)/(1<<30)),
		"took", time.Since(start).Round(time.Second).String())
	return want, nil
}

// checksumSuffix names the digest published beside a dataset artifact.
const checksumSuffix = ".sha256"

// partsSuffix names the manifest that, when present beside a dataset url, lists
// the ordered parts the artifact was split into.
const partsSuffix = ".parts"

// fetchManifest reads a parts manifest if one exists. A 404 means the artifact
// is a single file, which is not an error; any other failure is.
func fetchManifest(ctx context.Context, url string) (parts []string, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP %s for %s", resp.Status, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		// Tolerate a "sha256  name" manifest as well as a bare list, taking the
		// last field so either form yields the part filename.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		parts = append(parts, fields[len(fields)-1])
	}
	if len(parts) == 0 {
		return nil, false, fmt.Errorf("part manifest %s is empty", url)
	}
	return parts, true, nil
}

// downloadParts fetches each part in order and joins them into one staged file.
// The parts sit beside the dataset url, so their addresses are the dataset url
// with its last path segment replaced by the part name. The whole is verified
// against the published checksum afterwards, so a dropped or reordered part is
// caught before it is served.
func downloadParts(ctx context.Context, url string, parts []string, dest string, log *slog.Logger) (int64, error) {
	base := url[:strings.LastIndex(url, "/")+1]

	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var total int64
	for i, part := range parts {
		log.Info("downloading dataset part", "part", i+1, "of", len(parts), "name", part)
		// Remember where this part starts, so a failed attempt can be rewound
		// and retried rather than failing the whole multi-gigabyte transfer or
		// appending a partial part twice. The whole-file checksum is the final
		// backstop regardless.
		offset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return total, err
		}
		n, err := downloadRetrying(ctx, base+part, f, offset, log)
		if err != nil {
			return total, fmt.Errorf("part %s: %w", part, err)
		}
		total += n
	}
	return total, f.Sync()
}

// downloadAttempts is how many times one transfer is tried before the whole
// download is given up. Retries cover a dropped or stalled connection, which
// over a multi-gigabyte transfer is routine rather than exceptional.
const downloadAttempts = 3

// downloadRetrying fetches url into f starting at offset, rewinding and
// truncating back to offset before each retry so a partial attempt is never
// left in the file. It honours ctx between attempts as well as during them.
func downloadRetrying(ctx context.Context, url string, f *os.File, offset int64, log *slog.Logger) (int64, error) {
	var n int64
	var err error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		if attempt > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return 0, err
			}
			if err := f.Truncate(offset); err != nil {
				return 0, err
			}
			log.Info("retrying dataset download", "url", url, "attempt", attempt+1, "err", err)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
		}
		n, err = downloadInto(ctx, url, f, log)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	return n, err
}

func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+checksumSuffix, nil)
	if err != nil {
		return "", err
	}
	resp, err := fetchClient.Do(req)
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
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	written, err := downloadRetrying(ctx, url, f, 0, log)
	if err != nil {
		return written, err
	}
	return written, f.Sync()
}

// stallTimeout is how long a transfer may go without delivering a byte before
// it is cut and retried. Without it a half-open connection holds the download
// until the outer context expires, hours later.
const stallTimeout = 2 * time.Minute

// downloadInto streams one url into an already-open writer, so both a whole
// file and a run of parts share the same transfer, stall detection and
// progress logging.
func downloadInto(ctx context.Context, url string, w io.Writer, log *slog.Logger) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}

	// The stall timer cancels the request when no read completes in time;
	// every read that does complete pushes it back.
	var stalled atomic.Bool
	stall := time.AfterFunc(stallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer stall.Stop()

	// A multi-gigabyte download over a slow line looks identical to a hang
	// without this.
	n, err := io.Copy(w, &progressReader{
		r: resp.Body, total: resp.ContentLength, log: log, last: time.Now(),
		onRead: func() { stall.Reset(stallTimeout) },
	})
	if err != nil && stalled.Load() {
		return n, fmt.Errorf("no data received for %s, transfer stalled: %w", stallTimeout, err)
	}
	return n, err
}

type progressReader struct {
	r      io.Reader
	total  int64
	read   int64
	log    *slog.Logger
	last   time.Time
	onRead func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if n > 0 && p.onRead != nil {
		p.onRead()
	}
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
