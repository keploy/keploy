package models

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A minted value (keploy.yml's kind: minted) is one the application makes
// anew at its call, such as an id. Its form (MintedForm) is what every
// value seen at its rule's places shares: a literal prefix, and the classes
// of the bytes after it. A value made there anew has the length of the one
// recorded at its place, that prefix, and after it only bytes of those
// classes, wherever they stand: the form names what each byte may be, never
// which byte stands where, so an id of random letters keeps its form
// whichever letters, digits or symbols it draws.

// The classes of a minted value's bytes (MintedForm.Classes): a letter or a
// digit is one of these three; every other byte a minted value may hold
// (MintedClassOf) is a class of its own, written as that byte.
const (
	MintedClassLower = "lower"
	MintedClassUpper = "upper"
	MintedClassDigit = "digit"
)

// MintedForm is the form of a minted rule's values (see above): the prefix
// every value seen at the rule's places starts with (Prefix: their longest
// common start, cut back to end at a byte that is no letter or digit, so an
// id's random letters that start alike by chance are never a prefix), and
// the classes of every byte after it in any of them (Classes, sorted).
type MintedForm struct {
	Prefix  string   `json:"prefix,omitempty" yaml:"prefix,omitempty" bson:"prefix,omitempty"`
	Classes []string `json:"classes" yaml:"classes" bson:"classes"`
}

// MintedClassOf is the class of c (MintedForm.Classes), "" for a byte no
// minted value holds: a space, a control byte, or one outside ASCII.
func MintedClassOf(c byte) string {
	switch {
	case c >= 'a' && c <= 'z':
		return MintedClassLower
	case c >= 'A' && c <= 'Z':
		return MintedClassUpper
	case c >= '0' && c <= '9':
		return MintedClassDigit
	case c > ' ' && c < 0x7f:
		return string(c)
	}
	return ""
}

// isMintedClass reports whether s names a class: the class of some byte
// a minted value holds ('!' to '~', MintedClassOf).
func isMintedClass(s string) bool {
	for c := byte('!'); c <= '~'; c++ {
		if MintedClassOf(c) == s {
			return true
		}
	}
	return false
}

// MintedFormOf is the form of values, the values seen at one minted rule's
// places (see MintedForm); ok is false where there are none, where one
// holds a byte no minted value holds (MintedClassOf), or where one holds no
// letter or digit after the prefix (no value an application mints).
func MintedFormOf(values ...string) (MintedForm, bool) {
	// The first value is all prefix until the others cut it.
	f := MintedForm{Prefix: strings.Join(values[:min(len(values), 1)], "")}.Cut(values...)
	classes := map[string]bool{}
	for _, v := range values {
		rest := v[len(f.Prefix):]
		if !strings.ContainsFunc(rest, isLetterOrDigit) {
			return MintedForm{}, false
		}
		for i := 0; i < len(rest); i++ {
			c := MintedClassOf(rest[i])
			if c == "" {
				return MintedForm{}, false
			}
			classes[c] = true
		}
	}
	if len(classes) == 0 {
		// No value.
		return MintedForm{}, false
	}
	// Every value's bytes after the prefix, its first value's cut off
	// among them: the classes are theirs.
	f.Classes = f.Classes[:0]
	for c := range classes {
		f.Classes = append(f.Classes, c)
	}
	slices.Sort(f.Classes)
	return f, true
}

// Cut is f with its prefix cut to what values, values seen at its places
// too, share with it, back to its last byte that is no letter or digit:
// the bytes of f's prefix that now stand after it are of its classes too.
// A prefix one value lacks is no literal part of the rule's values (one
// recorded value alone is all prefix until another is seen).
func (f MintedForm) Cut(values ...string) MintedForm {
	prefix := f.Prefix
	for _, v := range values {
		n := 0
		for n < len(prefix) && n < len(v) && prefix[n] == v[n] {
			n++
		}
		prefix = prefix[:n]
	}
	prefix = prefix[:strings.LastIndexFunc(prefix, func(r rune) bool { return !isLetterOrDigit(r) })+1]
	out := MintedForm{Prefix: prefix, Classes: slices.Clone(f.Classes)}
	for i := len(prefix); i < len(f.Prefix); i++ {
		if c := MintedClassOf(f.Prefix[i]); !slices.Contains(out.Classes, c) {
			out.Classes = append(out.Classes, c)
		}
	}
	slices.Sort(out.Classes)
	return out
}

// isLetterOrDigit reports whether r is an ASCII letter or digit.
func isLetterOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// Keeps reports whether v, a value made anew at a place whose value was
// recorded as rec, keeps f there: rec's length, f's prefix, and after it
// only bytes of f's classes.
func (f MintedForm) Keeps(rec, v string) bool {
	if len(v) != len(rec) || !strings.HasPrefix(v, f.Prefix) {
		return false
	}
	for i := len(f.Prefix); i < len(v); i++ {
		// A byte no minted value holds has no class (""), which no form
		// names (Check).
		if !slices.Contains(f.Classes, MintedClassOf(v[i])) {
			return false
		}
	}
	return true
}

// Check is why f is no form keploy can check a minted value by, nil when it
// is one: it names at least one class, each a class keploy knows
// (MintedClassOf), and its prefix ends at a byte that is no letter or digit
// and holds only bytes a minted value holds.
func (f MintedForm) Check() error {
	if len(f.Classes) == 0 {
		return errors.New("a minted form names no class of byte")
	}
	for _, c := range f.Classes {
		if !isMintedClass(c) {
			return fmt.Errorf("a minted form names a class keploy does not know (%q)", c)
		}
	}
	if p := f.Prefix; p != "" && isLetterOrDigit(rune(p[len(p)-1])) {
		return fmt.Errorf("a minted form's prefix %q ends at a letter or digit", p)
	}
	for i := 0; i < len(f.Prefix); i++ {
		if MintedClassOf(f.Prefix[i]) == "" {
			return fmt.Errorf("a minted form's prefix %q holds a byte no minted value holds", f.Prefix)
		}
	}
	return nil
}

// Where is f as a line names it: its prefix, then its classes.
func (f MintedForm) Where() string {
	s := "only bytes of the classes " + strings.Join(f.Classes, " ")
	if f.Prefix != "" {
		return fmt.Sprintf("the prefix %q, then %s", f.Prefix, s)
	}
	return s
}
