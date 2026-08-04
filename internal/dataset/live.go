package dataset

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// Live serves from a Reader that can be replaced while requests are in flight,
// so an update installs a fresh dataset without dropping a request or taking
// the server down. Each lookup loads the current reader and finishes on it; a
// swap points new lookups at the replacement and closes the old reader after a
// grace window, once any in-flight lookups on it have returned.
type Live struct {
	current atomic.Pointer[Reader]
}

// NewLive wraps an open Reader.
func NewLive(r *Reader) *Live {
	l := &Live{}
	l.current.Store(r)
	return l
}

func (l *Live) reader() *Reader { return l.current.Load() }

// Swap installs next as the served reader and schedules the previous one to
// close after a grace window, which is far longer than any lookup takes so no
// in-flight request touches a closed database.
func (l *Live) Swap(next *Reader) {
	old := l.current.Swap(next)
	if old != nil && old != next {
		time.AfterFunc(30*time.Second, func() { old.Close() })
	}
}

// Info reports the currently served dataset.
func (l *Live) Info() Info { return l.reader().Info() }

// Close closes the reader in place. It is for shutdown, not for a swap.
func (l *Live) Close() error { return l.reader().Close() }

func (l *Live) Name() string { return l.reader().Name() }

func (l *Live) Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error) {
	return l.reader().Artist(ctx, mbid)
}

func (l *Live) Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error) {
	return l.reader().Album(ctx, mbid)
}

func (l *Live) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	return l.reader().SearchArtists(ctx, query, limit)
}

func (l *Live) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	return l.reader().SearchAlbums(ctx, query, artist, limit)
}
