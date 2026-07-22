package mbdump

import "fmt"

// Column positions for the MusicBrainz tables this project reads, taken from
// admin/sql/CreateTables.sql at schema sequence 31.
//
// COPY output carries no header, so a row is only meaningful against the
// schema it was dumped under. Every table therefore declares its column count
// and callers assert it before indexing: a column inserted upstream would
// otherwise shift every field after it, and the resulting dataset would be
// wrong while still being shaped exactly like valid JSON. That is the one
// failure mode the contract tests cannot catch, which is why the check is
// here and not left to reviewers.
const (
	// artist: id, gid, name, sort_name, begin/end dates, type, area, gender,
	// comment, edits_pending, last_updated, ended, begin_area, end_area
	ArtistColumns  = 19
	ArtistID       = 0
	ArtistGID      = 1
	ArtistName     = 2
	ArtistSortName = 3
	ArtistTypeID   = 10
	ArtistComment  = 13
	ArtistEnded    = 16

	// artist_alias
	ArtistAliasColumns  = 16
	ArtistAliasArtist   = 1
	ArtistAliasName     = 2
	ArtistAliasTypeID   = 6
	ArtistAliasSortName = 7

	// artist_meta: ratings are stored 0-100 and the contract wants 0-10.
	ArtistMetaColumns     = 3
	ArtistMetaID          = 0
	ArtistMetaRating      = 1
	ArtistMetaRatingCount = 2

	// artist_credit_name links a credit to the artists it names.
	ArtistCreditNameColumns  = 5
	ArtistCreditNameCredit   = 0
	ArtistCreditNamePosition = 1
	ArtistCreditNameArtist   = 2

	// release_group
	ReleaseGroupColumns      = 8
	ReleaseGroupID           = 0
	ReleaseGroupGID          = 1
	ReleaseGroupName         = 2
	ReleaseGroupArtistCredit = 3
	ReleaseGroupTypeID       = 4
	ReleaseGroupComment      = 5

	// release_group_meta carries the first release date, which is what the
	// contract's ReleaseDate reports.
	ReleaseGroupMetaColumns    = 7
	ReleaseGroupMetaID         = 0
	ReleaseGroupMetaFirstYear  = 2
	ReleaseGroupMetaFirstMonth = 3
	ReleaseGroupMetaFirstDay   = 4

	// release_group_secondary_type_join
	ReleaseGroupSecondaryJoinColumns = 3
	ReleaseGroupSecondaryJoinGroup   = 0
	ReleaseGroupSecondaryJoinType    = 1

	// release: the source of ReleaseStatuses, which decides whether Lidarr
	// shows an album at all.
	ReleaseColumns  = 14
	ReleaseID       = 0
	ReleaseGID      = 1
	ReleaseName     = 2
	ReleaseGroupRef = 4
	ReleaseStatusID = 5

	// Lookup tables (artist_type, release_group_primary_type,
	// release_group_secondary_type, release_status) share a layout.
	TypeTableColumns = 6
	TypeTableID      = 0
	TypeTableName    = 1

	// *_gid_redirect tables map a retired MBID to its replacement, which is
	// what the contract's OldIds lists.
	GIDRedirectColumns = 3
	GIDRedirectGID     = 0
	GIDRedirectNewID   = 1

	// medium is a disc within a release.
	MediumColumns    = 9
	MediumID         = 0
	MediumRelease    = 1
	MediumPosition   = 2
	MediumFormat     = 3
	MediumName       = 4
	MediumTrackCount = 7

	// track. Position is the ordinal, Number is the printed label, and they
	// differ on vinyl where a track is numbered "A1".
	TrackColumns      = 12
	TrackID           = 0
	TrackGID          = 1
	TrackRecording    = 2
	TrackMedium       = 3
	TrackPosition     = 4
	TrackNumber       = 5
	TrackName         = 6
	TrackArtistCredit = 7
	TrackLength       = 8

	// recording carries the MBID the contract reports as RecordingId.
	RecordingColumns = 9
	RecordingID      = 0
	RecordingGID     = 1

	// release_country and release_unknown_country hold release dates and
	// countries. The release table itself has neither, so a release's date
	// can only be found through these two.
	ReleaseCountryColumns = 5
	ReleaseCountryRelease = 0
	ReleaseCountryArea    = 1
	ReleaseCountryYear    = 2
	ReleaseCountryMonth   = 3
	ReleaseCountryDay     = 4

	ReleaseUnknownCountryColumns = 4
	ReleaseUnknownCountryRelease = 0
	ReleaseUnknownCountryYear    = 1
	ReleaseUnknownCountryMonth   = 2
	ReleaseUnknownCountryDay     = 3

	// area names the country a release came out in. Upstream emits the name
	// ("United States") rather than the ISO code ("US"), so the code table is
	// not what the contract wants.
	AreaColumns = 14
	AreaID      = 0
	AreaName    = 2

	ReleaseLabelColumns = 5
	ReleaseLabelRelease = 1
	ReleaseLabelLabel   = 2

	LabelColumns = 16
	LabelID      = 0
	LabelName    = 2

	// medium_format shares the lookup table layout except for width.
	MediumFormatColumns = 8

	// genre is the controlled vocabulary that decides which tags count as
	// genres. tag names that appear here are genres; the rest are folksonomy.
	GenreColumns = 6
	GenreName    = 2

	// tag maps a numeric tag id to its name. artist_tag and
	// release_group_tag reference tags by id.
	TagColumns = 3
	TagID      = 0
	TagName    = 1

	// artist_tag and release_group_tag carry the vote count that orders
	// genres by how strongly the community applied them.
	ArtistTagColumns = 4
	ArtistTagArtist  = 0
	ArtistTagTag     = 1
	ArtistTagCount   = 2

	ReleaseGroupTagColumns = 4
	ReleaseGroupTagGroup   = 0
	ReleaseGroupTagTag     = 1
	ReleaseGroupTagCount   = 2

	// l_artist_url and l_release_group_url link an entity to a url row.
	// entity0 is the entity, entity1 is the url.
	LinkURLColumns = 9
	LinkURLEntity0 = 2
	LinkURLEntity1 = 3

	// url holds the address itself. The link type Lidarr shows is derived
	// from the address, not from a relationship type.
	URLColumns = 5
	URLID      = 0
	URLValue   = 2
)

// CheckColumns verifies a row has the expected width, naming the table so a
// schema drift failure says which one moved.
func CheckColumns(table string, row []Field, want int) error {
	if len(row) != want {
		return fmt.Errorf("table %s: got %d columns, schema %d expects %d (has the MusicBrainz schema changed?)",
			table, len(row), SupportedSchema, want)
	}
	return nil
}
