package pipeline

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// staging holds the release, medium and track rows an album payload is
// assembled from.
//
// These live on disk rather than in memory because they cannot fit in it: an
// export carries roughly 35 million tracks and 30 million recordings, which
// is tens of gigabytes as Go structs. Writing them to SQLite and letting it
// do the join keeps a build inside the memory a CI runner actually has, at
// the cost of a slower build, which is the right way round for a job that
// runs twice a week on a machine nobody is waiting on.
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

CREATE TABLE s_release   (id INTEGER PRIMARY KEY, gid TEXT, name TEXT, rg INTEGER, status INTEGER, comment TEXT);
CREATE TABLE s_medium    (id INTEGER PRIMARY KEY, rel INTEGER, position INTEGER, format INTEGER, name TEXT, track_count INTEGER);
CREATE TABLE s_track     (medium INTEGER, position INTEGER, number TEXT, name TEXT, recording INTEGER, length INTEGER, gid TEXT, credit INTEGER);
CREATE TABLE s_recording (id INTEGER PRIMARY KEY, gid TEXT);
CREATE TABLE s_rel_label (rel INTEGER, label INTEGER);
CREATE TABLE s_rel_country (rel INTEGER, area INTEGER, y INTEGER, m INTEGER, d INTEGER);
CREATE TABLE s_rel_date  (rel INTEGER, y INTEGER, m INTEGER, d INTEGER);
`

const stagingIndexes = `
CREATE INDEX idx_release_rg ON s_release(rg);
CREATE INDEX idx_medium_rel ON s_medium(rel);
CREATE INDEX idx_track_medium ON s_track(medium);
CREATE INDEX idx_rel_label ON s_rel_label(rel);
CREATE INDEX idx_rel_country ON s_rel_country(rel);
CREATE INDEX idx_rel_date ON s_rel_date(rel);
`

var stagingInserts = map[string]string{
	"release":   `INSERT INTO s_release VALUES (?,?,?,?,?,?)`,
	"medium":    `INSERT INTO s_medium VALUES (?,?,?,?,?,?)`,
	"track":     `INSERT INTO s_track VALUES (?,?,?,?,?,?,?,?)`,
	"recording": `INSERT INTO s_recording VALUES (?,?)`,
	"label":     `INSERT INTO s_rel_label VALUES (?,?)`,
	"country":   `INSERT INTO s_rel_country VALUES (?,?,?,?,?)`,
	"date":      `INSERT INTO s_rel_date VALUES (?,?,?,?)`,
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
