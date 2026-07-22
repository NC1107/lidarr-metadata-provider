// Package skyhook pins the JSON contract that Lidarr's SkyHook metadata
// client consumes.
//
// The types are a field-for-field port of Lidarr's SkyHook resource DTOs
// (Lidarr/Lidarr, src/NzbDrone.Core/MetadataSource/SkyHook/Resource, develop
// branch as of 2026-07-22, GPL-3.0). The json tags do NOT come from the C#
// property names: Lidarr deserializes case-insensitively (Json.NET), so the
// casing we must emit is defined by the golden fixtures in fixtures/v0.4,
// captured from the live api.lidarr.audio/api/v0.4 service. That casing is
// inconsistent (artistname vs Albums vs remoteUrl) and load-bearing; it is
// preserved here exactly and verified against every fixture by the tests in
// this package.
//
// Upstream serializes the same conceptual entity differently depending on
// context (different key sets and different casing), so each context gets its
// own type instead of omitempty tricks:
//
//   - artist at the top level        -> ArtistResource (has "Albums")
//   - artist inside an album payload -> AlbumArtistResource (no "Albums" key)
//   - album at the top level         -> AlbumResource (lowercase keys)
//   - album inside an artist payload -> ArtistAlbumResource (capitalized keys)
//
// Slices are never omitted by upstream: an empty collection is emitted as [],
// not null and not absent. Builders must therefore always allocate slices; a
// nil slice would marshal as null and break the contract.
//
// C# DTO fields that upstream never emits are left out of the emitted shape on
// purpose (the fixtures are the contract; adding keys is drift). They are:
// ArtistResource.AristUrl (sic, typo in Lidarr), ImageResource.Height,
// ImageResource.Width, and TrackResource.Explicit. Lidarr fills them with
// zero values. Conversely, upstream emits sortname, aliases and remoteUrl,
// which Lidarr's DTOs do not declare; the contract keeps them.
package skyhook

// ArtistResource is the artist object served by GET /artist/{mbid}, returned
// as a list by GET /search?type=artist, and nested in search?type=all results.
// The "Albums" list is present in all of those contexts, even in search
// results (skeletal entries only).
type ArtistResource struct {
	ID             string                `json:"id"`
	OldIDs         []string              `json:"oldids"`
	ArtistName     string                `json:"artistname"`
	SortName       string                `json:"sortname"`
	ArtistAliases  []string              `json:"artistaliases"`
	Disambiguation string                `json:"disambiguation"`
	Overview       *string               `json:"overview"`
	Type           *string               `json:"type"`
	Status         string                `json:"status"`
	Genres         []string              `json:"genres"`
	Images         []ImageResource       `json:"images"`
	Links          []LinkResource        `json:"links"`
	Rating         RatingResource        `json:"rating"`
	Albums         []ArtistAlbumResource `json:"Albums"`
}

// AlbumArtistResource is the artist as embedded in an album payload's
// "artists" list. Identical to ArtistResource except that upstream never
// emits an "Albums" key in this context.
type AlbumArtistResource struct {
	ID             string          `json:"id"`
	OldIDs         []string        `json:"oldids"`
	ArtistName     string          `json:"artistname"`
	SortName       string          `json:"sortname"`
	ArtistAliases  []string        `json:"artistaliases"`
	Disambiguation string          `json:"disambiguation"`
	Overview       *string         `json:"overview"`
	Type           *string         `json:"type"`
	Status         string          `json:"status"`
	Genres         []string        `json:"genres"`
	Images         []ImageResource `json:"images"`
	Links          []LinkResource  `json:"links"`
	Rating         RatingResource  `json:"rating"`
}

// AlbumResource is the album (release group) object served by
// GET /album/{mbid}, returned as a list by GET /search?type=album, and nested
// in search?type=all results. Keys are lowercase in this context, unlike the
// capitalized ArtistAlbumResource shape of the same entity inside an artist
// payload.
type AlbumResource struct {
	ID              string                `json:"id"`
	OldIDs          []string              `json:"oldids"`
	Title           string                `json:"title"`
	Aliases         []string              `json:"aliases"`
	Disambiguation  string                `json:"disambiguation"`
	Overview        *string               `json:"overview"`
	Type            string                `json:"type"`
	SecondaryTypes  []string              `json:"secondarytypes"`
	ReleaseStatuses []string              `json:"releasestatuses"`
	ReleaseDate     *string               `json:"releasedate"`
	ArtistID        string                `json:"artistid"`
	Artists         []AlbumArtistResource `json:"artists"`
	Genres          []string              `json:"genres"`
	Images          []ImageResource       `json:"images"`
	Links           []LinkResource        `json:"links"`
	Rating          RatingResource        `json:"rating"`
	Releases        []ReleaseResource     `json:"Releases"`
}

