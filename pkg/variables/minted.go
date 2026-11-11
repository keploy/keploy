package variables

import (
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
