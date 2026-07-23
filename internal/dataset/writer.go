package dataset

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// compressionLevel trades a little build time for a smaller download. Measured
// on real payloads: BetterCompression is ~1.5% smaller than Default for ~1.4x
// the encode time, while BestCompression is 28x slower for barely more, so it
// stays rejected. Users pay the download; we pay the build once.
const compressionLevel = zstd.SpeedBetterCompression

// Writer builds a dataset file. It is not safe for concurrent use; a build is
// a single sequential job by design.
type Writer struct {
	db       *sql.DB
	tx       *sql.Tx
	path     string
	building string

	insertArtist *sql.Stmt
	insertAlbum  *sql.Stmt
	insertOldID  *sql.Stmt
	insertOldAlb *sql.Stmt

	artists int64
	albums  int64
	tracks  int64

	enc     *zstd.Encoder
	dict    []byte
	batched int
}

// batchSize bounds how many rows accumulate before a commit. Bulk inserting
// millions of rows in one transaction outruns available memory, and
// committing each row separately is slower by orders of magnitude.
const batchSize = 20_000

// Create starts a new dataset for path. The build happens in a sibling
// ".building" file and is renamed over path only once Finish succeeds, so a
// build that fails partway (an encoding error, a full disk) leaves the previous
// working dataset in place rather than destroying a multi-hour artifact, the
// same discipline the download path already follows.
func Create(path string) (*Writer, error) {
	building := path + ".building"
	if err := os.Remove(building); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	db, err := sql.Open("sqlite", building)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(createSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("dataset: creating schema: %w", err)
	}

	// zstd rather than flate because flate's 32 KB window cannot see the
	// repetition inside a large payload: a heavily reissued album stores the
	// same track list once per edition, and the same album compresses 8.9x
	// smaller here than it does with flate.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(compressionLevel))
	if err != nil {
		db.Close()
		return nil, err
	}

	w := &Writer{db: db, path: path, building: building, enc: enc}
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
		{&w.insertArtist, `INSERT INTO artist (mbid, name, norm, aliases, score, payload) VALUES (?, ?, ?, ?, ?, ?)`},
		{&w.insertAlbum, `INSERT INTO album (mbid, name, norm, artist_name, score, payload) VALUES (?, ?, ?, ?, ?, ?)`},
		{&w.insertOldID, `INSERT INTO artist_alias_mbid (old_mbid, mbid) VALUES (?, ?)`},
		{&w.insertOldAlb, `INSERT INTO album_alias_mbid (old_mbid, mbid) VALUES (?, ?)`},
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
	return w.addArtist(a, nil)
}

