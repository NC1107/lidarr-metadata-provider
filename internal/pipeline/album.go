package pipeline

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// Album assembly reads tracks in one ordered pass rather than querying per
// album.
//
// The obvious shape, "for each album fetch its releases and their tracks", is
// unusable at this scale. Four million albums times a handful of queries each
// is tens of millions of round trips: a measured run emitted fewer than a
// hundred thousand albums in half an hour, which extrapolates to about a day.
// Instead every table except tracks is held in memory, tracks are streamed
// once in album order, and the two are merged.
//
// Only tracks and recordings go to disk, because only they are too large to
// hold: roughly 35 million and 30 million rows. Releases and media together
// are a few hundred megabytes, which is worth spending to avoid the join.

type mediumRow struct {
	id         int
	position   int
	format     int
	name       string
	trackCount int
}

type releaseRow struct {
	id       int
	gid      string
	name     string
	statusID int
	media    []mediumRow

	labels    []int
	countries []int

	year, month, day int
	hasDate          bool
}

// albumHandlers reads the tables an album payload needs. They are only
// installed when a build produces albums, since streaming tens of millions of
// track rows is most of a build's cost and pointless otherwise.
func (c *collector) albumHandlers() map[string]mbdump.RowFunc {
	return map[string]mbdump.RowFunc{
		"medium":                  c.readMedium,
		"medium_format":           c.readTypeTable("medium_format", mbdump.MediumFormatColumns, c.mediumFormats),
		"track":                   c.readTrack,
		"recording":               c.readRecording,
		"release_label":           c.readReleaseLabel,
		"label":                   c.readLabel,
		"release_country":         c.readReleaseCountry,
		"release_unknown_country": c.readReleaseDate,
		"area":                    c.readArea,
	}
}

// readMedium buffers media by release. Tar order puts medium before release,
// so the release it belongs to has not been read yet.
func (c *collector) readMedium(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("medium", row, mbdump.MediumColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.MediumID])
	if err != nil {
		return err
	}
	release, err := atoi(row[mbdump.MediumRelease])
	if err != nil {
		return err
	}
	position, _ := optInt(row[mbdump.MediumPosition])
	format, _ := optInt(row[mbdump.MediumFormat])
	trackCount, _ := optInt(row[mbdump.MediumTrackCount])

	c.pendingMedia[release] = append(c.pendingMedia[release], mediumRow{
		id: id, position: position, format: format,
		name: row[mbdump.MediumName].Value, trackCount: trackCount,
	})
	return nil
}

// readReleaseStaged records a release and claims the media buffered for it.
func (c *collector) readReleaseStaged(row []mbdump.Field) error {
	if err := c.readRelease(row); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ReleaseID])
	if err != nil {
		return err
	}
	rg, err := atoi(row[mbdump.ReleaseGroupRef])
	if err != nil {
		return err
	}
	status, _ := optInt(row[mbdump.ReleaseStatusID])

	rel := &releaseRow{
		id: id, gid: row[mbdump.ReleaseGID].Value,
		name: row[mbdump.ReleaseName].Value, statusID: status,
		media: c.pendingMedia[id],
	}
	delete(c.pendingMedia, id)

	c.releaseByID[id] = rel
	c.releasesByRG[rg] = append(c.releasesByRG[rg], rel)
	c.releaseToGroup[id] = rg

	// Tracks arrive later carrying only a medium id, so the path back to an
	// album has to be recorded now, while both ends are known.
	for _, m := range rel.media {
		c.mediumToGroup[m.id] = rg
	}
	return nil
}

func (c *collector) readReleaseCountry(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_country", row, mbdump.ReleaseCountryColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ReleaseCountryRelease])
	if err != nil {
		return err
	}
	rel, ok := c.releaseByID[id]
	if !ok {
		return nil
	}
	if area, ok := optInt(row[mbdump.ReleaseCountryArea]); ok {
		rel.countries = append(rel.countries, area)
	}
	y, _ := optInt(row[mbdump.ReleaseCountryYear])
	m, _ := optInt(row[mbdump.ReleaseCountryMonth])
	d, _ := optInt(row[mbdump.ReleaseCountryDay])
	// A release can appear in several countries on different dates, and the
	// earliest is when it actually came out.
	if y > 0 && (!rel.hasDate || earlier(y, m, d, rel.year, rel.month, rel.day)) {
		rel.year, rel.month, rel.day, rel.hasDate = y, m, d, true
	}
	return nil
}

