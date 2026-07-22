package pipeline

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nc1107/lidarr-metadata-provider/internal/mbdump"
	"github.com/nc1107/lidarr-metadata-provider/internal/skyhook"
)

// row builds a COPY line, padding to width so column-count assertions see a
// realistically shaped row.
func row(width int, values map[int]string) string {
	cells := make([]string, width)
	for i := range cells {
		cells[i] = `\N`
	}
	for i, v := range values {
		cells[i] = v
	}
	return strings.Join(cells, "\t")
}

// fakeExport writes an archive shaped like a MusicBrainz export, with table
// files in the alphabetical order tar delivers them, which is what the single
// pass depends on.
func fakeExport(t *testing.T, which []string, tables map[string]string) string {
	t.Helper()

	names := []string{"TIMESTAMP", "SCHEMA_SEQUENCE", "REPLICATION_SEQUENCE"}
	files := map[string]string{
		"TIMESTAMP":            "2026-07-18 00:21:33+00\n",
		"SCHEMA_SEQUENCE":      "31\n",
		"REPLICATION_SEQUENCE": "187552\n",
	}
	// tar delivers table files in alphabetical order, and the single pass
	// depends on that ordering, so the fixture has to reproduce it rather
	// than whatever order the caller happened to list.
	sort.Strings(which)
	for _, name := range which {
		body := tables[name]
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		files["mbdump/"+name] = body
		names = append(names, "mbdump/"+name)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = &buf
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("bzip2 unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mbdump.tar.bz2")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	artistGID  = "ff3e88b3-7354-4f30-967c-1a61ebc8c642"
	albumGID   = "f57d03ff-b0a5-3b73-a14c-a5ed5f8cd956"
	liveGID    = "aaaaaaaa-0000-0000-0000-000000000001"
	noRelGID   = "bbbbbbbb-0000-0000-0000-000000000002"
	untypedGID = "cccccccc-0000-0000-0000-000000000003"
)

var coreTables = []string{
	"artist", "artist_alias", "artist_credit_name", "artist_gid_redirect",
	"artist_type", "release", "release_group", "release_group_gid_redirect",
	"release_group_primary_type", "release_group_secondary_type",
	"release_group_secondary_type_join", "release_status",
}

var derivedTables = []string{"artist_meta", "release_group_meta"}

func sampleTables() map[string]string {
	return map[string]string{
		"artist": row(mbdump.ArtistColumns, map[int]string{
			mbdump.ArtistID: "1", mbdump.ArtistGID: artistGID,
			mbdump.ArtistName: "The La's", mbdump.ArtistSortName: "La's, The",
			mbdump.ArtistTypeID: "2", mbdump.ArtistComment: "UK band",
			mbdump.ArtistEnded: "t",
		}),
		"artist_type": row(mbdump.TypeTableColumns, map[int]string{
			mbdump.TypeTableID: "2", mbdump.TypeTableName: "Group",
		}),
		"artist_alias": strings.Join([]string{
			row(mbdump.ArtistAliasColumns, map[int]string{
				mbdump.ArtistAliasArtist: "1", mbdump.ArtistAliasName: "The Las"}),
			// A duplicate alias must not appear twice in the payload.
			row(mbdump.ArtistAliasColumns, map[int]string{
				mbdump.ArtistAliasArtist: "1", mbdump.ArtistAliasName: "The Las"}),
			row(mbdump.ArtistAliasColumns, map[int]string{
				mbdump.ArtistAliasArtist: "1", mbdump.ArtistAliasName: "La's"}),
		}, "\n"),
		"artist_meta": row(mbdump.ArtistMetaColumns, map[int]string{
			mbdump.ArtistMetaID: "1", mbdump.ArtistMetaRating: "96",
			mbdump.ArtistMetaRatingCount: "86",
		}),
		"artist_gid_redirect": row(mbdump.GIDRedirectColumns, map[int]string{
			mbdump.GIDRedirectGID:   "00000000-dead-beef-0000-000000000000",
			mbdump.GIDRedirectNewID: "1",
		}),
		"artist_credit_name": row(mbdump.ArtistCreditNameColumns, map[int]string{
			mbdump.ArtistCreditNameCredit: "10", mbdump.ArtistCreditNameArtist: "1",
		}),
		"release": strings.Join([]string{
			row(mbdump.ReleaseColumns, map[int]string{
				mbdump.ReleaseID: "100", mbdump.ReleaseGroupRef: "50", mbdump.ReleaseStatusID: "1"}),
			row(mbdump.ReleaseColumns, map[int]string{
				mbdump.ReleaseID: "101", mbdump.ReleaseGroupRef: "50", mbdump.ReleaseStatusID: "3"}),
			row(mbdump.ReleaseColumns, map[int]string{
				mbdump.ReleaseID: "102", mbdump.ReleaseGroupRef: "51", mbdump.ReleaseStatusID: "1"}),
			// A release with no status contributes nothing.
			row(mbdump.ReleaseColumns, map[int]string{
				mbdump.ReleaseID: "103", mbdump.ReleaseGroupRef: "52"}),
		}, "\n"),
		"release_status": strings.Join([]string{
			row(mbdump.TypeTableColumns, map[int]string{
				mbdump.TypeTableID: "1", mbdump.TypeTableName: "Official"}),
			row(mbdump.TypeTableColumns, map[int]string{
				mbdump.TypeTableID: "3", mbdump.TypeTableName: "Bootleg"}),
		}, "\n"),
		"release_group": strings.Join([]string{
			row(mbdump.ReleaseGroupColumns, map[int]string{
				mbdump.ReleaseGroupID: "50", mbdump.ReleaseGroupGID: albumGID,
				mbdump.ReleaseGroupName: "The La's", mbdump.ReleaseGroupArtistCredit: "10",
				mbdump.ReleaseGroupTypeID: "1", mbdump.ReleaseGroupComment: ""}),
			row(mbdump.ReleaseGroupColumns, map[int]string{
				mbdump.ReleaseGroupID: "51", mbdump.ReleaseGroupGID: liveGID,
				mbdump.ReleaseGroupName: "Live", mbdump.ReleaseGroupArtistCredit: "10",
				mbdump.ReleaseGroupTypeID: "1"}),
			row(mbdump.ReleaseGroupColumns, map[int]string{
				mbdump.ReleaseGroupID: "52", mbdump.ReleaseGroupGID: noRelGID,
				mbdump.ReleaseGroupName: "Unreleased", mbdump.ReleaseGroupArtistCredit: "10",
				mbdump.ReleaseGroupTypeID: "1"}),
			// No primary type set, which MusicBrainz permits.
			row(mbdump.ReleaseGroupColumns, map[int]string{
				mbdump.ReleaseGroupID: "53", mbdump.ReleaseGroupGID: untypedGID,
				mbdump.ReleaseGroupName: "Untyped", mbdump.ReleaseGroupArtistCredit: "10"}),
		}, "\n"),
		"release_group_primary_type": row(mbdump.TypeTableColumns, map[int]string{
			mbdump.TypeTableID: "1", mbdump.TypeTableName: "Album",
		}),
		"release_group_secondary_type": row(mbdump.TypeTableColumns, map[int]string{
			mbdump.TypeTableID: "6", mbdump.TypeTableName: "Live",
		}),
		"release_group_secondary_type_join": row(mbdump.ReleaseGroupSecondaryJoinColumns, map[int]string{
			mbdump.ReleaseGroupSecondaryJoinGroup: "51",
			mbdump.ReleaseGroupSecondaryJoinType:  "6",
		}),
		"release_group_meta": strings.Join([]string{
			row(mbdump.ReleaseGroupMetaColumns, map[int]string{
				mbdump.ReleaseGroupMetaID: "50", mbdump.ReleaseGroupMetaFirstYear: "1990",
				mbdump.ReleaseGroupMetaFirstMonth: "10", mbdump.ReleaseGroupMetaFirstDay: "1"}),
			// Year only: the contract pads rather than omitting.
			row(mbdump.ReleaseGroupMetaColumns, map[int]string{
				mbdump.ReleaseGroupMetaID: "51", mbdump.ReleaseGroupMetaFirstYear: "2008"}),
		}, "\n"),
		"release_group_gid_redirect": row(mbdump.GIDRedirectColumns, map[int]string{
			mbdump.GIDRedirectGID:   "11111111-dead-beef-0000-000000000000",
			mbdump.GIDRedirectNewID: "50",
		}),
	}
}

// sampleExport returns the core and derived archives, split the way a real
// MusicBrainz export is.
func sampleExport(t *testing.T) (*mbdump.Archive, *mbdump.Archive) {
	t.Helper()
	tables := sampleTables()
	core, err := mbdump.Open(fakeExport(t, coreTables, tables))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := mbdump.Open(fakeExport(t, derivedTables, tables))
	if err != nil {
		t.Fatal(err)
	}
	return core, derived
}

func buildOne(t *testing.T) *skyhook.ArtistResource {
	t.Helper()
	core, derived := sampleExport(t)
	got, err := BuildArtists(core, derived, []string{artistGID})
	if err != nil {
		t.Fatal(err)
	}
	artist, ok := got[artistGID]
	if !ok {
		t.Fatal("artist missing from build output")
	}
	return artist
}

func TestBuildArtistFields(t *testing.T) {
	a := buildOne(t)

	if a.ArtistName != "The La's" || a.SortName != "La's, The" {
		t.Errorf("name/sortname = %q/%q", a.ArtistName, a.SortName)
	}
	if a.Type == nil || *a.Type != "Group" {
		t.Errorf("Type = %v, want Group", a.Type)
	}
	if a.Status != "ended" {
		t.Errorf("Status = %q, want ended", a.Status)
	}
	if a.Disambiguation != "UK band" {
		t.Errorf("Disambiguation = %q", a.Disambiguation)
	}
	// MusicBrainz stores ratings 0-100, the contract reports 0-10.
	if a.Rating.Value == nil || *a.Rating.Value != 9.6 || a.Rating.Count != 86 {
		t.Errorf("Rating = %+v, want 9.6 over 86 votes", a.Rating)
	}
	if len(a.ArtistAliases) != 2 {
		t.Errorf("aliases = %v, want the duplicate collapsed", a.ArtistAliases)
	}
	if len(a.OldIDs) != 1 || a.OldIDs[0] != "00000000-dead-beef-0000-000000000000" {
		t.Errorf("OldIDs = %v, want the redirected MBID", a.OldIDs)
	}
}

// The failure mode that makes an album invisible in Lidarr: every album must
// carry the statuses its releases were published under.
func TestBuildPopulatesReleaseStatuses(t *testing.T) {
	a := buildOne(t)

	byTitle := map[string]skyhook.ArtistAlbumResource{}
	for _, al := range a.Albums {
		byTitle[al.Title] = al
	}

	self := byTitle["The La's"]
	if len(self.ReleaseStatuses) != 2 ||
		self.ReleaseStatuses[0] != "Bootleg" || self.ReleaseStatuses[1] != "Official" {
		t.Errorf("ReleaseStatuses = %v, want both statuses sorted", self.ReleaseStatuses)
	}
	// A release group whose only release has no status ends up with none,
	// which correctly makes it invisible to every metadata profile.
	if got := byTitle["Unreleased"].ReleaseStatuses; len(got) != 0 {
		t.Errorf("statusless album got %v, want none", got)
	}
	if skyhook.StandardProfile.Allows(byTitle["Unreleased"]) {
		t.Error("an album with no statuses must not pass the Standard profile")
	}
	if !skyhook.StandardProfile.Allows(self) {
		t.Error("an official studio album must pass the Standard profile")
	}
}

func TestBuildAlbumTypesAndDates(t *testing.T) {
	a := buildOne(t)
	byTitle := map[string]skyhook.ArtistAlbumResource{}
	for _, al := range a.Albums {
		byTitle[al.Title] = al
	}

	self := byTitle["The La's"]
	if self.Type != "Album" {
		t.Errorf("Type = %q, want Album", self.Type)
	}
	if len(self.SecondaryTypes) != 0 {
		t.Errorf("SecondaryTypes = %v, want empty so Lidarr reads it as Studio", self.SecondaryTypes)
	}
	if self.ReleaseDate == nil || *self.ReleaseDate != "1990-10-01" {
		t.Errorf("ReleaseDate = %v, want 1990-10-01", self.ReleaseDate)
	}
	if len(self.OldIDs) != 1 {
		t.Errorf("album OldIDs = %v, want the redirected MBID", self.OldIDs)
	}

	live := byTitle["Live"]
	if len(live.SecondaryTypes) != 1 || live.SecondaryTypes[0] != "Live" {
		t.Errorf("SecondaryTypes = %v, want [Live]", live.SecondaryTypes)
	}
	// Year-only dates are padded, matching how the contract reports them.
	if live.ReleaseDate == nil || *live.ReleaseDate != "2008-01-01" {
		t.Errorf("ReleaseDate = %v, want 2008-01-01", live.ReleaseDate)
	}
	if live.Rating != nil {
		t.Error("skeletal albums carry a null Rating upstream")
	}
	if skyhook.StandardProfile.Allows(live) {
		t.Error("a live album must not pass the stock profile")
	}
}

// Empty collections must marshal as [] rather than null, which means they
// have to be allocated rather than left nil.
func TestBuildEmitsEmptySlicesNotNull(t *testing.T) {
	a := buildOne(t)
	diffs, err := skyhook.ContractDiff(mustJSON(t, a), &skyhook.ArtistResource{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("built payload does not round-trip through the contract: %v", diffs)
	}
	if a.Genres == nil || a.Images == nil || a.Links == nil || a.OldIDs == nil {
		t.Error("collections must be allocated so they marshal as []")
	}
}

func TestBuildReportsMissingArtist(t *testing.T) {
	core, derived := sampleExport(t)
	_, err := BuildArtists(core, derived, []string{"99999999-0000-0000-0000-000000000000"})
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("expected a missing-artist error, got %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// MusicBrainz leaves some release groups untyped. Upstream reports those as
// "Other"; an empty string would hide them from every metadata profile,
// including ones that allow Other.
func TestBuildDefaultsUntypedAlbumsToOther(t *testing.T) {
	a := buildOne(t)
	for _, al := range a.Albums {
		if al.Type == "" {
			t.Fatalf("album %q has an empty Type, which no profile can match", al.Title)
		}
		if al.Title == "Untyped" && al.Type != "Other" {
			t.Errorf("untyped album mapped to %q, want Other", al.Type)
		}
	}
}
