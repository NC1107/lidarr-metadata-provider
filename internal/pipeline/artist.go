// Package pipeline turns a MusicBrainz export into Lidarr metadata payloads.
//
// It runs on our machines, never a user's, so it may take its time and use
// memory freely. What it must not do is produce a payload that is shaped
// correctly but wrong, which is why every table read asserts its column count
// and why the output is diffed against golden fixtures.
package pipeline

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// BuildArtists extracts full artist payloads for the given MBIDs, reading
// each archive exactly once.
//
// A MusicBrainz export is split across two files and both are needed. The
// core entities live in mbdump.tar.bz2, while the derived archive carries the
// computed tables: artist_meta holds ratings and release_group_meta holds the
// first release date, which is the album date the contract reports. Building
// from the core archive alone yields albums with no dates at all.
//
// One pass per archive is a hard constraint rather than an optimisation,
// since each is a sequential bzip2 stream that costs minutes to re-read. Tar
// order is alphabetical, which happens to cooperate: artist and
// artist_credit_name arrive before release_group, so the credits needed to
// attribute a release group are already known when its row appears.
//
// The exception is release, which sorts before release_group and therefore
// arrives while the set of interesting release groups is still unknown.
// Statuses are accumulated for every release group rather than only the
// wanted ones, then discarded at assembly. That trades a few hundred MB for
// not reading 6.9 GB twice.
//
// The core archive must be read before the derived one, which only fills in
// entities the first pass established.
func BuildArtists(core, derived *mbdump.Archive, mbids []string) (map[string]*skyhook.ArtistResource, error) {
	if err := sameExport(core, derived); err != nil {
		return nil, err
	}

	want := make(map[string]bool, len(mbids))
	for _, id := range mbids {
		want[strings.ToLower(strings.TrimSpace(id))] = true
	}

	c := newCollector(want)
	if err := core.ReadTables(c.coreHandlers()); err != nil {
		return nil, err
	}
	if err := derived.ReadTables(c.derivedHandlers()); err != nil {
		return nil, err
	}
	return c.assemble()
}

// sameExport rejects archives from different exports. Mixing them would join
// meta rows against entity IDs that have since been renumbered, producing
// plausible-looking payloads with dates and ratings belonging to other
// albums.
func sameExport(core, derived *mbdump.Archive) error {
	coreInfo, err := core.Info()
	if err != nil {
		return err
	}
	derivedInfo, err := derived.Info()
	if err != nil {
		return err
	}
	if coreInfo.ReplicationSequence != derivedInfo.ReplicationSequence {
		return fmt.Errorf(
			"archives are from different exports: core is at replication %d, derived at %d",
			coreInfo.ReplicationSequence, derivedInfo.ReplicationSequence)
	}
	return nil
}

// statusMask records which release statuses a release group was released
// under, as a bitmask over status IDs. MusicBrainz defines a handful of
// statuses with small IDs, so a mask costs 4 bytes per release group where a
// set would cost an allocation.
type statusMask uint32

const maxStatusID = 31

func (m *statusMask) add(id int) {
	if id >= 0 && id <= maxStatusID {
		*m |= 1 << uint(id)
	}
}

func (m statusMask) has(id int) bool { return id >= 0 && id <= maxStatusID && m&(1<<uint(id)) != 0 }

type artistRow struct {
	id       int
	gid      string
	name     string
	sortName string
	typeID   int
	comment  string
	ended    bool
	aliases  []string
	oldIDs   []string
	rating   *float64
	ratings  int
	groups   []int
}

type groupRow struct {
	id         int
	gid        string
	name       string
	comment    string
	typeID     int
	secondary  []int
	oldIDs     []string
	firstYear  int
	firstMonth int
	firstDay   int
	hasDate    bool
	artistIDs  []int
}

type collector struct {
	want map[string]bool

	artistsByID   map[int]*artistRow
	artistIDByGID map[string]int

	// creditArtists maps an artist credit to the wanted artists it names, so
	// a release group can be attributed without holding every credit.
	creditArtists map[int][]int

	groups   map[int]*groupRow
	groupIDs map[int]bool

	// rgStatuses covers every release group, because release rows arrive
	// before the release groups they belong to.
	rgStatuses map[int]statusMask

	artistTypes    map[int]string
	primaryTypes   map[int]string
	secondaryTypes map[int]string
	statusNames    map[int]string
}

