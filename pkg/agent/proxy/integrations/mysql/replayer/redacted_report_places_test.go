package replayer

import (
	"context"
	"encoding/base64"
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

// A strict miss's field diffs are shown by the places of every recording
// its report names: the closest one, and the first one strict rejected,
// in either order of the pool.
func TestMySQLStrictMissFieldDiffsRedactByEveryRecordingTheReportNames(t *testing.T) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	keyed := func(key, org string) string {
		return "SELECT name FROM users WHERE api_key = '" + key + "' AND org = '" + org + "'"
	}
	plain := queryRecorded("mock-plain", keyed("k1", "initech"), nil)
	redacting := queryRecorded("mock-redacting", keyed(redactedPlaceholder, "acme"), redactedNoise)
	for _, order := range [][]*models.Mock{{plain, redacting}, {redacting, plain}} {
		db := &fakeMockDb{session: order}
		live := keyed(liveSecret, "globex")
		req := mysql.Request{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 80}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: live},
		}}
		_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok || miss == nil {
			t.Fatalf("pool %s, %s: strict served the COM_QUERY (%v, %+v)", order[0].Name, order[1].Name, err, miss)
		}
		if len(miss.fieldDiffs) == 0 {
			t.Fatalf("pool %s, %s: the miss has no field diffs", order[0].Name, order[1].Name)
		}
		for _, d := range miss.fieldDiffs {
			if strings.Contains(d.Actual, liveSecret) || strings.Contains(d.Expected, liveSecret) {
				t.Errorf("pool %s, %s: the report names %q and a field diff shows the live value: %+v", order[0].Name, order[1].Name, miss.closestMock, d)
			}
		}
	}
}

// The miss of an EXECUTE whose test's recording holds EXECUTEs that lost
// their statement names those recordings: its report shows the live bound
// values by their places, whichever of them redacted one.
func TestMySQLLostStatementMissRedactsTheNamedRecordingsBoundValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		// redacting is which recording redacted bound value 0: "a" is test
		// A's (the one the miss refuses to serve), "b" test B's (lost) and
		// "c" another EXECUTE of test B that lost its statement, after b.
		redacting string
		params    []string
	}{
		{"both, the miss refusing test A's", "ab", []string{liveSecret, "2026-10-15 09:00:00"}},
		{"the lost one, the miss refusing test A's", "b", []string{liveSecret, "2026-10-15 09:00:00"}},
		{"the one it refuses to serve", "a", []string{liveSecret, "2026-10-15 09:00:00"}},
		{"the lost one, no recording matched", "b", []string{liveSecret, "2026-10-15 09:00:00", "x"}},
		{"a lost one after the first, the miss refusing test A's", "c", []string{liveSecret, "2026-10-15 09:00:00"}},
		{"a lost one after the first, no recording matched", "c", []string{liveSecret, "2026-10-15 09:00:00", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, execA, execB := orderTests("")
			execC := executedMock("mock-execute-c", 3, execB.Spec.ReqTimestampMock.Add(time.Millisecond), "B: 201", "", "k2", "2026-10-14 12:00:21")
			db.session = append(db.session, execC)
			for name, m := range map[string]*models.Mock{"a": execA, "b": execB, "c": execC} {
				if strings.Contains(tc.redacting, name) {
					m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).Parameters[0].Value = redactedPlaceholder
					m.Noise = redactedNoise
				}
			}
			resp, ok, miss := matchLiveOrder(t, db, tc.params...)
			if ok {
				t.Fatalf("served %v", resp)
			}
			if miss == nil || !strings.Contains(miss.why, execB.Name) || !strings.Contains(miss.why, execC.Name) {
				t.Fatalf("the miss %+v does not name %s and %s", miss, execB.Name, execC.Name)
			}
			live := liveExecute(tc.params...).Message.(*mysql.StmtExecutePacket).Parameters
			if shown := formatExecParams(miss.places.liveParams(live)); strings.Contains(shown, liveSecret) {
				t.Errorf("the report of a miss naming %q (%s) shows the live bound value: %s", miss.closestMock, miss.why, shown)
			}
		})
	}
}

