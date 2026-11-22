package learn

import (
	"strconv"
	"strings"
)

// pgMarkersBlanked is sql, a Postgres statement's text as its recorder keeps
// it (models.PostgresV3QuerySpec.SQLNormalized), with the digits of each
// parameter marker $k blanked (payloadSpec), for k from 1 to n: the number
// of values the statement carries, its binds and the constants its text was
// normalized of (BindValues, Literals), so that each blanked marker names
// one of them. A marker is syntax the driver (or the normalizer) wrote to
// say where a value goes, never a value: the "1" of "$1" is no word of data
// an application could copy, and searched as one it ties every statement
// with a first parameter to every value holding a "1".
//
// Only a marker that stands as a token of the statement is blanked. Text
// inside a quoted string ('...', E'...'), a quoted identifier ("..."), a
// dollar-quoted string ($tag$...$tag$) or a comment stays as it is, as does
// a "$" inside an identifier (a$1), a "$" and digits run on into a word
// ($1a), and a marker whose number names no value the statement carries:
// each stays a word, the fail-closed side. A text whose quote or comment is
// not closed is read as quoted to its end.
func pgMarkersBlanked(sql string, n int) string {
	if n <= 0 || !strings.Contains(sql, "$") {
		return sql
	}
	var b []byte
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '\'':
			// E'...' reads a backslash as an escape; E stands alone before it.
			esc := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !pgIdentChar(sql[i-2]))
			i = pgQuoteEnd(sql, i+1, '\'', esc)
		case c == '"':
			i = pgQuoteEnd(sql, i+1, '"', false)
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(sql)
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = pgCommentEnd(sql, i+2)
		case c == '$':
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			switch {
			case j == i+1:
				// No digits: a dollar quote's opening tag, or a lone "$".
				if end, ok := pgDollarTag(sql, i); ok {
					tag := sql[i:end]
					if k := strings.Index(sql[end:], tag); k >= 0 {
						i = end + k + len(tag)
					} else {
						i = len(sql)
					}
				} else {
					i++
				}
				continue
			case j < len(sql) && pgIdentChar(sql[j]):
				// Digits run on into a word: no marker.
				for j < len(sql) && pgIdentChar(sql[j]) {
					j++
				}
				i = j
				continue
			}
			if k, err := strconv.Atoi(sql[i+1 : j]); err == nil && k >= 1 && k <= n {
				if b == nil {
					b = []byte(sql)
				}
				for d := i + 1; d < j; d++ {
					b[d] = ' '
				}
			}
			i = j
		case pgIdentChar(c):
			// A word (keyword, identifier or number): its "$" are its own.
			for i < len(sql) && pgIdentChar(sql[i]) {
				i++
			}
		default:
			i++
		}
	}
	if b == nil {
		return sql
	}
	return string(b)
}

// pgIdentChar reports whether c may stand inside a Postgres identifier (a
// letter, a digit, "_", "$", or a byte of a multi-byte character).
func pgIdentChar(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// pgQuoteEnd is where the quoted text of sql opened before from ends, just
// past its closing quote q: a doubled quote stands for one, and, where esc
// (an E'...' string), a backslash escapes the byte after it. len(sql) where
// it is not closed.
func pgQuoteEnd(sql string, from int, q byte, esc bool) int {
	for i := from; i < len(sql); i++ {
		switch {
		case esc && sql[i] == '\\':
			i++
		case sql[i] == q:
			if i+1 < len(sql) && sql[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}

// pgCommentEnd is where the block comment of sql opened before from ends,
// just past its closing "*/", nested comments read as Postgres reads them.
// len(sql) where it is not closed.
func pgCommentEnd(sql string, from int) int {
	depth := 1
	for i := from; i+1 < len(sql); i++ {
		switch {
		case sql[i] == '/' && sql[i+1] == '*':
			depth++
			i++
		case sql[i] == '*' && sql[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(sql)
}

// pgDollarTag reports whether a dollar quote's opening tag starts at sql[i]
// ("$$", or "$tag$" with tag an identifier that holds no "$"), and where it
// ends, just past its second "$". Its caller has read no digit after the
// first "$" (a digit there makes a marker), so a tag here starts with none.
func pgDollarTag(sql string, i int) (end int, ok bool) {
	for j := i + 1; j < len(sql); j++ {
		switch c := sql[j]; {
		case c == '$':
			return j + 1, true
		case !pgIdentChar(c):
			return 0, false
		}
	}
	return 0, false
}
