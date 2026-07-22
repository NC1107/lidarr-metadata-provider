// Command pipeline builds the dataset artifact from a MusicBrainz export.
//
// This runs on our machines, not on a user's. Everything expensive lives
// here so the server stays a stateless read-only process.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/checksum"
	"github.com/nc1107/lidarr-metadata-provider/internal/dataset"
	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/pipeline"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return usage()
	}
	switch args[0] {
	case "inspect":
		return inspect(args[1], args[2:])
	case "verify":
		if len(args) < 3 {
			return usage()
		}
		return verify(args[1], args[2:])
	case "build":
		if len(args) < 4 {
			return usage()
		}
		return buildDataset(args[1], args[2], args[3])
	case "build-artist":
		if len(args) < 4 {
			return usage()
		}
		return buildArtist(args[1], args[2], args[3:])
	default:
		return usage()
	}
}

func usage() error {
	fmt.Fprint(os.Stderr, `Usage:
  pipeline inspect <mbdump.tar.bz2> [table]
      Read an export's provenance and list its tables, or sample one table's
      rows. Confirms a download before spending an hour building from it.

  pipeline verify <SHA256SUMS> <file>...
      Check downloaded archives against the export's published manifest
      before building from them. A truncated download otherwise produces a
      dataset that is quietly missing rows.

  pipeline build <mbdump.tar.bz2> <mbdump-derived.tar.bz2> <out.db>
      Build the full dataset the server loads. Reads each archive once and
      streams artists into the output, so it is bounded by the join tables
      rather than by the number of payloads produced.

  pipeline build-artist <mbdump.tar.bz2> <mbdump-derived.tar.bz2> <mbid>...
      Build artist payloads straight from the export and print them. Both
      archives are required: the derived one carries release dates and
      ratings. One pass over each regardless of how many MBIDs are given, so
      ask for every artist of interest at once.
`)
	return fmt.Errorf("unknown command")
}

func inspect(path string, rest []string) error {
	archive, err := mbdump.Open(path)
	if err != nil {
		return err
	}
	// Report an unfamiliar schema rather than refusing, since inspecting a
	// new dump is exactly when you want to see what it says.
	archive.AllowSchemaMismatch = true

	info, err := archive.Info()
	if err != nil {
		return err
	}
	fmt.Printf("archive:              %s\n", path)
	fmt.Printf("timestamp:            %s\n", info.Timestamp)
	fmt.Printf("schema sequence:      %d", info.SchemaSequence)
	if info.SchemaSequence != mbdump.SupportedSchema {
		fmt.Printf("   [reader targets %d, a dataset build would refuse this]", mbdump.SupportedSchema)
	}
	fmt.Println()
	fmt.Printf("replication sequence: %d  (incremental updates resume here)\n", info.ReplicationSequence)

	if len(rest) > 0 {
		return sample(archive, rest[0])
	}

	tables, err := archive.Tables()
	if err != nil {
		// A truncated archive still yields whatever was read before the
		// stream ended, which is enough to confirm provenance.
		fmt.Printf("\ntables: could not list them all (%v)\n", err)
		if len(tables) > 0 {
			fmt.Printf("read %d before stopping: %s\n", len(tables), strings.Join(tables, ", "))
		}
		return nil
	}
	fmt.Printf("\ntables (%d):\n", len(tables))
	for _, t := range tables {
		fmt.Printf("  %s\n", t)
	}
	return nil
}

// sample prints the first few rows of one table, so a column layout can be
// eyeballed against the MusicBrainz schema docs before code depends on it.
func sample(archive *mbdump.Archive, table string) error {
	const limit = 5
	rows := 0

	err := archive.ReadTables(map[string]mbdump.RowFunc{
		table: func(row []mbdump.Field) error {
			rows++
			if rows > limit {
				return errEnough
			}
			fmt.Printf("\nrow %d (%d columns):\n", rows, len(row))
			for i, f := range row {
				value := f.Value
				if f.IsNull {
					value = "NULL"
				} else {
					value = fmt.Sprintf("%q", truncate(value, 70))
				}
				fmt.Printf("  [%2d] %s\n", i, value)
			}
			return nil
		},
	})
	if err != nil && !strings.Contains(err.Error(), errEnough.Error()) {
		return err
	}
	fmt.Printf("\nstopped after %d rows\n", min(rows, limit))
	return nil
}