func (c *collector) readReleaseDate(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_unknown_country", row, mbdump.ReleaseUnknownCountryColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ReleaseUnknownCountryRelease])
	if err != nil {
		return err
	}
	rel, ok := c.releaseByID[id]
	if !ok {
		return nil
	}
	y, _ := optInt(row[mbdump.ReleaseUnknownCountryYear])
	m, _ := optInt(row[mbdump.ReleaseUnknownCountryMonth])
	d, _ := optInt(row[mbdump.ReleaseUnknownCountryDay])
	if y > 0 && !rel.hasDate {
		rel.year, rel.month, rel.day, rel.hasDate = y, m, d, true
	}
	return nil
}

func (c *collector) readReleaseLabel(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_label", row, mbdump.ReleaseLabelColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ReleaseLabelRelease])
	if err != nil {
		return err
	}
	rel, ok := c.releaseByID[id]
	if !ok {
		return nil
	}
	if label, ok := optInt(row[mbdump.ReleaseLabelLabel]); ok {
		rel.labels = append(rel.labels, label)
	}
	return nil
}

func (c *collector) readLabel(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("label", row, mbdump.LabelColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.LabelID])
	if err != nil {
		return err
	}
	c.labelNames[id] = row[mbdump.LabelName].Value
	return nil
}

// readArea keeps the country names releases are labelled with. Upstream emits
// "United States" where the ISO table would give "US", and the fixtures are
// the authority on which the contract wants.
func (c *collector) readArea(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("area", row, mbdump.AreaColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.AreaID])
	if err != nil {
		return err
	}
	c.countryCodes[id] = row[mbdump.AreaName].Value
	return nil
}

func (c *collector) readRecording(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("recording", row, mbdump.RecordingColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.RecordingID])
	if err != nil {
		return err
	}
	return c.staging.insert("recording", id, row[mbdump.RecordingGID].Value)
}

// readTrack stages a track with its album already resolved, so the emit pass
// can read tracks in album order without joining back through medium and
// release.
func (c *collector) readTrack(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("track", row, mbdump.TrackColumns); err != nil {
		return err
	}
	medium, err := atoi(row[mbdump.TrackMedium])
	if err != nil {
		return err
	}
	rg, ok := c.mediumToGroup[medium]
	if !ok {
		return nil
	}
	position, _ := optInt(row[mbdump.TrackPosition])
	recording, _ := optInt(row[mbdump.TrackRecording])
	credit, _ := optInt(row[mbdump.TrackArtistCredit])

	// Length is genuinely unknown for many tracks, and a zero would claim a
	// duration rather than admit there is not one.
	var length any
	if ms, ok := optInt(row[mbdump.TrackLength]); ok {
		length = ms
	}
	return c.staging.insert("track", rg, medium, position, row[mbdump.TrackNumber].Value,
		row[mbdump.TrackName].Value, recording, length, row[mbdump.TrackGID].Value, credit)
}

// stagedTrack is one row of the ordered track stream.
type stagedTrack struct {
	group     int
	medium    int
	position  int
	number    string
	name      string
	gid       string
	length    sql.NullInt64
	credit    int
	recording sql.NullString
}