// A strict miss of an EXECUTE beside EXECUTEs that lost their statement,
// which no mock matched: its report names the lost recordings, and shows
// the live bound values, in its summary and its field diffs, by the places
// of each, the one strict rejected first or not.
func TestMySQLStrictLostStatementMissRedactsEveryNamedRecordingsBoundValue(t *testing.T) {
	db, _, execB := orderTests("")
	execC := executedMock("mock-execute-c", 3, execB.Spec.ReqTimestampMock.Add(time.Millisecond), "B: 201", "", redactedPlaceholder, "2026-10-14 12:00:21")
	execC.Noise = redactedNoise
	db.session = []*models.Mock{db.session[0], execB, execC}
	req := liveExecute(liveSecret, "2026-10-15 09:00:00")
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, StrictEngineForTest(), nil)
	if err != nil || ok || miss == nil {
		t.Fatalf("ok %v, err %v, miss %v: want a miss", ok, err, miss)
	}
	if miss.strictRejected == 0 || !strings.Contains(miss.why, execC.Name) {
		t.Fatalf("the miss %+v is not strict's, or does not name %s", miss, execC.Name)
	}
	live := req.Message.(*mysql.StmtExecutePacket).Parameters
	if shown := formatExecParams(miss.places.liveParams(live)); strings.Contains(shown, liveSecret) {
		t.Errorf("the report of a miss naming %s (%s) shows the live bound value: %s", execC.Name, miss.why, shown)
	}
	for _, d := range miss.fieldDiffs {
		if strings.Contains(d.Actual, liveSecret) {
			t.Errorf("a field diff of a miss naming %s shows the live bound value: %+v", execC.Name, d)
		}
	}
}

// A strict COM_STMT_SEND_LONG_DATA miss: the obfuscator redacted a value
// in the recorded chunk. Every diff of the chunk is redacted, whatever
// form the live chunk takes.
func TestMySQLStrictLongDataMissRedactsTheChunkTheRecordingRedacted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		noise    map[string][]string
		live     string
	}{
		{"a JSON chunk, no longer JSON", `{"token":"` + redactedPlaceholder + `","n":1}`, map[string][]string{"body.data.token": {}},
			"token=" + liveSecret + "&n=2"},
		{"a JSON chunk, another field changed", `{"token":"` + redactedPlaceholder + `","n":1}`, map[string][]string{"body.data.token": {}},
			`{"token":"` + liveSecret + `","n":2,"key":"` + liveSecret + `"}`},
		{"a chunk of text", "token=" + redactedPlaceholder + "&n=1", nil, "token=" + liveSecret + "&n=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sldMockOf("sld1", []byte(tc.recorded))
			m.Spec.ReqBodyNoise = tc.noise
			m.Noise = redactedNoise
			db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
			_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), sldReq([]byte(tc.live)), db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
			if err == nil && ok {
				t.Fatal("strict served the chunk")
			}
			if miss == nil || len(miss.fieldDiffs) == 0 {
				t.Fatalf("no miss with field diffs: %+v", miss)
			}
			for _, d := range miss.fieldDiffs {
				raw, _ := base64.StdEncoding.DecodeString(strings.Trim(d.Actual, `"`))
				if strings.Contains(d.Actual, liveSecret) || strings.Contains(string(raw), liveSecret) {
					t.Errorf("a field diff of the chunk carries the live value: %+v (decoded %q)", d, raw)
				}
			}
		})
	}
}

