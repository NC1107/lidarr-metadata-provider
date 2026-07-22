// Package dataset stores and serves the precomputed metadata payloads.
//
// The runtime job is a lookup and nothing more. Payloads are assembled once
// by the build pipeline and stored whole, keyed by MBID, so answering
// /artist/{mbid} is one indexed read and a decompress rather than a join
// across a dozen tables. That is what makes a 1000-album artist fast enough
// to stop being the thing people complain about.
//
// Rows are keyed by MBID rather than by any internal identifier, which is
// what makes an update expressible as a patch of changed rows. The freshness
// plan depends on shipping deltas instead of a multi-gigabyte artifact twice
// a week, and a format that cannot be patched row by row forecloses that on
// day one.
package dataset

import (
	"strings"
	"unicode"
)

// schemaVersion guards the file format, not the MusicBrainz schema. A reader
// refuses a version it was not written against rather than misreading a
// changed layout.
const schemaVersion = 4

// createSQL is applied to a new dataset file.
//
// Payloads are stored compressed: the artifact is downloaded by every user,
// so its size is a bandwidth cost paid many times over, while decompressing
// one row costs a millisecond paid once per request.
// A larger page holds several payloads outright instead of spilling each one
// into an overflow page that then sits mostly empty. Measured on payloads
// matching this dataset's size distribution: 4 KB pages cost 1.92x the
// payload bytes, 16 KB pages with a rowid table cost 1.17x.
const createSQL = `
PRAGMA page_size = 16384;
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- The zstd dictionary the payloads were compressed with, trained on this
-- build's own payloads. Stored here so a reader needs nothing external to
-- decompress. Absent means payloads were compressed without one.
CREATE TABLE IF NOT EXISTS dictionary (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    data BLOB NOT NULL
);

-- norm is the name with case, punctuation and "&" folded away, so an exact
-- match can be found by lookup. Full text ranking alone puts "Yes Yes Yes"
-- above "Yes", which is the single largest source of wrong top results.
--
-- score is a notability proxy used to break ties between artists sharing a
-- name, of which MusicBrainz has a great many. Album count stands in for it:
-- the Prince people mean has hundreds, the others have none.
CREATE TABLE IF NOT EXISTS artist (
    mbid    TEXT NOT NULL,
    name    TEXT NOT NULL,
    norm    TEXT NOT NULL,
    aliases TEXT NOT NULL,
    score   INTEGER NOT NULL,
    payload BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS album (
    mbid        TEXT NOT NULL,
    name        TEXT NOT NULL,
    norm        TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    score       INTEGER NOT NULL,
    payload     BLOB NOT NULL
);

-- Retired MBIDs redirect to their replacement. Lidarr holds ids for years,
-- so a library that predates a MusicBrainz merge still resolves.
CREATE TABLE IF NOT EXISTS artist_alias_mbid (
    old_mbid TEXT NOT NULL,
    mbid     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS album_alias_mbid (
    old_mbid TEXT NOT NULL,
    mbid     TEXT NOT NULL
);
`

// The full text index stores normalised text, not the raw name. Indexing the
// raw name splits "The La's" into "the", "la", "s" while a normalised query
// asks for "las", so the two could never meet.
//
// searchSQL builds the full text indexes. Kept separate from createSQL
// because a build populates the payload tables first and indexes afterwards,
// which is markedly faster than maintaining an index during a bulk insert.
// Built after the rows are in, since maintaining an index across millions of
// inserts costs far more than building it once.
const searchIndexes = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_artist_mbid ON artist(mbid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_album_mbid ON album(mbid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artist_old ON artist_alias_mbid(old_mbid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_album_old ON album_alias_mbid(old_mbid);
CREATE INDEX IF NOT EXISTS idx_artist_norm ON artist(norm);
CREATE INDEX IF NOT EXISTS idx_album_norm ON album(norm);
`

const searchSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS artist_fts USING fts5(
    name, aliases, mbid UNINDEXED, tokenize = "unicode61 remove_diacritics 2"
);

CREATE VIRTUAL TABLE IF NOT EXISTS album_fts USING fts5(
    title, artist_name, mbid UNINDEXED, tokenize = "unicode61 remove_diacritics 2"
);
`

// Meta keys describing the artifact's provenance and contents.
const (
	MetaSchemaVersion  = "schema_version"
	MetaBuiltAt        = "built_at"
	MetaExportStamp    = "musicbrainz_export"
	MetaReplicationSeq = "replication_sequence"
	MetaArtistCount    = "artist_count"
	MetaAlbumCount     = "album_count"
	MetaTrackCount     = "track_count"
)

// Normalize folds a name to the form exact matching compares.
//
// The rules follow how people type a band name rather than how MusicBrainz
// stores it. Case is ignored and "&" reads as "and", since nobody types the
// symbol. Whitespace separates words, but other punctuation is dropped
// without separating, because "The La's" and "AC/DC" are typed as words
// rather than as "la s" and "ac dc".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false

	write := func(text string) {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(text)
	}

	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			write(string(r))
		case r == '&':
			write("and")
		case unicode.IsSpace(r):
			pendingSpace = true
		}
	}
	return b.String()
}
