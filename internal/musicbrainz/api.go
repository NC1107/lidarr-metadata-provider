package musicbrainz

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// DefaultSearchLimit is how many search hits to request. Lidarr shows a
// modest list and each extra hit costs nothing extra (one request either
// way), but a smaller page keeps responses quick to render.
const DefaultSearchLimit = 25

type artistSearchResponse struct {
	Artists []mbArtist `json:"artists"`
}

type releaseGroupSearchResponse struct {
	ReleaseGroups []mbReleaseGroup `json:"release-groups"`
}

type releaseBrowseResponse struct {
	Releases     []mbRelease `json:"releases"`
	ReleaseCount int         `json:"release-count"`
}

// SearchArtists maps a MusicBrainz artist search onto the SkyHook artist
// shape. The Albums list on each hit is left empty: filling it would cost one
// paginated browse per hit, and Lidarr only needs the album list once the
// user actually adds an artist, at which point it calls Artist.
func (c *Client) SearchArtists(ctx context.Context, query string, limit int) ([]skyhook.ArtistResource, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	params := url.Values{}
	params.Set("query", query)
	params.Set("limit", strconv.Itoa(limit))

	var resp artistSearchResponse
	if err := c.get(ctx, "/artist", params, &resp); err != nil {
		return nil, err
	}

	out := make([]skyhook.ArtistResource, 0, len(resp.Artists))
	for i := range resp.Artists {
		out = append(out, toArtist(&resp.Artists[i]))
	}
	return out, nil
}

// SearchAlbums maps a MusicBrainz release group search onto the SkyHook album
// shape, optionally constrained to an artist name. Releases are left empty
// for the same reason as SearchArtists: expanding every hit would cost a
// request per result, and Lidarr fetches the full album once one is chosen.
//
// The consequence is that these hits carry no ReleaseStatuses, so anything
// that runs them through Lidarr's album filter will discard them. That is
// fine for the search-then-add flow, which filters only in GetArtistInfo.
func (c *Client) SearchAlbums(ctx context.Context, query, artist string, limit int) ([]skyhook.AlbumResource, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	lucene := escapeLucene(query)
	if artist = strings.TrimSpace(artist); artist != "" {
		lucene = fmt.Sprintf(`%s AND artist:"%s"`, lucene, escapeLucene(artist))
	}
	params := url.Values{}
	params.Set("query", lucene)
	params.Set("limit", strconv.Itoa(limit))

	var resp releaseGroupSearchResponse
	if err := c.get(ctx, "/release-group", params, &resp); err != nil {
		return nil, err
	}

	out := make([]skyhook.AlbumResource, 0, len(resp.ReleaseGroups))
	for i := range resp.ReleaseGroups {
		out = append(out, toAlbum(&resp.ReleaseGroups[i], nil))
	}
	return out, nil
}

// Artist fetches an artist and its album list.
//
// The album list is derived from a browse over the artist's releases rather
// than over release groups, because it has to be: release status lives on the
// release, and an album whose ReleaseStatuses is empty is invisible to every
// Lidarr metadata profile. One pass over releases yields both the set of
// release groups and the statuses each one was released under.
//
// A release group with no releases is therefore omitted. That matches what
// Lidarr would do with it anyway, since it could never pass the filter.
func (c *Client) Artist(ctx context.Context, mbid string) (*skyhook.ArtistResource, error) {
	params := url.Values{}
	params.Set("inc", "aliases+genres+url-rels+ratings")

	var a mbArtist
	if err := c.get(ctx, "/artist/"+mbid, params, &a); err != nil {
		return nil, err
	}
	artist := toArtist(&a)

	groups, statuses, err := c.browseArtistReleases(ctx, mbid)
	if err != nil {
		return nil, err
	}
	for _, id := range sortedGroupIDs(groups) {
		artist.Albums = append(artist.Albums, toArtistAlbum(groups[id], sortedKeys(statuses[id])))
	}
	return &artist, nil
}

// browseArtistReleases walks the artist's releases, collecting each distinct
// release group and the statuses its releases carry.
func (c *Client) browseArtistReleases(ctx context.Context, mbid string) (map[string]*mbReleaseGroup, map[string]map[string]bool, error) {
	groups := map[string]*mbReleaseGroup{}
	statuses := map[string]map[string]bool{}

	for page := 0; page < c.maxPages(); page++ {
		params := url.Values{}
		params.Set("artist", mbid)
		params.Set("inc", "release-groups")
		params.Set("limit", strconv.Itoa(pageSize))
		params.Set("offset", strconv.Itoa(page*pageSize))

		var resp releaseBrowseResponse
		if err := c.get(ctx, "/release", params, &resp); err != nil {
			return nil, nil, err
		}
		for i := range resp.Releases {
			r := &resp.Releases[i]
			if r.ReleaseGroup == nil {
				continue
			}
			id := r.ReleaseGroup.ID
			if _, ok := groups[id]; !ok {
				groups[id] = r.ReleaseGroup
				statuses[id] = map[string]bool{}
			}
			if r.Status != "" {
				statuses[id][r.Status] = true
			}
		}
		if len(resp.Releases) < pageSize {
			break
		}
	}
	return groups, statuses, nil
}

// Album fetches a release group with its releases and tracks.
func (c *Client) Album(ctx context.Context, mbid string) (*skyhook.AlbumResource, error) {
	params := url.Values{}
	params.Set("inc", "artists+genres+ratings")

	var rg mbReleaseGroup
	if err := c.get(ctx, "/release-group/"+mbid, params, &rg); err != nil {
		return nil, err
	}

	var releases []mbRelease
	for page := 0; page < c.maxPages(); page++ {
		p := url.Values{}
		p.Set("release-group", mbid)
		p.Set("inc", "recordings+media+labels+artist-credits")
		p.Set("limit", strconv.Itoa(pageSize))
		p.Set("offset", strconv.Itoa(page*pageSize))

		var resp releaseBrowseResponse
		if err := c.get(ctx, "/release", p, &resp); err != nil {
			return nil, err
		}
		releases = append(releases, resp.Releases...)
		if len(resp.Releases) < pageSize {
			break
		}
	}

	album := toAlbum(&rg, releases)
	return &album, nil
}

// sortedGroupIDs orders release groups by release date then title, so a
// repeated lookup returns a stable list instead of Go's random map order.
func sortedGroupIDs(groups map[string]*mbReleaseGroup) []string {
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sortStrings(ids, func(a, b string) bool {
		ga, gb := groups[a], groups[b]
		if ga.FirstReleaseDate != gb.FirstReleaseDate {
			return ga.FirstReleaseDate < gb.FirstReleaseDate
		}
		if ga.Title != gb.Title {
			return ga.Title < gb.Title
		}
		return ga.ID < gb.ID
	})
	return ids
}

// escapeLucene neutralises the query syntax characters MusicBrainz's search
// index would otherwise interpret, so a user searching for "AC/DC" or
// "Where Are We Now?" gets a literal search instead of a syntax error.
func escapeLucene(s string) string {
	const special = `+-&|!(){}[]^"~*?:\/`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sortStrings is sort.Slice with a comparator over the slice's own values,
// kept local so entities.go and api.go share one sorting idiom.
func sortStrings(s []string, less func(a, b string) bool) {
	sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
}