// A miss report shows the live command by the places any recording of the
// pool redacted, as the logs do: a miss that names the nearest recording,
// which redacted nothing, still redacts the value another recording hid.
func TestMySQLMissRedactsByThePoolWhenItNamesAPlainRecording(t *testing.T) {
	db := &fakeMockDb{session: []*models.Mock{nativeHandshake(),
		queryRecorded("mock-redacting", "SELECT name FROM users WHERE api_key = '"+redactedPlaceholder+"'", redactedNoise),
		queryRecorded("mock-plain", "SELECT name, email FROM users WHERE id = 7", nil)}}
	rep, logs := missOf(t, db, models.OutgoingOptions{}, func(c mysqlClient) {
		c.send(0, append([]byte{mysql.COM_QUERY}, "SELECT name, email FROM users WHERE api_key = '"+liveSecret+"'"...))
	})
	if rep.ClosestMock != "mock-plain" {
		t.Fatalf("precondition: the miss names %q; want mock-plain, the nearest recording", rep.ClosestMock)
	}
	if strings.Contains(reportText(rep), liveSecret) {
		t.Errorf("the report naming %q shows the live value another recording of the pool redacted: %q", rep.ClosestMock, rep.ActualSummary)
	}
	if !strings.Contains(rep.ActualSummary, valueRedacted) {
		t.Errorf("the summary %q redacts nothing", rep.ActualSummary)
	}
	if strings.Contains(logText(logs), liveSecret) {
		t.Errorf("the logs show the live value")
	}
}

// A strict miss of an EXECUTE beside EXECUTEs that lost their statement
// shows its bound values by the pool's places too: the other test's
// recording, which strict rejected, redacted the value, and neither lost
// one did.
func TestMySQLStrictLostStatementMissRedactsByThePool(t *testing.T) {
	db, execA, execB := orderTests("")
	execC := executedMock("mock-execute-c", 3, execB.Spec.ReqTimestampMock.Add(time.Millisecond), "B: 201", "", "k2", "2026-10-14 12:00:21")
	db.session = append(db.session, execC)
	execA.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).Parameters[0].Value = redactedPlaceholder
	execA.Noise = redactedNoise
	req := liveExecute(liveSecret, "2026-10-15 09:00:00")
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, StrictEngineForTest(), nil)
	if err != nil || ok || miss == nil {
		t.Fatalf("ok %v, err %v, miss %v: want a miss", ok, err, miss)
	}
	if !strings.Contains(miss.why, execB.Name) || !strings.Contains(miss.why, execC.Name) || strings.Contains(miss.why, "does not answer it with") {
		t.Fatalf("the miss %+v is not the one no recording matched, naming %s and %s", miss, execB.Name, execC.Name)
	}
	live := req.Message.(*mysql.StmtExecutePacket).Parameters
	if shown := formatExecParams(miss.places.liveParams(live)); strings.Contains(shown, liveSecret) {
		t.Errorf("the report of a miss naming %q shows the live bound value %s's recording redacted: %s", miss.closestMock, execA.Name, shown)
	}
}

// The miss of an EXECUTE that another test's recording would have answered,
// beside EXECUTEs that lost their statement, shows its bound values by the
// pool's places too: a recording of another statement redacted the value,
// and none the miss names did.
func TestMySQLLostStatementMissRefusingAnotherTestsRedactsByThePool(t *testing.T) {
	db, execA, execB := orderTests("")
	execC := executedMock("mock-execute-c", 3, execB.Spec.ReqTimestampMock.Add(time.Millisecond), "B: 201", "", "k2", "2026-10-14 12:00:21")
	execD := executedMock("mock-execute-d", 9, execB.Spec.ReqTimestampMock.Add(2*time.Millisecond), "D: rows", "SELECT name FROM users WHERE api_key = ?", redactedPlaceholder)
	execD.Noise = redactedNoise
	db.session = append(db.session, execC, execD)
	params := []string{liveSecret, "2026-10-15 09:00:00"}
	resp, ok, miss := matchLiveOrder(t, db, params...)
	if ok {
		t.Fatalf("served %v", resp)
	}
	if miss == nil || !strings.Contains(miss.why, "does not answer it with "+execA.Name) || strings.Contains(miss.why, execD.Name) {
		t.Fatalf("the miss %+v is not the one refusing %s, naming not %s", miss, execA.Name, execD.Name)
	}
	live := liveExecute(params...).Message.(*mysql.StmtExecutePacket).Parameters
	if shown := formatExecParams(miss.places.liveParams(live)); strings.Contains(shown, liveSecret) {
		t.Errorf("the report of a miss naming %q shows the live bound value %s's recording redacted: %s", miss.closestMock, execD.Name, shown)
	}
}