// emitAlbums walks albums in id order alongside one ordered scan of the track
// table, so every track row is read exactly once.
func (c *collector) emitAlbums(emit func(*skyhook.AlbumResource) error) error {
	ids := make([]int, 0, len(c.groups))
	for id := range c.groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	rows, err := c.staging.db.Query(`
		SELECT t.rg, t.medium, t.position, t.number, t.name, t.gid, t.length, t.credit, r.gid
		FROM s_track t LEFT JOIN s_recording r ON r.id = t.recording
		ORDER BY t.rg, t.medium, t.position`)
	if err != nil {
		return err
	}
	defer rows.Close()

	stream := &trackStream{rows: rows}
	for _, id := range ids {
		tracks, err := stream.take(id)
		if err != nil {
			return err
		}
		if err := emit(c.fullAlbum(c.groups[id], tracks)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The release-level joins are finished once every album is emitted. The
	// artist pass that runs next reads c.groups but none of these, so free
	// them (~2 GB) rather than hold them flat through both phases.
	c.releaseByID = nil
	c.releasesByRG = nil
	c.mediumToGroup = nil
	c.mediumFormats = nil
	c.labelNames = nil
	c.countryCodes = nil
	c.pendingMedia = nil
	return nil
}

// trackStream hands out one album's tracks at a time from a single forward
// scan.
type trackStream struct {
	rows *sql.Rows
	held stagedTrack
	have bool
}

func (s *trackStream) take(group int) ([]stagedTrack, error) {
	var out []stagedTrack
	for {
		if !s.have {
			if !s.rows.Next() {
				return out, s.rows.Err()
			}
			var t stagedTrack
			if err := s.rows.Scan(&t.group, &t.medium, &t.position, &t.number,
				&t.name, &t.gid, &t.length, &t.credit, &t.recording); err != nil {
				return out, err
			}
			s.held, s.have = t, true
		}
		switch {
		case s.held.group == group:
			out = append(out, s.held)
			s.have = false
		case s.held.group < group:
			// Belongs to an album already passed, so it has no home here.
			s.have = false
		default:
			return out, nil
		}
	}
}

func (c *collector) fullAlbum(g *groupRow, tracks []stagedTrack) *skyhook.AlbumResource {
	byMedium := map[int][]stagedTrack{}
	for _, t := range tracks {
		byMedium[t.medium] = append(byMedium[t.medium], t)
	}

	releases := make([]skyhook.ReleaseResource, 0, len(c.releasesByRG[g.id]))
	statusSet := map[string]bool{}
	for _, rel := range c.releasesByRG[g.id] {
		status := c.statusNames[rel.statusID]
		if status != "" {
			statusSet[status] = true
		}
		releases = append(releases, c.release(rel, status, byMedium))
	}

	// The release group's credited artists, plus every per-track artist not
	// already among them: see completeAlbumArtists for why the omission is
	// fatal to Lidarr rather than merely incomplete.
	artists := make([]skyhook.AlbumArtistResource, 0, len(g.artistIDs))
	present := map[string]bool{}
	artistID := ""
	for _, id := range g.artistIDs {
		a, ok := c.artistsByID[id]
		if !ok {
			continue
		}
		if artistID == "" {
			artistID = a.gid
		}
		if !present[a.gid] {
			present[a.gid] = true
			artists = append(artists, c.albumArtist(a))
		}
	}
	c.completeAlbumArtists(&artists, present, artistID, releases)

	secondary := make([]string, 0, len(g.secondary))
	for _, id := range g.secondary {
		if name, ok := c.secondaryTypes[id]; ok {
			secondary = append(secondary, name)
		}
	}
	sort.Strings(secondary)

	return &skyhook.AlbumResource{
		ID:              g.gid,
		OldIDs:          sortedUnique(g.oldIDs),
		Title:           g.name,
		Aliases:         []string{},
		Disambiguation:  g.comment,
		Overview:        nil,
		Type:            c.primaryType(g.typeID),
		SecondaryTypes:  secondary,
		ReleaseStatuses: sortedKeys(statusSet),
		ReleaseDate:     formatDate(g),
		ArtistID:        artistID,
		Artists:         artists,
		Genres:          c.genresFor(c.groupTags[g.id]),
		Images:          c.coverFor(g),
		Links:           c.linksFor(c.groupURLs[g.id]),
		Rating:          skyhook.RatingResource{Count: g.ratings, Value: g.rating},
		Releases:        releases,
	}
}

// completeAlbumArtists guarantees every artist a track credits appears in the
// album's artist list. Lidarr builds a lookup keyed by the album's artists and
// throws a KeyNotFoundException, discarding the whole album, the moment a track
// points at an artist the list omits. Featured performers are credited per
// track rather than on the release group, so without this an album with a
// guest verse renders as having no tracks at all.
//
// A track crediting an artist absent from the export (deleted since the export
// was cut) is reattributed to an artist that is present, since a dangling
// reference crashes Lidarr just the same. The server applies the identical
// safeguard at request time; doing it here as well makes the dataset correct at
// rest rather than relying on the read path to repair it.
func (c *collector) completeAlbumArtists(artists *[]skyhook.AlbumArtistResource, present map[string]bool, primary string, releases []skyhook.ReleaseResource) {
	fallback := primary
	if !present[fallback] && len(*artists) > 0 {
		fallback = (*artists)[0].ID
	}
	for i := range releases {
		for j := range releases[i].Tracks {
			t := &releases[i].Tracks[j]
			if t.ArtistID == "" || present[t.ArtistID] {
				continue
			}
			if id, ok := c.artistIDByGID[t.ArtistID]; ok {
				if a := c.artistsByID[id]; a != nil {
					present[t.ArtistID] = true
					*artists = append(*artists, c.albumArtist(a))
					continue
				}
			}
			t.ArtistID = fallback
		}
	}
}

func (c *collector) release(rel *releaseRow, status string, byMedium map[int][]stagedTrack) skyhook.ReleaseResource {
	countries := make([]string, 0, len(rel.countries))
	for _, area := range rel.countries {
		if code, ok := c.countryCodes[area]; ok && code != "" {
			countries = append(countries, code)
		}
	}
	sort.Strings(countries)

	labels := make([]string, 0, len(rel.labels))
	seen := map[string]bool{}
	for _, id := range rel.labels {
		if name, ok := c.labelNames[id]; ok && name != "" && !seen[name] {
			seen[name] = true
			labels = append(labels, name)
		}
	}
	sort.Strings(labels)

	out := skyhook.ReleaseResource{
		ID: rel.gid, OldIDs: []string{}, Title: rel.name, Status: status,
		Country: countries, Label: labels, ReleaseDate: releaseDate(rel),
		Media:  make([]skyhook.MediumResource, 0, len(rel.media)),
		Tracks: []skyhook.TrackResource{},
	}

	media := append([]mediumRow(nil), rel.media...)
	sort.Slice(media, func(i, j int) bool { return media[i].position < media[j].position })

	for _, m := range media {
		out.Media = append(out.Media, skyhook.MediumResource{
			Name: m.name, Format: c.mediumFormats[m.format], Position: m.position,
		})
		out.TrackCount += m.trackCount
		for _, t := range byMedium[m.id] {
			out.Tracks = append(out.Tracks, c.track(t, m.position))
		}
	}
	return out
}

func (c *collector) track(t stagedTrack, mediumPosition int) skyhook.TrackResource {
	track := skyhook.TrackResource{
		ID: t.gid, OldIDs: []string{}, OldRecordingIDs: []string{},
		RecordingID: t.recording.String, TrackName: t.name,
		TrackNumber: t.number, TrackPosition: t.position, MediumNumber: mediumPosition,
	}
	if t.length.Valid {
		ms := int(t.length.Int64)
		track.DurationMs = &ms
	}
	if track.TrackNumber == "" {
		track.TrackNumber = fmt.Sprint(t.position)
	}
	for _, artistID := range c.creditArtists[t.credit] {
		if a, ok := c.artistsByID[artistID]; ok {
			track.ArtistID = a.gid
			break
		}
	}
	return track
}

func releaseDate(rel *releaseRow) *string {
	if !rel.hasDate {
		return nil
	}
	m, d := rel.month, rel.day
	if m == 0 {
		m = 1
	}
	if d == 0 {
		d = 1
	}
	s := fmt.Sprintf("%04d-%02d-%02d", rel.year, m, d)
	return &s
}

func earlier(y1, m1, d1, y2, m2, d2 int) bool {
	if y1 != y2 {
		return y1 < y2
	}
	if m1 != m2 {
		return m1 < m2
	}
	return d1 < d2
}

// albumArtist builds the artist shape embedded in an album, and caches it.
//
// It deliberately does not go through the full artist payload. That version
// carries the artist's entire album list and sorts it, so building it once
// per album made assembly quadratic in an artist's catalogue: every one of
// Bach's six thousand albums rebuilt and re-sorted a six thousand entry list
// only to discard it. The embedded shape has no album list at all, and it is
// identical for every album by the same artist, so it is computed once.
func (c *collector) albumArtist(a *artistRow) skyhook.AlbumArtistResource {
	if a.embedded != nil {
		return *a.embedded
	}
	// The embedded shape deliberately omits images and overview even for
	// artists that have them: the fixtures show upstream leaving them out of an
	// album's artist list to keep album payloads small. The image and
	// biography belong to the full artist payload only.
	out := skyhook.AlbumArtistResource{
		ID:             a.gid,
		OldIDs:         sortedUnique(a.oldIDs),
		ArtistName:     a.name,
		SortName:       a.sortName,
		ArtistAliases:  sortedUnique(a.aliases),
		Disambiguation: a.comment,
		Overview:       nil,
		Status:         statusFor(a.ended),
		Genres:         []string{},
		Images:         []skyhook.ImageResource{},
		Links:          []skyhook.LinkResource{},
		Rating:         skyhook.RatingResource{Count: a.ratings, Value: a.rating},
	}
	if name, ok := c.artistTypes[a.typeID]; ok && name != "" {
		out.Type = &name
	}
	a.embedded = &out
	return out
}

// sortedKeys returns a set's members in a stable order, so rebuilding the
// same export produces byte-identical payloads.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
