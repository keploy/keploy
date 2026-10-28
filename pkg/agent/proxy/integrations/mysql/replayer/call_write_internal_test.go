package replayer

import (
	"errors"
	"io"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// commandMock is a recording of one command: a COM_QUERY of sql, or, with
// execute, a COM_STMT_EXECUTE whose recording kept sql as its query ("" for
// none, as an older keploy's).
func commandMock(name, sql string, execute bool) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	b := mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1)}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: sql},
	}
	if execute {
		b = mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 16}, Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 1, IterationCount: 1},
		}
	}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: b}}
	if execute && sql != "" {
		m.Spec.MySQLRequests[0].SetStoredQuery(sql)
	}
	return m
}

// A MySQL recording's command writes when its statement's first keyword
// (inert comments aside) changes what the server holds, and says which
// kind of statement it is, never a table. A COM_QUERY's statement is its
// text, an EXECUTE's the query its recording kept. A read, a statement
// keploy cannot tell, an EXECUTE whose recording kept no query, another
// command and a recording of more than one request write nothing.
func TestAStatementThatChangesWhatTheServerHoldsIsAWrite(t *testing.T) {
	writes := map[string]string{
		"INSERT INTO t VALUES (1)":                     "MySQL INSERT",
		"insert ignore into t values (1)":              "MySQL INSERT",
		"/* app=api,route=/x */ UPDATE t SET a = 1":    "MySQL UPDATE",
		"UPDATE`t`SET a = 1":                           "MySQL UPDATE",
		"DELETE FROM t WHERE id = 1":                   "MySQL DELETE",
		"REPLACE INTO t VALUES (1)":                    "MySQL REPLACE",
		"LOAD DATA LOCAL INFILE 'x' INTO TABLE t":      "MySQL LOAD DATA",
		"load  xml infile 'x' into table t":            "MySQL LOAD XML",
		"CREATE INDEX i ON t (a)":                      "MySQL CREATE",
		"ALTER TABLE t ADD COLUMN b INT":               "MySQL ALTER",
		"DROP TABLE t":                                 "MySQL DROP",
		"TRUNCATE TABLE t":                             "MySQL TRUNCATE",
		"RENAME TABLE a TO b":                          "MySQL RENAME",
		"GRANT SELECT ON db.* TO 'u'@'%'":              "MySQL GRANT",
		"REVOKE SELECT ON db.* FROM 'u'@'%'":           "MySQL REVOKE",
		"DELETE":                                       "MySQL DELETE",
		"UPDATE t SET a = 1; SELECT ROW_COUNT()":       "MySQL UPDATE",
		"\n\tINSERT INTO t VALUES (1)":                 "MySQL INSERT",
		"-- the sweep\nDELETE FROM leases WHERE x < 1": "MySQL DELETE",
	}
	for sql, want := range writes {
		for _, execute := range []bool{false, true} {
			if got, ok := models.CallWriteOf(commandMock("m", sql, execute)); !ok || got != want {
				t.Errorf("%q (execute %v): write %q, %v; want %q", sql, execute, got, ok, want)
			}
		}
	}

	reads := []string{
		"SELECT * FROM t FOR UPDATE",
		"SHOW TABLES",
		"SET NAMES utf8mb4",
		"BEGIN",
		"COMMIT",
		"CALL sweep()",
		"WITH c AS (SELECT 1) UPDATE t SET a = 1",
		"/*!40101 SET character_set_client = utf8 */",
		"LOAD INDEX INTO CACHE t",
		"LOAD",
		"UPDATES t",
		"UPDATE_COUNTS(1)",
		"DELETE$OLD",
		"INSERT2 t",
		"SELECT 1; DELETE FROM t",
		"(SELECT 1)",
		"",
	}
	for _, sql := range reads {
		for _, execute := range []bool{false, true} {
			if got, ok := models.CallWriteOf(commandMock("m", sql, execute)); ok || got != "" {
				t.Errorf("%q (execute %v): write %q, %v; want none", sql, execute, got, ok)
			}
		}
	}

	prepare := commandMock("prep", "", false)
	prepare.Spec.MySQLRequests[0].Header.Type, prepare.Spec.MySQLRequests[0].Message = "COM_STMT_PREPARE", &mysql.StmtPreparePacket{Query: "INSERT INTO t VALUES (?)"}
	two := commandMock("two", "INSERT INTO t VALUES (1)", false)
	two.Spec.MySQLRequests = append(two.Spec.MySQLRequests, two.Spec.MySQLRequests[0])
	none := commandMock("none", "INSERT INTO t VALUES (1)", false)
	none.Spec.MySQLRequests = nil
	typedNil := commandMock("nil", "", false)
	typedNil.Spec.MySQLRequests[0].Message = (*mysql.QueryPacket)(nil)
	for name, m := range map[string]*models.Mock{
		"a PREPARE of an INSERT":   prepare,
		"two requests":             two,
		"no request":               none,
		"a COM_QUERY with no text": typedNil,
	} {
		if got, ok := callWrite(m); ok || got != "" {
			t.Errorf("%s: write %q, %v; want none", name, got, ok)
		}
	}
	if got, ok := callWrite(nil); ok || got != "" {
		t.Errorf("nil: write %q, %v; want none", got, ok)
	}
}

// exactConsumedDb is markingDb whose open test's answered recordings are
// consumed (integrations.ExactConsumedReader), as a store serving exactly
// lists them, holding itself to every round.
type exactConsumedDb struct {
	markingDb
	consumed []*models.Mock
}

func (m exactConsumedDb) ConsumedTestMocks() []*models.Mock { return m.consumed }

func (m exactConsumedDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return m, func() bool { return false }
}

// A command the open test runs once more than it recorded, after its
// recording answered, is filed by the command phase as that miss, naming
// the recording it repeats, and is never logged as a command the set never
// recorded (REPLAY-ORPHAN): the set recorded it, and it answered. The same
// command no recording answered still is.
func TestACommandRunOnceMoreIsNoOrphanOfTheRecording(t *testing.T) {
	const orphan = "REPLAY-ORPHAN"
	for _, tc := range []struct {
		name     string
		consumed []*models.Mock
		extra    bool
	}{
		// With another kind's answered recording, which records no MySQL
		// command.
		{"its recording answered already", []*models.Mock{{Name: "http-1", Kind: models.HTTP}, commandMock("own-1", "SELECT 1", false)}, true},
		{"no recording of it answered", []*models.Mock{commandMock("own-2", "SELECT 2", false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.ErrorLevel)
			db := exactConsumedDb{markingDb{&fakeMockDb{session: []*models.Mock{nativeHandshake()}}, false}, tc.consumed}
			client, done := replayLogged(t, db, zap.New(core))
			client.greetAndAnswer("app", mysql.Native)
			if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
				t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
			}
			client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1"...))
			if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
				t.Fatalf("the client read %q, %v after its query; want its connection closed unanswered", got, err)
			}
			err := <-done
			var r reportCarrier
			if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
				t.Fatalf("Replay returned %v; want the query's miss with its report", err)
			}
			rep := r.MismatchReport()
			if extra := rep.MissKind == models.MissExtraCall && rep.ClosestMock == "own-1" && rep.Recorded == 1; extra != tc.extra {
				t.Fatalf("report %+v: an extra call repeating own-1: %v, want %v", *rep, extra, tc.extra)
			}
			if logged := logs.FilterMessageSnippet(orphan).Len() > 0; logged == tc.extra {
				t.Fatalf("the command's %s line was logged: %v, want %v", orphan, logged, !tc.extra)
			}
		})
	}
}