func newCollector(want map[string]bool) *collector {
	return &collector{
		want:           want,
		artistsByID:    map[int]*artistRow{},
		artistIDByGID:  map[string]int{},
		creditArtists:  map[int][]int{},
		groups:         map[int]*groupRow{},
		groupIDs:       map[int]bool{},
		rgStatuses:     map[int]statusMask{},
		artistTypes:    map[int]string{},
		primaryTypes:   map[int]string{},
		secondaryTypes: map[int]string{},
		statusNames:    map[int]string{},
	}
}

// coreHandlers reads mbdump.tar.bz2, which carries the entities themselves.
func (c *collector) coreHandlers() map[string]mbdump.RowFunc {
	return map[string]mbdump.RowFunc{
		"artist":                            c.readArtist,
		"artist_alias":                      c.readArtistAlias,
		"artist_gid_redirect":               c.readArtistRedirect,
		"artist_credit_name":                c.readArtistCreditName,
		"artist_type":                       c.readTypeTable(c.artistTypes),
		"release":                           c.readRelease,
		"release_group":                     c.readReleaseGroup,
		"release_group_gid_redirect":        c.readReleaseGroupRedirect,
		"release_group_secondary_type_join": c.readSecondaryTypeJoin,
		"release_group_primary_type":        c.readTypeTable(c.primaryTypes),
		"release_group_secondary_type":      c.readTypeTable(c.secondaryTypes),
		"release_status":                    c.readTypeTable(c.statusNames),
	}
}

// derivedHandlers reads mbdump-derived.tar.bz2, which carries the computed
// tables. These only annotate entities the core pass already found.
func (c *collector) derivedHandlers() map[string]mbdump.RowFunc {
	return map[string]mbdump.RowFunc{
		"artist_meta":        c.readArtistMeta,
		"release_group_meta": c.readReleaseGroupMeta,
	}
}

func (c *collector) readTypeTable(into map[int]string) mbdump.RowFunc {
	return func(row []mbdump.Field) error {
		if err := mbdump.CheckColumns("type table", row, mbdump.TypeTableColumns); err != nil {
			return err
		}
		id, err := atoi(row[mbdump.TypeTableID])
		if err != nil {
			return err
		}
		into[id] = row[mbdump.TypeTableName].Value
		return nil
	}
}

func (c *collector) readArtist(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist", row, mbdump.ArtistColumns); err != nil {
		return err
	}
	gid := row[mbdump.ArtistGID].Value
	if !c.want[gid] {
		return nil
	}
	id, err := atoi(row[mbdump.ArtistID])
	if err != nil {
		return err
	}
	typeID, _ := optInt(row[mbdump.ArtistTypeID])
	c.artistsByID[id] = &artistRow{
		id:       id,
		gid:      gid,
		name:     row[mbdump.ArtistName].Value,
		sortName: row[mbdump.ArtistSortName].Value,
		typeID:   typeID,
		comment:  row[mbdump.ArtistComment].Value,
		ended:    row[mbdump.ArtistEnded].Value == "t",
		aliases:  []string{},
		oldIDs:   []string{},
	}
	c.artistIDByGID[gid] = id
	return nil
}

func (c *collector) readArtistAlias(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist_alias", row, mbdump.ArtistAliasColumns); err != nil {
		return err
	}
	artistID, err := atoi(row[mbdump.ArtistAliasArtist])
	if err != nil {
		return err
	}
	a, ok := c.artistsByID[artistID]
	if !ok {
		return nil
	}
	if name := row[mbdump.ArtistAliasName].Value; name != "" {
		a.aliases = append(a.aliases, name)
	}
	return nil
}

