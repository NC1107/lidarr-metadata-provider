package source

import (
	"context"
	"errors"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// stub stands in for one link of the chain, recording whether it was asked.
type stub struct {
	name    string
	artist  *skyhook.ArtistResource
	artists []skyhook.ArtistResource
	err     error
	asked   *int
}

func (s stub) Name() string { return s.name }

func (s stub) Artist(context.Context, string) (*skyhook.ArtistResource, error) {
	*s.asked++
	if s.err != nil {
		return nil, s.err
	}
	return s.artist, nil
}

func (s stub) Album(context.Context, string) (*skyhook.AlbumResource, error) {
	*s.asked++
	if s.err != nil {
		return nil, s.err
	}
	return &skyhook.AlbumResource{}, nil
}

func (s stub) SearchArtists(context.Context, string, int) ([]skyhook.ArtistResource, error) {
	*s.asked++
	if s.err != nil {
		return nil, s.err
	}
	return s.artists, nil
}

func (s stub) SearchAlbums(context.Context, string, string, int) ([]skyhook.AlbumResource, error) {
	*s.asked++
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

// The property the whole design rests on: a working dataset means the network
// is never touched. Getting this backwards would send every lookup over the
// wire at one request per second.
func TestChainDoesNotConsultFallbackWhenTheFirstSourceAnswers(t *testing.T) {
	var datasetAsked, fallbackAsked int
	chain := Chain{
		stub{name: "dataset", artist: &skyhook.ArtistResource{ID: "found"}, asked: &datasetAsked},
		stub{name: "musicbrainz", artist: &skyhook.ArtistResource{ID: "remote"}, asked: &fallbackAsked},
	}

	got, err := chain.Artist(context.Background(), "any")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "found" {
		t.Errorf("answered from %q, want the first source", got.ID)
	}
	if fallbackAsked != 0 {
		t.Errorf("fallback was consulted %d times despite the dataset answering", fallbackAsked)
	}
}

func TestChainFallsThroughOnNotFound(t *testing.T) {
	var datasetAsked, fallbackAsked int
	chain := Chain{
		stub{name: "dataset", err: ErrNotFound, asked: &datasetAsked},
		stub{name: "musicbrainz", artist: &skyhook.ArtistResource{ID: "remote"}, asked: &fallbackAsked},
	}

	got, err := chain.Artist(context.Background(), "any")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "remote" {
		t.Errorf("got %q, want the fallback's answer", got.ID)
	}
	if fallbackAsked != 1 {
		t.Errorf("fallback asked %d times, want 1", fallbackAsked)
	}
}

// A broken source must not take the server down with it. A dataset that fails
// to read is exactly when the fallback earns its place.
func TestChainFallsThroughOnTransportError(t *testing.T) {
	var a, b int
	chain := Chain{
		stub{name: "dataset", err: errors.New("disk went away"), asked: &a},
		stub{name: "musicbrainz", artist: &skyhook.ArtistResource{ID: "remote"}, asked: &b},
	}
	got, err := chain.Artist(context.Background(), "any")
	if err != nil {
		t.Fatalf("chain gave up on a recoverable failure: %v", err)
	}
	if got.ID != "remote" {
		t.Errorf("got %q", got.ID)
	}
}

func TestChainReportsNotFoundWhenNobodyHasIt(t *testing.T) {
	var a, b int
	chain := Chain{
		stub{name: "dataset", err: ErrNotFound, asked: &a},
		stub{name: "musicbrainz", err: ErrNotFound, asked: &b},
	}
	if _, err := chain.Artist(context.Background(), "any"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound so the server answers 404", err)
	}
}

// Search falls through only on an empty result, so a dataset that answers
// with hits never triggers a network call.
func TestChainSearchFallsThroughOnlyWhenEmpty(t *testing.T) {
	var datasetAsked, fallbackAsked int
	chain := Chain{
		stub{name: "dataset", artists: []skyhook.ArtistResource{{ID: "local"}}, asked: &datasetAsked},
		stub{name: "musicbrainz", artists: []skyhook.ArtistResource{{ID: "remote"}}, asked: &fallbackAsked},
	}
	got, err := chain.SearchArtists(context.Background(), "q", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "local" {
		t.Errorf("got %+v, want the local hit", got)
	}
	if fallbackAsked != 0 {
		t.Errorf("fallback consulted despite local hits")
	}

	empty := Chain{
		stub{name: "dataset", artists: []skyhook.ArtistResource{}, asked: &datasetAsked},
		stub{name: "musicbrainz", artists: []skyhook.ArtistResource{{ID: "remote"}}, asked: &fallbackAsked},
	}
	got, err = empty.SearchArtists(context.Background(), "q", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "remote" {
		t.Errorf("got %+v, want the fallback hit after an empty local search", got)
	}
}

// An empty search must return [] rather than null, which the contract
// requires and which Lidarr's deserializer expects.
func TestChainSearchReturnsEmptySliceNotNil(t *testing.T) {
	var a int
	chain := Chain{stub{name: "dataset", artists: []skyhook.ArtistResource{}, asked: &a}}
	got, err := chain.SearchArtists(context.Background(), "q", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("returned nil, which marshals as null")
	}
}

func TestChainNames(t *testing.T) {
	var a, b int
	chain := Chain{
		stub{name: "dataset", asked: &a},
		stub{name: "musicbrainz", asked: &b},
	}
	names := chain.Names()
	if len(names) != 2 || names[0] != "dataset" || names[1] != "musicbrainz" {
		t.Errorf("Names() = %v, order matters since it is the query order", names)
	}
}
