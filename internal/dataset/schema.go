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

// schemaVersion guards the file format, not the MusicBrainz schema. A reader
// refuses a version it was not written against rather than misreading a
// changed layout.
const schemaVersion = 1

// createSQL is applied to a new dataset file.
//
// Payloads are stored compressed: the artifact is downloaded by every user,
// so its size is a bandwidth cost paid many times over, while decompressing
// one row costs a millisecond paid once per request.
const createSQL = `
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS artist (
    mbid    TEXT PRIMARY KEY,
    name    TEXT NOT NULL,
    payload BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS album (
    mbid    TEXT PRIMARY KEY,
    name    TEXT NOT NULL,
    payload BLOB NOT NULL
) WITHOUT ROWID;

-- Retired MBIDs redirect to their replacement. Lidarr holds ids for years,
-- so a library that predates a MusicBrainz merge still resolves.
CREATE TABLE IF NOT EXISTS artist_alias_mbid (
    old_mbid TEXT PRIMARY KEY,
    mbid     TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS album_alias_mbid (
    old_mbid TEXT PRIMARY KEY,
    mbid     TEXT NOT NULL
) WITHOUT ROWID;
`

// searchSQL builds the full text indexes. Kept separate from createSQL
// because a build populates the payload tables first and indexes afterwards,
// which is markedly faster than maintaining an index during a bulk insert.
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
