// Command parity measures search quality against the live Lidarr service.
//
// Correctness of a single lookup is provable against a fixture; search
// quality is not, because it is a ranking. The only workable definition is
// agreement with the service users are switching away from: if someone
// searches for a band and we put a different one first, they will notice and
// they will blame us.
//
// Top-1 agreement is the headline because that is what a user clicks. Top-5
// containment is reported alongside it to separate "we ranked it second"
// from "we do not have it at all", which are very different problems.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

const officialBase = "https://api.lidarr.audio/api/v0.4"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("base", "http://localhost:5001", "the server under test")
	queryFile := flag.String("queries", "fixtures/search-queries.txt", "file of queries, one per line")
	minTop1 := flag.Float64("min-top1", 0, "fail below this top-1 agreement percentage")
	verbose := flag.Bool("v", false, "print every query, not only the disagreements")
	flag.Parse()

	queries, err := readQueries(*queryFile)
	if err != nil {
		return err
	}
	fmt.Printf("Comparing %s against the official service over %d queries\n\n", *base, len(queries))

	var top1, top5, missing, failed int
	for _, q := range queries {
		ours, ourErr := searchArtists(*base+"/search", q)
		theirs, theirErr := searchArtists(officialBase+"/search", q)

		if ourErr != nil || theirErr != nil {
			failed++
			fmt.Printf("  ERROR  %-40q %v%v\n", q, ourErr, theirErr)
			continue
		}
		// A query the official service cannot answer says nothing about us.
		if len(theirs) == 0 {
			continue
		}
		want := theirs[0].ID

		switch {
		case len(ours) == 0:
			missing++
			fmt.Printf("  EMPTY  %-40q official: %s\n", q, theirs[0].ArtistName)
		case ours[0].ID == want:
			top1++
			top5++
			if *verbose {
				fmt.Printf("  match  %-40q %s\n", q, ours[0].ArtistName)
			}
		default:
			if rank := indexOf(ours, want) + 1; rank > 0 && rank <= 5 {
				top5++
				fmt.Printf("  rank%d  %-40q ours: %s | official: %s\n",
					rank, q, ours[0].ArtistName, theirs[0].ArtistName)
			} else {
				fmt.Printf("  MISS   %-40q ours: %s | official: %s\n",
					q, ours[0].ArtistName, theirs[0].ArtistName)
			}
		}
		// The official service is someone else's infrastructure and this
		// walks a few dozen queries through it, so do not hammer it.
		time.Sleep(250 * time.Millisecond)
	}

	compared := len(queries) - failed
	if compared == 0 {
		return fmt.Errorf("no queries could be compared")
	}
	pct := func(n int) float64 { return float64(n) / float64(compared) * 100 }

	fmt.Printf("\n  top-1 agreement   %5.1f%%  (%d of %d)\n", pct(top1), top1, compared)
	fmt.Printf("  top-5 containment %5.1f%%  (%d of %d)\n", pct(top5), top5, compared)
	fmt.Printf("  no results        %5d\n", missing)
	if failed > 0 {
		fmt.Printf("  errors            %5d\n", failed)
	}

	if *minTop1 > 0 && pct(top1) < *minTop1 {
		return fmt.Errorf("top-1 agreement %.1f%% is below the required %.1f%%", pct(top1), *minTop1)
	}
	return nil
}

func indexOf(artists []skyhook.ArtistResource, id string) int {
	for i, a := range artists {
		if a.ID == id {
			return i
		}
	}
	return -1
}

func searchArtists(endpoint, query string) ([]skyhook.ArtistResource, error) {
	v := url.Values{"type": {"artist"}, "query": {query}}
	req, err := http.NewRequest(http.MethodGet, endpoint+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LidarrMetadataProvider-parity/0.1")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	var out []skyhook.ArtistResource
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func readQueries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Lidarr lowercases and trims before sending, so match that here or
		// the gate measures queries the client never issues.
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, scanner.Err()
}
