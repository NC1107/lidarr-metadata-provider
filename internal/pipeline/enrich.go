package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// This file adds genres and links to payloads. Both come from tables already
// in the export, so neither needs a third party. Only images and biographies
// genuinely require enrichment MusicBrainz does not carry.

// genreHandlers reads the tables genres are built from. Genres are the tags an
// entity was given whose name is a recognised genre, ordered by how many
// people applied them.
func (c *collector) enrichHandlers() map[string]mbdump.RowFunc {
	return map[string]mbdump.RowFunc{
		"genre":               c.readGenre,
		"tag":                 c.readTag,
		"artist_tag":          c.readArtistTag,
		"release_group_tag":   c.readReleaseGroupTag,
		"url":                 c.readURL,
		"l_artist_url":        c.readArtistURL,
		"l_release_group_url": c.readReleaseGroupURL,
	}
}

func (c *collector) readGenre(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("genre", row, mbdump.GenreColumns); err != nil {
		return err
	}
	c.genreNames[strings.ToLower(row[mbdump.GenreName].Value)] = true
	return nil
}

func (c *collector) readTag(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("tag", row, mbdump.TagColumns); err != nil {
		return err
	}
	c.tagPhase = true
	id, err := atoi(row[mbdump.TagID])
	if err != nil {
		return err
	}
	// Keep only tags something we emit was tagged with. Most tags in the
	// export apply to entities this dataset does not carry.
	if c.neededTags[id] {
		c.tagNames[id] = row[mbdump.TagName].Value
	}
	return nil
}

type weightedTag struct {
	tag   int
	count int
}

func (c *collector) readArtistTag(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist_tag", row, mbdump.ArtistTagColumns); err != nil {
		return err
	}
	artist, err := atoi(row[mbdump.ArtistTagArtist])
	if err != nil {
		return err
	}
	if _, ok := c.artistsByID[artist]; !ok {
		return nil
	}
	tag, _ := optInt(row[mbdump.ArtistTagTag])
	count, _ := optInt(row[mbdump.ArtistTagCount])
	// A tag downvoted to zero or below is not a genre this artist has.
	if count <= 0 {
		return nil
	}
	if err := c.needTag(tag); err != nil {
		return err
	}
	c.artistTags[artist] = append(c.artistTags[artist], weightedTag{tag, count})
	return nil
}

// needTag records a referenced tag, refusing if the tag table has already
// been read: filtering it then would have dropped this reference.
func (c *collector) needTag(id int) error {
	if c.tagPhase {
		return fmt.Errorf("tag table was read before the tag join tables; genres would be dropped")
	}
	c.neededTags[id] = true
	return nil
}

func (c *collector) readReleaseGroupTag(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group_tag", row, mbdump.ReleaseGroupTagColumns); err != nil {
		return err
	}
	rg, err := atoi(row[mbdump.ReleaseGroupTagGroup])
	if err != nil {
		return err
	}
	if _, ok := c.groups[rg]; !ok {
		return nil
	}
	tag, _ := optInt(row[mbdump.ReleaseGroupTagTag])
	count, _ := optInt(row[mbdump.ReleaseGroupTagCount])
	if count <= 0 {
		return nil
	}
	if err := c.needTag(tag); err != nil {
		return err
	}
	c.groupTags[rg] = append(c.groupTags[rg], weightedTag{tag, count})
	return nil
}

func (c *collector) readURL(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("url", row, mbdump.URLColumns); err != nil {
		return err
	}
	c.urlPhase = true
	id, err := atoi(row[mbdump.URLID])
	if err != nil {
		return err
	}
	// Keep only URLs an artist or release group links to. Most URLs in the
	// export are on recordings, releases and labels this dataset omits.
	if c.neededURLs[id] {
		c.urls[id] = row[mbdump.URLValue].Value
	}
	return nil
}

// needURL records a referenced url, refusing if the url table has already
// been read: filtering it then would have dropped this link.
func (c *collector) needURL(id int) error {
	if c.urlPhase {
		return fmt.Errorf("url table was read before the link tables; links would be dropped")
	}
	c.neededURLs[id] = true
	return nil
}

func (c *collector) readArtistURL(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("l_artist_url", row, mbdump.LinkURLColumns); err != nil {
		return err
	}
	artist, err := atoi(row[mbdump.LinkURLEntity0])
	if err != nil {
		return err
	}
	if _, ok := c.artistsByID[artist]; !ok {
		return nil
	}
	url, _ := optInt(row[mbdump.LinkURLEntity1])
	if err := c.needURL(url); err != nil {
		return err
	}
	c.artistURLs[artist] = append(c.artistURLs[artist], url)
	return nil
}

func (c *collector) readReleaseGroupURL(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("l_release_group_url", row, mbdump.LinkURLColumns); err != nil {
		return err
	}
	rg, err := atoi(row[mbdump.LinkURLEntity0])
	if err != nil {
		return err
	}
	if _, ok := c.groups[rg]; !ok {
		return nil
	}
	url, _ := optInt(row[mbdump.LinkURLEntity1])
	if err := c.needURL(url); err != nil {
		return err
	}
	c.groupURLs[rg] = append(c.groupURLs[rg], url)
	return nil
}

