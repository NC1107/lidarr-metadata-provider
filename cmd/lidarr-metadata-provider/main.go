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
	"strings"
	"syscall"
	"time"

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
	var (
		addr     = flag.String("addr", ":5001", "address to listen on")
		web      = flag.Bool("web", false, "mount the local dev console at /ui")
		fallback = flag.Bool("fallback", false,
			"query MusicBrainz live for lookups the dataset does not have (off by default; requires -contact)")
		contact = flag.String("contact", "",
			"contact URL or email identifying this instance to MusicBrainz, required by -fallback")
		interval = flag.Duration("fallback-interval", ratelimit.DefaultInterval,
			"minimum spacing between MusicBrainz requests; below 1s risks a block")
		maxPages = flag.Int("fallback-max-pages", musicbrainz.DefaultMaxPages,
			"page cap per MusicBrainz browse, bounding how long one cold lookup can take")
	)
	flag.Usage = usage
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var chain source.Chain
	var limiter *ratelimit.Limiter

	// The dataset source lands here in Phase 1 and goes first in the chain,
	// so the network is only consulted for what it does not have.

	if *fallback {
		if strings.TrimSpace(*contact) == "" {
			return errors.New("-fallback requires -contact: MusicBrainz throttles user agents that carry no way to reach the maintainer")
		}
		limiter = ratelimit.New(*interval)
		client := musicbrainz.New(musicbrainz.UserAgent(version, *contact), limiter)
		client.MaxPages = *maxPages
		chain = append(chain, source.FromMusicBrainz(client))
		log.Info("live fallback enabled",
			"contact", *contact,
			"interval", interval.String(),
			"maxPages", *maxPages)
	}

	if len(chain) == 0 {
		return errors.New("no sources configured: this build has no dataset yet, so start it with -fallback -contact <email or url>")
	}

	srv := server.New(chain, server.Config{
		Version:       version,
		FallbackNames: chain.Names(),
		EnableWebUI:   *web,
		Limiter:       limiter,
		Logger:        log,
	})

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
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

Flags:
`)
	flag.PrintDefaults()
}
