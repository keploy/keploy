package learn

import "strings"

// withoutPlaceholders is a Postgres statement's text with each parameter
// placeholder ($1, $2, ...) written "$". A placeholder is the position of a
// value the statement was sent with (a bind, or a constant the recorder
// lifted out of the text), not a value: the 4 of "c.relkind = $4" is no
// evidence that a 4 the app bound later flowed from that statement.
//
// Only a placeholder is rewritten. What the statement holds as text is read
// as before: a string constant ('..', E'..', U&'..', B'..', X'..'), a quoted
// name (".."), a dollar-quoted body ($$..$$, $fn$..$fn$, a function's own
// $1 among it), a comment (-- to the line's end, /* .. */ nested) and a name
// that holds a dollar sign (foo$1). The reading fails closed: what it cannot
// read for certain keeps every word it had. A string constant holding a
// backslash ends where standard_conforming_strings says, a setting the
// recording does not carry, so from such a constant on the text is read
// whole; so is the rest of a constant, name, body or comment never closed.
func withoutPlaceholders(sql string) string {
	if !strings.Contains(sql, "$") {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql))
	copied := 0
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '\'':
			end, ok := literalEnd(sql, i)
			if !ok {
				// Where the constant ends is not certain: so is nothing
				// after it.
				b.WriteString(sql[copied:])
				return b.String()
			}
			i = end
		case c == '"':
			i = quotedNameEnd(sql, i)
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(sql)
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = blockCommentEnd(sql, i)
		case c == '$' && i > 0 && pgIdentByte(sql[i-1]):
			// Inside a name (foo$1): a dollar sign continues it.
			i++
		case c == '$' && i+1 < len(sql) && isDigitByte(sql[i+1]):
			j := i + 1
			for j < len(sql) && isDigitByte(sql[j]) {
				j++
			}
			b.WriteString(sql[copied:i])
			b.WriteByte('$')
			copied, i = j, j
		case c == '$':
			i = dollarQuoteEnd(sql, i)
		default:
			i++
		}
	}
	b.WriteString(sql[copied:])
	return b.String()
}

// literalEnd is the index past the quote that closes the string constant
// opening at sql[i]. In an escape string (E'..') a backslash escapes the
// byte after it; in any other a doubled quote is one quote, and a backslash
// is false: it is an escape or a plain byte as standard_conforming_strings
// says. A U&'..' constant needs that setting on, so its backslashes (its
// Unicode escapes) are plain bytes. A constant never closed ends the text.
func literalEnd(sql string, i int) (int, bool) {
	escape := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i == 1 || !pgIdentByte(sql[i-2]))
	unicode := i > 1 && sql[i-1] == '&' && (sql[i-2] == 'U' || sql[i-2] == 'u') && (i == 2 || !pgIdentByte(sql[i-3]))
	for j := i + 1; j < len(sql); j++ {
		switch sql[j] {
		case '\\':
			if escape {
				j++
				continue
			}
			if !unicode {
				return 0, false
			}
		case '\'':
			if j+1 < len(sql) && sql[j+1] == '\'' {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return len(sql), true
}

// quotedNameEnd is the index past the quote that closes the quoted name
// opening at sql[i], a doubled quote inside it being one quote; the text's
// end for one never closed.
func quotedNameEnd(sql string, i int) int {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] != '"' {
			continue
		}
		if j+1 < len(sql) && sql[j+1] == '"' {
			j++
			continue
		}
		return j + 1
	}
	return len(sql)
}

// blockCommentEnd is the index past the "*/" that closes the comment opening
// at sql[i]; Postgres nests them. The text's end for one never closed.
func blockCommentEnd(sql string, i int) int {
	depth := 0
	for j := i; j+1 < len(sql); j++ {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j++
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(sql)
}

// dollarQuoteEnd is the index past the body that the dollar quote opening
// at sql[i] ($$ or $tag$) closes, the text's end for one never closed, and
// i+1 for a dollar sign that opens none.
func dollarQuoteEnd(sql string, i int) int {
	j := i + 1
	for j < len(sql) && (pgIdentByte(sql[j]) && sql[j] != '$') {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return i + 1
	}
	tag := sql[i : j+1]
	if k := strings.Index(sql[j+1:], tag); k >= 0 {
		return j + 1 + k + len(tag)
	}
	return len(sql)
}

// pgIdentByte reports whether c continues a Postgres name: a letter, a
// digit, '_', '$', or any byte of a character beyond ASCII.
func pgIdentByte(c byte) bool {
	return c == '_' || c == '$' || isDigitByte(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }
