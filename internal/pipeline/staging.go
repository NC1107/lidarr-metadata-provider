package pipeline

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// staging holds the track and recording rows an album payload is assembled
// from.
//
// Only these two go to disk, because only these two are too large to hold: an
// export carries roughly 35 million tracks and 30 million recordings, which
// is tens of gigabytes as Go structs. Everything else an album needs, its
// releases and media, is a few hundred megabytes and stays in memory where
// assembling a payload does not have to query for it.
//
// Indexes are created after loading, never during. Maintaining an index
// across 35 million individual inserts is dramatically slower than building
// it once over a finished table.
type staging struct {
	db   *sql.DB
	tx   *sql.Tx
	path string

	stmts   map[string]*sql.Stmt
	pending int
}

const stagingBatch = 50_000

const stagingSchema = `
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;
PRAGMA cache_size = -200000;

-- rg is resolved at load time so the emit pass can read tracks in album
-- order without joining back through medium and release.
CREATE TABLE s_track     (rg INTEGER, medium INTEGER, position INTEGER, number TEXT,
                          name TEXT, recording INTEGER, length INTEGER, gid TEXT, credit INTEGER);
CREATE TABLE s_recording (id INTEGER PRIMARY KEY, gid TEXT);
`

// Ordering the index the same way the emit pass reads lets that scan walk the
// index directly instead of sorting 35 million rows into a temp file.
const stagingIndexes = `
CREATE INDEX idx_track_album ON s_track(rg, medium, position);
`

var stagingInserts = map[string]string{
	"track":     `INSERT INTO s_track VALUES (?,?,?,?,?,?,?,?,?)`,
	"recording": `INSERT INTO s_recording VALUES (?,?)`,
}

func newStaging(path string) (*staging, error) {
	os.Remove(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(stagingSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("pipeline: creating staging tables: %w", err)
	}
	s := &staging{db: db, path: path, stmts: map[string]*sql.Stmt{}}
	if err := s.begin(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *staging) begin() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	s.tx = tx
	for name, query := range stagingInserts {
		stmt, err := tx.Prepare(query)
		if err != nil {
			return err
		}
		s.stmts[name] = stmt
	}
	return nil
}

func (s *staging) insert(table string, args ...any) error {
	if _, err := s.stmts[table].Exec(args...); err != nil {
		return fmt.Errorf("pipeline: staging %s: %w", table, err)
	}
	s.pending++
	if s.pending < stagingBatch {
		return nil
	}
	if err := s.tx.Commit(); err != nil {
		return err
	}
	s.pending = 0
	return s.begin()
}

// ready commits the load and builds the indexes the assembly queries need.
func (s *staging) ready() error {
	if err := s.tx.Commit(); err != nil {
		return err
	}
	if _, err := s.db.Exec(stagingIndexes); err != nil {
		return fmt.Errorf("pipeline: indexing staging tables: %w", err)
	}
	return nil
}

// close releases the database and removes the file, which is scratch space
// worth several gigabytes and of no use once a build finishes.
func (s *staging) close() error {
	err := s.db.Close()
	os.Remove(s.path)
	return err
}