// A strict COM_STMT_SEND_LONG_DATA miss redacts the chunk's diffs by the
// pool's places too: a recording of a statement redacted a value, and the
// recorded chunk none.
func TestMySQLStrictLongDataMissRedactsByThePool(t *testing.T) {
	const recorded, live = "token=k1&n=1", "token=" + liveSecret + "&n=1"
	db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{sldMockOf("sld1", []byte(recorded)),
		queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = '"+redactedPlaceholder+"'", redactedNoise)}}}
	_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), sldReq([]byte(live)), db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err == nil && ok {
		t.Fatal("strict served the chunk")
	}
	if miss == nil || len(miss.fieldDiffs) == 0 || miss.closestMock != "sld1" {
		t.Fatalf("no miss naming sld1 with field diffs: %+v", miss)
	}
	for _, d := range miss.fieldDiffs {
		raw, _ := base64.StdEncoding.DecodeString(strings.Trim(d.Actual, `"`))
		if strings.Contains(d.Actual, liveSecret) || strings.Contains(string(raw), liveSecret) {
			t.Errorf("a field diff of the chunk carries the live value: %+v (decoded %q)", d, raw)
		}
	}
}

// A lost-statement miss names the first lost recordings only, and reads
// the places of every one of them, and of the recording it refuses to
// serve.
func TestMySQLLostStatementMissReadsThePlacesOfEveryLostRecording(t *testing.T) {
	lostExec := func(name, value string, noise []string) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL, Noise: noise}
		m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 16}, Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{StatementID: 1, Parameters: []mysql.Parameter{{Type: 254, Value: value}}},
		}}}
		return m
	}
	var lost []*models.Mock
	for i := range maxUnknownStatementsNamed {
		lost = append(lost, lostExec("mock-lost-"+string(rune('a'+i)), "k1", nil))
	}
	lost = append(lost, lostExec("mock-lost-last", redactedPlaceholder, redactedNoise))
	miss := unknownStatementsMiss(lost, nil, len(lost))
	if strings.Contains(miss.why, "mock-lost-last") || !strings.Contains(miss.why, "and 1 more") {
		t.Fatalf("precondition: the miss names the last lost recording, or not the count of the rest: %s", miss.why)
	}
	if !miss.places.params[0] {
		t.Errorf("the miss's places %+v: want bound value 0, which the lost recording it does not name redacted", miss.places)
	}
	instead := lostExec("mock-instead", redactedPlaceholder, redactedNoise)
	if miss := unknownStatementsMiss(lost[:1], instead, 1); !miss.places.params[0] {
		t.Errorf("the miss's places %+v: want bound value 0, which the recording it refuses to serve redacted", miss.places)
	}
}

// A strict COM_STMT_SEND_LONG_DATA miss of a pool that redacted nothing
// keeps the chunk's diffs as compared.
func TestMySQLStrictLongDataMissKeepsAChunkNoRecordingRedacted(t *testing.T) {
	const recorded, live = "token=k1&n=1", "token=k2&n=1"
	db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{sldMockOf("sld1", []byte(recorded))}}}
	_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), sldReq([]byte(live)), db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err == nil && ok {
		t.Fatal("strict served the chunk")
	}
	if miss == nil || len(miss.fieldDiffs) == 0 {
		t.Fatalf("no miss with field diffs: %+v", miss)
	}
	shown := false
	for _, d := range miss.fieldDiffs {
		raw, _ := base64.StdEncoding.DecodeString(strings.Trim(d.Actual, `"`))
		shown = shown || strings.Contains(d.Actual, live) || strings.Contains(string(raw), live)
		if d.Actual == valueRedacted || d.Expected == valueRedacted {
			t.Errorf("a field diff of a chunk no recording redacted is redacted: %+v", d)
		}
	}
	if !shown {
		t.Errorf("no field diff shows the live chunk: %+v", miss.fieldDiffs)
	}
}

