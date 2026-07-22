package dataset

import (
	"compress/flate"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
	"github.com/nc1107/lidarr-metadata-provider/internal/source"
)

// Reader serves payloads from a dataset file. It is read-only and safe for
// concurrent use.
type Reader struct {
	db   *sql.DB
	info Info
}

// Info is the dataset's provenance and contents, for the status view.
type Info struct {
	SchemaVersion       int
	BuiltAt             string
	ExportStamp         string
	ReplicationSequence int
	Artists             int64
	Albums              int64
	Tracks              int64
}

// ErrUnsupportedSchema means the file was written by a different version of
// this project than the one trying to read it.
var ErrUnsupportedSchema = errors.New("dataset: unsupported schema version")

// Open opens a dataset read-only.
//
// The immutable flag tells SQLite the file cannot change underneath it, which
// removes locking entirely. That is accurate here: updates are installed by
// replacing the file, never by writing into a live one.
func Open(path string) (*Reader, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	r := &Reader{db: db}
	if r.info, err = r.readInfo(); err != nil {
		db.Close()
		return nil, err
	}
	if r.info.SchemaVersion != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%w: file is version %d, this build reads version %d",
			ErrUnsupportedSchema, r.info.SchemaVersion, schemaVersion)
	}
	return r, nil
}

func (r *Reader) readInfo() (Info, error) {
	rows, err := r.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return Info{}, fmt.Errorf("dataset: reading metadata, is this a dataset file? %w", err)
	}
	defer rows.Close()

	var info Info
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return info, err
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case MetaSchemaVersion:
			info.SchemaVersion = int(n)
		case MetaBuiltAt:
			info.BuiltAt = v
		case MetaExportStamp:
			info.ExportStamp = v
		case MetaReplicationSeq:
			info.ReplicationSequence = int(n)
		case MetaArtistCount:
			info.Artists = n
		case MetaAlbumCount:
			info.Albums = n
		case MetaTrackCount:
			info.Tracks = n
		}
	}
	return info, rows.Err()
}

// Info returns the dataset's provenance and contents.
func (r *Reader) Info() Info { return r.info }

// Close releases the underlying handle.
func (r *Reader) Close() error { return r.db.Close() }

// Name identifies this source in the chain.
func (r *Reader) Name() string { return "dataset" }

// Artist serves a stored artist payload, following a retired MBID to its
// replacement when needed.
func (r *Reader) Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error) {
	var out skyhook.ArtistResource
	if err := r.lookup(ctx, "artist", "artist_alias_mbid", mbid, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Album serves a stored album payload.
func (r *Reader) Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error) {
	var out skyhook.AlbumResource
	if err := r.lookup(ctx, "album", "album_alias_mbid", mbid, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Reader) lookup(ctx context.Context, table, aliases, mbid string, into any) error {
	mbid = strings.ToLower(strings.TrimSpace(mbid))

	blob, err := r.payload(ctx, table, mbid)
	if errors.Is(err, sql.ErrNoRows) {
		// Lidarr holds MBIDs for as long as a library exists, so an id
		// retired by a MusicBrainz merge must still resolve.
		var current string
		row := r.db.QueryRowContext(ctx,
			`SELECT mbid FROM `+aliases+` WHERE old_mbid = ?`, mbid)
		if err := row.Scan(&current); err != nil {
			return source.ErrNotFound
		}
		if blob, err = r.payload(ctx, table, current); err != nil {
			return source.ErrNotFound
		}
	} else if err != nil {
		return err
	}
	return decode(blob, into)
}

func (r *Reader) payload(ctx context.Context, table, mbid string) ([]byte, error) {
	var blob []byte
	row := r.db.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE mbid = ?`, mbid)
	return blob, row.Scan(&blob)
}

// SearchArtists runs a full text search over artist names.
func (r *Reader) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.payload FROM artist_fts f
		JOIN artist a ON a.mbid = f.mbid
		WHERE artist_fts MATCH ? ORDER BY rank LIMIT ?`, ftsQuery(query), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []skyhook.ArtistResource{}
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var a skyhook.ArtistResource
		if err := decode(blob, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SearchAlbums runs a full text search over album titles.
func (r *Reader) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.payload FROM album_fts f
		JOIN album a ON a.mbid = f.mbid
		WHERE album_fts MATCH ? ORDER BY rank LIMIT ?`, ftsQuery(query), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []skyhook.AlbumResource{}
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var a skyhook.AlbumResource
		if err := decode(blob, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ftsQuery turns a user's words into an FTS5 expression.
//
// Every term is quoted because FTS5 treats bare punctuation as syntax: a
// search for "AC/DC" or "Where Are We Now?" would otherwise be a query error
// rather than a search. Terms are ANDed so extra words narrow the result the
// way someone typing them expects.
func ftsQuery(q string) string {
	fields := strings.Fields(strings.ToLower(q))
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, "")
		if f != "" {
			quoted = append(quoted, `"`+f+`"`)
		}
	}
	if len(quoted) == 0 {
		// FTS5 rejects an empty MATCH, and a query that matches nothing is
		// the honest answer to an empty search.
		return `"____no_such_term____"`
	}
	return strings.Join(quoted, " AND ")
}

func decode(blob []byte, into any) error {
	zr := flate.NewReader(strings.NewReader(string(blob)))
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("dataset: decompressing payload: %w", err)
	}
	return json.Unmarshal(raw, into)
}
