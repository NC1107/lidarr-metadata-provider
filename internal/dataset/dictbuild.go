package dataset

import (
	"encoding/json"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// DictBuilder trains a compression dictionary from the first payloads of a
// build, then writes everything with it.
//
// The dictionary has to exist before the first payload is compressed, but it
// can only be trained from payloads that have been rendered, so the two are
// bootstrapped: the first sampleSize payloads are buffered uncompressed, the
// dictionary is trained from them, and then the buffer and the rest of the
// stream flow through the parallel writer with the dictionary in place.
//
// Albums are written before artists, so the sample and therefore the
// dictionary are album-shaped. Measured on a real build that still compresses
// artists 1.75x better than no dictionary, because the payloads share a
// vocabulary regardless of entity: album cover of the artist case at a
// fraction of the code of maintaining two dictionaries.
type DictBuilder struct {
	w          *Writer
	workers    int
	sampleSize int

	par     *Parallel
	samples [][]byte
	bufA    []*skyhook.ArtistResource
	bufB    []*skyhook.AlbumResource
	ready   bool
	err     error
}

// NewDictBuilder returns a builder that trains on the first sampleSize
// payloads. A sampleSize of zero uses a sensible default.
func NewDictBuilder(w *Writer, workers, sampleSize int) *DictBuilder {
	if sampleSize <= 0 {
		sampleSize = 40_000
	}
	return &DictBuilder{w: w, workers: workers, sampleSize: sampleSize}
}

// AddArtist queues an artist, buffering it during the sampling phase.
func (b *DictBuilder) AddArtist(a *skyhook.ArtistResource) error {
	if b.err != nil {
		return b.err
	}
	if b.ready {
		return b.par.AddArtist(a)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b.samples = append(b.samples, raw)
	b.bufA = append(b.bufA, a)
	return b.maybeTrain()
}

// AddAlbum queues an album, buffering it during the sampling phase.
func (b *DictBuilder) AddAlbum(a *skyhook.AlbumResource) error {
	if b.err != nil {
		return b.err
	}
	if b.ready {
		return b.par.AddAlbum(a)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b.samples = append(b.samples, raw)
	b.bufB = append(b.bufB, a)
	return b.maybeTrain()
}

// maybeTrain trains and flushes once enough samples are collected.
func (b *DictBuilder) maybeTrain() error {
	if len(b.samples) < b.sampleSize {
		return nil
	}
	return b.train()
}

// train builds the dictionary from the buffered samples, configures the
// writer, and flushes the buffer through the parallel writer.
func (b *DictBuilder) train() error {
	dict, err := trainDictionary(b.samples)
	if err != nil {
		b.err = err
		return err
	}
	// dict may be nil (too few samples, or no zstd binary); the writer then
	// compresses without one, which is still valid.
	if err := b.w.SetDictionary(dict); err != nil {
		b.err = err
		return err
	}
	b.samples = nil

	// Once the Parallel exists it owns worker goroutines, so any failure while
	// flushing the buffer through it must close it (draining those workers) and
	// remember the error. Without this a later Close() would see !ready, train
	// again over a live writer, leak this Parallel, and report success.
	b.par = NewParallel(b.w, b.workers)
	flush := func() error {
		for _, a := range b.bufB {
			if err := b.par.AddAlbum(a); err != nil {
				return err
			}
		}
		for _, a := range b.bufA {
			if err := b.par.AddArtist(a); err != nil {
				return err
			}
		}
		return nil
	}
	if err := flush(); err != nil {
		b.err = err
		b.par.Close()
		b.par = nil
		return err
	}
	b.bufA, b.bufB = nil, nil
	b.ready = true
	return nil
}

// Close trains on whatever was collected if the sample threshold was never
// reached, flushes, and reports the first error.
func (b *DictBuilder) Close() error {
	if b.err != nil {
		return b.err
	}
	if !b.ready {
		if err := b.train(); err != nil {
			return err
		}
	}
	return b.par.Close()
}