// ArtistAlbumResource is the skeletal album entry inside an artist payload's
// "Albums" list: capitalized keys, no artists/images/links/releases. Rating
// is declared by Lidarr's DTO but was null in every one of the 16k+ album
// entries across the fixture set, hence the pointer.
type ArtistAlbumResource struct {
	ID              string          `json:"Id"`
	OldIDs          []string        `json:"OldIds"`
	Title           string          `json:"Title"`
	Type            string          `json:"Type"`
	SecondaryTypes  []string        `json:"SecondaryTypes"`
	ReleaseStatuses []string        `json:"ReleaseStatuses"`
	ReleaseDate     *string         `json:"ReleaseDate"`
	Rating          *RatingResource `json:"Rating"`
}

// ReleaseResource is a concrete release inside an album payload's "Releases"
// list.
type ReleaseResource struct {
	ID             string           `json:"Id"`
	OldIDs         []string         `json:"OldIds"`
	Title          string           `json:"Title"`
	Disambiguation string           `json:"Disambiguation"`
	Status         string           `json:"Status"`
	Country        []string         `json:"Country"`
	Label          []string         `json:"Label"`
	ReleaseDate    *string          `json:"ReleaseDate"`
	Media          []MediumResource `json:"Media"`
	TrackCount     int              `json:"TrackCount"`
	Tracks         []TrackResource  `json:"Tracks"`
}

// TrackResource is a track inside a release's "Tracks" list. Lidarr's DTO
// also declares Explicit (bool); upstream never emits it.
type TrackResource struct {
	ID              string   `json:"Id"`
	OldIDs          []string `json:"OldIds"`
	RecordingID     string   `json:"RecordingId"`
	OldRecordingIDs []string `json:"OldRecordingIds"`
	ArtistID        string   `json:"ArtistId"`
	TrackName       string   `json:"TrackName"`
	TrackNumber     string   `json:"TrackNumber"`
	TrackPosition   int      `json:"TrackPosition"`
	MediumNumber    int      `json:"MediumNumber"`
	DurationMs      *int     `json:"DurationMs"`
}

// MediumResource is a disc/medium inside a release's "Media" list.
type MediumResource struct {
	Name     string `json:"Name"`
	Format   string `json:"Format"`
	Position int    `json:"Position"`
}

// ImageResource is an artist or album image. Lidarr's DTO declares Height and
// Width, which upstream never emits; upstream instead adds remoteUrl (the
// original third-party URL behind the images.lidarr.audio cache).
type ImageResource struct {
	CoverType string `json:"CoverType"`
	URL       string `json:"Url"`
	RemoteURL string `json:"remoteUrl"`
}

// LinkResource is an external link on an artist or album.
type LinkResource struct {
	Target string `json:"target"`
	Type   string `json:"type"`
}

// RatingResource is a community rating. Value is null when there are no
// votes (Count 0).
type RatingResource struct {
	Count int      `json:"Count"`
	Value *float64 `json:"Value"`
}

// EntityResource is one result of GET /search?type=all: exactly one of Artist
// or Album is set. Lidarr's DTO capitalizes Score/Artist/Album; upstream
// emits lowercase.
type EntityResource struct {
	Score  int             `json:"score"`
	Artist *ArtistResource `json:"artist"`
	Album  *AlbumResource  `json:"album"`
}

// RecentUpdatesResource answers GET /recent/artist and GET /recent/album.
// Lidarr's DTO capitalizes the fields; upstream emits lowercase. Since is an
// ISO 8601 timestamp with offset. When Limited is true Lidarr discards Items
// and falls back to its normal full refresh, so a static dataset can always
// answer {"since": <echo>, "count": 0, "limited": true, "items": []}.
type RecentUpdatesResource struct {
	Since   string   `json:"since"`
	Count   int      `json:"count"`
	Limited bool     `json:"limited"`
	Items   []string `json:"items"`
}

// ServerInfo answers GET /. Not a SkyHook DTO (Lidarr tolerates failures
// here); the shape mirrors the live upstream root object.
type ServerInfo struct {
	Version         string `json:"version"`
	Branch          string `json:"branch"`
	Commit          string `json:"commit"`
	ReplicationDate string `json:"replication_date"`
}
