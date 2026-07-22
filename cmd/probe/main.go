// Command probe runs ad-hoc queries against a Lidarr metadata server - the
// live api.lidarr.audio by default, our own server via -base - prints the
// response, and reports any drift from the contract pinned in
// internal/skyhook. It is also the fixture capture tool: -save writes the
// exact response bytes, which is the format fixtures/v0.4 requires.
package main

import (
	"bytes"
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

const defaultBase = "https://api.lidarr.audio/api/v0.4"

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: probe [flags] <command> [args]

Commands:
  root                          server info (version, replication date)
  artist <mbid>                 artist with skeletal album list
  album <mbid>                  album with releases and tracks
  search-artist <query>
  search-album <query> [artist]
  search-all <query>
  recent-artist [unix-since]    since defaults to 7 days ago
  recent-album [unix-since]

Search queries are lowercased and trimmed exactly like Lidarr sends them.
The response body goes to stdout; status, timing and the contract check go
to stderr, so output pipes cleanly into jq.

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	base := flag.String("base", defaultBase, "metadata server base URL")
	raw := flag.Bool("raw", false, "print exact response bytes instead of pretty JSON")
	save := flag.String("save", "", "write exact response bytes to this file (fixture capture)")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	path, query, target, err := route(flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		usage()
		os.Exit(2)
	}

	u := strings.TrimRight(*base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	start := time.Now()
	status, body, err := fetch(u)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "GET %s\nHTTP %d, %d bytes, %s\n", u, status, len(body), time.Since(start).Round(time.Millisecond))
	if status != http.StatusOK {
		os.Stdout.Write(append(body, '\n'))
		os.Exit(1)
	}

	if *save != "" {
		if err := os.WriteFile(*save, body, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "saved exact bytes to %s - add it to fixtures/v0.4/README.md provenance\n", *save)
	}

	if *raw {
		os.Stdout.Write(body)
	} else {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err != nil {
			os.Stdout.Write(body)
		} else {
			pretty.WriteTo(os.Stdout)
		}
	}
	fmt.Println()

	diffs, err := skyhook.ContractDiff(body, target)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "contract: not checkable: %v\n", err)
	case len(diffs) == 0:
		fmt.Fprintln(os.Stderr, "contract: OK (matches internal/skyhook)")
	default:
		if len(diffs) > 20 {
			diffs = append(diffs[:20], fmt.Sprintf("... and %d more", len(diffs)-20))
		}
		fmt.Fprintf(os.Stderr, "contract: DRIFT vs internal/skyhook:\n  %s\n", strings.Join(diffs, "\n  "))
	}
}

// route maps a command line to the request path, query, and the skyhook type
// that pins the route's contract.
func route(args []string) (string, url.Values, any, error) {
	need := func(n int) error {
		if len(args) < n+1 {
			return fmt.Errorf("%s needs %d argument(s)", args[0], n)
		}
		return nil
	}
	// Lidarr lowercases and trims search terms before sending; probing with
	// anything else would test queries the client never issues.
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	q := url.Values{}

	switch args[0] {
	case "root":
		return "/", nil, &skyhook.ServerInfo{}, nil
	case "artist":
		if err := need(1); err != nil {
			return "", nil, nil, err
		}
		return "/artist/" + args[1], nil, &skyhook.ArtistResource{}, nil
	case "album":
		if err := need(1); err != nil {
			return "", nil, nil, err
		}
		return "/album/" + args[1], nil, &skyhook.AlbumResource{}, nil
	case "search-artist":
		if err := need(1); err != nil {
			return "", nil, nil, err
		}
		q.Set("type", "artist")
		q.Set("query", norm(args[1]))
		return "/search", q, &[]skyhook.ArtistResource{}, nil
	case "search-album":
		if err := need(1); err != nil {
			return "", nil, nil, err
		}
		artist := ""
		if len(args) > 2 {
			artist = args[2]
		}
		q.Set("type", "album")
		q.Set("query", norm(args[1]))
		q.Set("artist", norm(artist))
		q.Set("includeTracks", "1")
		return "/search", q, &[]skyhook.AlbumResource{}, nil
	case "search-all":
		if err := need(1); err != nil {
			return "", nil, nil, err
		}
		q.Set("type", "all")
		q.Set("query", norm(args[1]))
		return "/search", q, &[]skyhook.EntityResource{}, nil
	case "recent-artist", "recent-album":
		since := fmt.Sprint(time.Now().Add(-7 * 24 * time.Hour).Unix())
		if len(args) > 1 {
			since = args[1]
		}
		q.Set("since", since)
		return "/recent/" + strings.TrimPrefix(args[0], "recent-"), q, &skyhook.RecentUpdatesResource{}, nil
	}
	return "", nil, nil, fmt.Errorf("unknown command %q", args[0])
}

func fetch(u string) (int, []byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("User-Agent", "lidarr-metadata-provider-probe/0.1 (+https://github.com/nc1107/lidarr-metadata-provider)")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		// The live service occasionally answers a one-off 500 (observed
		// 2026-07-22); a single retry keeps ad-hoc probing honest without
		// masking a real outage.
		if resp.StatusCode >= 500 && attempt == 0 {
			fmt.Fprintf(os.Stderr, "HTTP %d, retrying once...\n", resp.StatusCode)
			time.Sleep(time.Second)
			continue
		}
		return resp.StatusCode, body, nil
	}
}
