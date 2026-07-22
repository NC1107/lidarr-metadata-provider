// Package mbdump reads MusicBrainz full export archives.
//
// An export is a bzip2-compressed tar carrying provenance files at the root
// (TIMESTAMP, SCHEMA_SEQUENCE, REPLICATION_SEQUENCE) and one file per table
// under mbdump/. Each table file is PostgreSQL's text COPY format: one row
// per line, columns separated by tabs, with backslash escapes and \N for
// null.
//
// The archive is read as a stream and never extracted. mbdump.tar.bz2 is
// 6.9 GB compressed and roughly 40 GB unpacked, so staging it to disk would
// impose a footprint most contributors do not have. The cost of streaming is
// that tar is sequential: tables are visited in archive order, not the order
// a caller asks for.
package mbdump

import (
	"strings"
)

// Null is the COPY representation of a NULL column.
const Null = `\N`

// Field is one column of a row. COPY cannot distinguish an empty string from
// NULL without this, and the difference matters: a NULL disambiguation is
// absent while an empty one is a real empty string.
type Field struct {
	Value  string
	IsNull bool
}

// String returns the field value, with NULL rendered as the empty string.
func (f Field) String() string { return f.Value }

// Or returns the field value, or def when the field is NULL.
func (f Field) Or(def string) string {
	if f.IsNull {
		return def
	}
	return f.Value
}

// splitRow splits one COPY line into fields, decoding escapes.
//
// PostgreSQL's text format escapes the characters that would otherwise break
// the line or column framing. Decoding has to happen after splitting on raw
// tabs, because an escaped \t inside a value is two characters at this stage
// and must not be treated as a separator.
func splitRow(line string, into []Field) []Field {
	into = into[:0]
	for _, raw := range strings.Split(line, "\t") {
		if raw == Null {
			into = append(into, Field{IsNull: true})
			continue
		}
		into = append(into, Field{Value: unescape(raw)})
	}
	return into
}

// unescape decodes the backslash escapes PostgreSQL emits in COPY text.
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		default:
			// Not a recognised escape: PostgreSQL emits the backslash
			// literally in that case, so preserve both characters.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
