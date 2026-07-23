package musicbrainz

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nc1107/lidarr-metadata-provider/internal/format"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// MusicBrainz web service JSON. Only the fields we map are declared.

type mbArtist struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	SortName       string       `json:"sort-name"`
	Disambiguation string       `json:"disambiguation"`
	Type           *string      `json:"type"`
	LifeSpan       *mbLifeSpan  `json:"life-span"`
	Aliases        []mbAlias    `json:"aliases"`
	Genres         []mbGenre    `json:"genres"`
	Relations      []mbRelation `json:"relations"`
	Rating         *mbRating    `json:"rating"`
}

type mbLifeSpan struct {
	Ended bool `json:"ended"`
}

type mbAlias struct {
	Name string `json:"name"`
}

type mbGenre struct {
	Name string `json:"name"`
}

type mbRelation struct {
	Type string `json:"type"`
	URL  *struct {
		Resource string `json:"resource"`
	} `json:"url"`
}

type mbRating struct {
	Value      *float64 `json:"value"`
	VotesCount int      `json:"votes-count"`
}

type mbArtistCredit struct {
	Name   string    `json:"name"`
	Artist *mbArtist `json:"artist"`
}

type mbReleaseGroup struct {
	ID               string           `json:"id"`
	Title            string           `json:"title"`
	Disambiguation   string           `json:"disambiguation"`
	PrimaryType      *string          `json:"primary-type"`
	SecondaryTypes   []string         `json:"secondary-types"`
	FirstReleaseDate string           `json:"first-release-date"`
	ArtistCredit     []mbArtistCredit `json:"artist-credit"`
	Genres           []mbGenre        `json:"genres"`
	Rating           *mbRating        `json:"rating"`
}

type mbRelease struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Status         string          `json:"status"`
	Date           string          `json:"date"`
	Country        string          `json:"country"`
	Disambiguation string          `json:"disambiguation"`
	TrackCount     int             `json:"track-count"`
	LabelInfo      []mbLabelInfo   `json:"label-info"`
	Media          []mbMedium      `json:"media"`
	ReleaseGroup   *mbReleaseGroup `json:"release-group"`
}

type mbLabelInfo struct {
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`
}

type mbMedium struct {
	Title      string    `json:"title"`
	Format     string    `json:"format"`
	Position   int       `json:"position"`
	TrackCount int       `json:"track-count"`
	Tracks     []mbTrack `json:"tracks"`
}

type mbTrack struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Number       string           `json:"number"`
	Position     int              `json:"position"`
	Length       *int             `json:"length"`
	Recording    *mbRecording     `json:"recording"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
}