// The pool's places are read once for a command whose report shows its
// statement (a miss), and never for one whose report shows none: a
// COM_PING, or a COM_QUERY answered, debug lines on or off. Its lines show
// the statement by its shape, which reads no places.
func TestMySQLReadsThePoolsPlacesOnlyForAMiss(t *testing.T) {
	ping := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: "COM_PING"},
		Message: &mysql.PingPacket{Command: mysql.COM_PING},
	}}
	pingMock := &models.Mock{Name: "mock-ping", Kind: models.MySQL}
	pingMock.TestModeInfo.Lifetime = models.LifetimeSession
	pingMock.Spec.Metadata = map[string]string{"type": "mocks"}
	pingMock.Spec.ReqTimestampMock = stmtT0
	pingMock.Spec.MySQLRequests = []mysql.Request{ping}
	pingMock.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: mysql.StatusToString(mysql.OK)},
		Message: &mysql.OKPacket{},
	}}}
	const recorded = "SELECT name FROM users WHERE api_key = '" + redactedPlaceholder + "'"
	query := func(sql string) mysql.Request {
		return mysql.Request{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1)}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: sql},
		}}
	}
	for _, tc := range []struct {
		name     string
		req      mysql.Request
		debug    bool
		answered bool
		reads    int
	}{
		{"a COM_PING, debug on", ping, true, true, 0},
		{"a COM_QUERY answered, debug off", query(recorded), false, true, 0},
		{"a COM_QUERY answered, debug on", query(recorded), true, true, 0},
		{"a COM_QUERY missed, debug off", query("SELECT name FROM users WHERE api_key = '" + liveSecret + "' AND id = 1"), false, false, 1},
		{"a COM_QUERY missed, debug on", query("SELECT name FROM users WHERE api_key = '" + liveSecret + "' AND id = 1"), true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeMockDb{session: []*models.Mock{pingMock, queryRecorded("mock-users", recorded, redactedNoise)}}
			level := zapcore.InfoLevel
			if tc.debug {
				level = zapcore.DebugLevel
			}
			core, logs := observer.New(level)
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			reads := 0
			_, ok, miss, err := matchCommandWith(ctx, zap.New(core), tc.req, db, newDecodeCtx(), nil, nil, matchOptions{placesRead: &reads})
			if err != nil || ok != tc.answered {
				t.Fatalf("ok %v, err %v, miss %+v: want answered %v", ok, err, miss, tc.answered)
			}
			if reads != tc.reads {
				t.Errorf("the command read the pool's places %d time(s); want %d", reads, tc.reads)
			}
			// The lines show the statement's shape, no literal of it.
			const shown = "SELECT name FROM users WHERE api_key = ? AND id = ?"
			lt := logText(logs.All())
			if tc.debug && !tc.answered && !strings.Contains(lt, shown) {
				t.Errorf("the miss's lines do not show %q:\n%s", shown, lt)
			}
			if strings.Contains(lt, liveSecret) {
				t.Errorf("the lines show the live value:\n%s", lt)
			}
		})
	}
}

// A strict miss redacts the live command by the pool's places too when the
// recording that redacted the value is none the miss names: neither the
// closest, nor the one strict rejected first.
func TestMySQLStrictMissRedactsByARecordingItDoesNotName(t *testing.T) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	keyed := func(key, org string) string {
		return "SELECT name FROM users WHERE api_key = '" + key + "' AND org = '" + org + "'"
	}
	rejected := queryRecorded("mock-rejected", keyed("k0", "acme"), nil)
	closest := queryRecorded("mock-closest", keyed("k1", "initech"), nil)
	other := queryRecorded("mock-other", "SELECT email FROM accounts WHERE token = '"+redactedPlaceholder+"'", redactedNoise)
	db := &fakeMockDb{session: []*models.Mock{rejected, closest, other}}
	live := keyed(liveSecret, "globex")
	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 80}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: live},
	}}
	_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, newDecodeCtx(), mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err != nil || ok || miss == nil || miss.strictRejected == 0 {
		t.Fatalf("ok %v, err %v, miss %+v: want a strict miss", ok, err, miss)
	}
	if miss.closestMock == other.Name || placesOf(rejected).redactsAny() || placesOf(closest).redactsAny() {
		t.Fatalf("precondition: the miss names %q; want a recording that redacted nothing", miss.closestMock)
	}
	if shown := miss.places.liveStatement(live); strings.Contains(shown, liveSecret) {
		t.Errorf("the report of a strict miss naming %q shows the live value %s redacted: %s", miss.closestMock, other.Name, shown)
	}
	for _, d := range miss.fieldDiffs {
		if strings.Contains(d.Actual, liveSecret) {
			t.Errorf("the field diff %s shows the live value: %q", d.Path, d.Actual)
		}
	}
}

