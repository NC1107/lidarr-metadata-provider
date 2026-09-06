// Command lidarr-metadata-provider serves Lidarr's metadata routes.
//
// The default configuration is offline and self-contained: it answers from
// the local dataset and never touches the network. Live fallback to
// MusicBrainz is opt-in, because depending on a third-party API at request
// time is exactly the failure mode this project exists to avoid.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/dataset"
	"github.com/nc1107/lidarr-metadata-provider/internal/musicbrainz"
	"github.com/nc1107/lidarr-metadata-provider/internal/ratelimit"
	"github.com/nc1107/lidarr-metadata-provider/internal/server"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lidarr-metadata-provider:", err)
		os.Exit(1)
	}
}

func run() error {
	// Every flag can also be set from the environment, which is how compose
	// files and orchestrators expect to pass settings. A value that does not
	// parse is an error rather than a silent fallback to the default: an
	// operator who wrote LMP_DATASET_REFRESH=3d wants refresh on, and finding
	// out months later that it never ran is worse than a refused start.
	env := &envConfig{}
	var (
		addr        = flag.String("addr", env.str("LMP_ADDR", ":5001"), "address to listen on")
		datasetPath = flag.String("dataset", env.str("LMP_DATASET", ""), "path to the dataset file to serve from")
		datasetURL  = flag.String("dataset-url", env.str("LMP_DATASET_URL", ""),
			"download the dataset from here when the file is absent, verifying it before use")
		refresh = flag.Duration("dataset-refresh", env.dur("LMP_DATASET_REFRESH", 0),
			"check dataset-url this often and install a newer dataset when one is published; 0 disables (opt in, the download is large)")
		web      = flag.Bool("web", env.boolean("LMP_WEB", false), "mount the local dev console at /ui")
		fallback = flag.Bool("fallback", env.boolean("LMP_FALLBACK", false),
			"query MusicBrainz live for lookups the dataset does not have (off by default; requires -contact)")
		contact = flag.String("contact", env.str("LMP_CONTACT", ""),
			"contact URL or email identifying this instance to MusicBrainz, required by -fallback")
		interval = flag.Duration("fallback-interval", env.dur("LMP_FALLBACK_INTERVAL", ratelimit.DefaultInterval),
			"minimum spacing between MusicBrainz requests; below 1s risks a block")
		maxPages = flag.Int("fallback-max-pages", env.integer("LMP_FALLBACK_MAX_PAGES", musicbrainz.DefaultMaxPages),
			"page cap per MusicBrainz browse, bounding how long one cold lookup can take")
	)
	flag.Usage = usage
	flag.Parse()
	if err := env.err(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Established before the dataset loads so both the first download and the
	// background refresh loop stop with the process. Without this a docker
	// stop during the initial multi-gigabyte download would be ignored until
	// the kill timeout.
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *refresh > 0 && *datasetURL == "" {
		log.Warn("-dataset-refresh is set but there is no -dataset-url to check, so updates are off")
	}
	if strings.HasPrefix(strings.ToLower(*datasetURL), "http://") {
		log.Warn("dataset-url is plain http; the checksum guards against a truncated download, not a tampered one",
			"url", *datasetURL, "fix", "use an https url")
	}

	var chain source.Chain
	var limiter *ratelimit.Limiter
	compare := map[string]source.Source{}
	var status server.DatasetStatus
	var liveStatus func() server.DatasetStatus
	var health func(context.Context) error

	// The dataset goes first so the network is only consulted for what it
	// does not already have.
	if *datasetPath != "" {
		if *datasetURL != "" {
			ctx, cancel := context.WithTimeout(appCtx, 6*time.Hour)
			err := dataset.Fetch(ctx, *datasetURL, *datasetPath, log)
			cancel()
			if err != nil {
				if appCtx.Err() != nil {
					log.Info("stopped during the dataset download")
					return nil
				}
				log.Error("refusing to start: the dataset could not be downloaded", "err", err)
				return err
			}
		}
		if _, err := os.Stat(*datasetPath); os.IsNotExist(err) {
			log.Error("refusing to start: no dataset at that path", "path", *datasetPath)
			log.Error("get one", "option 1", "-dataset-url <url> to download it automatically",
				"option 2", "build your own, see docs/BUILDING.md",
				"option 3", "-fallback -contact you@example.com to run without a dataset")
			return fmt.Errorf("no dataset at %s", *datasetPath)
		}
		reader, err := dataset.Open(*datasetPath)
		if err != nil {
			log.Error("refusing to start: the dataset could not be opened",
				"path", *datasetPath, "err", err)
			return err
		}

		info := reader.Info()
		status = datasetStatusFrom(info)

		// The live wrapper is always used, so the health probe and the console
		// see whatever is currently serving whether or not updates are on.
		live := dataset.NewLive(reader)
		defer live.Close()
		chain = append(chain, live)
		health = live.Probe

		// Updates are opt in: only when an interval is set and there is a url to
		// check does the dataset get replaced in place while serving.
		if *refresh > 0 && *datasetURL != "" {
			liveStatus = startRefresh(appCtx, live, *datasetURL, *datasetPath, *refresh, log)
		} else {
			liveStatus = func() server.DatasetStatus { return datasetStatusFrom(live.Info()) }
		}
		log.Info("dataset loaded", "path", *datasetPath, "export", info.ExportStamp,
			"artists", info.Artists, "albums", info.Albums, "tracks", info.Tracks)
	}
	datasetLoaded := status.Present

	var fallbackLookups func() int64
	if *fallback {
		if strings.TrimSpace(*contact) == "" {
			log.Error("refusing to start: -fallback needs -contact",
				"why", "MusicBrainz blocks user agents that carry no way to reach the maintainer",
				"fix", "-fallback -contact you@example.com")
			return errors.New("-fallback requires -contact")
		}
		limiter = ratelimit.New(*interval)
		client := musicbrainz.New(musicbrainz.UserAgent(version, *contact), limiter)
		client.MaxPages = *maxPages
		mb := source.Count(source.FromMusicBrainz(client))
		chain = append(chain, mb)
		fallbackLookups = mb.Calls
		// Also offered to the console as a comparison source, so an operator
		// can see the dataset and MusicBrainz side by side.
		compare["musicbrainz"] = mb
		log.Info("live fallback enabled",
			"contact", *contact,
			"interval", interval.String(),
			"maxPages", *maxPages)
	}

	// Refusing to start beats serving empty results. A server that answers
	// 200 with no albums looks healthy to Lidarr and quietly empties a
	// library, which is far worse than not coming up at all.
	if !datasetLoaded && len(chain) == 0 {
		log.Error("refusing to start: no metadata source available")
		log.Error("no dataset is loaded", "reason", "-dataset was not given")
		log.Error("and live fallback is off", "reason", "-fallback was not passed")
		log.Error("pick one", "option 1", "-dataset /path/to/dataset.db to serve offline",
			"option 2", "-fallback -contact you@example.com to answer from MusicBrainz live")
		return errors.New("no metadata source: pass -dataset <file>, or -fallback -contact <email or url>")
	}

	if !datasetLoaded && len(chain) > 0 {
		log.Warn("running without a dataset",
			"impact", "every lookup goes to MusicBrainz over the network, paced at "+interval.String()+" per request",
			"note", "this is a development configuration, not the intended offline setup")
	}

	srv := server.New(chain, server.Config{
		Version:         version,
		FallbackNames:   fallbackNames(chain),
		EnableWebUI:     *web,
		Dataset:         status,
		LiveDataset:     liveStatus,
		Health:          health,
		FallbackLookups: fallbackLookups,
		Compare:         compare,
		Limiter:         limiter,
		Logger:          log,
	})

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// ReadHeaderTimeout stops a slow-header client and IdleTimeout reaps
		// parked keep-alives. WriteTimeout has to cover the worst case rather
		// than the typical one: a live fallback lookup for a large artist can
		// spend tens of seconds paced through MusicBrainz, and a big artist
		// page is several megabytes to a client on a slow link.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "sources", strings.Join(chain.Names(), ","))
		if *web {
			log.Info("dev console", "url", consoleURL(*addr))
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-appCtx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// fallbackNames lists the network sources in the chain. The dataset is
// excluded because the console uses this to answer "does this instance ever
// leave the machine", and the dataset never does.
func fallbackNames(chain source.Chain) []string {
	out := []string{}
	for _, name := range chain.Names() {
		if name != "dataset" {
			out = append(out, name)
		}
	}
	return out
}

// datasetStatusFrom builds the console's dataset status from a reader's info.
func datasetStatusFrom(info dataset.Info) server.DatasetStatus {
	st := server.DatasetStatus{
		Present: true, Version: info.BuiltAt, ExportTimestamp: info.ExportStamp,
		Artists: info.Artists, Albums: info.Albums, Tracks: info.Tracks,
	}
	if built, err := time.Parse(time.RFC3339, info.BuiltAt); err == nil {
		st.InstalledAt = &built
	}
	return st
}

// startRefresh launches the background loop that installs a newer dataset when
// one is published, swapping it into live without dropping a request. It
// returns a status function that always reports the dataset currently served,
// so the console reflects an update without a restart.
//
// Establishing the baseline digest can mean hashing the whole file once, which
// on a slow disk takes minutes, so it happens inside the loop rather than
// before the server starts listening. A failure there disables update checks
// but keeps serving what is loaded.
func startRefresh(ctx context.Context, live *dataset.Live, url, path string, interval time.Duration, log *slog.Logger) func() server.DatasetStatus {
	schedule := "every " + interval.String()
	var nextCheck atomic.Pointer[time.Time]

	go func() {
		current, err := dataset.InstalledDigest(path, log)
		if err != nil {
			log.Warn("dataset update checks disabled: could not read the installed digest", "err", err)
			return
		}
		for {
			next := time.Now().Add(interval)
			nextCheck.Store(&next)
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			c, cancel := context.WithTimeout(ctx, 6*time.Hour)
			newDigest, updated, err := dataset.Refresh(c, url, path, current, log)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("dataset update check failed, keeping current dataset", "err", err)
				continue
			}
			if !updated {
				continue
			}
			// The download was validated before it was installed, so this open
			// is expected to succeed; the guard stays because the old reader
			// keeps its own file handles and remains a safe thing to keep serving.
			nr, err := dataset.Open(path)
			if err != nil {
				log.Error("installed a new dataset but could not open it, keeping current", "err", err)
				continue
			}
			live.Swap(nr)
			current = newDigest
			ni := nr.Info()
			log.Info("dataset updated", "export", ni.ExportStamp,
				"artists", ni.Artists, "albums", ni.Albums, "tracks", ni.Tracks)
		}
	}()

	return func() server.DatasetStatus {
		st := datasetStatusFrom(live.Info())
		st.UpdateSchedule = schedule
		st.NextCheck = nextCheck.Load()
		return st
	}
}

// envConfig reads flag defaults from the environment and collects parse
// errors, so a misspelt value refuses the start with a message naming the
// variable instead of silently running with the default.
type envConfig struct {
	errs []error
}

func (e *envConfig) err() error { return errors.Join(e.errs...) }

func (e *envConfig) str(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func (e *envConfig) dur(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not a duration (use forms like 6h, 72h, 30m)", key, v))
		return fallback
	}
	return d
}

func (e *envConfig) boolean(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not a boolean (use true or false)", key, v))
		return fallback
	}
	return b
}

func (e *envConfig) integer(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not an integer", key, v))
		return fallback
	}
	return n
}

func consoleURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	return "http://" + addr + "/ui"
}

func usage() {
	fmt.Fprintf(os.Stderr, `lidarr-metadata-provider - a self hosted Lidarr metadata server

Serves the routes Lidarr calls at the root of this address, so point Lidarr's
metadataSource at http://host:5001/ and it will work.

By default nothing leaves the machine. Live fallback covers the window
between a release appearing in MusicBrainz and appearing in a dataset
artifact, and is opt-in:

  lidarr-metadata-provider -fallback -contact you@example.com

Add -web for a local console at /ui to try searches and compare them with the
live cloud service without going through Lidarr.

Every flag has an environment variable of the same name prefixed LMP_, in
upper case with dashes as underscores (LMP_DATASET_URL, LMP_FALLBACK, ...).
Flags win over the environment.

Flags:
`)
	flag.PrintDefaults()
}