type mbRecording struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Length       *int             `json:"length"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
}

// normalizeDate converts a partial MusicBrainz date to the full ISO date the
// SkyHook contract uses. MusicBrainz records "2026" or "2026-06" when only
// part of a date is known; the upstream service pads the missing components,
// so "2001" arrives at Lidarr as "2001-01-01". An empty date stays absent
// rather than becoming a fake one.
func normalizeDate(d string) *string {
	d = strings.TrimSpace(d)
	if d == "" {
		return nil
	}
	switch strings.Count(d, "-") {
	case 0:
		d += "-01-01"
	case 1:
		d += "-01"
	}
	return &d
}

func mapGenres(in []mbGenre) []string {
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g.Name != "" {
			// Upstream title-cases genres; the raw web-service names are
			// lowercase (see [format.TitleCase] and AUDIT.md 33).
			out = append(out, format.TitleCase(g.Name))
		}
	}
	return out
}

func mapAliases(in []mbAlias) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, a := range in {
		if a.Name != "" && !seen[a.Name] {
			seen[a.Name] = true
			out = append(out, a.Name)
		}
	}
	return out
}

func mapLinks(in []mbRelation) []skyhook.LinkResource {
	out := make([]skyhook.LinkResource, 0, len(in))
	for _, r := range in {
		if r.URL != nil && r.URL.Resource != "" {
			// Upstream types links by domain label, not by MusicBrainz's own
			// relationship vocabulary (see [format.LinkType] and AUDIT.md 32).
			out = append(out, skyhook.LinkResource{Target: r.URL.Resource, Type: format.LinkType(r.URL.Resource)})
		}
	}
	return out
}

func mapRating(r *mbRating) skyhook.RatingResource {
	if r == nil {
		return skyhook.RatingResource{}
	}
	return skyhook.RatingResource{Count: r.VotesCount, Value: r.Value}
}

// artistStatus mirrors the upstream vocabulary, which reports "ended" for
// disbanded or deceased artists and "active" otherwise.
func artistStatus(a *mbArtist) string {
	if a.LifeSpan != nil && a.LifeSpan.Ended {
		return "ended"
	}
	return "active"
}

// toArtist maps a MusicBrainz artist to the top-level SkyHook artist shape.
// Albums is left empty for the caller to fill; every other collection is
// allocated because the contract emits [] rather than null.
//
// Images and Overview stay empty: MusicBrainz carries neither. Those come
// from build-time enrichment, so fallback results are deliberately thinner
// than dataset results.
func toArtist(a *mbArtist) skyhook.ArtistResource {
	return skyhook.ArtistResource{
		ID:             a.ID,
		OldIDs:         []string{},
		ArtistName:     a.Name,
		SortName:       a.SortName,
		ArtistAliases:  mapAliases(a.Aliases),
		Disambiguation: a.Disambiguation,
		Overview:       nil,
		Type:           a.Type,
		Status:         artistStatus(a),
		Genres:         mapGenres(a.Genres),
		Images:         []skyhook.ImageResource{},
		Links:          mapLinks(a.Relations),
		Rating:         mapRating(a.Rating),
		Albums:         []skyhook.ArtistAlbumResource{},
	}
}

// toAlbumArtist maps a MusicBrainz artist to the embedded shape used inside
// an album payload, which omits the Albums key entirely.
func toAlbumArtist(a *mbArtist) skyhook.AlbumArtistResource {
	full := toArtist(a)
	return skyhook.AlbumArtistResource{
		ID:             full.ID,
		OldIDs:         full.OldIDs,
		ArtistName:     full.ArtistName,
		SortName:       full.SortName,
		ArtistAliases:  full.ArtistAliases,
		Disambiguation: full.Disambiguation,
		Overview:       full.Overview,
		Type:           full.Type,
		Status:         full.Status,
		Genres:         full.Genres,
		Images:         full.Images,
		Links:          full.Links,
		Rating:         full.Rating,
	}
}

func primaryType(rg *mbReleaseGroup) string {
	if rg.PrimaryType != nil {
		return format.PrimaryTypeOrOther(*rg.PrimaryType)
	}
	// An empty Type matches no metadata profile and hides the album; upstream
	// reports untyped groups as "Other" (see [format.PrimaryTypeOrOther] and
	// AUDIT.md 31).
	return "Other"
}

// toArtistAlbum maps a release group to the skeletal entry in an artist's
// Albums list. statuses must hold every release status seen for this release
// group: Lidarr drops any album whose ReleaseStatuses does not intersect the
// user's metadata profile, so an empty list makes the album invisible.
func toArtistAlbum(rg *mbReleaseGroup, statuses []string) skyhook.ArtistAlbumResource {
	sec := rg.SecondaryTypes
	if sec == nil {
		sec = []string{}
	}
	if statuses == nil {
		statuses = []string{}
	}
	return skyhook.ArtistAlbumResource{
		ID:              rg.ID,
		OldIDs:          []string{},
		Title:           rg.Title,
		Type:            primaryType(rg),
		SecondaryTypes:  sec,
		ReleaseStatuses: statuses,
		ReleaseDate:     normalizeDate(rg.FirstReleaseDate),
		Rating:          nil,
	}
}

// toAlbum maps a release group to the top-level album shape. releases may be
// empty, in which case ReleaseStatuses ends up empty too and Lidarr will
// filter the album out - which is correct, since an album with no releases
// is not something Lidarr can grab.
func toAlbum(rg *mbReleaseGroup, releases []mbRelease) skyhook.AlbumResource {
	artists := make([]skyhook.AlbumArtistResource, 0, len(rg.ArtistCredit))
	present := map[string]bool{}
	artistID := ""
	for _, credit := range rg.ArtistCredit {
		if credit.Artist == nil {
			continue
		}
		if artistID == "" {
			artistID = credit.Artist.ID
		}
		if !present[credit.Artist.ID] {
			present[credit.Artist.ID] = true
			artists = append(artists, toAlbumArtist(credit.Artist))
		}
	}
	// Every artist a track credits must appear in the album's artist list, or
	// Lidarr throws a KeyNotFoundException and discards the whole album. Guest
	// performers are credited per track rather than on the release group, and a
	// brand-new release with a guest verse is exactly what the fallback exists
	// to serve, so the dataset build's safeguard has to hold here too. The
	// track credit already carries the full artist, so no extra lookup is
	// needed.
	for i := range releases {
		for j := range releases[i].Media {
			for k := range releases[i].Media[j].Tracks {
				for _, c := range trackCredits(&releases[i].Media[j].Tracks[k]) {
					if c.Artist == nil || present[c.Artist.ID] {
						continue
					}
					present[c.Artist.ID] = true
					artists = append(artists, toAlbumArtist(c.Artist))
				}
			}
		}
	}

	mapped := make([]skyhook.ReleaseResource, 0, len(releases))
	statusSet := map[string]bool{}
	for i := range releases {
		r := &releases[i]
		if r.Status != "" {
			statusSet[r.Status] = true
		}
		mapped = append(mapped, toRelease(r))
	}

	sec := rg.SecondaryTypes
	if sec == nil {
		sec = []string{}
	}
	return skyhook.AlbumResource{
		ID:              rg.ID,
		OldIDs:          []string{},
		Title:           rg.Title,
		Aliases:         []string{},
		Disambiguation:  rg.Disambiguation,
		Overview:        nil,
		Type:            primaryType(rg),
		SecondaryTypes:  sec,
		ReleaseStatuses: sortedKeys(statusSet),
		ReleaseDate:     normalizeDate(rg.FirstReleaseDate),
		ArtistID:        artistID,
		Artists:         artists,
		Genres:          mapGenres(rg.Genres),
		Images:          []skyhook.ImageResource{},
		Links:           []skyhook.LinkResource{},
		Rating:          mapRating(rg.Rating),
		Releases:        mapped,
	}
}

func toRelease(r *mbRelease) skyhook.ReleaseResource {
	labels := make([]string, 0, len(r.LabelInfo))
	for _, li := range r.LabelInfo {
		if li.Label != nil && li.Label.Name != "" {
			labels = append(labels, li.Label.Name)
		}
	}
	countries := []string{}
	if r.Country != "" {
		countries = append(countries, r.Country)
	}

	media := make([]skyhook.MediumResource, 0, len(r.Media))
	tracks := []skyhook.TrackResource{}
	total := 0
	for i := range r.Media {
		m := &r.Media[i]
		media = append(media, skyhook.MediumResource{
			Name:     m.Title,
			Format:   m.Format,
			Position: m.Position,
		})
		total += m.TrackCount
		for j := range m.Tracks {
			tracks = append(tracks, toTrack(&m.Tracks[j], m.Position))
		}
	}
	if r.TrackCount > 0 {
		total = r.TrackCount
	}

	return skyhook.ReleaseResource{
		ID:             r.ID,
		OldIDs:         []string{},
		Title:          r.Title,
		Disambiguation: r.Disambiguation,
		Status:         r.Status,
		Country:        countries,
		Label:          labels,
		ReleaseDate:    normalizeDate(r.Date),
		Media:          media,
		TrackCount:     total,
		Tracks:         tracks,
	}
}

// trackCredits returns the artist credits that attribute a track: its own if
// present, otherwise the recording's. Used both to set a track's artist and to
// complete the album's artist list, so the two never disagree.
func trackCredits(t *mbTrack) []mbArtistCredit {
	if len(t.ArtistCredit) > 0 {
		return t.ArtistCredit
	}
	if t.Recording != nil {
		return t.Recording.ArtistCredit
	}
	return nil
}

func toTrack(t *mbTrack, mediumNumber int) skyhook.TrackResource {
	track := skyhook.TrackResource{
		ID:              t.ID,
		OldIDs:          []string{},
		OldRecordingIDs: []string{},
		TrackName:       t.Title,
		TrackNumber:     t.Number,
		TrackPosition:   t.Position,
		MediumNumber:    mediumNumber,
		DurationMs:      t.Length,
	}
	if t.Recording != nil {
		track.RecordingID = t.Recording.ID
		if track.TrackName == "" {
			track.TrackName = t.Recording.Title
		}
		if track.DurationMs == nil {
			track.DurationMs = t.Recording.Length
		}
	}
	for _, c := range trackCredits(t) {
		if c.Artist != nil {
			track.ArtistID = c.Artist.ID
			break
		}
	}
	if track.TrackNumber == "" {
		track.TrackNumber = fmt.Sprint(t.Position)
	}
	return track
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
