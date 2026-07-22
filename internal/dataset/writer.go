package dataset

import (
	"bytes"
	"compress/flate"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// Writer builds a dataset file. It is not safe for concurrent use; a build is
// a single sequential job by design.
type Writer struct {
	db   *sql.DB
	tx   *sql.Tx
	path string

	insertArtist *sql.Stmt
	insertAlbum  *sql.Stmt
	insertOldID  *sql.Stmt
	insertOldAlb *sql.Stmt

	artists int64
	albums  int64
	tracks  int64

	buf     bytes.Buffer
	batched int
}

// batchSize bounds how many rows accumulate before a commit. Bulk inserting
// millions of rows in one transaction outruns available memory, and
// committing each row separately is slower by orders of magnitude.
const batchSize = 20_000

// Create starts a new dataset at path, replacing anything already there.
func Create(path string) (*Writer, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(createSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("dataset: creating schema: %w", err)
	}

	w := &Writer{db: db, path: path}
	if err := w.begin(); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *Writer) begin() error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	w.tx = tx

	stmts := []struct {
		dst **sql.Stmt
		sql string
	}{
		{&w.insertArtist, `INSERT OR REPLACE INTO artist (mbid, name, payload) VALUES (?, ?, ?)`},
		{&w.insertAlbum, `INSERT OR REPLACE INTO album (mbid, name, payload) VALUES (?, ?, ?)`},
		{&w.insertOldID, `INSERT OR REPLACE INTO artist_alias_mbid (old_mbid, mbid) VALUES (?, ?)`},
		{&w.insertOldAlb, `INSERT OR REPLACE INTO album_alias_mbid (old_mbid, mbid) VALUES (?, ?)`},
	}
	for _, s := range stmts {
		stmt, err := tx.Prepare(s.sql)
		if err != nil {
			return err
		}
		*s.dst = stmt
	}
	return nil
}

func (w *Writer) rotate() error {
	if w.batched < batchSize {
		return nil
	}
	if err := w.tx.Commit(); err != nil {
		return err
	}
	w.batched = 0
	return w.begin()
}

// AddArtist stores one artist payload plus its retired MBIDs.
func (w *Writer) AddArtist(a *skyhook.ArtistResource) error {
	blob, err := w.encode(a)
	if err != nil {
		return err
	}
	if _, err := w.insertArtist.Exec(a.ID, a.ArtistName, blob); err != nil {
		return fmt.Errorf("dataset: writing artist %s: %w", a.ID, err)
	}
	for _, old := range a.OldIDs {
		if _, err := w.insertOldID.Exec(old, a.ID); err != nil {
			return err
		}
	}
	w.artists++
	w.batched++
	return w.rotate()
}

// AddAlbum stores one album payload plus its retired MBIDs.
func (w *Writer) AddAlbum(a *skyhook.AlbumResource) error {
	blob, err := w.encode(a)
	if err != nil {
		return err
	}
	if _, err := w.insertAlbum.Exec(a.ID, a.Title, blob); err != nil {
		return fmt.Errorf("dataset: writing album %s: %w", a.ID, err)
	}
	for _, old := range a.OldIDs {
		if _, err := w.insertOldAlb.Exec(old, a.ID); err != nil {
			return err
		}
	}
	for _, rel := range a.Releases {
		w.tracks += int64(len(rel.Tracks))
	}
	w.albums++
	w.batched++
	return w.rotate()
}

// encode marshals and compresses a payload, reusing one buffer across rows so
// a multi-million row build does not spend its time in the allocator.
func (w *Writer) encode(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	w.buf.Reset()
	zw, err := flate.NewWriter(&w.buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return append([]byte(nil), w.buf.Bytes()...), nil
}

// Finish writes provenance, builds the search indexes, and closes the file.
//
// Indexes are built here rather than during insertion because maintaining an
// FTS index across millions of individual inserts is dramatically slower than
// building it once over a populated table.
func (w *Writer) Finish(exportStamp string, replicationSeq int) error {
	if err := w.tx.Commit(); err != nil {
		return err
	}

	if _, err := w.db.Exec(searchSQL); err != nil {
		return fmt.Errorf("dataset: creating search indexes: %w", err)
	}
	if _, err := w.db.Exec(`
		INSERT INTO artist_fts (name, aliases, mbid) SELECT name, '', mbid FROM artist;
		INSERT INTO album_fts (title, artist_name, mbid) SELECT name, '', mbid FROM album;
	`); err != nil {
		return fmt.Errorf("dataset: populating search indexes: %w", err)
	}

	meta := map[string]string{
		MetaSchemaVersion:  strconv.Itoa(schemaVersion),
		MetaBuiltAt:        time.Now().UTC().Format(time.RFC3339),
		MetaExportStamp:    exportStamp,
		MetaReplicationSeq: strconv.Itoa(replicationSeq),
		MetaArtistCount:    strconv.FormatInt(w.artists, 10),
		MetaAlbumCount:     strconv.FormatInt(w.albums, 10),
		MetaTrackCount:     strconv.FormatInt(w.tracks, 10),
	}
	for k, v := range meta {
		if _, err := w.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}

	// A dataset is written once and then only read, so trading build time for
	// a smaller download and a tighter page layout is the right way round.
	if _, err := w.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("dataset: vacuum: %w", err)
	}
	return w.db.Close()
}

// Counts reports what has been written so far.
func (w *Writer) Counts() (artists, albums, tracks int64) {
	return w.artists, w.albums, w.tracks
}
