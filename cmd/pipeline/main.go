// Command pipeline builds the dataset artifact from a MusicBrainz export.
//
// This runs on our machines, not on a user's. Everything expensive lives
// here so the server stays a stateless read-only process.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 || args[0] != "inspect" {
		fmt.Fprint(os.Stderr, `Usage: pipeline inspect <mbdump.tar.bz2>

Reads an export's provenance and lists the tables it carries, without
unpacking it. Useful for confirming a download before spending an hour
building a dataset from it.
`)
		return fmt.Errorf("unknown command")
	}
	path := args[1]

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

	if len(args) > 2 {
		return sample(archive, args[2])
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