func (c *collector) readArtistMeta(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist_meta", row, mbdump.ArtistMetaColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ArtistMetaID])
	if err != nil {
		return err
	}
	a, ok := c.artistsByID[id]
	if !ok {
		return nil
	}
	// MusicBrainz stores ratings 0-100; the contract reports 0-10.
	if raw, ok := optInt(row[mbdump.ArtistMetaRating]); ok {
		value := float64(raw) / 10
		a.rating = &value
	}
	a.ratings, _ = optInt(row[mbdump.ArtistMetaRatingCount])
	return nil
}

func (c *collector) readArtistRedirect(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist_gid_redirect", row, mbdump.GIDRedirectColumns); err != nil {
		return err
	}
	newID, err := atoi(row[mbdump.GIDRedirectNewID])
	if err != nil {
		return err
	}
	if a, ok := c.artistsByID[newID]; ok {
		a.oldIDs = append(a.oldIDs, row[mbdump.GIDRedirectGID].Value)
	}
	return nil
}

func (c *collector) readArtistCreditName(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("artist_credit_name", row, mbdump.ArtistCreditNameColumns); err != nil {
		return err
	}
	artistID, err := atoi(row[mbdump.ArtistCreditNameArtist])
	if err != nil {
		return err
	}
	if _, ok := c.artistsByID[artistID]; !ok {
		return nil
	}
	credit, err := atoi(row[mbdump.ArtistCreditNameCredit])
	if err != nil {
		return err
	}
	c.creditArtists[credit] = append(c.creditArtists[credit], artistID)
	return nil
}

// readRelease accumulates statuses for every release group, since the wanted
// set is not known yet at this point in the archive.
func (c *collector) readRelease(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release", row, mbdump.ReleaseColumns); err != nil {
		return err
	}
	statusID, ok := optInt(row[mbdump.ReleaseStatusID])
	if !ok {
		// A release with no status contributes nothing; Lidarr filters on
		// status names, and an unset one is not a name.
		return nil
	}
	rg, err := atoi(row[mbdump.ReleaseGroupRef])
	if err != nil {
		return err
	}
	mask := c.rgStatuses[rg]
	mask.add(statusID)
	c.rgStatuses[rg] = mask
	return nil
}

func (c *collector) readReleaseGroup(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group", row, mbdump.ReleaseGroupColumns); err != nil {
		return err
	}
	credit, err := atoi(row[mbdump.ReleaseGroupArtistCredit])
	if err != nil {
		return err
	}
	artistIDs, ok := c.creditArtists[credit]
	if !ok {
		return nil
	}
	id, err := atoi(row[mbdump.ReleaseGroupID])
	if err != nil {
		return err
	}
	typeID, _ := optInt(row[mbdump.ReleaseGroupTypeID])
	c.groups[id] = &groupRow{
		id:        id,
		gid:       row[mbdump.ReleaseGroupGID].Value,
		name:      row[mbdump.ReleaseGroupName].Value,
		comment:   row[mbdump.ReleaseGroupComment].Value,
		typeID:    typeID,
		oldIDs:    []string{},
		artistIDs: append([]int(nil), artistIDs...),
	}
	c.groupIDs[id] = true
	for _, artistID := range artistIDs {
		c.artistsByID[artistID].groups = append(c.artistsByID[artistID].groups, id)
	}
	return nil
}

func (c *collector) readReleaseGroupMeta(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group_meta", row, mbdump.ReleaseGroupMetaColumns); err != nil {
		return err
	}
	id, err := atoi(row[mbdump.ReleaseGroupMetaID])
	if err != nil {
		return err
	}
	g, ok := c.groups[id]
	if !ok {
		return nil
	}
	year, hasYear := optInt(row[mbdump.ReleaseGroupMetaFirstYear])
	if !hasYear {
		return nil
	}
	g.firstYear = year
	g.firstMonth, _ = optInt(row[mbdump.ReleaseGroupMetaFirstMonth])
	g.firstDay, _ = optInt(row[mbdump.ReleaseGroupMetaFirstDay])
	g.hasDate = true
	return nil
}

