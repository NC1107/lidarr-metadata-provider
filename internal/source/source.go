// Package source defines where metadata comes from and how sources fall back
// to one another.
//
// The shipping configuration is a Chain of one: the local dataset, which
// needs no network and cannot be rate limited. Users who opt in add the
// MusicBrainz web service behind it, which covers the window between a
// release appearing in MusicBrainz and appearing in a dataset artifact.
// Ordering is what makes that safe - the network is only ever touched for
// something the dataset does not have.
package source

import (
	"context"
	"errors"

	"github.com/nc1107/lidarr-metadata-provider/internal/musicbrainz"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// ErrNotFound means this source has no such entity. It is the signal a Chain
// uses to try the next source, as opposed to a transport error.
var ErrNotFound = errors.New("source: not found")

// Source answers the four lookups the Lidarr routes need.
type Source interface {
	Name() string
	Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error)
	Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error)
	SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error)
	SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error)
}

// Chain queries sources in order. A lookup falls through on "not found" and
// on transport errors alike, so a source that is merely broken does not take
// the whole server down with it. Searches fall through only when a source
// returns no hits, so a working dataset never triggers a network call.
type Chain []Source

// Names lists the chain's sources in query order, for logs and the dev UI.
func (ch Chain) Names() []string {
	out := make([]string, 0, len(ch))
	for _, s := range ch {
		out = append(out, s.Name())
	}
	return out
}

func (ch Chain) Name() string { return "chain" }

func (ch Chain) Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error) {
	var lastErr error = ErrNotFound
	for _, s := range ch {
		got, err := s.Artist(ctx, mbid)
		if err == nil {
			return got, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (ch Chain) Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error) {
	var lastErr error = ErrNotFound
	for _, s := range ch {
		got, err := s.Album(ctx, mbid)
		if err == nil {
			return got, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (ch Chain) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	var lastErr error
	for _, s := range ch {
		got, err := s.SearchArtists(ctx, query, limit)
		if err == nil && len(got) > 0 {
			return got, nil
		}
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return []skyhook.ArtistResource{}, nil
}

func (ch Chain) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	var lastErr error
	for _, s := range ch {
		got, err := s.SearchAlbums(ctx, query, artist, limit)
		if err == nil && len(got) > 0 {
			return got, nil
		}
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return []skyhook.AlbumResource{}, nil
}

// FromMusicBrainz adapts a rate-limited MusicBrainz client into a Source.
func FromMusicBrainz(c *musicbrainz.Client) Source { return mbSource{c: c} }

type mbSource struct{ c *musicbrainz.Client }

func (m mbSource) Name() string { return "musicbrainz" }

func (m mbSource) Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error) {
	got, err := m.c.Artist(ctx, mbid)
	return got, translate(err)
}

func (m mbSource) Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error) {
	got, err := m.c.Album(ctx, mbid)
	return got, translate(err)
}

func (m mbSource) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	got, err := m.c.SearchArtists(ctx, query, limit)
	return got, translate(err)
}

func (m mbSource) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	got, err := m.c.SearchAlbums(ctx, query, artist, limit)
	return got, translate(err)
}

func translate(err error) error {
	if errors.Is(err, musicbrainz.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
