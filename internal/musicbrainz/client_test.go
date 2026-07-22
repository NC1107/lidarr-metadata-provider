package musicbrainz

import (
	"strings"
	"testing"
	"time"
)

// MusicBrainz refuses any user agent whose product token starts with a
// lowercase "lidarr", so this is a regression guard rather than a style
// preference: renaming the token to match the repository name would make
// every live fallback request fail with a 403.
func TestUserAgentAvoidsTheBlockedPrefix(t *testing.T) {
	ua := UserAgent("1.2.3", "me@example.com")

	if strings.HasPrefix(ua, "lidarr") {
		t.Fatalf("user agent %q starts with the prefix MusicBrainz blocks", ua)
	}
	if !strings.Contains(ua, "1.2.3") {
		t.Errorf("user agent %q should carry the version", ua)
	}
	if !strings.Contains(ua, "me@example.com") {
		t.Errorf("user agent %q should carry the contact", ua)
	}
	// MusicBrainz asks for "Name/version ( contact )".
	if !strings.Contains(ua, "/") || !strings.Contains(ua, "(") {
		t.Errorf("user agent %q does not follow the documented shape", ua)
	}
}

func TestUserAgentFillsInAMissingVersion(t *testing.T) {
	if ua := UserAgent("", "me@example.com"); !strings.Contains(ua, "0.0.0-dev") {
		t.Errorf("UserAgent with no version = %q, want a placeholder version", ua)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	const def = 5 * time.Second
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", def},
		{"12", 12 * time.Second},
		{"  7  ", 7 * time.Second},
		{"not a number", def},
		{"-1", def},
	}
	for _, c := range cases {
		if got := retryAfter(c.header, def); got != c.want {
			t.Errorf("retryAfter(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestNormalizeDatePadsPartialDates(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026", "2026-01-01"},
		{"2026-06", "2026-06-01"},
		{"2026-06-12", "2026-06-12"},
	}
	for _, c := range cases {
		got := normalizeDate(c.in)
		if got == nil || *got != c.want {
			t.Errorf("normalizeDate(%q) = %v, want %q", c.in, got, c.want)
		}
	}
	if got := normalizeDate(""); got != nil {
		t.Errorf("normalizeDate(\"\") = %v, want nil rather than a fabricated date", *got)
	}
}

func TestEscapeLuceneNeutralisesQuerySyntax(t *testing.T) {
	if got := escapeLucene("AC/DC"); got != `AC\/DC` {
		t.Errorf("escapeLucene(%q) = %q", "AC/DC", got)
	}
	if got := escapeLucene("Where Are We Now?"); !strings.HasSuffix(got, `\?`) {
		t.Errorf("escapeLucene left a bare ? in %q", got)
	}
	if got := escapeLucene("plain query"); got != "plain query" {
		t.Errorf("escapeLucene altered a plain query: %q", got)
	}
}
