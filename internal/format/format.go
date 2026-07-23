// Package format renders MusicBrainz values the way the upstream metadata
// service does. Both the dataset pipeline and the live fallback assemble
// SkyHook resources, and their responses must be byte-identical; keeping these
// transforms in one place is what stops the two paths from drifting (the
// fallback path had silently missed all three of these; see AUDIT.md 31-33).
package format

import (
	"strings"
	"unicode"
)

// TitleCase capitalises the first letter of each word, matching how upstream
// renders genre names. Word boundaries are space, '-', and '&'; the rest of a
// word is left untouched. Upstream does this naively - it renders "Idm", not
// "IDM" - so this deliberately does not special-case acronyms.
func TitleCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	wordStart := true
	for _, r := range s {
		if wordStart {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		wordStart = r == ' ' || r == '-' || r == '&'
	}
	return b.String()
}

// LinkType is the label upstream gives a link: the host's second-to-last
// component, so "https://www.discogs.com/artist/1" types as "discogs".
//
// The only compound suffixes upstream strips are co.uk and co.jp (bbc.co.uk ->
// "bbc"). It strips no other, so gov.au types as "gov", co.kr as "co", ac.jp as
// "ac", com.br as "com" - odd, but what the golden fixtures show.
func LinkType(url string) string {
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// Isolate the authority before touching '@': a path can carry one
	// (tiktok.com/@handle), and only the authority's '@' is userinfo.
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(strings.ToLower(host), ".") // tolerate a trailing-dot FQDN
	host = strings.TrimPrefix(host, "www.")

	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	last := parts[len(parts)-1]
	if len(parts) >= 3 && parts[len(parts)-2] == "co" && (last == "uk" || last == "jp") {
		return parts[len(parts)-3]
	}
	return parts[len(parts)-2]
}

// PrimaryTypeOrOther is a release group's primary type, defaulting an untyped
// group to "Other". An empty Type intersects no Lidarr metadata profile, which
// makes the album invisible and unmonitorable; upstream reports untyped groups
// as "Other" (see AUDIT.md 31).
func PrimaryTypeOrOther(t string) string {
	if t == "" {
		return "Other"
	}
	return t
}
