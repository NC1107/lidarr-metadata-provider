package pipeline

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// albumTables extends the artist fixture with the release, medium and track
// rows a full album payload is assembled from.
func albumTables() map[string]string {
	t := sampleTables()

	t["medium"] = strings.Join([]string{
		row(mbdump.MediumColumns, map[int]string{
			mbdump.MediumID: "500", mbdump.MediumRelease: "100", mbdump.MediumPosition: "1",
			mbdump.MediumFormat: "1", mbdump.MediumName: "", mbdump.MediumTrackCount: "2"}),
		row(mbdump.MediumColumns, map[int]string{
			mbdump.MediumID: "501", mbdump.MediumRelease: "100", mbdump.MediumPosition: "2",
			mbdump.MediumFormat: "1", mbdump.MediumName: "Bonus", mbdump.MediumTrackCount: "1"}),
	}, "\n")

	// medium_format is wider than the other lookup tables.
	t["medium_format"] = row(mbdump.MediumFormatColumns, map[int]string{
		mbdump.TypeTableID: "1", mbdump.TypeTableName: "CD"})

	t["track"] = strings.Join([]string{
		row(mbdump.TrackColumns, map[int]string{
			mbdump.TrackID: "9001", mbdump.TrackGID: "t0000001-0000-0000-0000-000000000001",
			mbdump.TrackRecording: "700", mbdump.TrackMedium: "500", mbdump.TrackPosition: "1",
			mbdump.TrackNumber: "1", mbdump.TrackName: "Son of a Gun",
			mbdump.TrackArtistCredit: "10", mbdump.TrackLength: "116386"}),
		// No length: the payload must say null rather than claim zero.
		row(mbdump.TrackColumns, map[int]string{
			mbdump.TrackID: "9002", mbdump.TrackGID: "t0000002-0000-0000-0000-000000000002",
			mbdump.TrackRecording: "701", mbdump.TrackMedium: "500", mbdump.TrackPosition: "2",
			mbdump.TrackNumber: "2", mbdump.TrackName: "There She Goes",
			mbdump.TrackArtistCredit: "10"}),
		// Vinyl-style number that differs from its ordinal position.
		row(mbdump.TrackColumns, map[int]string{
			mbdump.TrackID: "9003", mbdump.TrackGID: "t0000003-0000-0000-0000-000000000003",
			mbdump.TrackRecording: "702", mbdump.TrackMedium: "501", mbdump.TrackPosition: "1",
			mbdump.TrackNumber: "A1", mbdump.TrackName: "Callin' All",
			mbdump.TrackArtistCredit: "10", mbdump.TrackLength: "142000"}),
	}, "\n")

	t["recording"] = strings.Join([]string{
		row(mbdump.RecordingColumns, map[int]string{
			mbdump.RecordingID: "700", mbdump.RecordingGID: "r0000001-0000-0000-0000-000000000001"}),
		row(mbdump.RecordingColumns, map[int]string{
			mbdump.RecordingID: "701", mbdump.RecordingGID: "r0000002-0000-0000-0000-000000000002"}),
		row(mbdump.RecordingColumns, map[int]string{
			mbdump.RecordingID: "702", mbdump.RecordingGID: "r0000003-0000-0000-0000-000000000003"}),
	}, "\n")

	t["release_label"] = row(mbdump.ReleaseLabelColumns, map[int]string{
		mbdump.ReleaseLabelRelease: "100", mbdump.ReleaseLabelLabel: "42"})
	t["label"] = row(mbdump.LabelColumns, map[int]string{
		mbdump.LabelID: "42", mbdump.LabelName: "Go! Discs"})

	// Two countries with different dates: the earliest is the release date.
	t["release_country"] = strings.Join([]string{
		row(mbdump.ReleaseCountryColumns, map[int]string{
			mbdump.ReleaseCountryRelease: "100", mbdump.ReleaseCountryArea: "221",
			mbdump.ReleaseCountryYear: "1990", mbdump.ReleaseCountryMonth: "10",
			mbdump.ReleaseCountryDay: "1"}),
		row(mbdump.ReleaseCountryColumns, map[int]string{
			mbdump.ReleaseCountryRelease: "100", mbdump.ReleaseCountryArea: "222",
			mbdump.ReleaseCountryYear: "1991", mbdump.ReleaseCountryMonth: "3",
			mbdump.ReleaseCountryDay: "5"}),
	}, "\n")
	t["area"] = strings.Join([]string{
		row(mbdump.AreaColumns, map[int]string{mbdump.AreaID: "221", mbdump.AreaName: "United Kingdom"}),
		row(mbdump.AreaColumns, map[int]string{mbdump.AreaID: "222", mbdump.AreaName: "United States"}),
	}, "\n")

	// A release with no country still carries a date, recorded separately.
	t["release_unknown_country"] = row(mbdump.ReleaseUnknownCountryColumns, map[int]string{
		mbdump.ReleaseUnknownCountryRelease: "102",
		mbdump.ReleaseUnknownCountryYear:    "1992"})

	return t
}

var albumCoreTables = append(append([]string{}, coreTables...),
	"area", "label", "medium", "medium_format", "recording",
	"release_country", "release_label", "release_unknown_country", "track")

func init() { derivedTables = append(derivedTables, "release_group_tag") }

