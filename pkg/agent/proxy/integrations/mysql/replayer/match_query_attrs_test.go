package replayer

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// Two traceparents, as Connector/J 8.1+ sends with every statement of a traced
// call: a replay's never equals its recording's.
const (
	recordedTraceparent = "00-a5ff4ff820b14ad28693c1b155015340-0000000000000001-01"
	replayTraceparent   = "00-60de6a56e2b8444e8e403145868fca9f-0000000000000001-01"
)

// attrQueryReq is a live COM_QUERY as the replayer reads it: sql sent by
// Connector/J with a traceparent query attribute, decoded from the wire.
func attrQueryReq(t *testing.T, tp, sql string) mysql.Request {
	t.Helper()
	data := []byte{mysql.COM_QUERY, 0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeString), 0x00, byte(len("traceparent"))}
	data = append(data, "traceparent"...)
	data = append(data, byte(len(tp)))
	data = append(data, tp...)
	data = append(data, sql...)
	p, err := query.DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
	if err != nil {
		t.Fatal(err)
	}
	req := comQueryReq(sql)
	req.PacketBundle.Message = p
	return req
}

// attrQueryMock is a recording of attrQueryReq(tp, sql) by this keploy, which
// reads the attribute's name.
func attrQueryMock(name, tp, sql, marker string) *models.Mock {
	m := readbackMock(name, sql, marker, zeroTime())
	qp := m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket)
	qp.ParameterCount, qp.NewParamsBindFlag, qp.NullBitmap = 1, 1, []byte{0}
	qp.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "traceparent", Value: tp}}
	return m
}

// legacyAttrQueryMock is the recording of attrQueryReq(tp, sql) by a keploy
// that did not read the attribute's name (v3.6.126 recorded these from the
// java lane's Connector/J 8.4): the name taken for the value, the value's
// length and the value glued to the head of the query.
func legacyAttrQueryMock(name, tp, sql, marker string) *models.Mock {
	m := readbackMock(name, string(rune(len(tp)))+tp+sql, marker, zeroTime())
	qp := m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket)
	qp.ParameterCount, qp.NewParamsBindFlag, qp.NullBitmap = 1, 1, []byte{0}
	qp.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "traceparent"}}
	return m
}

// A COM_QUERY's query attributes are not part of what it is matched on, nor a
// request field that drifts (and is learned as noise), unless the statement
// reads one: the server answers with them only through
// mysql_query_attribute_string(), and Connector/J sends a traceparent that
// differs on every call.
func TestMatchCommand_AQueryAttributeCountsOnlyForAStatementThatReadsIt(t *testing.T) {
	logger := zap.NewNop()
	const sql = "SELECT 1 AS v"
	modes := map[string]func() *schemanoise.Engine{
		"lenient":   func() *schemanoise.Engine { return nil },
		"strict":    func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, false, true) },
		"detection": func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, true, false) },
	}
	for name, eng := range modes {
		t.Run(name, func(t *testing.T) {
			db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{attrQueryMock("q1", recordedTraceparent, sql, "row-1")}}}
			resp, ok, miss, err := matchCommand(context.Background(), logger, attrQueryReq(t, replayTraceparent, sql), db, newDecodeCtx(), eng(), nil)
			if err != nil || !ok || resp.Payload != "row-1" {
				t.Fatalf("a traceparent that differs kept the recording from matching (ok=%v err=%v miss=%+v)", ok, err, miss)
			}
			if noise := db.capturedNoiseFor("q1"); len(noise) != 0 {
				t.Fatalf("the traceparent was learned as noise: %v", noise)
			}
		})
	}

	t.Run("strict, a statement reading the attribute", func(t *testing.T) {
		const reads = "SELECT mysql_query_attribute_string('traceparent') AS v"
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{attrQueryMock("q1", recordedTraceparent, reads, "row-1")}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, miss, err := matchCommand(context.Background(), logger, attrQueryReq(t, replayTraceparent, reads), db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict served the answer to another traceparent of a statement that reads it")
		}
		if miss == nil || miss.strictRejected == 0 {
			t.Fatalf("strict did not reject it, miss %+v", miss)
		}
		found := false
		for _, d := range miss.fieldDiffs {
			if d.Path == "body.parameters.0.value" && d.Expected == recordedTraceparent && d.Actual == replayTraceparent {
				found = true
			}
		}
		if !found {
			t.Fatalf("the miss does not say the attribute drifted: %+v", miss.fieldDiffs)
		}
	})
}

// A COM_QUERY with query attributes an older keploy recorded (it read no
// attribute names, so its query text carries the attribute's value at its
// head) still matches: the live one is read as that keploy read it. Read as
// this keploy reads it, its query is not the recording's.
func TestMatchCommand_AnOlderKeployRecordingOfAQueryWithAttributesStillMatches(t *testing.T) {
	logger := zap.NewNop()
	for name, eng := range map[string]func() *schemanoise.Engine{
		"lenient": func() *schemanoise.Engine { return nil },
		"strict":  func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, false, true) },
	} {
		t.Run(name, func(t *testing.T) {
			db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{
				legacyAttrQueryMock("q1", recordedTraceparent, "SELECT 1 AS v", "row-1"),
				legacyAttrQueryMock("q2", recordedTraceparent, "SELECT 2 AS v", "row-2"),
			}}}
			resp, ok, miss, err := matchCommand(context.Background(), logger, attrQueryReq(t, recordedTraceparent, "SELECT 2 AS v"), db, newDecodeCtx(), eng(), nil)
			if err != nil || !ok || resp.Payload != "row-2" {
				t.Fatalf("the older keploy's recording did not match (ok=%v err=%v miss=%+v)", ok, err, miss)
			}
		})
	}

	// A live query whose attribute is named "" is read as this keploy reads
	// it: a recording of it names none either way.
	t.Run("an attribute named nothing", func(t *testing.T) {
		data := []byte{mysql.COM_QUERY, 0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeString), 0x00, 0x00, 0x01, 'x'}
		data = append(data, "SELECT 1 AS v"...)
		p, err := query.DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err != nil || p.LegacyQuery == "" {
			t.Fatalf("precondition: an older keploy read it too (%v, %+v)", err, p)
		}
		live := comQueryReq("SELECT 1 AS v")
		live.PacketBundle.Message = p
		m := readbackMock("q1", "SELECT 1 AS v", "row-1", zeroTime())
		m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "x"}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		resp, ok, miss, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), nil, nil)
		if err != nil || !ok || resp.Payload != "row-1" {
			t.Fatalf("its recording did not match (ok=%v err=%v miss=%+v)", ok, err, miss)
		}
	})

	// A recording of this keploy's beside it is matched as this keploy reads
	// the live one.
	t.Run("beside a recording of this keploy's", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{
			legacyAttrQueryMock("q1", recordedTraceparent, "SELECT 1 AS v", "row-1"),
			attrQueryMock("q2", recordedTraceparent, "SELECT 2 AS v", "row-2"),
		}}}
		resp, ok, miss, err := matchCommand(context.Background(), logger, attrQueryReq(t, replayTraceparent, "SELECT 2 AS v"), db, newDecodeCtx(), nil, nil)
		if err != nil || !ok || resp.Payload != "row-2" {
			t.Fatalf("this keploy's recording did not match (ok=%v err=%v miss=%+v)", ok, err, miss)
		}
	})
}
