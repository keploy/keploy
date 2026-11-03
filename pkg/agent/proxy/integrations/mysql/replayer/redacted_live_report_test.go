package replayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The value the obfuscator wrote in a recording in place of a secret, and
// the secret a live call sends at that place. The live value matches none
// of the mock's patterns: they are anchored on the placeholder.
const (
	redactedPlaceholder = "xq7Lm2Pw9Rt4Zk"
	liveSecret          = "sk9LIVEsecret77"
)

// redactedNoise is the mock's pattern for the placeholder, as the
// obfuscator writes it.
var redactedNoise = []string{"^" + regexp.QuoteMeta(redactedPlaceholder) + "$"}

// missOf runs one replayed connection from db: the handshake, then send's
// commands. It returns the miss's report and every log entry the replay
// wrote, at debug level too.
func missOf(t *testing.T, db *fakeMockDb, opts models.OutgoingOptions, send func(c mysqlClient)) (*models.MockMismatchReport, []observer.LoggedEntry) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	clientEnd, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close(); _ = agentEnd.Close() })
	done := make(chan error, 1)
	go func() {
		done <- Replay(context.Background(), zap.New(core), agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, opts)
		_ = agentEnd.Close()
	}()
	client := mysqlClient{t: t, c: clientEnd}
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	send(client)
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %x, %v; want its connection closed unanswered", got, err)
	}
	err := <-done
	var r reportCarrier
	if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want the command's miss with its report", err)
	}
	return r.MismatchReport(), logs.All()
}

// reportText is every field of rep, as the report YAML and the platform
// carry them.
func reportText(rep *models.MockMismatchReport) string {
	return fmt.Sprintf("%+v", *rep)
}

// logText is every log entry's message and fields.
func logText(entries []observer.LoggedEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %v\n", e.Message, e.ContextMap())
	}
	return b.String()
}

// queryRecorded is a recorded COM_QUERY of sql, with the mock's patterns
// noise.
func queryRecorded(name, sql string, noise []string) *models.Mock {
	m := readbackMock(name, sql, "rows of "+name, stmtT0)
	m.Noise = noise
	return m
}

// executeBytes is a COM_STMT_EXECUTE of statement stmtID binding the
// strings vals (a statement of no parameters sends none of their parts).
func executeBytes(stmtID uint32, vals ...string) []byte {
	exec := []byte{mysql.COM_STMT_EXECUTE}
	exec = binary.LittleEndian.AppendUint32(exec, stmtID)
	exec = append(exec, 0x00)                        // flags
	exec = binary.LittleEndian.AppendUint32(exec, 1) // iteration count
	if len(vals) == 0 {
		return exec
	}
	exec = append(exec, make([]byte, (len(vals)+7)/8)...)
	exec = append(exec, 0x01) // new params bound
	for range vals {
		exec = append(exec, 0xfd, 0x00) // VAR_STRING
	}
	for _, v := range vals {
		exec = append(exec, lenencString(v)...)
	}
	return exec
}

