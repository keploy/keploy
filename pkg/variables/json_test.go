package variables

import (
	"encoding/json"
	"testing"
)

// TestJSONUnitReadsAsEncodingJSONDecodes: jsonUnit reads the unit a string
// literal's text begins with as encoding/json decodes it, its length as
// written and as decoded. It reads none where encoding/json refuses the
// unit, or where it reads an invalid UTF-8 byte as U+FFFD (no byte of it is
// kept as written).
func TestJSONUnitReadsAsEncodingJSONDecodes(t *testing.T) {
	decode := func(text string) (string, bool) {
		var s string
		return s, json.Unmarshal([]byte(`"`+text+`"`), &s) == nil
	}
	for _, c := range []struct{ s, unit string }{
		{`\u0000 x`, `\u0000`},
		{`\uFFFF`, `\uFFFF`},
		{`\uffffx`, `\uffff`},
		{`\u00E9x`, `\u00E9`},
		{`\ud83d\ude00 x`, `\ud83d\ude00`},
		{`\uD83D\uDE00`, `\uD83D\uDE00`},
		{`\udbff\udfff`, `\udbff\udfff`},
		// A lone surrogate decodes to U+FFFD, as does a high surrogate
		// before an escape it pairs with none (that escape is the next
		// unit) or before a text that is no escape.
		{`\ud800 x`, `\ud800`},
		{`\ude00`, `\ude00`},
		{`\ud83d\u0041`, `\ud83d`},
		{`\ud83d\ud83d\ude00`, `\ud83d`},
		{`\ude00\ud83d`, `\ude00`},
		{`\ud83d\uZZZZ`, `\ud83d`},
		{`\/x`, `\/`},
		{`\"`, `\"`},
		{`\\`, `\\`},
		{"é x", "é"},
		// U+FFFD written as its three bytes is a character as any other.
		{"\xef\xbf\xbd x", "\xef\xbf\xbd"},
		{" x", " "},
		{"\x7f", "\x7f"},
	} {
		want, ok := decode(c.unit)
		if !ok {
			t.Fatalf("encoding/json refuses %q", c.unit)
		}
		if n, dec := jsonUnit(c.s); n != len(c.unit) || dec != len(want) {
			t.Errorf("jsonUnit(%q) = %d, %d; want %d, %d (%q decodes to %q)",
				c.s, n, dec, len(c.unit), len(want), c.unit, want)
		}
		// encoding/json splits the whole text there too.
		if whole, ok := decode(c.s); ok {
			if rest, _ := decode(c.s[len(c.unit):]); whole != want+rest {
				t.Errorf("encoding/json decodes %q to %q, not %q then %q",
					c.s, whole, want, rest)
			}
		}
	}
	// Malformed escapes and bytes a literal may not hold as written.
	for _, s := range []string{
		`\u12G4`, `\u004`, `\u`, `\x41`, `\U0041`, `\`, `"`, "\x00", "\x1f", "\n",
	} {
		if _, ok := decode(s); ok {
			t.Fatalf("encoding/json reads %q", s)
		}
		if n, dec := jsonUnit(s); n != 0 || dec != 0 {
			t.Errorf("jsonUnit(%q) = %d, %d; want 0, 0: encoding/json refuses it",
				s, n, dec)
		}
	}
	if got, ok := decode("\xff"); !ok || got != "\ufffd" {
		t.Fatalf("encoding/json reads %q as %q, %v", "\xff", got, ok)
	}
	if n, dec := jsonUnit("\xff x"); n != 0 || dec != 0 {
		t.Errorf("jsonUnit(%q) = %d, %d; want 0, 0: an invalid byte is kept by no unit",
			"\xff x", n, dec)
	}
	// Every byte after a backslash, and every byte as written: a unit
	// exactly where encoding/json reads one, and decoded as it decodes.
	for b := 0; b < 256; b++ {
		escape := string([]byte{'\\', byte(b)}) + "0041"
		written := string([]byte{byte(b)}) + "x"
		for _, s := range []string{escape, written} {
			n, dec := jsonUnit(s)
			whole, ok := decode(s)
			if s == written && b >= 0x80 {
				// A lone byte of no valid UTF-8: encoding/json reads
				// U+FFFD, and no byte of it is kept as written.
				if !ok || whole != "\xef\xbf\xbdx" {
					t.Errorf("encoding/json reads %q as %q, %v", s, whole, ok)
				}
				ok = false
			}
			if !ok {
				if n != 0 || dec != 0 {
					t.Errorf("jsonUnit(%q) = %d, %d; want 0, 0", s, n, dec)
				}
				continue
			}
			unit, uok := decode(s[:n])
			if n == 0 || !uok || dec != len(unit) || unit+s[n:] != whole {
				t.Errorf("jsonUnit(%q) = %d, %d; encoding/json decodes it to %q",
					s, n, dec, whole)
			}
		}
	}
}
