package mbdump

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// SupportedSchema is the MusicBrainz schema sequence this reader was written
// against. The export declares its own in SCHEMA_SEQUENCE, and a mismatch
// means columns may have moved. Failing loudly on an unknown schema is the
// entire point: silently reading a shifted column would produce a dataset
// that is wrong in ways no contract test can see.
const SupportedSchema = 31

// ErrSchemaMismatch is returned when the archive declares a schema sequence
// this reader has not been verified against.
var ErrSchemaMismatch = errors.New("mbdump: unsupported schema sequence")

// Info is the provenance an export carries at its root.
type Info struct {
	// Timestamp is when MusicBrainz produced the export.
	Timestamp string
	// SchemaSequence is the MusicBrainz database schema version.
	SchemaSequence int
	// ReplicationSequence is the point in the Live Data Feed this export
	// corresponds to, and therefore where incremental updates resume.
	ReplicationSequence int
}

// Archive reads a MusicBrainz export tarball.
type Archive struct {
	path string
	// AllowSchemaMismatch proceeds past an unrecognised schema sequence.
	// Intended for investigating a new dump, not for producing a dataset.
	AllowSchemaMismatch bool
}

// Open returns an Archive over the export at path. The file is opened per
// pass rather than held, because reading the archive is inherently a
// sequential scan and callers may make several.
func Open(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &Archive{path: path}, nil
}

// Info reads the provenance files. It stops as soon as it has them rather
// than scanning the whole archive, since they sit at the front.
func (a *Archive) Info() (Info, error) {
	var info Info
	found := 0

	err := a.walk(func(name string, r io.Reader) (bool, error) {
		switch name {
		case "TIMESTAMP", "SCHEMA_SEQUENCE", "REPLICATION_SEQUENCE":
		default:
			return true, nil
		}
		body, err := io.ReadAll(io.LimitReader(r, 4096))
		if err != nil {
			return false, err
		}
		text := strings.TrimSpace(string(body))

		switch name {
		case "TIMESTAMP":
			info.Timestamp = text
		case "SCHEMA_SEQUENCE":
			if info.SchemaSequence, err = strconv.Atoi(text); err != nil {
				return false, fmt.Errorf("mbdump: unreadable SCHEMA_SEQUENCE %q: %w", text, err)
			}
		case "REPLICATION_SEQUENCE":
			if info.ReplicationSequence, err = strconv.Atoi(text); err != nil {
				return false, fmt.Errorf("mbdump: unreadable REPLICATION_SEQUENCE %q: %w", text, err)
			}
		}
		found++
		return found < 3, nil
	})
	if err != nil {
		return info, err
	}
	if found == 0 {
		return info, errors.New("mbdump: no provenance files found, is this a MusicBrainz export?")
	}
	if !a.AllowSchemaMismatch && info.SchemaSequence != SupportedSchema {
		return info, fmt.Errorf("%w: archive declares %d, this reader targets %d",
			ErrSchemaMismatch, info.SchemaSequence, SupportedSchema)
	}
	return info, nil
}

// Tables lists the table files in the archive, in archive order.
func (a *Archive) Tables() ([]string, error) {
	var names []string
	err := a.walk(func(name string, _ io.Reader) (bool, error) {
		if dir, table := path.Split(name); dir == "mbdump/" && table != "" {
			names = append(names, table)
		}
		return true, nil
	})
	return names, err
}

// RowFunc receives each row of a table. Fields are only valid until the next
// call, so a handler that keeps a value must copy it. Returning an error
// stops the scan and surfaces from ReadTables.
type RowFunc func(row []Field) error

// ReadTables scans the archive once, dispatching each wanted table's rows to
// its handler. Passing every table needed in one call matters: the archive is
// a sequential bzip2 stream, so each extra pass re-decompresses gigabytes.
//
// Tables absent from the archive are reported rather than ignored, since a
// silently missing table would produce a quietly incomplete dataset.
func (a *Archive) ReadTables(handlers map[string]RowFunc) error {
	if len(handlers) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(handlers))
	fields := make([]Field, 0, 32)

	err := a.walk(func(name string, r io.Reader) (bool, error) {
		dir, table := path.Split(name)
		if dir != "mbdump/" {
			return true, nil
		}
		handler, want := handlers[table]
		if !want {
			return true, nil
		}
		seen[table] = true

		scanner := bufio.NewScanner(r)
		// MusicBrainz rows can be long: an artist annotation or a release
		// comment easily exceeds bufio's 64 KB default, which would
		// otherwise surface as a truncated row rather than an error.
		scanner.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

		line := 0
		for scanner.Scan() {
			line++
			fields = splitRow(scanner.Text(), fields)
			if err := handler(fields); err != nil {
				return false, fmt.Errorf("mbdump: %s line %d: %w", table, line, err)
			}
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("mbdump: reading %s: %w", table, err)
		}
		return len(seen) < len(handlers), nil
	})
	if err != nil {
		return err
	}

	missing := make([]string, 0)
	for table := range handlers {
		if !seen[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("mbdump: tables not present in archive: %s", strings.Join(missing, ", "))
	}
	return nil
}

// ParallelTools are external bzip2 decompressors tried before the standard
// library's. bzip2 stores independently compressed blocks, so decompression
// parallelises across cores, and the single-threaded decoder is the whole
// bottleneck when reading a 6.9 GB export: minutes rather than a minute or
// two. Neither tool is required, and neither changes the output.
var ParallelTools = []string{"lbzip2", "pbzip2"}

// decompress opens the archive, preferring a parallel external decompressor.
// The returned closer must be called; for an external tool it also reaps the
// child, which matters because callers routinely stop reading early.
func (a *Archive) decompress() (io.Reader, func(), error) {
	for _, tool := range ParallelTools {
		bin, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, "-dc", a.path)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			continue
		}
		if err := cmd.Start(); err != nil {
			continue
		}
		return bufio.NewReaderSize(stdout, 4*1024*1024), func() {
			// Stopping early leaves the child mid-stream, so kill rather
			// than wait for it to finish decompressing gigabytes nobody
			// will read.
			stdout.Close()
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			cmd.Wait()
		}, nil
	}

	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}
	// A large buffer in front of bzip2 measurably reduces syscall overhead
	// on a multi-gigabyte sequential read.
	return bzip2.NewReader(bufio.NewReaderSize(f, 4*1024*1024)), func() { f.Close() }, nil
}

// walk streams the archive, calling fn for each entry. fn returns false to
// stop early, which lets Info and ReadTables skip the remaining gigabytes
// once they have what they came for.
func (a *Archive) walk(fn func(name string, r io.Reader) (bool, error)) error {
	stream, closeStream, err := a.decompress()
	if err != nil {
		return err
	}
	defer closeStream()

	tr := tar.NewReader(stream)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mbdump: reading %s: %w", a.path, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		more, err := fn(path.Clean(header.Name), tr)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}