// A MySQL miss report and the replay's logs carry the live call's value at
// a place the recording redacted (a SQL literal, a bound value) as
// "(redacted)", never the live value: the place, not the value, says it is
// a secret. A place the recording did not redact keeps its live value.
func TestMySQLMissRedactsTheLiveValueAtARedactedPlace(t *testing.T) {
	const usersByKey = "SELECT name FROM users WHERE id = ? AND api_key = ?"
	for _, tc := range []struct {
		name string
		db   func() *fakeMockDb
		send func(c mysqlClient)
		// kept is a live value at a place the recording did not redact,
		// which the report still shows.
		kept string
		// strict replays under the mock-noise strict gate, where a
		// value that drifted is a miss.
		strict bool
	}{{
		name: "a query whose statement differs from the recording's, its literal redacted",
		db: func() *fakeMockDb {
			return &fakeMockDb{session: []*models.Mock{nativeHandshake(),
				queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = '"+redactedPlaceholder+"'", redactedNoise)}}
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT name, email FROM users WHERE api_key = '"+liveSecret+"'"...))
		},
		kept: "email",
	}, {
		name: "a query whose literal holds the redacted value among other text",
		db: func() *fakeMockDb {
			return &fakeMockDb{session: []*models.Mock{nativeHandshake(),
				queryRecorded("mock-sessions", "SELECT id FROM sessions WHERE auth = 'Bearer "+redactedPlaceholder+"'", redactedNoise)}}
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT id, user_id FROM sessions WHERE auth = 'Bearer "+liveSecret+"'"...))
		},
		kept: "user_id",
	}, {
		name: "a query of the recording's statement, another literal of it drifted",
		db: func() *fakeMockDb {
			return &fakeMockDb{session: []*models.Mock{nativeHandshake(),
				queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = '"+redactedPlaceholder+"' AND org = 'acme'", redactedNoise)}}
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT name FROM users WHERE api_key = '"+liveSecret+"' AND org = 'globex'"...))
		},
		kept:   "globex",
		strict: true,
	}, {
		name: "an execute whose bound value the recording redacted",
		db: func() *fakeMockDb {
			exec := executedMock("mock-execute-users", 1, stmtT0.Add(time.Millisecond), "user row", "", "41", redactedPlaceholder)
			exec.Noise = redactedNoise
			return &fakeMockDb{session: []*models.Mock{nativeHandshake(), preparedMock("mock-prepare-users", "0", 1, usersByKey, stmtT0), exec}}
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_STMT_PREPARE}, usersByKey...))
			if ok := c.read(); len(ok) < 5 || ok[0] != 0x00 {
				c.t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", ok)
			}
			c.send(0, executeBytes(1, "42", liveSecret))
		},
		kept:   "42",
		strict: true,
	}, {
		name: "an execute of a statement whose literal the recording redacted",
		db: func() *fakeMockDb {
			stmt := keyedPrepare(redactedPlaceholder, "acme")
			prep := preparedMock("mock-prepare-keyed", "0", 1, stmt, stmtT0)
			exec := executedMock("mock-execute-keyed", 1, stmtT0.Add(time.Millisecond), "user row", stmt, "41")
			prep.Noise, exec.Noise = redactedNoise, redactedNoise
			return &fakeMockDb{session: []*models.Mock{nativeHandshake(), prep, exec}}
		},
		send: func(c mysqlClient) {
			// Strict serves no recorded PREPARE of drifted literals: the
			// replay answers it with a statement of its own, of no
			// parameters, and the EXECUTE of it is the miss.
			c.send(0, append([]byte{mysql.COM_STMT_PREPARE}, keyedPrepare(liveSecret, "globex")...))
			ok := c.read()
			if len(ok) < 5 || ok[0] != 0x00 {
				c.t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", ok)
			}
			c.send(0, executeBytes(binary.LittleEndian.Uint32(ok[1:5])))
		},
		kept:   "globex",
		strict: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rep, logs := missOf(t, tc.db(), models.OutgoingOptions{MockNoiseStrict: tc.strict}, tc.send)
			text, logged := reportText(rep), logText(logs)
			if strings.Contains(text, liveSecret) {
				t.Errorf("the report carries the live value at a redacted place:\n%s", text)
			}
			if strings.Contains(logged, liveSecret) {
				t.Errorf("the replay logged the live value at a redacted place:\n%s", logged)
			}
			if !strings.Contains(rep.ActualSummary, valueRedacted) {
				t.Errorf("the summary does not say the redacted place's value is redacted: %q", rep.ActualSummary)
			}
			if !strings.Contains(rep.ActualSummary, tc.kept) {
				t.Errorf("the summary lost %q, which the recording did not redact: %q", tc.kept, rep.ActualSummary)
			}
		})
	}
}

// keyedPrepare is a prepared statement of a key literal and an org literal.
func keyedPrepare(key, org string) string {
	return "SELECT name FROM users WHERE api_key = '" + key + "' AND org = '" + org + "'"
}

