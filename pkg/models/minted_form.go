package models

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A minted value (keploy.yml's kind: minted) is one the application makes
// anew at its call, such as an id. Its form (MintedForm) is what every
// value seen at its rule's places shares: a literal prefix, and a length in
// the range of theirs. A value made there anew starts with that prefix, has
// such a length, and holds only printable characters and no whitespace.
// The form names no character after the prefix: a few recorded values
// cannot show every character a generator draws (one id of URL-safe base64
// may hold no '-'), so a fresh id is never refused for a character the
// recordings did not show. A value reused, or of another length or prefix,
// is still refused.

// MintedForm is the form of a minted rule's values (see above): the prefix
// every value seen at the rule's places starts with (Prefix: their longest
// common start, cut back to end at an ASCII byte that is no letter or
// digit, so an id's random letters that start alike by chance are never a
// prefix), and the shortest and longest of their lengths in bytes (MinLen,
// MaxLen: one length for a generator of one length).
type MintedForm struct {
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty" bson:"prefix,omitempty"`
	MinLen int    `json:"min_len" yaml:"min_len" bson:"min_len"`
	MaxLen int    `json:"max_len" yaml:"max_len" bson:"max_len"`
}

// MintedFormOf is the form of values, the values seen at one minted rule's
// places (see MintedForm); ok is false where there are none, or where they
// keep no form keploy can check (MintedForm.Check, MintedForm.Keeps: a
// value with nothing after the prefix, or a character that is no printable
// one or is whitespace).
func MintedFormOf(values ...string) (MintedForm, bool) {
	// The first value is all prefix until the others cut it; with none, the
	// form is the empty one, which Check refuses.
	first := strings.Join(values[:min(len(values), 1)], "")
	f := MintedForm{Prefix: first, MinLen: len(first), MaxLen: len(first)}.With(values...)
	if f.Check() != nil {
		return MintedForm{}, false
	}
	for _, v := range values {
		if !f.Keeps(v) {
			return MintedForm{}, false
		}
	}
	return f, true
}

// With is f with values, more values seen at its places: its prefix cut to
// what they share with it, back to its last ASCII byte that is no letter or
// digit, and its lengths widened to hold theirs. A prefix one value lacks
// is no literal part of the rule's values (one recorded value alone is all
// prefix until another is seen).
func (f MintedForm) With(values ...string) MintedForm {
	prefix := f.Prefix
	for _, v := range values {
		n := 0
		for n < min(len(prefix), len(v)) && prefix[n] == v[n] {
			n++
		}
		prefix = prefix[:n]
		f.MinLen, f.MaxLen = min(f.MinLen, len(v)), max(f.MaxLen, len(v))
	}
	f.Prefix = prefix[:strings.LastIndexFunc(prefix, isMintedSeparator)+1]
	return f
}

// isMintedSeparator reports whether r ends a minted form's prefix: an
// ASCII character that is no letter or digit.
func isMintedSeparator(r rune) bool {
	return r < utf8.RuneSelf && !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
}

// mintable reports whether s holds only what a minted value may: UTF-8
// whose every character is printable and no whitespace.
func mintable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r == ' ' || !unicode.IsPrint(r) })
}

// Keeps reports whether v, a value made anew at one of f's places, keeps
// f: its prefix, a length from MinLen to MaxLen, and after the prefix only
// printable characters and no whitespace.
func (f MintedForm) Keeps(v string) bool {
	return len(v) >= f.MinLen && len(v) <= f.MaxLen && strings.HasPrefix(v, f.Prefix) && mintable(v[len(f.Prefix):])
}

// Check is why f is no form keploy can check a minted value by, nil when it
// is one: its lengths leave a byte after its prefix and run from the
// shortest to the longest, and its prefix ends at an ASCII byte that is no
// letter or digit and holds only what a minted value may.
func (f MintedForm) Check() error {
	if f.MinLen <= len(f.Prefix) {
		return fmt.Errorf("a minted form's shortest length (%d) leaves nothing after its prefix %q", f.MinLen, f.Prefix)
	}
	if f.MaxLen < f.MinLen {
		return errors.New("a minted form's longest length is shorter than its shortest")
	}
	if p := f.Prefix; p != "" && !isMintedSeparator(rune(p[len(p)-1])) {
		return fmt.Errorf("a minted form's prefix %q ends at no ASCII separator", p)
	}
	if !mintable(f.Prefix) {
		return fmt.Errorf("a minted form's prefix %q holds whitespace or an unprintable character", f.Prefix)
	}
	return nil
}

// Where is f as a line names it: its prefix, then its lengths.
func (f MintedForm) Where() string {
	n := fmt.Sprintf("a length of %d", f.MinLen)
	if f.MaxLen != f.MinLen {
		n = fmt.Sprintf("a length from %d to %d", f.MinLen, f.MaxLen)
	}
	s := n + ", and only printable characters and no whitespace"
	if f.Prefix != "" {
		return fmt.Sprintf("the prefix %q, %s", f.Prefix, s)
	}
	return s
}