// errEnough stops a scan once enough rows have been seen, rather than
// decompressing the rest of a multi-gigabyte archive.
var errEnough = fmt.Errorf("sampled enough rows")

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// buildArtist builds payloads for the given MBIDs and prints them, so the
// mapping can be checked against a golden fixture before any dataset format
// exists to store it in.
func buildArtist(corePath, derivedPath string, mbids []string) error {
	core, err := mbdump.Open(corePath)
	if err != nil {
		return err
	}
	derived, err := mbdump.Open(derivedPath)
	if err != nil {
		return err
	}
	info, err := core.Info()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "export %s (schema %d, replication %d)\n",
		info.Timestamp, info.SchemaSequence, info.ReplicationSequence)
	fmt.Fprintf(os.Stderr, "scanning for %d artist(s), one pass over each archive...\n", len(mbids))

	start := time.Now()
	built, err := pipeline.BuildArtists(core, derived, mbids)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "built %d artist(s) in %s\n", len(built), time.Since(start).Round(time.Second))

	for _, mbid := range mbids {
		artist, ok := built[strings.ToLower(strings.TrimSpace(mbid))]
		if !ok {
			continue
		}
		kept := len(skyhook.StandardProfile.Filter(artist.Albums))
		fmt.Fprintf(os.Stderr, "  %s: %d albums, %d pass the stock profile\n",
			artist.ArtistName, len(artist.Albums), kept)

		body, err := json.Marshal(artist)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
	}
	return nil
}

// verify checks downloaded archives against the manifest MusicBrainz
// publishes beside them.
func verify(manifestPath string, files []string) error {
	f, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer f.Close()

	sums, err := checksum.ParseSums(f)
	if err != nil {
		return err
	}

	failed := 0
	for _, path := range files {
		if err := checksum.VerifyAgainstManifest(path, sums); err != nil {
			fmt.Printf("FAIL  %s\n      %v\n", filepath.Base(path), err)
			failed++
			continue
		}
		fmt.Printf("ok    %s\n", filepath.Base(path))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d file(s) failed verification", failed, len(files))
	}
	return nil
}

// buildDataset produces the file the server loads.
func buildDataset(corePath, derivedPath, outPath string) error {
	core, err := mbdump.Open(corePath)
	if err != nil {
		return err
	}
	derived, err := mbdump.Open(derivedPath)
	if err != nil {
		return err
	}
	info, err := core.Info()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "export %s (schema %d, replication %d)\nbuilding %s\n",
		info.Timestamp, info.SchemaSequence, info.ReplicationSequence, outPath)

	writer, err := dataset.Create(outPath)
	if err != nil {
		return err
	}

	start := time.Now()
	written := 0
	err = pipeline.BuildAllArtists(core, derived, func(a *skyhook.ArtistResource) error {
		written++
		// A build runs for minutes with no other output, and a silent process
		// is indistinguishable from a hung one.
		if written%100_000 == 0 {
			fmt.Fprintf(os.Stderr, "  %s artists written, %s elapsed\n",
				humanCount(written), time.Since(start).Round(time.Second))
		}
		return writer.AddArtist(a)
	})
	if err != nil {
		return err
	}

	if err := writer.Finish(info.Timestamp, info.ReplicationSequence); err != nil {
		return err
	}
	artists, albums, tracks := writer.Counts()

	size := int64(0)
	if st, err := os.Stat(outPath); err == nil {
		size = st.Size()
	}
	fmt.Fprintf(os.Stderr, "done in %s: %s artists, %s albums, %s tracks, %.2f GB\n",
		time.Since(start).Round(time.Second), humanCount(int(artists)),
		humanCount(int(albums)), humanCount(int(tracks)), float64(size)/(1<<30))
	return nil
}

func humanCount(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