func buildAlbums(t *testing.T) map[string]*skyhook.AlbumResource {
	t.Helper()
	tables := albumTables()

	core, err := mbdump.Open(fakeExport(t, albumCoreTables, tables))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := mbdump.Open(fakeExport(t, derivedTables, tables))
	if err != nil {
		t.Fatal(err)
	}
	// A cover-art archive marking release group 50 (the self-titled album) as
	// having a cover.
	caTables := map[string]string{
		"cover_art_archive.release_group_cover_art": row(mbdump.ReleaseGroupCoverArtColumns,
			map[int]string{mbdump.ReleaseGroupCoverArtGroup: "50"}),
	}
	coverArt, err := mbdump.Open(fakeExport(t,
		[]string{"cover_art_archive.release_group_cover_art"}, caTables))
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]*skyhook.AlbumResource{}
	err = BuildAll(core, derived, coverArt, filepath.Join(t.TempDir(), "staging.db"), Emitter{
		Artist: func(*skyhook.ArtistResource) error { return nil },
		Album:  func(a *skyhook.AlbumResource) error { out[a.ID] = a; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildAlbumAssemblesReleasesAndTracks(t *testing.T) {
	albums := buildAlbums(t)

	album, ok := albums[albumGID]
	if !ok {
		t.Fatalf("album %s missing, got %d albums", albumGID, len(albums))
	}
	if album.Title != "The La's" || album.Type != "Album" {
		t.Errorf("title/type = %q/%q", album.Title, album.Type)
	}
	if len(album.Artists) != 1 || album.Artists[0].ArtistName != "The La's" {
		t.Errorf("artists = %+v", album.Artists)
	}
	if album.ArtistID == "" {
		t.Error("ArtistID must be set, Lidarr links the album to its artist through it")
	}

	if len(album.Releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(album.Releases))
	}
	rel := album.Releases[0]
	if rel.Status != "Official" {
		t.Errorf("release status = %q", rel.Status)
	}
	if len(rel.Label) != 1 || rel.Label[0] != "Go! Discs" {
		t.Errorf("labels = %v", rel.Label)
	}
	if len(rel.Country) != 2 {
		t.Errorf("countries = %v, want both", rel.Country)
	}
	// The earliest country date is the release date.
	if rel.ReleaseDate == nil || *rel.ReleaseDate != "1990-10-01" {
		t.Errorf("ReleaseDate = %v, want the earliest country date", rel.ReleaseDate)
	}
	if rel.TrackCount != 3 {
		t.Errorf("TrackCount = %d, want the sum across both media", rel.TrackCount)
	}
	if len(rel.Media) != 2 || rel.Media[0].Format != "CD" {
		t.Errorf("media = %+v", rel.Media)
	}
}

func TestBuildAlbumTrackDetail(t *testing.T) {
	album := buildAlbums(t)[albumGID]
	tracks := album.Releases[0].Tracks

	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3 across both media", len(tracks))
	}
	first := tracks[0]
	if first.TrackName != "Son of a Gun" {
		t.Errorf("TrackName = %q", first.TrackName)
	}
	if first.RecordingID != "r0000001-0000-0000-0000-000000000001" {
		t.Errorf("RecordingID = %q, Lidarr matches files on it", first.RecordingID)
	}
	if first.DurationMs == nil || *first.DurationMs != 116386 {
		t.Errorf("DurationMs = %v", first.DurationMs)
	}
	if first.ArtistID == "" {
		t.Error("track ArtistID should resolve through its credit")
	}

	// An unknown duration must stay null rather than become zero.
	if tracks[1].DurationMs != nil {
		t.Errorf("track with no length got DurationMs %v, want null", *tracks[1].DurationMs)
	}

	// Vinyl numbering: the printed number and the ordinal differ.
	third := tracks[2]
	if third.TrackNumber != "A1" || third.TrackPosition != 1 {
		t.Errorf("number/position = %q/%d, want A1/1", third.TrackNumber, third.TrackPosition)
	}
	if third.MediumNumber != 2 {
		t.Errorf("MediumNumber = %d, want 2 for the second disc", third.MediumNumber)
	}
}

// A release with no country still has a date, held in a separate table.
func TestBuildAlbumFallsBackToUnknownCountryDate(t *testing.T) {
	albums := buildAlbums(t)
	album := albums[liveGID]
	if album == nil {
		t.Fatalf("live album missing")
	}
	if len(album.Releases) != 1 {
		t.Fatalf("got %d releases", len(album.Releases))
	}
	rel := album.Releases[0]
	if len(rel.Country) != 0 {
		t.Errorf("countries = %v, want none", rel.Country)
	}
	if rel.ReleaseDate == nil || *rel.ReleaseDate != "1992-01-01" {
		t.Errorf("ReleaseDate = %v, want the unknown-country date padded", rel.ReleaseDate)
	}
}

func TestBuiltAlbumMatchesTheContract(t *testing.T) {
	album := buildAlbums(t)[albumGID]
	raw, err := json.Marshal(album)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := skyhook.ContractDiff(raw, &skyhook.AlbumResource{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("built album drifted from the contract: %v", diffs)
	}
}

// A release group in the cover art archive carries a deterministic Cover Art
// Archive image; one that is not stays imageless.
func TestBuildAlbumCoverArt(t *testing.T) {
	albums := buildAlbums(t)

	withArt := albums[albumGID]
	if len(withArt.Images) != 1 {
		t.Fatalf("album with a cover got %d images", len(withArt.Images))
	}
	img := withArt.Images[0]
	if img.CoverType != "Cover" {
		t.Errorf("CoverType = %q", img.CoverType)
	}
	if img.URL != "https://coverartarchive.org/release-group/"+albumGID+"/front-500" {
		t.Errorf("image URL = %q", img.URL)
	}

	// The live album (group 51) is not in the cover art archive.
	if got := len(albums[liveGID].Images); got != 0 {
		t.Errorf("album with no cover got %d images", got)
	}
}
