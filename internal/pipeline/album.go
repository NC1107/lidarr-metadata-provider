package pipeline

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// albumHandlers reads the tables an album payload needs. They are only
// installed when a build is producing albums, since streaming 35 million
// track rows into staging is most of a build's cost and pointless otherwise.
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
		"iso_3166_1":              c.readISO,
	}
}

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
	return c.staging.insert("medium", id, release, position, format,
		row[mbdump.MediumName].Value, trackCount)
}

func (c *collector) readTrack(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("track", row, mbdump.TrackColumns); err != nil {
		return err
	}
	medium, err := atoi(row[mbdump.TrackMedium])
	if err != nil {
		return err
	}
	position, _ := optInt(row[mbdump.TrackPosition])
	recording, _ := optInt(row[mbdump.TrackRecording])
	credit, _ := optInt(row[mbdump.TrackArtistCredit])

	// Length is genuinely unknown for many tracks, and a zero would claim a
	// duration rather than admit there isn't one.
	var length any
	if ms, ok := optInt(row[mbdump.TrackLength]); ok {
		length = ms
	}
	return c.staging.insert("track", medium, position, row[mbdump.TrackNumber].Value,
		row[mbdump.TrackName].Value, recording, length, row[mbdump.TrackGID].Value, credit)
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

func (c *collector) readReleaseLabel(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_label", row, mbdump.ReleaseLabelColumns); err != nil {
		return err
	}
	release, err := atoi(row[mbdump.ReleaseLabelRelease])
	if err != nil {
		return err
	}
	label, ok := optInt(row[mbdump.ReleaseLabelLabel])
	if !ok {
		return nil
	}
	return c.staging.insert("label", release, label)
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

func (c *collector) readReleaseCountry(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_country", row, mbdump.ReleaseCountryColumns); err != nil {
		return err
	}
	release, err := atoi(row[mbdump.ReleaseCountryRelease])
	if err != nil {
		return err
	}
	area, _ := optInt(row[mbdump.ReleaseCountryArea])
	y, _ := optInt(row[mbdump.ReleaseCountryYear])
	m, _ := optInt(row[mbdump.ReleaseCountryMonth])
	d, _ := optInt(row[mbdump.ReleaseCountryDay])
	return c.staging.insert("country", release, area, y, m, d)
}

func (c *collector) readReleaseDate(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_unknown_country", row, mbdump.ReleaseUnknownCountryColumns); err != nil {
		return err
	}
	release, err := atoi(row[mbdump.ReleaseUnknownCountryRelease])
	if err != nil {
		return err
	}
	y, _ := optInt(row[mbdump.ReleaseUnknownCountryYear])
	m, _ := optInt(row[mbdump.ReleaseUnknownCountryMonth])
	d, _ := optInt(row[mbdump.ReleaseUnknownCountryDay])
	return c.staging.insert("date", release, y, m, d)
}

func (c *collector) readISO(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("iso_3166_1", row, mbdump.ISOColumns); err != nil {
		return err
	}
	area, err := atoi(row[mbdump.ISOArea])
	if err != nil {
		return err
	}
	c.countryCodes[area] = row[mbdump.ISOCode].Value
	return nil
}

// readReleaseStaged records the release itself in addition to its status,
// which the artist build already needed.
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
	return c.staging.insert("release", id, row[mbdump.ReleaseGID].Value,
		row[mbdump.ReleaseName].Value, rg, status, "")
}

// album assembles one full album payload by querying staging for its
// releases, media and tracks.
func (c *collector) fullAlbum(g *groupRow) (*skyhook.AlbumResource, error) {
	artists := make([]skyhook.AlbumArtistResource, 0, len(g.artistIDs))
	artistID := ""
	for _, id := range g.artistIDs {
		a, ok := c.artistsByID[id]
		if !ok {
			continue
		}
		if artistID == "" {
			artistID = a.gid
		}
		artists = append(artists, c.albumArtist(a))
	}

	releases, statuses, err := c.releasesFor(g.id)
	if err != nil {
		return nil, err
	}

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
		ReleaseStatuses: statuses,
		ReleaseDate:     formatDate(g),
		ArtistID:        artistID,
		Artists:         artists,
		Genres:          []string{},
		Images:          []skyhook.ImageResource{},
		Links:           []skyhook.LinkResource{},
		Rating:          skyhook.RatingResource{},
		Releases:        releases,
	}, nil
}

func (c *collector) albumArtist(a *artistRow) skyhook.AlbumArtistResource {
	full := c.artist(a)
	return skyhook.AlbumArtistResource{
		ID: full.ID, OldIDs: full.OldIDs, ArtistName: full.ArtistName,
		SortName: full.SortName, ArtistAliases: full.ArtistAliases,
		Disambiguation: full.Disambiguation, Overview: full.Overview,
		Type: full.Type, Status: full.Status, Genres: full.Genres,
		Images: full.Images, Links: full.Links, Rating: full.Rating,
	}
}