func (w *Writer) addArtist(a *skyhook.ArtistResource, blob []byte) error {
	var err error
	if blob == nil {
		if blob, err = w.encode(a); err != nil {
			return err
		}
	}
	// Aliases are indexed too, so an artist found under a different spelling
	// or a non-Latin name is still reachable by the one a user types.
	aliases := make([]string, 0, len(a.ArtistAliases))
	for _, alias := range a.ArtistAliases {
		if n := Normalize(alias); n != "" {
			aliases = append(aliases, n)
		}
	}
	// Album count stands in for notability, which is what decides between the
	// dozens of artists sharing a name.
	if _, err := w.insertArtist.Exec(a.ID, a.ArtistName, Normalize(a.ArtistName),
		strings.Join(aliases, " "), len(a.Albums), blob); err != nil {
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
	return w.addAlbum(a, nil)
}

func (w *Writer) addAlbum(a *skyhook.AlbumResource, blob []byte) error {
	var err error
	if blob == nil {
		if blob, err = w.encode(a); err != nil {
			return err
		}
	}
	// The artist name is indexed alongside the title, so searching an artist
	// finds their albums rather than only albums with their name in the title.
	credits := make([]string, 0, len(a.Artists))
	for _, ar := range a.Artists {
		if n := Normalize(ar.ArtistName); n != "" {
			credits = append(credits, n)
		}
	}
	if _, err := w.insertAlbum.Exec(a.ID, a.Title, Normalize(a.Title),
		strings.Join(credits, " "), len(a.Releases), blob); err != nil {
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

// encode marshals and compresses a payload.
func (w *Writer) encode(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return w.enc.EncodeAll(raw, nil), nil
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

	if _, err := w.db.Exec(searchIndexes); err != nil {
		return fmt.Errorf("dataset: creating name indexes: %w", err)
	}
	if _, err := w.db.Exec(searchSQL); err != nil {
		return fmt.Errorf("dataset: creating search indexes: %w", err)
	}
	if _, err := w.db.Exec(`
		INSERT INTO artist_fts (name, aliases, mbid) SELECT norm, aliases, mbid FROM artist;
		INSERT INTO album_fts (title, artist_name, mbid) SELECT norm, artist_name, mbid FROM album;
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
	w.enc.Close()
	if err := w.db.Close(); err != nil {
		return err
	}
	// The build is complete and consistent; only now replace the previous
	// dataset with it.
	return os.Rename(w.building, w.path)
}

// SetDictionary switches encoding to use a trained dictionary and records it
// in the file so a reader can decompress. It must be called before any
// payload is written, since payloads written earlier would not carry it.
func (w *Writer) SetDictionary(dict []byte) error {
	if len(dict) == 0 {
		return nil
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(compressionLevel), zstd.WithEncoderDict(dict))
	if err != nil {
		return err
	}
	w.enc.Close()
	w.enc = enc
	w.dict = dict
	if _, err := w.db.Exec(`INSERT OR REPLACE INTO dictionary (id, data) VALUES (1, ?)`, dict); err != nil {
		return fmt.Errorf("dataset: storing dictionary: %w", err)
	}
	return nil
}

// Dictionary returns the encoding dictionary, or nil if none is set.
func (w *Writer) Dictionary() []byte { return w.dict }

// Counts reports what has been written so far.
func (w *Writer) Counts() (artists, albums, tracks int64) {
	return w.artists, w.albums, w.tracks
}

// Parallel wraps a Writer so marshalling and compression run across cores
// while writes stay on one goroutine.
//
// A build spends almost all its time turning payloads into compressed bytes,
// which is pure CPU and embarrassingly parallel, and almost none of it
// inserting: the measured split was roughly two hours of the former against
// minutes of the latter, on one core of eight. SQLite writes stay serial
// because they must.
type Parallel struct {
	w       *Writer
	jobs    chan job
	results chan result
	done    chan struct{}

	// The writer goroutine records failures while the producer checks them,
	// so this needs guarding rather than a bare field.
	mu  sync.Mutex
	err error
}

func (p *Parallel) fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *Parallel) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

type job struct {
	artist *skyhook.ArtistResource
	album  *skyhook.AlbumResource
	seq    int
}

type result struct {
	job
	blob []byte
	err  error
}

// NewParallel starts workers feeding a single writer. Close must be called.
func NewParallel(w *Writer, workers int) *Parallel {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	p := &Parallel{
		w: w,
		// Bounded so a fast producer cannot outrun the writer and hold every
		// pending payload in memory at once.
		jobs:    make(chan job, workers*4),
		results: make(chan result, workers*4),
		done:    make(chan struct{}),
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker owns an encoder; sharing one would serialise them
			// again on its internal state.
			opts := []zstd.EOption{zstd.WithEncoderLevel(compressionLevel)}
			if p.w.dict != nil {
				opts = append(opts, zstd.WithEncoderDict(p.w.dict))
			}
			enc, err := zstd.NewWriter(nil, opts...)
			if err != nil {
				return
			}
			defer enc.Close()

			for j := range p.jobs {
				var raw []byte
				var err error
				if j.artist != nil {
					raw, err = json.Marshal(j.artist)
				} else {
					raw, err = json.Marshal(j.album)
				}
				if err != nil {
					p.results <- result{job: j, err: err}
					continue
				}
				p.results <- result{job: j, blob: enc.EncodeAll(raw, nil)}
			}
		}()
	}
	go func() { wg.Wait(); close(p.results) }()

	go func() {
		defer close(p.done)
		for r := range p.results {
			// Drain rather than return: the producer is still sending, and
			// abandoning the channel would deadlock it.
			if p.failed() != nil {
				continue
			}
			if r.err != nil {
				p.fail(r.err)
				continue
			}
			if r.artist != nil {
				p.fail(p.w.addArtist(r.artist, r.blob))
			} else {
				p.fail(p.w.addAlbum(r.album, r.blob))
			}
		}
	}()
	return p
}

// AddArtist queues an artist for encoding and writing.
func (p *Parallel) AddArtist(a *skyhook.ArtistResource) error {
	if err := p.failed(); err != nil {
		return err
	}
	p.jobs <- job{artist: a}
	return nil
}

// AddAlbum queues an album for encoding and writing.
func (p *Parallel) AddAlbum(a *skyhook.AlbumResource) error {
	if err := p.failed(); err != nil {
		return err
	}
	p.jobs <- job{album: a}
	return nil
}

// Close drains the queue and reports the first error any stage hit.
func (p *Parallel) Close() error {
	close(p.jobs)
	<-p.done
	return p.failed()
}
