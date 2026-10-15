package recorder

import (
	"context"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func queryRequest(q string) []mysql.Request {
	return []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: q},
	}}}
}

// Each MySQL command and statement class is stated the lifetime the founder's
// decision 24 gives it: the connection's state is session, a prepared
// statement's lifecycle is connection, and data is per-test.
func TestStatedLifetimeOfMySQLMocks(t *testing.T) {
	const (
		perTest    = models.MetadataLifetimePerTest
		session    = models.MetadataLifetimeSession
		connection = models.MetadataLifetimeConnection
	)
	for _, tc := range []struct {
		name     string
		mockType string
		op       string
		query    string
		want     string
	}{
		{"handshake", "config", "HandshakeV10", "", session},
		{"prepare", "connection", "COM_STMT_PREPARE", "", connection},
		{"close", "mocks", "COM_STMT_CLOSE", "", connection},
		{"reset", "mocks", "COM_STMT_RESET", "", connection},
		{"execute", "mocks", "COM_STMT_EXECUTE", "", perTest},
		{"fetch", "mocks", "0x1c", "", perTest},
		{"send long data", "mocks", "COM_STMT_SEND_LONG_DATA", "", perTest},
		{"ping", "mocks", "COM_PING", "", session},
		{"statistics", "mocks", "COM_STATISTICS", "", session},
		{"reset connection", "mocks", "COM_RESET_CONNECTION", "", session},
		{"init db", "mocks", "COM_INIT_DB", "", session},
		{"set option", "mocks", "0x1b", "", session},
		{"change user", "mocks", "COM_CHANGE_USER", "", session},
		{"quit", "mocks", "COM_QUIT", "", session},
		{"field list", "mocks", "COM_FIELD_LIST", "", perTest},

		{"SET NAMES", "mocks", "COM_QUERY", "SET NAMES utf8mb4", session},
		{"SET autocommit", "mocks", "COM_QUERY", "SET autocommit=0", session},
		{"SET isolation", "mocks", "COM_QUERY", "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED", session},
		{"SHOW VARIABLES", "mocks", "COM_QUERY", "SHOW VARIABLES LIKE 'lower_case_table_names'", session},
		{"SHOW SESSION VARIABLES", "mocks", "COM_QUERY", "SHOW SESSION VARIABLES WHERE Variable_name = 'sql_mode'", session},
		{"SHOW GLOBAL VARIABLES", "mocks", "COM_QUERY", "SHOW GLOBAL VARIABLES LIKE 'max_allowed_packet'", session},
		{"SHOW COLLATION", "mocks", "COM_QUERY", "SHOW COLLATION", session},
		{"SHOW CHARACTER SET", "mocks", "COM_QUERY", "SHOW CHARACTER SET", session},
		{"SHOW WARNINGS", "mocks", "COM_QUERY", "SHOW WARNINGS", perTest},
		{"SHOW STATUS", "mocks", "COM_QUERY", "SHOW STATUS LIKE 'Threads_connected'", perTest},
		{"SHOW TABLES", "mocks", "COM_QUERY", "SHOW TABLES", perTest},
		{"SHOW TABLE STATUS", "mocks", "COM_QUERY", "SHOW TABLE STATUS LIKE 'orders'", perTest},
		{"SHOW CREATE TABLE", "mocks", "COM_QUERY", "SHOW CREATE TABLE orders", perTest},
		{"USE", "mocks", "COM_QUERY", "USE shop", session},
		{"BEGIN", "mocks", "COM_QUERY", "BEGIN", session},
		{"START TRANSACTION", "mocks", "COM_QUERY", "START TRANSACTION", session},
		{"COMMIT", "mocks", "COM_QUERY", "COMMIT", session},
		{"ROLLBACK", "mocks", "COM_QUERY", "ROLLBACK", session},
		{"SAVEPOINT", "mocks", "COM_QUERY", "SAVEPOINT sp1", session},
		{"ROLLBACK TO", "mocks", "COM_QUERY", "ROLLBACK TO SAVEPOINT sp1", session},
		{"RELEASE", "mocks", "COM_QUERY", "RELEASE SAVEPOINT sp1", session},
		{"JDBC variables", "mocks", "COM_QUERY", "/* mysql-connector-j-8.4.0 */SELECT  @@session.auto_increment_increment AS auto_increment_increment, @@character_set_client AS character_set_client, @@max_allowed_packet AS max_allowed_packet", session},
		{"transaction read only", "mocks", "COM_QUERY", "select @@session.transaction_read_only", session},
		{"validation SELECT 1", "mocks", "COM_QUERY", "SELECT 1", session},
		{"validation FROM DUAL", "mocks", "COM_QUERY", "SELECT 1 FROM DUAL", session},
		{"literal string", "mocks", "COM_QUERY", "SELECT 'ok'", session},
		{"DATABASE()", "mocks", "COM_QUERY", "SELECT DATABASE()", session},
		{"SCHEMA()", "mocks", "COM_QUERY", "SELECT SCHEMA()", session},
		{"VERSION()", "mocks", "COM_QUERY", "SELECT VERSION()", session},
		{"USER()", "mocks", "COM_QUERY", "SELECT USER()", session},
		{"CURRENT_USER()", "mocks", "COM_QUERY", "SELECT CURRENT_USER()", session},
		{"CONNECTION_ID()", "mocks", "COM_QUERY", "SELECT CONNECTION_ID()", session},

		{"connection setup after a comment", "mocks", "COM_QUERY", "-- pool check\n/* jdbc */ SELECT @@tx_isolation", session},
		{"setup in an executable comment", "mocks", "COM_QUERY", "/*!40101 SET NAMES utf8mb4 */", session},
		{"data write in an executable comment", "mocks", "COM_QUERY", "/*!40101 DELETE FROM users */", perTest},
		{"table read past an executable comment", "mocks", "COM_QUERY", "/*!50000 SELECT 1 */ FROM users", perTest},
		{"@@identity", "mocks", "COM_QUERY", "SELECT @@identity", perTest},
		{"@@last_insert_id", "mocks", "COM_QUERY", "SELECT @@session.last_insert_id", perTest},
		{"@@insert_id", "mocks", "COM_QUERY", "SELECT @@insert_id", perTest},
		{"@@timestamp", "mocks", "COM_QUERY", "SELECT @@timestamp", perTest},
		{"@@rand_seed1", "mocks", "COM_QUERY", "SELECT @@rand_seed1", perTest},
		{"@@warning_count", "mocks", "COM_QUERY", "SELECT @@warning_count", perTest},
		{"@@error_count", "mocks", "COM_QUERY", "SELECT @@error_count", perTest},
		{"@@GLOBAL.gtid_executed", "mocks", "COM_QUERY", "SELECT @@GLOBAL.gtid_executed", perTest},
		{"setup variable beside a value one", "mocks", "COM_QUERY", "SELECT @@autocommit, @@warning_count", perTest},
		{"longer than any setup statement", "mocks", "COM_QUERY", "SET NAMES utf8mb4 /*" + strings.Repeat(" ", maxSessionStateQuery) + "*/", perTest},
		{"NOW()", "mocks", "COM_QUERY", "SELECT NOW()", perTest},
		{"UUID()", "mocks", "COM_QUERY", "SELECT UUID()", perTest},
		{"LAST_INSERT_ID()", "mocks", "COM_QUERY", "SELECT LAST_INSERT_ID()", perTest},
		{"RAND()", "mocks", "COM_QUERY", "SELECT RAND()", perTest},
		{"arithmetic", "mocks", "COM_QUERY", "SELECT 41 + 1", perTest},
		{"user variable", "mocks", "COM_QUERY", "SELECT @last_id", perTest},
		{"system variable beside data", "mocks", "COM_QUERY", "SELECT @@version, UUID()", perTest},
		{"table read", "mocks", "COM_QUERY", "SELECT name FROM users WHERE id = 1", perTest},
		{"SELECT 1 from a table", "mocks", "COM_QUERY", "SELECT 1 FROM users LIMIT 1", perTest},
		{"insert", "mocks", "COM_QUERY", "INSERT INTO users(name) VALUES ('a')", perTest},
		{"update", "mocks", "COM_QUERY", "UPDATE users SET name = 'b' WHERE id = 1", perTest},
		{"delete", "mocks", "COM_QUERY", "DELETE FROM users WHERE id = 1", perTest},
		{"DDL", "mocks", "COM_QUERY", "CREATE TABLE t (id INT)", perTest},
		{"two statements", "mocks", "COM_QUERY", "SET autocommit=0; SELECT name FROM users", perTest},
		{"unparseable", "mocks", "COM_QUERY", "SELEKT nothing", perTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reqs []mysql.Request
			if tc.op == "COM_QUERY" {
				reqs = queryRequest(tc.query)
			}
			if got := statedLifetime(tc.mockType, tc.op, reqs); got != tc.want {
				t.Fatalf("stated %q, want %q", got, tc.want)
			}
		})
	}
}

