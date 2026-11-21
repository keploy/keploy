package replayer

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// A log line shows a statement by its shape: its keywords, names and
// operators, never a literal or a comment's text, whichever way the
// literal is written.
func TestSQLLogShapeShowsNoLiteral(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct{ sql, want string }{
		{"SELECT token FROM sessions WHERE token = '" + s + "'", "SELECT token FROM sessions WHERE token = ?"},
		{`INSERT INTO users (email, pw) VALUES ("` + s + `", 'a\'b')`, "INSERT INTO users (email, pw) VALUES (?, ?..."},
		{"CREATE USER 'app'@'%' IDENTIFIED BY '" + s + "'", "CREATE USER ?@? IDENTIFIED BY ?"},
		{"SELECT * FROM `orders` WHERE id = 42 AND total > 1.5e3 AND flags = 0xDEAD", "SELECT * FROM `orders` WHERE id = ? AND total > ? AND flags = ?"},
		{"/* tenant=" + s + " */ SELECT 1", "/*?*/ SELECT ?"},
		{"SELECT 1 -- " + s + "\nFROM dual # " + s, "SELECT ? -- ?\nFROM dual -- ?"},
		{"SELECT X'" + s + "', b'0101'", "SELECT X?, b?"},
		{"SELECT * FROM utf8mb4_t WHERE c = 7" + s, "SELECT * FROM utf8mb4_t WHERE c = ?"},
		{"SELECT '" + s, "SELECT ?"},
		{"SELECT * FROM `users WHERE token = '" + s + "'", "SELECT * FROM `?"},
		{`SELECT * FROM files WHERE dir = 'C:\' OR token = '` + s + `'`, "SELECT * FROM files WHERE dir = ?..."},
	} {
		if got := sqlLogShape(tc.sql); got != tc.want {
			t.Errorf("sqlLogShape = %q, want %q", got, tc.want)
		}
	}
	long := "SELECT " + strings.Repeat("a, ", 200) + "'" + s + "'"
	if got := sqlLogShape(long); len(got) > maxLoggedSQL+3 || strings.Contains(got, s) {
		t.Errorf("a long statement is shown as %d bytes; want at most %d, no literal", len(got), maxLoggedSQL+3)
	}
}

// A name with non-ASCII characters at the maxLoggedSQL limit is never cut
// inside one: the shape either stops after a whole character or moves its
// cut back to one's first byte. The name starts 1 to 3 bytes before the
// limit, so the shape stops exactly at it or writes a character past it.
func TestSQLLogShapeNeverSplitsACharacter(t *testing.T) {
	const head = "SELECT "
	for name, ident := range map[string]string{
		"hindi (3-byte characters)":   strings.Repeat("ग्राहक", 4),
		"accents (2-byte characters)": strings.Repeat("é", 20),
	} {
		for back := 1; back <= 3; back++ {
			t.Run(fmt.Sprintf("%s, %d bytes back", name, back), func(t *testing.T) {
				pad := maxLoggedSQL - back - len(head)
				require.Positive(t, pad)
				sql := head + strings.Repeat("a", pad) + ident + " FROM customers"
				got := sqlLogShape(sql)
				require.Truef(t, utf8.ValidString(got), "the shape splits a character: %q", got)
				require.Truef(t, strings.HasSuffix(got, "..."), "the shape does not say it was cut: %q", got)
				kept := strings.TrimSuffix(got, "...")
				require.Truef(t, strings.HasPrefix(sql, kept), "the shape is not the start of the statement: %q", kept)
				require.LessOrEqual(t, len(kept), maxLoggedSQL)
				require.GreaterOrEqual(t, len(kept), maxLoggedSQL-3)
			})
		}
	}
}

// A live command is shown by its statement's shape and how many values it
// bound, never the values.
func TestLiveCommandShapeShowsNoBoundValue(t *testing.T) {
	const s = logleak.Canary
	dctx := &wire.DecodeContext{StmtIDToQuery: map[uint32]string{3: "SELECT name FROM users WHERE api_key = ? AND org = 'acme'"}}
	exec := &mysql.StmtExecutePacket{StatementID: 3, Parameters: []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: s}}}
	for _, tc := range []struct {
		msg  any
		want string
	}{
		{&mysql.QueryPacket{Query: "DELETE FROM sessions WHERE token = '" + s + "'"}, "DELETE FROM sessions WHERE token = ?"},
		{&mysql.StmtPreparePacket{Query: "UPDATE users SET pw = '" + s + "' WHERE id = ?"}, "UPDATE users SET pw = ? WHERE id = ?"},
		{exec, "SELECT name FROM users WHERE api_key = ? AND org = ? (1 bound values)"},
		{&mysql.StmtSendLongDataPacket{ParameterID: 0, Data: []byte(s)}, "(param 0, 21 streamed bytes)"},
	} {
		if got := liveCommandShape(tc.msg, dctx); got != tc.want {
			t.Errorf("liveCommandShape = %q, want %q", got, tc.want)
		}
	}
}