// A recording that redacted nothing leaves the live call's values in the
// report as they are.
func TestMySQLMissKeepsTheLiveValueTheRecordingDidNotRedact(t *testing.T) {
	db := &fakeMockDb{session: []*models.Mock{nativeHandshake(),
		queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = 'k1'", nil)}}
	rep, _ := missOf(t, db, models.OutgoingOptions{}, func(c mysqlClient) {
		c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT name, email FROM users WHERE api_key = '"+liveSecret+"'"...))
	})
	if !strings.Contains(rep.ActualSummary, liveSecret) || strings.Contains(rep.ActualSummary, valueRedacted) {
		t.Errorf("the summary changed a value no recording redacted: %q", rep.ActualSummary)
	}
}

// A strict miss's field diffs show "(redacted)" on both sides of a place
// the recording redacted (a bound value, a literal), and the live and
// recorded values of every other place.
func TestMySQLStrictMissRedactsTheLiveValueAtARedactedPlace(t *testing.T) {
	check := func(t *testing.T, miss *mockMiss, redacted, kept string) {
		t.Helper()
		if miss == nil {
			t.Fatal("no miss")
		}
		var sawRedacted, sawKept bool
		for _, d := range miss.fieldDiffs {
			if strings.Contains(d.Actual, liveSecret) || strings.Contains(d.Expected, liveSecret) {
				t.Fatalf("a field diff holds the live value at a redacted place: %+v", miss.fieldDiffs)
			}
			if d.Path == redacted && d.Expected == valueRedacted && d.Actual == valueRedacted {
				sawRedacted = true
			}
			if d.Path == kept && d.Actual != valueRedacted && d.Actual != "" {
				sawKept = true
			}
		}
		if !sawRedacted || !sawKept {
			t.Fatalf("the diffs %+v: want %s redacted on both sides and %s shown", miss.fieldDiffs, redacted, kept)
		}
	}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	t.Run("an EXECUTE", func(t *testing.T) {
		const byKey = "SELECT name FROM users WHERE id = ? AND api_key = ?"
		at := stmtT0.Add(time.Second)
		m := executedMock("mock-1", 1, at, "row", byKey, "41", redactedPlaceholder)
		m.Noise = redactedNoise
		db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = byKey
		_, ok, miss, err := matchCommand(ctx, zap.NewNop(), liveExecute("42", liveSecret), db, dctx, mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the EXECUTE (%v)", err)
		}
		check(t, miss, "body.parameters.1.value", "body.parameters.0.value")
		if !miss.places.params[1] || miss.places.params[0] {
			t.Fatalf("the miss's places %+v: want the rejected recording's bound value 1 alone", miss.places)
		}
	})
	t.Run("a COM_QUERY", func(t *testing.T) {
		m := queryRecorded("q1", "SELECT name FROM users WHERE org = 'acme' AND api_key = '"+redactedPlaceholder+"'", redactedNoise)
		db := &fakeMockDb{session: []*models.Mock{m}}
		req := mysql.Request{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 80}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: "SELECT name FROM users WHERE org = 'globex' AND api_key = '" + liveSecret + "'"},
		}}
		_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the COM_QUERY (%v)", err)
		}
		check(t, miss, "body.query.literals.1", "body.query.literals.0")
		if !miss.places.literals[1] || miss.places.literals[0] {
			t.Fatalf("the miss's places %+v: want the rejected recording's literal 1 alone", miss.places)
		}
	})
}

// loggedAt is the field key of every log entry of message msg, as text.
func loggedAt(entries []observer.LoggedEntry, msg, key string) []string {
	var out []string
	for _, e := range entries {
		if e.Message == msg {
			out = append(out, fmt.Sprint(e.ContextMap()[key]))
		}
	}
	return out
}

