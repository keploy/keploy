package replayer

import (
	"strings"
	"testing"

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
