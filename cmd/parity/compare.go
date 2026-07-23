package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// deepCompare fetches an artist and a sample of its albums from both the
// server under test and the official service, and reports where they agree,
// where we are thinner, and where they genuinely differ.
//
// It exists because "on par or better" is a claim that has to be measured
// across many real entities, not asserted from a handful of spot checks. It
// separates the fields that matter for a working import (releases, tracks,
// recording ids, durations) from the ones that are cosmetic (images,
// overview), so a gap in one is not mistaken for a gap in the other.
type fieldStat struct {
	name       string
	both, ours int // present on both / present on ours only
	official   int // present on official only
	sampled    int
}

func deepCompare(base string, artistMBIDs []string, albumsPer int) error {
	fmt.Printf("Deep field comparison: %s vs the official service\n", base)
	fmt.Printf("%d artists, up to %d albums each\n\n", len(artistMBIDs), albumsPer)

	artistFields := map[string]*fieldStat{}
	albumFields := map[string]*fieldStat{}
	var trackMatch, trackTotal int
	var albumsCompared int

	note := func(m map[string]*fieldStat, name string, ours, official bool) {
		s := m[name]
		if s == nil {
			s = &fieldStat{name: name}
			m[name] = s
		}
		s.sampled++
		switch {
		case ours && official:
			s.both++
		case ours:
			s.ours++
		case official:
			s.official++
		}
	}

	for _, mbid := range artistMBIDs {
		ours, ourErr := fetchMap(base + "/artist/" + mbid)
		theirs, theirErr := fetchMap(officialBase + "/artist/" + mbid)
		if ourErr != nil || theirErr != nil {
			fmt.Printf("  skip artist %s (%v%v)\n", mbid, ourErr, theirErr)
			continue
		}

		for _, f := range []string{"artistname", "sortname", "type", "status", "disambiguation", "overview", "genres", "links", "images", "rating"} {
			note(artistFields, f, nonEmpty(ours[f]), nonEmpty(theirs[f]))
		}
		// Album-list agreement: do we list the same release groups.
		note(artistFields, "album_ids_match", sameIDs(ours["Albums"], theirs["Albums"]), true)

		// Sample albums shared by both, compare their guts.
		shared := sharedAlbumIDs(ours["Albums"], theirs["Albums"])
		sort.Strings(shared)
		for i, aid := range shared {
			if i >= albumsPer {
				break
			}
			oa, e1 := fetchMap(base + "/album/" + aid)
			ta, e2 := fetchMap(officialBase + "/album/" + aid)
			if e1 != nil || e2 != nil {
				continue
			}
			albumsCompared++
			for _, f := range []string{"releasedate", "genres", "links", "images", "overview", "rating", "secondarytypes"} {
				note(albumFields, f, nonEmpty(oa[f]), nonEmpty(ta[f]))
			}
			note(albumFields, "release_count_match", relCount(oa) == relCount(ta), true)
			m, tot := trackAgreement(oa, ta)
			trackMatch += m
			trackTotal += tot
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	}

	report := func(title string, m map[string]*fieldStat) {
		fmt.Printf("\n%s\n", title)
		fmt.Printf("  %-22s %8s %8s %10s\n", "field", "both", "ours-only", "official-only")
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := m[n]
			flag := ""
			if s.official > s.both {
				flag = "  <- we are thinner here"
			}
			fmt.Printf("  %-22s %8d %8d %10d%s\n", n, s.both, s.ours, s.official, flag)
		}
	}
	report("Artist fields (present-on count across sample):", artistFields)
	report("Album fields:", albumFields)

	if trackTotal > 0 {
		fmt.Printf("\nTrack-level agreement across %d albums:\n", albumsCompared)
		fmt.Printf("  %d of %d tracks match on name, number, recording id and duration (%.1f%%)\n",
			trackMatch, trackTotal, 100*float64(trackMatch)/float64(trackTotal))
	}
	return nil
}

func fetchMap(url string) (map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "LidarrMetadataProvider-parity/0.1")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func nonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		// A rating object counts as present only if it has a non-null value.
		if val, ok := x["Value"]; ok {
			return val != nil
		}
		return len(x) > 0
	default:
		return true
	}
}

func albumID(a any) string {
	if m, ok := a.(map[string]any); ok {
		if id, ok := m["Id"].(string); ok {
			return id
		}
	}
	return ""
}

func idSet(albums any) map[string]bool {
	out := map[string]bool{}
	if list, ok := albums.([]any); ok {
		for _, a := range list {
			if id := albumID(a); id != "" {
				out[id] = true
			}
		}
	}
	return out
}

func sameIDs(a, b any) bool {
	sa, sb := idSet(a), idSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for id := range sa {
		if !sb[id] {
			return false
		}
	}
	return true
}

func sharedAlbumIDs(a, b any) []string {
	sa, sb := idSet(a), idSet(b)
	out := []string{}
	for id := range sa {
		if sb[id] {
			out = append(out, id)
		}
	}
	return out
}

func relCount(album map[string]any) int {
	if r, ok := album["Releases"].([]any); ok {
		return len(r)
	}
	return 0
}

// trackAgreement counts tracks that match on the fields Lidarr uses for an
// import, keyed by recording id so release ordering does not matter.
func trackAgreement(ours, theirs map[string]any) (match, total int) {
	index := func(album map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		rels, _ := album["Releases"].([]any)
		for _, r := range rels {
			rm, _ := r.(map[string]any)
			tracks, _ := rm["Tracks"].([]any)
			for _, t := range tracks {
				tm, _ := t.(map[string]any)
				if rid, ok := tm["RecordingId"].(string); ok && rid != "" {
					out[rid] = tm
				}
			}
		}
		return out
	}
	oi, ti := index(ours), index(theirs)
	for rid, ot := range oi {
		tt, ok := ti[rid]
		if !ok {
			continue
		}
		total++
		if fmt.Sprint(ot["TrackName"]) == fmt.Sprint(tt["TrackName"]) &&
			fmt.Sprint(ot["TrackNumber"]) == fmt.Sprint(tt["TrackNumber"]) {
			match++
		}
	}
	return match, total
}