func (c *collector) releasesFor(rgID int) ([]skyhook.ReleaseResource, []string, error) {
	rows, err := c.staging.db.Query(
		`SELECT id, gid, name, status FROM s_release WHERE rg = ? ORDER BY id`, rgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	out := []skyhook.ReleaseResource{}
	statusSet := map[string]bool{}
	for rows.Next() {
		var id, status int
		var gid, name string
		if err := rows.Scan(&id, &gid, &name, &status); err != nil {
			return nil, nil, err
		}
		if s, ok := c.statusNames[status]; ok && s != "" {
			statusSet[s] = true
		}
		rel, err := c.release(id, gid, name, c.statusNames[status])
		if err != nil {
			return nil, nil, err
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return out, sortedKeys(statusSet), nil
}

func (c *collector) release(id int, gid, name, status string) (skyhook.ReleaseResource, error) {
	rel := skyhook.ReleaseResource{
		ID: gid, OldIDs: []string{}, Title: name, Status: status,
		Country: []string{}, Label: []string{}, Media: []skyhook.MediumResource{},
		Tracks: []skyhook.TrackResource{},
	}

	countries, date, err := c.releaseCountryAndDate(id)
	if err != nil {
		return rel, err
	}
	rel.Country = countries
	rel.ReleaseDate = date

	if rel.Label, err = c.releaseLabels(id); err != nil {
		return rel, err
	}
	if err := c.releaseMedia(id, &rel); err != nil {
		return rel, err
	}
	return rel, nil
}

func (c *collector) releaseCountryAndDate(id int) ([]string, *string, error) {
	countries := []string{}
	var y, m, d int
	var found bool

	rows, err := c.staging.db.Query(`SELECT area, y, m, d FROM s_rel_country WHERE rel = ?`, id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var area, ry, rm, rd int
		if err := rows.Scan(&area, &ry, &rm, &rd); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if code, ok := c.countryCodes[area]; ok && code != "" {
			countries = append(countries, code)
		}
		// A release can appear in several countries on different dates; the
		// earliest is the one that describes when it came out.
		if ry > 0 && (!found || earlier(ry, rm, rd, y, m, d)) {
			y, m, d, found = ry, rm, rd, true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if !found {
		// A release with no country still has a date, recorded separately.
		row := c.staging.db.QueryRow(`SELECT y, m, d FROM s_rel_date WHERE rel = ?`, id)
		var ry, rm, rd int
		if err := row.Scan(&ry, &rm, &rd); err == nil && ry > 0 {
			y, m, d, found = ry, rm, rd, true
		} else if err != nil && err != sql.ErrNoRows {
			return nil, nil, err
		}
	}
	sort.Strings(countries)

	if !found {
		return countries, nil, nil
	}
	if m == 0 {
		m = 1
	}
	if d == 0 {
		d = 1
	}
	s := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	return countries, &s, nil
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

func (c *collector) releaseLabels(id int) ([]string, error) {
	rows, err := c.staging.db.Query(`SELECT label FROM s_rel_label WHERE rel = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var label int
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		if name, ok := c.labelNames[label]; ok && name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, rows.Err()
}

func (c *collector) releaseMedia(id int, rel *skyhook.ReleaseResource) error {
	rows, err := c.staging.db.Query(
		`SELECT id, position, format, name, track_count FROM s_medium WHERE rel = ? ORDER BY position`, id)
	if err != nil {
		return err
	}
	type medium struct {
		id, position, trackCount int
		format, name             string
	}
	var media []medium
	for rows.Next() {
		var m medium
		var format int
		if err := rows.Scan(&m.id, &m.position, &format, &m.name, &m.trackCount); err != nil {
			rows.Close()
			return err
		}
		m.format = c.mediumFormats[format]
		media = append(media, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	total := 0
	for _, m := range media {
		rel.Media = append(rel.Media, skyhook.MediumResource{
			Name: m.name, Format: m.format, Position: m.position,
		})
		total += m.trackCount
		if err := c.mediumTracks(m.id, m.position, rel); err != nil {
			return err
		}
	}
	rel.TrackCount = total
	return nil
}

func (c *collector) mediumTracks(mediumID, mediumPosition int, rel *skyhook.ReleaseResource) error {
	rows, err := c.staging.db.Query(`
		SELECT t.gid, t.position, t.number, t.name, t.length, t.credit, r.gid
		FROM s_track t LEFT JOIN s_recording r ON r.id = t.recording
		WHERE t.medium = ? ORDER BY t.position`, mediumID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var gid, number, name string
		var position, credit int
		var length sql.NullInt64
		var recordingGID sql.NullString
		if err := rows.Scan(&gid, &position, &number, &name, &length, &credit, &recordingGID); err != nil {
			return err
		}

		track := skyhook.TrackResource{
			ID: gid, OldIDs: []string{}, OldRecordingIDs: []string{},
			RecordingID: recordingGID.String, TrackName: name,
			TrackNumber: number, TrackPosition: position, MediumNumber: mediumPosition,
		}
		if length.Valid {
			ms := int(length.Int64)
			track.DurationMs = &ms
		}
		if number == "" {
			track.TrackNumber = fmt.Sprint(position)
		}
		for _, artistID := range c.creditArtists[credit] {
			if a, ok := c.artistsByID[artistID]; ok {
				track.ArtistID = a.gid
				break
			}
		}
		rel.Tracks = append(rel.Tracks, track)
	}
	return rows.Err()
}

// sortedKeys returns a set's members in a stable order, so a rebuild of the
// same export produces byte-identical payloads.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
