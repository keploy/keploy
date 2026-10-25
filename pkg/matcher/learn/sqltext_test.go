package learn

import "testing"

// A placeholder's number is a parameter's position, so it is read "$"; what
// the statement holds as text (a constant, a quoted name, a dollar-quoted
// body, a comment, a name with a dollar sign in it) keeps its words, and so
// does all that follows what the scan cannot read for certain.
func TestWithoutPlaceholdersReadsOnlyAPlaceholdersNumberAsSyntax(t *testing.T) {
	for _, row := range []struct{ name, sql, want string }{
		{"no dollar sign", "SELECT 1", "SELECT 1"},
		{"placeholders", "WHERE n.nspname = $1 AND c.relkind = $4", "WHERE n.nspname = $ AND c.relkind = $"},
		{"a placeholder of more digits, beside punctuation", "VALUES ($1,$12)", "VALUES ($,$)"},
		{"a placeholder cast", "word <> ALL ($2::text[])", "word <> ALL ($::text[])"},
		{"a placeholder ends the text", "LIMIT $3", "LIMIT $"},
		{"a constant's text", "relkind = '$4' AND x = $4", "relkind = '$4' AND x = $"},
		{"a doubled quote in a constant", "x = 'it''s $4' AND y = $4", "x = 'it''s $4' AND y = $"},
		{"an escape string's escaped quote", `x = E'it\'s $4' AND y = $4`, `x = E'it\'s $4' AND y = $`},
		{"an escape string's escaped backslash", `x = e'\\' AND y = $4`, `x = e'\\' AND y = $`},
		{"a name ending in e is no escape string", `CASE WHEN a THEN 'x' ELSE'$4' END`, `CASE WHEN a THEN 'x' ELSE'$4' END`},
		{"a Unicode constant's backslash", `x = U&'\0024' AND y = $4`, `x = U&'\0024' AND y = $`},
		{"a standard constant's backslash: the rest is read whole", `x = 'a\' AND y = $4 AND z = $5`, `x = 'a\' AND y = $4 AND z = $5`},
		{"a constant never closed", "x = $1 AND y = 'a $4", "x = $ AND y = 'a $4"},
		{"a bit string", "x = B'0101' AND y = $4", "x = B'0101' AND y = $"},
		{"a quoted name", `SELECT "col$4", "a""$4" FROM t WHERE x = $4`, `SELECT "col$4", "a""$4" FROM t WHERE x = $`},
		{"a quoted name never closed", `SELECT $1, "col $4`, `SELECT $, "col $4`},
		{"a name with a dollar sign", "SELECT foo$4, a$b FROM t WHERE x = $4", "SELECT foo$4, a$b FROM t WHERE x = $"},
		{"a dollar-quoted body", "AS $$ SELECT $4 $$ LANGUAGE sql; SELECT $4", "AS $$ SELECT $4 $$ LANGUAGE sql; SELECT $"},
		{"a tagged body holding $$", "AS $fn$ SELECT $$x$$, $4 $fn$, $4", "AS $fn$ SELECT $$x$$, $4 $fn$, $"},
		{"a body never closed", "SELECT $1 AS $q$ $4", "SELECT $ AS $q$ $4"},
		{"a dollar sign that opens nothing", "SELECT $ $abc + $4", "SELECT $ $abc + $"},
		{"a line comment", "-- step $4\nSELECT $4", "-- step $4\nSELECT $"},
		{"a line comment ends the text", "SELECT $1 -- $4", "SELECT $ -- $4"},
		{"a nested block comment", "/* a /* $4 */ $4 */ SELECT $4", "/* a /* $4 */ $4 */ SELECT $"},
		{"a block comment never closed", "SELECT $1 /* $4", "SELECT $ /* $4"},
		{"a comment's marker inside a constant", "x = '--' AND y = $4", "x = '--' AND y = $"},
		{"beyond ASCII", "SELECT 'ä' AS ä$1, $2", "SELECT 'ä' AS ä$1, $"},
	} {
		if got := withoutPlaceholders(row.sql); got != row.want {
			t.Errorf("%s: withoutPlaceholders(%q) = %q, want %q", row.name, row.sql, got, row.want)
		}
	}
}
