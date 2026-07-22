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
	"runtime/debug"
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
	// A build holds several gigabytes of join tables, and Go's collector will
	// happily let the heap grow to twice that before running. On a machine
	// sized for the job rather than for the garbage, that headroom is the
	// difference between fitting and being killed, so cap it. The limit is
	// soft: exceeding it makes collection more aggressive rather than
	// failing an allocation.
	if limit := os.Getenv("LMP_MEMORY_LIMIT_GB"); limit != "" {
		if gb, err := strconv.Atoi(limit); err == nil && gb > 0 {
			debug.SetMemoryLimit(int64(gb) << 30)
		}
	} else {
		debug.SetMemoryLimit(defaultMemoryLimit)
	}

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		os.Exit(1)
	}
}

// defaultMemoryLimit keeps a build inside what a standard CI runner has,
// since that is where these are produced. Override with LMP_MEMORY_LIMIT_GB.
const defaultMemoryLimit = 10 << 30

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
	case "stats":
		return stats(args[1])
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

  pipeline stats <dataset.db>
      Report what a built dataset contains and where its size went. The
      artifact is downloaded by every user, so knowing which entity is
      responsible for its bulk decides whether anything is worth changing.

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
	// Marshalling and compressing dominate a build and parallelise cleanly;
	// only the SQLite writes have to stay serial. The builder also trains a
	// compression dictionary from the first payloads it sees.
	par := dataset.NewDictBuilder(writer, 0, 0)

	start := time.Now()
	artistsWritten, albumsWritten := 0, 0
	// A build runs for tens of minutes with no other output, and a silent
	// process is indistinguishable from a hung one.
	progress := func(kind string, n int) {
		if n%100_000 == 0 {
			fmt.Fprintf(os.Stderr, "  %s %s written, %s elapsed\n",
				humanCount(n), kind, time.Since(start).Round(time.Second))
		}
	}

	staging := outPath + ".staging"
	err = pipeline.BuildAll(core, derived, staging, pipeline.Emitter{
		Artist: func(a *skyhook.ArtistResource) error {
			artistsWritten++
			progress("artists", artistsWritten)
			return par.AddArtist(a)
		},
		Album: func(a *skyhook.AlbumResource) error {
			albumsWritten++
			progress("albums", albumsWritten)
			return par.AddAlbum(a)
		},
	})
	if err != nil {
		par.Close()
		return err
	}
	if err := par.Close(); err != nil {
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

// stats reports a built dataset's contents and size distribution.
func stats(path string) error {
	reader, err := dataset.Open(path)
	if err != nil {
		return err
	}
	defer reader.Close()

	info := reader.Info()
	fmt.Printf("dataset:     %s\n", path)
	fmt.Printf("built:       %s\n", info.BuiltAt)
	fmt.Printf("export:      %s (replication %d)\n", info.ExportStamp, info.ReplicationSequence)
	fmt.Printf("contents:    %s artists, %s albums, %s tracks\n\n",
		humanCount(int(info.Artists)), humanCount(int(info.Albums)), humanCount(int(info.Tracks)))

	sizes, err := reader.Sizes()
	if err != nil {
		return err
	}
	fmt.Printf("%-8s %12s %10s %12s %10s\n", "table", "rows", "total", "mean", "largest")
	var stored int64
	for _, s := range sizes {
		stored += s.Bytes
		mean := int64(0)
		if s.Rows > 0 {
			mean = s.Bytes / s.Rows
		}
		fmt.Printf("%-8s %12s %9.2fG %11dB %9s\n", s.Table, humanCount(int(s.Rows)),
			float64(s.Bytes)/(1<<30), mean, human(s.Largest))
	}

	if st, err := os.Stat(path); err == nil {
		fmt.Printf("\nfile:        %.2f GB, of which %.2f GB is stored payloads\n",
			float64(st.Size())/(1<<30), float64(stored)/(1<<30))
	}

	// The mean hides the shape. A few heavily reissued albums carry hundreds
	// of releases while most carry one, and which of those dominates decides
	// whether a smaller format would help.
	for _, table := range []string{"artist", "album"} {
		p, err := reader.PayloadPercentiles(table)
		if err != nil {
			return err
		}
		fmt.Printf("%-8s payload sizes: p50 %s  p90 %s  p99 %s  p99.9 %s\n",
			table, human(p["p50"]), human(p["p90"]), human(p["p99"]), human(p["p999"]))
	}
	return nil
}

func human(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}
