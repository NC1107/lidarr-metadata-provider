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

// SearchArtists finds artists by name.
//
// Ranking runs in stages because full text relevance alone gets the common
// cases wrong. Someone typing a band's exact name wants that band, but bm25
// happily puts "Yes Yes Yes" above "Yes", and MusicBrainz holds dozens of
// artists sharing a name where only one has a catalogue. Exact matches come
// first, ordered by notability; text relevance fills the rest.
func (r *Reader) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	blobs, err := r.searchStaged(ctx, "artist", "artist_fts", query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]skyhook.ArtistResource, 0, len(blobs))
	for _, blob := range blobs {
		var a skyhook.ArtistResource
		if err := decode(blob, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// SearchAlbums finds albums by title, narrowed to an artist when one is
// given.
//
// Lidarr sends the artist for manual import, where the file already says who
// made the record and the only question is which album it is. Ignoring it
// returns every album sharing a title, and "Greatest Hits" is a title
// thousands of artists share.
//
// The narrowing happens after retrieval rather than in the query, because the
// full text index holds titles alone. That means asking for more candidates
// than are wanted and discarding the ones credited to somebody else.
func (r *Reader) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	if limit <= 0 {
		limit = 25
	}
	want := limit
	if artist = Normalize(artist); artist != "" {
		want = limit * 8
	}

	blobs, err := r.searchStaged(ctx, "album", "album_fts", query, want)
	if err != nil {
		return nil, err
	}

	out := make([]skyhook.AlbumResource, 0, limit)
	for _, blob := range blobs {
		var a skyhook.AlbumResource
		if err := decode(blob, &a); err != nil {
			return nil, err
		}
		if artist != "" && !creditedTo(a, artist) {
			continue
		}
		out = append(out, a)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// creditedTo reports whether an album names this artist.
//
// Matching is loose in one direction on purpose: a file tagged "Simon &
// Garfunkel" should find an album credited to "Simon and Garfunkel", and a
// compilation credited to several artists should be found by any one of them.
func creditedTo(a skyhook.AlbumResource, artist string) bool {
	for _, credited := range a.Artists {
		name := Normalize(credited.ArtistName)
		if name == artist || strings.Contains(name, artist) || strings.Contains(artist, name) {
			return true
		}
	}
	return false
}

// searchStaged runs exact, then all-terms, then any-terms, stopping once it
// has enough. Later stages only ever add results the earlier ones missed, so
// a better match can never be pushed down by a worse one.
func (r *Reader) searchStaged(ctx context.Context, table, fts, query string, limit int) ([][]byte, error) {
	if limit <= 0 {
		limit = 25
	}
	seen := map[string]bool{}
	var out [][]byte

	collect := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() && len(out) < limit {
			var mbid string
			var blob []byte
			if err := rows.Scan(&mbid, &blob); err != nil {
				return err
			}
			if seen[mbid] {
				continue
			}
			seen[mbid] = true
			out = append(out, blob)
		}
		return rows.Err()
	}

	if norm := Normalize(query); norm != "" {
		err := collect(r.db.QueryContext(ctx,
			`SELECT mbid, payload FROM `+table+` WHERE norm = ? ORDER BY score DESC LIMIT ?`,
			norm, limit))
		if err != nil {
			return nil, err
		}
	}

	// All terms, then any term. The second pass matters for names joined by
	// a word the artist spells as a symbol, where "and" cannot match "&".
	for _, conjunction := range []string{" AND ", " OR "} {
		if len(out) >= limit {
			break
		}
		match := ftsQuery(query, conjunction)
		if match == "" {
			continue
		}
		err := collect(r.db.QueryContext(ctx, `
			SELECT a.mbid, a.payload FROM `+fts+` f
			JOIN `+table+` a ON a.mbid = f.mbid
			WHERE `+fts+` MATCH ? ORDER BY rank LIMIT ?`, match, limit*4))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ftsQuery turns a user's words into an FTS5 expression joined by
// conjunction.
//
// Every term is quoted because FTS5 treats bare punctuation as syntax: a
// search for "AC/DC" or "Where Are We Now?" would otherwise be a query error
// rather than a search. Normalising first means a query of pure punctuation
// yields no terms, and an empty expression is returned so the caller can skip
// the stage rather than send FTS5 something it rejects.
func ftsQuery(q, conjunction string) string {
	fields := strings.Fields(Normalize(q))
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+f+`"`)
	}
	if len(quoted) == 0 {
		return ""
	}
	return strings.Join(quoted, conjunction)
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

// SizeBreakdown reports where a dataset's bytes went.
//
// The artifact is downloaded by every user, so knowing which entity is
// responsible for its size is the difference between guessing at a fix and
// choosing one.
type SizeBreakdown struct {
	Table    string
	Rows     int64
	Bytes    int64
	Largest  int64
	Smallest int64
}

// Sizes measures stored payload sizes per table.
func (r *Reader) Sizes() ([]SizeBreakdown, error) {
	out := []SizeBreakdown{}
	for _, table := range []string{"artist", "album"} {
		var b SizeBreakdown
		b.Table = table
		row := r.db.QueryRow(`SELECT count(*), coalesce(sum(length(payload)),0),
			coalesce(max(length(payload)),0), coalesce(min(length(payload)),0) FROM ` + table)
		if err := row.Scan(&b.Rows, &b.Bytes, &b.Largest, &b.Smallest); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// PayloadPercentiles reports the distribution of payload sizes for a table.
//
// The mean alone is misleading here: a handful of heavily reissued albums
// carry hundreds of releases while most carry one, so knowing whether the
// bulk is large or the tail is long decides what, if anything, is worth
// changing.
func (r *Reader) PayloadPercentiles(table string) (map[string]int64, error) {
	var total int64
	if err := r.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&total); err != nil {
		return nil, err
	}
	if total == 0 {
		return map[string]int64{}, nil
	}

	out := map[string]int64{}
	for _, p := range []struct {
		label string
		frac  float64
	}{{"p50", 0.50}, {"p90", 0.90}, {"p99", 0.99}, {"p999", 0.999}} {
		offset := int64(p.frac * float64(total))
		if offset >= total {
			offset = total - 1
		}
		var size int64
		err := r.db.QueryRow(`SELECT length(payload) FROM `+table+
			` ORDER BY length(payload) LIMIT 1 OFFSET ?`, offset).Scan(&size)
		if err != nil {
			return nil, err
		}
		out[p.label] = size
	}
	return out, nil
}
