package musicbrainz

import "testing"

// TestToAlbumIncludesFeaturedTrackArtist guards the invariant that broke Lidarr
// imports: a track crediting a guest not on the release-group credit must still
// appear in the album's artist list, or Lidarr discards the whole album. The
// dataset build enforces this; the fallback path must too.
func TestToAlbumIncludesFeaturedTrackArtist(t *testing.T) {
	primary := &mbArtist{ID: "artist-primary", Name: "Primary", SortName: "Primary"}
	guest := &mbArtist{ID: "artist-guest", Name: "Guest", SortName: "Guest"}

	rg := &mbReleaseGroup{
		ID:           "rg-1",
		Title:        "An Album",
		ArtistCredit: []mbArtistCredit{{Name: "Primary", Artist: primary}},
	}
	releases := []mbRelease{{
		ID:     "rel-1",
		Status: "Official",
		Media: []mbMedium{{
			Position:   1,
			TrackCount: 2,
			Tracks: []mbTrack{
				{ID: "t1", Title: "Solo", Position: 1,
					ArtistCredit: []mbArtistCredit{{Artist: primary}}},
				// A guest verse credited only on this track.
				{ID: "t2", Title: "Feature", Position: 2,
					ArtistCredit: []mbArtistCredit{{Artist: guest}}},
			},
		}},
	}}

	album := toAlbum(rg, releases)

	present := map[string]bool{}
	for _, a := range album.Artists {
		present[a.ID] = true
	}
	if !present["artist-primary"] {
		t.Error("primary artist missing from album Artists")
	}
	if !present["artist-guest"] {
		t.Error("featured track artist missing from album Artists (would crash Lidarr's import)")
	}

	// Every track's ArtistID must resolve within the album's artist list.
	for _, rel := range album.Releases {
		for _, tr := range rel.Tracks {
			if tr.ArtistID != "" && !present[tr.ArtistID] {
				t.Errorf("track %q credits %q, absent from album Artists", tr.TrackName, tr.ArtistID)
			}
		}
	}
}

// TestToAlbumUsesRecordingCreditWhenTrackHasNone checks the fallback to the
// recording's credit, so a track without its own artist-credit still resolves.
func TestToAlbumUsesRecordingCreditWhenTrackHasNone(t *testing.T) {
	primary := &mbArtist{ID: "p", Name: "P", SortName: "P"}
	guest := &mbArtist{ID: "g", Name: "G", SortName: "G"}
	rg := &mbReleaseGroup{ID: "rg", Title: "A", ArtistCredit: []mbArtistCredit{{Artist: primary}}}
	releases := []mbRelease{{ID: "r", Media: []mbMedium{{Position: 1, Tracks: []mbTrack{
		{ID: "t", Title: "T", Position: 1,
			Recording: &mbRecording{ID: "rec", ArtistCredit: []mbArtistCredit{{Artist: guest}}}},
	}}}}}

	album := toAlbum(rg, releases)
	present := map[string]bool{}
	for _, a := range album.Artists {
		present[a.ID] = true
	}
	if !present["g"] {
		t.Error("recording-credited artist missing from album Artists")
	}
	if album.Releases[0].Tracks[0].ArtistID != "g" {
		t.Errorf("track ArtistID = %q, want g", album.Releases[0].Tracks[0].ArtistID)
	}
}
