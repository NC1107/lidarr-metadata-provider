package skyhook

// MetadataProfile is the album filter Lidarr applies to an artist payload
// after fetching it. Ported from SkyHookProxy.FilterAlbums (Lidarr/Lidarr,
// GPL-3.0).
//
// This runs entirely on Lidarr's side: the artist route takes no filter
// parameters. It matters here because it decides what a user actually sees,
// and because it means a skeletal album with no ReleaseStatuses is invisible
// no matter how correct the rest of its JSON is. The pipeline uses this to
// check its output, and the dev UI uses it to show the album count Lidarr
// would display rather than the raw one.
type MetadataProfile struct {
	PrimaryTypes    []string
	SecondaryTypes  []string
	ReleaseStatuses []string
}

// StandardProfile is Lidarr's stock profile, from
// MetadataProfileService.AddDefaultProfile: studio albums, officially
// released. It is what most users are running.
var StandardProfile = MetadataProfile{
	PrimaryTypes:    []string{"Album"},
	SecondaryTypes:  []string{"Studio"},
	ReleaseStatuses: []string{"Official"},
}

// Allows reports whether Lidarr would keep this album under the profile.
//
// The empty-SecondaryTypes case is the subtle one: Lidarr treats an album
// with no secondary types as a studio album, so it survives only when the
// profile allows "Studio".
func (p MetadataProfile) Allows(a ArtistAlbumResource) bool {
	if !contains(p.PrimaryTypes, a.Type) {
		return false
	}
	if len(a.SecondaryTypes) == 0 {
		if !contains(p.SecondaryTypes, "Studio") {
			return false
		}
	} else if !intersects(p.SecondaryTypes, a.SecondaryTypes) {
		return false
	}
	// An empty ReleaseStatuses never intersects anything, so the album is
	// dropped. This is the failure mode a dataset bug would produce.
	return intersects(p.ReleaseStatuses, a.ReleaseStatuses)
}

// Filter returns the albums Lidarr would keep, in their original order.
func (p MetadataProfile) Filter(albums []ArtistAlbumResource) []ArtistAlbumResource {
	out := make([]ArtistAlbumResource, 0, len(albums))
	for _, a := range albums {
		if p.Allows(a) {
			out = append(out, a)
		}
	}
	return out
}

func contains(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, x := range b {
		if contains(a, x) {
			return true
		}
	}
	return false
}
