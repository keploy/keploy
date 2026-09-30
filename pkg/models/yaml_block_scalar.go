package models

import (
	"strings"
	"unicode/utf8"
)

// YAMLBlockScalarUnsafe reports whether yaml.v3 (v3.0.1), left to choose how
// to write the string s, writes it as a literal block scalar that does not
// read back as s.
//
// It writes a string that holds a line break as a literal block (`|`). When
// the string's first character is whitespace or a line break, the block needs
// an indentation indicator: the emitter writes none for a first tab, which
// the parser then refuses ("found a tab character where an indentation space
// is expected"), and for a first space or line break it writes one that is
// right only at the top level. Nested in a mock (a sequence item, a mapping
// under one), the document does not load ("did not find expected key") or
// loads another string. Tab-indented SQL and JSON, and text that opens with a
// line break, are ordinary payloads. Such a string is written double-quoted
// instead, which yaml.v3 escapes and reads back exactly anywhere.
//
// A string that is not valid UTF-8 is written as base64 (!!binary), which
// never starts with whitespace, so it is never unsafe here.
func YAMLBlockScalarUnsafe(s string) bool {
	if s == "" || strings.IndexByte(s, '\n') < 0 {
		return false
	}
	switch s[0] {
	case ' ', '\t', '\n', '\r':
	default:
		// The other line breaks YAML knows: NEL, LINE SEPARATOR,
		// PARAGRAPH SEPARATOR.
		r, _ := utf8.DecodeRuneInString(s)
		if r != '\u0085' && r != '\u2028' && r != '\u2029' {
			return false
		}
	}
	return utf8.ValidString(s)
}