func (c *collector) readReleaseGroupRedirect(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group_gid_redirect", row, mbdump.GIDRedirectColumns); err != nil {
		return err
	}
	newID, err := atoi(row[mbdump.GIDRedirectNewID])
	if err != nil {
		return err
	}
	if g, ok := c.groups[newID]; ok {
		g.oldIDs = append(g.oldIDs, row[mbdump.GIDRedirectGID].Value)
	}
	return nil
}

func (c *collector) readSecondaryTypeJoin(row []mbdump.Field) error {
	if err := mbdump.CheckColumns("release_group_secondary_type_join", row, mbdump.ReleaseGroupSecondaryJoinColumns); err != nil {
		return err
	}
	rg, err := atoi(row[mbdump.ReleaseGroupSecondaryJoinGroup])
	if err != nil {
		return err
	}
	g, ok := c.groups[rg]
	if !ok {
		return nil
	}
	typeID, err := atoi(row[mbdump.ReleaseGroupSecondaryJoinType])
	if err != nil {
		return err
	}
	g.secondary = append(g.secondary, typeID)
	return nil
}

func (c *collector) assemble() (map[string]*skyhook.ArtistResource, error) {
	out := make(map[string]*skyhook.ArtistResource, len(c.artistsByID))

	for _, a := range c.artistsByID {
		artist := &skyhook.ArtistResource{
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
			Albums:         []skyhook.ArtistAlbumResource{},
		}
		if name, ok := c.artistTypes[a.typeID]; ok && name != "" {
			artist.Type = &name
		}

		for _, gid := range a.groups {
			g := c.groups[gid]
			artist.Albums = append(artist.Albums, c.album(g))
		}
		sort.Slice(artist.Albums, func(i, j int) bool {
			return albumLess(artist.Albums[i], artist.Albums[j])
		})
		out[a.gid] = artist
	}

	for gid := range c.want {
		if _, ok := out[gid]; !ok {
			return out, fmt.Errorf("artist %s not present in this export", gid)
		}
	}
	return out, nil
}

func (c *collector) album(g *groupRow) skyhook.ArtistAlbumResource {
	secondary := make([]string, 0, len(g.secondary))
	for _, id := range g.secondary {
		if name, ok := c.secondaryTypes[id]; ok {
			secondary = append(secondary, name)
		}
	}
	sort.Strings(secondary)

	// The field that decides whether Lidarr shows this album at all.
	statuses := []string{}
	if mask, ok := c.rgStatuses[g.id]; ok {
		for id, name := range c.statusNames {
			if mask.has(id) {
				statuses = append(statuses, name)
			}
		}
		sort.Strings(statuses)
	}

	return skyhook.ArtistAlbumResource{
		ID:              g.gid,
		OldIDs:          sortedUnique(g.oldIDs),
		Title:           g.name,
		Type:            c.primaryTypes[g.typeID],
		SecondaryTypes:  secondary,
		ReleaseStatuses: statuses,
		ReleaseDate:     formatDate(g),
		Rating:          nil,
	}
}

// formatDate renders a partial MusicBrainz date the way the contract does,
// padding unknown components rather than omitting them.
func formatDate(g *groupRow) *string {
	if !g.hasDate {
		return nil
	}
	month, day := g.firstMonth, g.firstDay
	if month == 0 {
		month = 1
	}
	if day == 0 {
		day = 1
	}
	s := fmt.Sprintf("%04d-%02d-%02d", g.firstYear, month, day)
	return &s
}

func statusFor(ended bool) string {
	if ended {
		return "ended"
	}
	return "active"
}

func albumLess(a, b skyhook.ArtistAlbumResource) bool {
	ad, bd := "", ""
	if a.ReleaseDate != nil {
		ad = *a.ReleaseDate
	}
	if b.ReleaseDate != nil {
		bd = *b.ReleaseDate
	}
	if ad != bd {
		return ad < bd
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return a.ID < b.ID
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func atoi(f mbdump.Field) (int, error) {
	n, err := strconv.Atoi(f.Value)
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", f.Value)
	}
	return n, nil
}

// optInt reads a nullable integer column, reporting whether it was set.
func optInt(f mbdump.Field) (int, bool) {
	if f.IsNull || f.Value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(f.Value)
	if err != nil {
		return 0, false
	}
	return n, true
}