// The replay's logs show the live statement by every place any recording
// of the pool redacted, whichever recording a line compares it with: a
// line of its comparison with a recording that redacted nothing, the
// parser's error on it, an answer the replay made up for it.
func TestMySQLLogsRedactTheLiveValueAtAPlaceAnyRecordingRedacted(t *testing.T) {
	keyed := func(key, org string) string {
		return "SELECT name FROM users WHERE api_key = '" + key + "' AND org = '" + org + "'"
	}
	redacting := func(m *models.Mock) *models.Mock {
		m.Noise = redactedNoise
		return m
	}
	for _, tc := range []struct {
		name   string
		pool   []*models.Mock
		send   func(c mysqlClient)
		strict bool
		// lines are log lines of the live value: message, then key.
		lines [][2]string
	}{{
		name: "its comparison with a recording that redacted nothing",
		pool: []*models.Mock{
			queryRecorded("mock-redacted", keyed(redactedPlaceholder, "acme"), redactedNoise),
			queryRecorded("mock-plain", keyed("k1", "initech"), nil),
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, keyed(liveSecret, "globex")...))
		},
		strict: true,
		lines:  [][2]string{{"query matched with drifted inline literals", "actual query"}},
	}, {
		name: "the parser's error on it",
		pool: []*models.Mock{
			queryRecorded("mock-update", "UPDATE users SET api_key = '"+redactedPlaceholder+"' WHERE id = 1", redactedNoise),
			queryRecorded("mock-select", "SELECT name FROM users WHERE id = 1", nil),
		},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, "UPDATE users SET api_key = 1 '"+liveSecret+"'"...))
		},
		lines: [][2]string{{"failed to get actual query structure", "error"}, {"failed to get actual query structure", "actual Query"},
			{"actual query is dml but expected is not", "actual query"}},
	}, {
		name: "an answer the replay made up for it",
		pool: []*models.Mock{queryRecorded("mock-users", keyed(redactedPlaceholder, "acme"), redactedNoise)},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_QUERY}, "CREATE TABLE tokens (k VARCHAR(64) DEFAULT '"+liveSecret+"')"...))
			if ok := c.read(); len(ok) == 0 || ok[0] != mysql.OK {
				c.t.Fatalf("the CREATE was answered with %x, want the replay's OK", ok)
			}
			c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1 FROM missing"...))
		},
		lines: [][2]string{{"Returning synthetic OK for unmocked control/DDL", "query"}},
	}, {
		name: "a prepared statement the replay answered",
		pool: []*models.Mock{redacting(preparedMock("mock-prepare", "0", 1, keyed(redactedPlaceholder, "acme")+" AND id = ?", stmtT0))},
		send: func(c mysqlClient) {
			c.send(0, append([]byte{mysql.COM_STMT_PREPARE}, keyed(liveSecret, "acme")+" AND id = ?"...))
			if ok := c.read(); len(ok) < 5 || ok[0] != 0x00 {
				c.t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", ok)
			}
			c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT 1 FROM missing"...))
		},
		lines: [][2]string{{"Recorded runtime PREP mapping with new statement ID", "query"}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeMockDb{session: append([]*models.Mock{nativeHandshake()}, tc.pool...)}
			rep, logs := missOf(t, db, models.OutgoingOptions{MockNoiseStrict: tc.strict}, tc.send)
			if text := reportText(rep); strings.Contains(text, liveSecret) {
				t.Errorf("the report carries the live value at a redacted place:\n%s", text)
			}
			if logged := logText(logs); strings.Contains(logged, liveSecret) {
				t.Errorf("the replay logged the live value at a place a recording redacted:\n%s", logged)
			}
			for _, l := range tc.lines {
				if got := loggedAt(logs, l[0], l[1]); len(got) == 0 || !strings.Contains(strings.Join(got, " "), valueRedacted) {
					t.Errorf("the line %q shows %s %q; want the live value redacted", l[0], l[1], got)
				}
			}
		})
	}
}

// Where no recording of the pool redacted a place, the logs show the live
// statement as it is.
func TestMySQLLogsKeepTheLiveValueNoRecordingRedacted(t *testing.T) {
	db := &fakeMockDb{session: []*models.Mock{nativeHandshake(),
		queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = 'k1'", nil),
		queryRecorded("mock-update", "UPDATE users SET name = 'n' WHERE id = 1", nil)}}
	_, logs := missOf(t, db, models.OutgoingOptions{}, func(c mysqlClient) {
		c.send(0, append([]byte{mysql.COM_QUERY}, "UPDATE users SET api_key = 1 '"+liveSecret+"'"...))
	})
	for _, l := range []struct{ line, key string }{
		{"actual query is dml but expected is not", "actual query"},
		{"failed to get actual query structure", "actual Query"},
		{"failed to get actual query structure", "error"},
		{"mock miss", "actual_query"},
	} {
		if got := loggedAt(logs, l.line, l.key); len(got) == 0 || !strings.Contains(strings.Join(got, " "), liveSecret) {
			t.Errorf("the line %q shows %s %q; want the live value, which no recording redacted", l.line, l.key, got)
		}
	}
}

// A strict miss names its closest recording and the first one strict
// rejected: the report redacts the live value at a place either redacted,
// the rejected one's when the closest redacted none.
func TestMySQLStrictMissRedactsByEveryRecordingItNames(t *testing.T) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	keyed := func(key, org string) string {
		return "SELECT name FROM users WHERE api_key = '" + key + "' AND org = '" + org + "'"
	}
	rejected := queryRecorded("mock-rejected", keyed(redactedPlaceholder, "acme"), redactedNoise)
	closest := queryRecorded("mock-closest", keyed("k1", "initech"), nil)
	db := &fakeMockDb{session: []*models.Mock{rejected, closest}}
	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 80}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: keyed(liveSecret, "globex")},
	}}
	_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err != nil || ok || miss == nil {
		t.Fatalf("strict served the COM_QUERY (%v, %v)", err, miss)
	}
	if miss.closestMock != closest.Name {
		t.Fatalf("the miss names %q; want %q, the closest", miss.closestMock, closest.Name)
	}
	if !miss.places.literals[0] || miss.places.literals[1] {
		t.Fatalf("the miss's places %+v: want the rejected recording's literal 0 alone", miss.places)
	}
}
