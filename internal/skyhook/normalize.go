package skyhook

import (
	"strings"
	"unicode"
)

// Normalize folds a name to the form exact matching compares.
//
// The rules follow how people type a band name rather than how MusicBrainz
// stores it. Case is ignored and "&" reads as "and", since nobody types the
// symbol. Whitespace separates words, but other punctuation is dropped
// without separating, because "The La's" and "AC/DC" are typed as words
// rather than as "la s" and "ac dc".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false

	write := func(text string) {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(text)
	}

	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			write(string(r))
		case r == '&':
			write("and")
		case unicode.IsSpace(r):
			pendingSpace = true
		}
	}
	return b.String()
}