// Both recorders write the lifetime they state into the mock's metadata, where
// a replay reads it (models.MetadataLifetimeKey), beside the type tag they
// always wrote, and stamp the same lifetime in memory.
func TestRecordersWriteTheStatedLifetime(t *testing.T) {
	ts := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	resps := []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "OK"}}}}
	command := func(op string) []mysql.Request {
		return []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: op}}}}
	}
	check := func(t *testing.T, m *models.Mock, mockType, stated string, lifetime models.Lifetime) {
		t.Helper()
		if got := m.Spec.Metadata[models.MetadataLifetimeKey]; got != stated {
			t.Fatalf("metadata %s = %q, want %q", models.MetadataLifetimeKey, got, stated)
		}
		if got := m.Spec.Metadata["type"]; got != mockType {
			t.Fatalf("type tag %q, want %q as before", got, mockType)
		}
		if m.TestModeInfo.Lifetime != lifetime || !m.TestModeInfo.LifetimeDerived {
			t.Fatalf("in memory %v (derived %v), want %v", m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived, lifetime)
		}
	}

	// The legacy recorder delivers through the sync mock manager, which hands
	// on a config mock as it comes; the others wait for their test's window.
	t.Run("recorder", func(t *testing.T) {
		out := make(chan *models.Mock, 1)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(out)
		ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1"), mgr)
		recordMock(ctx, command("HandshakeResponse41"), resps, "config", "HandshakeResponse41", "OK", nil, ts, ts.Add(time.Millisecond), models.OutgoingOptions{})
		check(t, <-out, "config", models.MetadataLifetimeSession, models.LifetimeSession)
	})

	for _, tc := range []struct {
		name, mockType, op string
		reqs               []mysql.Request
		stated             string
		lifetime           models.Lifetime
	}{
		{"handshake", "config", "HandshakeResponse41", command("HandshakeResponse41"), models.MetadataLifetimeSession, models.LifetimeSession},
		{"data", "mocks", "COM_QUERY", queryRequest("SELECT name FROM users"), models.MetadataLifetimePerTest, models.LifetimePerTest},
		{"connection state", "mocks", "COM_QUERY", queryRequest("SET NAMES utf8mb4"), models.MetadataLifetimeSession, models.LifetimeSession},
		{"statement close", "mocks", "COM_STMT_CLOSE", command("COM_STMT_CLOSE"), models.MetadataLifetimeConnection, models.LifetimeConnection},
	} {
		t.Run("v2 recorder/"+tc.name, func(t *testing.T) {
			h := newV2Harness(t)
			emitMockV2(context.Background(), h.sess, tc.reqs, resps, tc.mockType, tc.op, "OK", ts, ts.Add(time.Millisecond))
			check(t, <-h.mocks, tc.mockType, tc.stated, tc.lifetime)
		})
	}
}

// sessionStateCandidate decides, without parsing, what can only be per-test:
// a statement whose first keyword after its comments opens none of the
// session classes, a SELECT from a table, a statement longer than any of
// them. Everything else is left to the parse.
func TestSessionStateCandidate(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"SET NAMES utf8mb4", true},
		{"  /* a */ /* b */ set autocommit=1", true},
		{"-- comment\nSHOW VARIABLES", true},
		{"# comment\nBEGIN", true},
		{"START TRANSACTION", true},
		{"SELECT @@version", true},
		{"SELECT 1 FROM DUAL", true},
		{"SELECT name FROM users", false},
		{"INSERT INTO t VALUES (1)", false},
		{"UPDATE t SET a = 1", false},
		{"/* unterminated", false},
		{"/*!40101 SET NAMES utf8 */", true},
		{"/*!40101 DELETE FROM users */ SELECT 1", false},
		{"/*! SELECT 1 */ FROM users", false},
		{"", false},
		{"SET x = 1" + strings.Repeat(" ", maxSessionStateQuery), false},
	} {
		if got := sessionStateCandidate(tc.query); got != tc.want {
			t.Errorf("sessionStateCandidate(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}
