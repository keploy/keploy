package variables

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// A minted value (keploy.yml's kind: minted) is one the application makes
// anew at its call, such as an id, and it keeps its shape from run to run:
// its length, and at each position a letter of its alphabet where it was
// recorded with one, else the byte it was recorded with (a UUID's '-'). Its
// alphabet is the narrowest that holds every letter and digit of each run's
// value and keeps that shape between the runs: the digits, the hex digits
// (a UUID's, whichever of its positions hold digits by chance), the letters
// and digits, then standard base64's ('+' and '/' as letters) or URL-safe
// base64's ('-' and '_'), whose letters move from run to run.

// Alphabet is the letters a minted value is made of.
type Alphabet uint8

const (
	// AlphabetDigits is 0-9.
	AlphabetDigits Alphabet = iota + 1
	// AlphabetHex is 0-9, a-f and A-F.
	AlphabetHex
	// AlphabetAlnum is 0-9, a-z and A-Z.
	AlphabetAlnum
	// AlphabetBase64 is AlphabetAlnum, '+' and '/'.
	AlphabetBase64
	// AlphabetBase64URL is AlphabetAlnum, '-' and '_'.
	AlphabetBase64URL
)

// MintedAlphabet is the alphabet of rec and others, one place's minted
// value in different runs (see above); ok is false where none keeps rec's
// shape in every one of others (Keeps), or where rec holds no letter or
// digit (no value an application mints).
func MintedAlphabet(rec string, others ...string) (Alphabet, bool) {
	if !strings.ContainsFunc(rec, func(c rune) bool { return c < utf8.RuneSelf && AlphabetAlnum.holds(byte(c)) }) {
		return 0, false
	}
	vs := append([]string{rec}, others...)
	for a := AlphabetDigits; a <= AlphabetBase64URL; a++ {
		if !slices.ContainsFunc(vs, a.lacksALetter) && !slices.ContainsFunc(others, func(v string) bool { return !a.Keeps(rec, v) }) {
			return a, true
		}
	}
	return 0, false
}

// Keeps reports whether v keeps rec's shape in a: its length, and at each
// position a byte of a where rec holds one, else rec's byte.
func (a Alphabet) Keeps(rec, v string) bool {
	if len(v) != len(rec) {
		return false
	}
	for i := 0; i < len(rec); i++ {
		if a.holds(rec[i]) != a.holds(v[i]) || !a.holds(rec[i]) && v[i] != rec[i] {
			return false
		}
	}
	return true
}

// lacksALetter reports whether v holds a letter or digit a does not hold.
func (a Alphabet) lacksALetter(v string) bool {
	for i := 0; i < len(v); i++ {
		if AlphabetAlnum.holds(v[i]) && !a.holds(v[i]) {
			return true
		}
	}
	return false
}

// holds reports whether c is a letter of a.
func (a Alphabet) holds(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return a >= AlphabetDigits
	case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		return a >= AlphabetHex
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return a >= AlphabetAlnum
	case c == '+', c == '/':
		return a == AlphabetBase64
	case c == '-', c == '_':
		return a == AlphabetBase64URL
	}
	return false
}

// String is a's name in a mock (models.MintedShape.Alphabet), "" for no
// alphabet keploy knows.
func (a Alphabet) String() string {
	switch a {
	case AlphabetDigits:
		return "digits"
	case AlphabetHex:
		return "hex"
	case AlphabetAlnum:
		return "alnum"
	case AlphabetBase64:
		return "base64"
	case AlphabetBase64URL:
		return "base64url"
	}
	return ""
}

// ParseAlphabet is the alphabet named s (Alphabet.String); ok is false for
// a name keploy does not know.
func ParseAlphabet(s string) (Alphabet, bool) {
	for a := AlphabetDigits; a <= AlphabetBase64URL; a++ {
		if a.String() == s {
			return a, true
		}
	}
	return 0, false
}

// MintedWhy is why b, the binding a call makes of a minted variable
// (Binding.Minted: an id keploy.yml's rule declares the application makes
// anew at that call), may not be bound: "" where it may, or where b is no
// minted variable's. The value must not be its recorded one (an id the
// application did not make anew), must keep the recorded one's shape in
// its rule's alphabet (Alphabet.Keeps; an alphabet keploy cannot read
// fails closed), and must not be the value of any of earlier that is a
// minted variable's binding (one the run bound before at a minted place,
// or one the call binds before b): an id made anew is never one the run
// had already. Each text leads with what the replay's miss says: "minted
// value reused" or "minted value of another shape".
func MintedWhy(b Binding, earlier ...Binding) string {
	if b.Minted == nil {
		return ""
	}
	rule := "keploy.yml's rule " + b.Minted.Rule + " declares variable " + b.Name + " made anew at " + b.Place + " of " + b.Mock
	if b.Value == b.Recorded {
		return fmt.Sprintf("minted value reused: %s, and this call sends the value it was recorded with there, %q", rule, b.Value)
	}
	for _, e := range earlier {
		if e.Minted != nil && e.Value == b.Value {
			return fmt.Sprintf("minted value reused: %s, and this call sends %q there, the value variable %s was bound to earlier in this run, at %s of %s", rule, b.Value, e.Name, e.Place, e.Mock)
		}
	}
	a, ok := ParseAlphabet(b.Minted.Alphabet)
	if !ok {
		return fmt.Sprintf("minted value of another shape: %s, in an alphabet keploy does not know (%q), so it cannot check the value this call sends there", rule, b.Minted.Alphabet)
	}
	if !a.Keeps(b.Recorded, b.Value) {
		return fmt.Sprintf("minted value of another shape: %s, and this call sends %q there, which does not keep the shape of the recorded %q (its length, and at each position a letter of the alphabet %s where the recorded value holds one, else the recorded byte)", rule, b.Value, b.Recorded, a)
	}
	return ""
}
