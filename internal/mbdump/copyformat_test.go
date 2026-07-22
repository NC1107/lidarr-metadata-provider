package mbdump

import "testing"

func TestSplitRowSeparatesNullFromEmpty(t *testing.T) {
	// The distinction matters downstream: a NULL disambiguation is absent
	// while an empty one is a real empty string, and the SkyHook contract
	// emits those differently.
	got := splitRow("a\t\\N\t\tb", nil)
	if len(got) != 4 {
		t.Fatalf("got %d fields, want 4", len(got))
	}
	if got[1].IsNull != true || got[1].Value != "" {
		t.Errorf("field 1 = %+v, want NULL", got[1])
	}
	if got[2].IsNull != false || got[2].Value != "" {
		t.Errorf("field 2 = %+v, want an empty string", got[2])
	}
	if got[2].Or("fallback") != "" {
		t.Error("Or must not substitute for an empty but non-NULL field")
	}
	if got[1].Or("fallback") != "fallback" {
		t.Error("Or must substitute for a NULL field")
	}
}

// An escaped tab inside a value must not split the row, which is why
// unescaping happens after splitting rather than before.
func TestSplitRowKeepsEscapedTabsInsideValues(t *testing.T) {
	got := splitRow(`one\ttwo`+"\t"+`three`, nil)
	if len(got) != 2 {
		t.Fatalf("got %d fields, want 2: %+v", len(got), got)
	}
	if got[0].Value != "one\ttwo" {
		t.Errorf("field 0 = %q, want %q", got[0].Value, "one\ttwo")
	}
}

func TestUnescape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`line\nbreak`, "line\nbreak"},
		{`tab\there`, "tab\there"},
		{`back\\slash`, `back\slash`},
		{`carriage\rreturn`, "carriage\rreturn"},
		// Not a recognised escape, so PostgreSQL emits it literally.
		{`\q`, `\q`},
		// A trailing backslash has nothing to escape.
		{`trailing\`, `trailing\`},
	}
	for _, c := range cases {
		if got := unescape(c.in); got != c.want {
			t.Errorf("unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitRowReusesBacking(t *testing.T) {
	buf := make([]Field, 0, 8)
	first := splitRow("a\tb\tc", buf)
	if len(first) != 3 {
		t.Fatalf("got %d fields", len(first))
	}
	second := splitRow("x\ty", buf)
	if len(second) != 2 {
		t.Fatalf("got %d fields on reuse", len(second))
	}
	if second[0].Value != "x" {
		t.Errorf("reused buffer produced %q", second[0].Value)
	}
}
