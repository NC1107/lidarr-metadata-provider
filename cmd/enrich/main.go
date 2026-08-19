// Command enrich builds the artist image and biography table the pipeline
// bakes into a dataset.
//
// It is a build-time tool that runs on our machines. The artist photos and
// biographies MusicBrainz does not carry are gathered here from Wikidata and
// Wikipedia, both open data, keyed by MusicBrainz artist MBID, and written to a
// file the pipeline reads. The file is a persistent cache: a later run keeps
// every biography whose Wikipedia article has not changed and only fetches the
// rest, so refreshing for a new dump is cheap.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/enrich"
	"github.com/nc1107/lidarr-metadata-provider/internal/musicbrainz"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "enrich:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "enrich/artists.jsonl", "the enrichment file to build and update")
	contact := flag.String("contact", "", "contact email or URL, required to identify the client to Wikimedia")
	workers := flag.Int("workers", 16, "concurrent biography fetches")
	imagesOnly := flag.Bool("images-only", false, "harvest images and article titles but skip biography fetching")
	flag.Parse()

	if *contact == "" {
		return fmt.Errorf("a -contact is required: Wikimedia asks clients to identify themselves")
	}
	userAgent := musicbrainz.UserAgent("", *contact)
	client := enrich.DefaultClient()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	cached, err := enrich.Load(*out)
	if err != nil {
		return fmt.Errorf("reading existing %s: %w", *out, err)
	}
	logf("loaded %d cached entries from %s", len(cached), *out)

	start := time.Now()
	logf("harvesting image and article links from Wikidata...")
	fresh, err := enrich.Harvest(client, userAgent, logf)
	if err != nil {
		// Enrichment (images and biographies) is a nice-to-have layered on top
		// of the dataset; a query service outage must not sink the whole build.
		// Keep the enrichment we already have and let the pipeline proceed with
		// it rather than publishing nothing.
		logf("harvest failed, building with the existing enrichment cache: %v", err)
		if serr := enrich.Save(*out, cached); serr != nil {
			return fmt.Errorf("preserving enrichment after a failed harvest: %w", serr)
		}
		return nil
	}
	// Carry forward any cached artist the harvest did not return, so a slice
	// that throttled this run does not drop enrichment already gathered.
	if restored := enrich.CarryMissing(fresh, cached); restored > 0 {
		logf("carried %d cached artists the harvest did not return", restored)
	}
	withImage, withWiki := 0, 0
	for _, a := range fresh {
		if a.Image != "" {
			withImage++
		}
		if a.Wiki != "" {
			withWiki++
		}
	}
	logf("harvested %d artists: %d with an image, %d with an article (%s)",
		len(fresh), withImage, withWiki, time.Since(start).Round(time.Second))

	carried := enrich.CarryOverviews(fresh, cached)
	logf("carried %d biographies forward from the cache", carried)

	if *imagesOnly {
		logf("images-only: skipping biography fetch")
	} else {
		save := func() {
			if err := enrich.Save(*out, fresh); err != nil {
				logf("  checkpoint save failed: %v", err)
			}
		}
		if err := enrich.FetchBios(client, userAgent, fresh, *workers, logf, save); err != nil {
			return err
		}
	}

	if err := enrich.Save(*out, fresh); err != nil {
		return err
	}
	withBio := 0
	for _, a := range fresh {
		if a.Overview != "" {
			withBio++
		}
	}
	logf("wrote %s: %d artists, %d with an image, %d with a biography (%s total)",
		*out, len(fresh), withImage, withBio, time.Since(start).Round(time.Second))
	return nil
}