// The miss of an EXECUTE no recording answers, whose statement every
// recording keeps, shows its bound values by the pool's places: another
// statement's recording redacted the value, and the one the miss names
// none.
func TestMySQLExecuteMissRedactsByAnotherStatementsRecording(t *testing.T) {
	const update = "UPDATE orders SET created_at = ? WHERE idmp_key = ?"
	plain := executedMock("mock-execute-plain", 1, stmtT0.Add(time.Millisecond), "A: 201", ordersInsert, "k1", "2026-10-14 12:00:10")
	other := executedMock("mock-execute-other", 9, stmtT0.Add(2*time.Millisecond), "D: rows", "SELECT name FROM users WHERE api_key = ?", redactedPlaceholder)
	other.Noise = redactedNoise
	db := &fakeMockDb{session: []*models.Mock{plain, other}}
	req := liveExecute(liveSecret, "k1")
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = update
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil || ok || miss == nil {
		t.Fatalf("ok %v, err %v, resp %v: want a miss", ok, err, resp)
	}
	if miss.closestMock == other.Name || strings.Contains(miss.why, "lost") {
		t.Fatalf("precondition: the miss %+v names %q; want the final miss, naming no recording that redacted", miss, miss.closestMock)
	}
	live := req.Message.(*mysql.StmtExecutePacket).Parameters
	if shown := formatExecParams(miss.places.liveParams(live)); strings.Contains(shown, liveSecret) {
		t.Errorf("the report of a miss naming %q shows the live bound value %s redacted: %s", miss.closestMock, other.Name, shown)
	}
}

// The miss of an EXECUTE beside EXECUTEs that lost their statement shows its
// bound values by the pool's places as well as by the lost recordings':
// a lost recording redacted bound value 0, another statement's recording
// bound value 1.
func TestMySQLLostStatementMissRedactsByThePoolBesideTheLostRecordings(t *testing.T) {
	db, execA, execB := orderTests("")
	execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).Parameters[0].Value = redactedPlaceholder
	execB.Noise = redactedNoise
	execD := executedMock("mock-execute-d", 9, execB.Spec.ReqTimestampMock.Add(time.Millisecond), "D: rows", "SELECT name FROM users WHERE org = ? AND api_key = ?", "acme", redactedPlaceholder)
	execD.Noise = redactedNoise
	db.session = append(db.session, execD)
	const liveOther = "sk9LIVEother88"
	params := []string{liveSecret, liveOther}
	resp, ok, miss := matchLiveOrder(t, db, params...)
	if ok {
		t.Fatalf("served %v", resp)
	}
	if miss == nil || !strings.Contains(miss.why, "does not answer it with "+execA.Name) || !strings.Contains(miss.why, execB.Name) {
		t.Fatalf("the miss %+v is not the one refusing %s, naming %s", miss, execA.Name, execB.Name)
	}
	live := liveExecute(params...).Message.(*mysql.StmtExecutePacket).Parameters
	shown := formatExecParams(miss.places.liveParams(live))
	if strings.Contains(shown, liveSecret) || strings.Contains(shown, liveOther) {
		t.Errorf("the report of a miss naming %q shows a live bound value a recording of the pool redacted: %s", miss.closestMock, shown)
	}
}
