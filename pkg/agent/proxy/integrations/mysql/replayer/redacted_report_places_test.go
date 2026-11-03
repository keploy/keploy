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