// genresFor renders an entity's genres: its genre tags, ordered by vote count,
// title-cased the way upstream presents them ("pop rock" -> "Pop Rock").
func (c *collector) genresFor(tags []weightedTag) []string {
	if len(tags) == 0 {
		return []string{}
	}
	// Strongest first; ties broken by name so a rebuild is deterministic.
	sorted := append([]weightedTag(nil), tags...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count > sorted[j].count
		}
		return c.tagNames[sorted[i].tag] < c.tagNames[sorted[j].tag]
	})

	out := make([]string, 0, len(sorted))
	seen := map[string]bool{}
	for _, t := range sorted {
		name := c.tagNames[t.tag]
		if name == "" || !c.genreNames[strings.ToLower(name)] {
			continue
		}
		title := titleCase(name)
		if !seen[title] {
			seen[title] = true
			out = append(out, title)
		}
	}
	return out
}

// linksFor renders an entity's external links. The type is the address's
// second-level domain, which is how upstream labels them: a link to
// discogs.com is typed "discogs", one to thebeatles.com is typed "thebeatles".
func (c *collector) linksFor(urlIDs []int) []skyhook.LinkResource {
	out := make([]skyhook.LinkResource, 0, len(urlIDs))
	seen := map[string]bool{}
	for _, id := range urlIDs {
		url := c.urls[id]
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, skyhook.LinkResource{Target: url, Type: linkType(url)})
	}
	// Stable order across rebuilds.
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// titleCase capitalises each word, matching how upstream renders genre names.
// A word starts at the beginning and after a space, a hyphen, or an ampersand,
// so "j-pop" becomes "J-Pop" and "r&b" becomes "R&B", matching the golden
// fixtures. Splitting only on whitespace left everything after a hyphen or
// ampersand lowercase.
func titleCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	wordStart := true
	for _, r := range s {
		if wordStart {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		wordStart = r == ' ' || r == '-' || r == '&'
	}
	return b.String()
}

// linkType extracts the label upstream gives a link, which is the host's
// second-to-last component: "https://www.discogs.com/artist/1" -> "discogs".
//
// The one exception the golden fixtures show is co.uk and co.jp, which upstream
// strips so bbc.co.uk types as "bbc". It does not strip any other compound
// suffix: gov.au types as "gov", co.kr as "co", ac.jp as "ac", com.br as "com".
// An earlier version stripped a broad hardcoded list (gov, com, org, net, ac),
// which disagreed with upstream on every one of those.
func linkType(url string) string {
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// Drop any userinfo, so user:pass@example.com reads as example, not user.
	if i := strings.IndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(strings.ToLower(host), ".") // tolerate a trailing-dot FQDN
	host = strings.TrimPrefix(host, "www.")

	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	last := parts[len(parts)-1]
	if len(parts) >= 3 && parts[len(parts)-2] == "co" && (last == "uk" || last == "jp") {
		return parts[len(parts)-3]
	}
	return parts[len(parts)-2]
}

// coverArtHandlers reads the cover art archive. Only which release groups have
// a cover is needed: the image URL is derived from the MBID.
func (c *collector) coverArtHandlers() map[string]mbdump.RowFunc {
	return map[string]mbdump.RowFunc{
		"cover_art_archive.cover_art":               c.readReleaseCover,
		"cover_art_archive.release_group_cover_art": c.readCoverArt,
	}
}

// readReleaseCover marks an album as having a cover when any of its releases
// carries artwork, which is how most albums have art. The release group front
// endpoint serves it regardless of whether a representative was chosen.
func (c *collector) readReleaseCover(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("cover_art", row, mbdump.CoverArtColumns); err != nil {
		return err
	}
	release, err := atoi(row[mbdump.CoverArtRelease])
	if err != nil {
		return err
	}
	if rg, ok := c.releaseToGroup[release]; ok {
		c.groupHasCover[rg] = true
	}
	return nil
}

func (c *collector) readCoverArt(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group_cover_art", row, mbdump.ReleaseGroupCoverArtColumns); err != nil {
		return err
	}
	rg, err := atoi(row[mbdump.ReleaseGroupCoverArtGroup])
	if err != nil {
		return err
	}
	if _, ok := c.groups[rg]; ok {
		c.groupHasCover[rg] = true
	}
	return nil
}

// coverFor renders an album's artwork. The Cover Art Archive serves a release
// group's chosen cover at a URL built from its MBID, so no per-image data is
// stored. Url and RemoteURL are the same address because, unlike upstream, we
// do not run an image cache to sit in front of it.
func (c *collector) coverFor(g *groupRow) []skyhook.ImageResource {
	if !c.groupHasCover[g.id] {
		return []skyhook.ImageResource{}
	}
	base := "https://coverartarchive.org/release-group/" + g.gid
	return []skyhook.ImageResource{{
		CoverType: "Cover",
		URL:       base + "/front-500",
		RemoteURL: base + "/front-500",
	}}
}
