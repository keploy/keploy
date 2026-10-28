package replayer

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The debug log of an EXECUTE the matcher compares masks its query
// attributes' values: an attribute named as a secret (a password) holds one
// whatever it looks like, and was logged in the clear. The statement's own
// values, its attributes' names and types are logged as they are.
func TestMatchCommand_TheDebugLogMasksAnExecutesAttributeValues(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	at := stmtT0.Add(time.Second)
	db := &fakeMockDb{session: []*models.Mock{executedMock("mock-1", 1, at, "row", ordersInsert, "k1", "t1")},
		winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	req := liveExecute("k1", "t1")
	p := req.Message.(*mysql.StmtExecutePacket)
	p.Parameters = append(p.Parameters, mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret})
	p.ParameterCount = len(p.Parameters)
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	if _, _, _, err := matchCommand(ctx, zap.New(core), req, db, dctx, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("Trying to match the mock with com-stmt-execute request").All()
	if len(entries) == 0 {
		t.Fatal("precondition: the matcher logged no EXECUTE it compared")
	}
	for _, e := range entries {
		logged := fmt.Sprintf("%+v", e.ContextMap()["Req"])
		if strings.Contains(logged, secret) {
			t.Fatalf("the debug log holds the attribute's value: %s", logged)
		}
		if !strings.Contains(logged, "password") || !strings.Contains(logged, maskedAttributeValue) || !strings.Contains(logged, "k1") {
			t.Fatalf("the debug log does not show the EXECUTE as it is, its attribute's value masked: %s", logged)
		}
	}
	if p.Parameters[2].Value != secret {
		t.Fatalf("logging the EXECUTE changed it: %+v", p.Parameters[2])
	}
}

// The mismatch summary of an EXECUTE masks its query attributes' values as
// the debug log does: it is logged at WARN on every miss, and saved in the
// test report.
func TestFormatExecParams_MasksAttributeValues(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	got := formatExecParams([]mysql.Parameter{
		{Type: 253, Value: "k1"},
		{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret},
		{Type: uint16(mysql.FieldTypeString), Name: "unset"},
	})
	if strings.Contains(got, secret) || !strings.Contains(got, "k1") || !strings.Contains(got, maskedAttributeValue) {
		t.Fatalf("the summary is %q: want the statement's own value, and the attribute's masked", got)
	}
}

// A strict miss's field diffs mask the recorded and live values of a query
// attribute, of an EXECUTE and of a COM_QUERY; a statement's own value's
// drift is shown as it is.
func TestMatchCommand_AStrictMissMasksAttributeValues(t *testing.T) {
	const secret, old = "correct-horse-battery-staple", "old-password"
	password := func(v string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: v}
	}
	check := func(t *testing.T, miss *mockMiss, ownPath string) {
		t.Helper()
		if miss == nil {
			t.Fatal("no miss")
		}
		var masked, own bool
		for _, d := range miss.fieldDiffs {
			// An enveloped value ($bin) is held base64-encoded.
			for _, v := range []string{secret, old, base64.StdEncoding.EncodeToString([]byte(secret)), base64.StdEncoding.EncodeToString([]byte(old))} {
				if strings.Contains(d.Expected, v) || strings.Contains(d.Actual, v) {
					t.Fatalf("a field diff holds the attribute's value: %+v", miss.fieldDiffs)
				}
			}
			if (d.Kind == models.DiffKindMissingInLive && d.Actual != "") || (d.Kind == models.DiffKindMissingInMock && d.Expected != "") {
				t.Fatalf("a field diff fills the side its field is absent from: %+v", miss.fieldDiffs)
			}
			if strings.Contains(d.Path, ".value") && (d.Expected == maskedAttributeValue || d.Actual == maskedAttributeValue) {
				masked = true
			}
			if d.Path == ownPath && d.Expected == "k0" && d.Actual == "k1" {
				own = true
			}
		}
		if !masked || (ownPath != "" && !own) {
			t.Fatalf("the diffs %+v: want the attribute's masked and the statement's own shown", miss.fieldDiffs)
		}
	}
	t.Run("an EXECUTE", func(t *testing.T) {
		// A statement that reads its attribute: one that does not leaves its
		// attributes out of what strict compares.
		const readsExec = "SELECT ? AS k, ? AS t, mysql_query_attribute_string('password') AS v"
		at := stmtT0.Add(time.Second)
		m := executedMock("mock-1", 1, at, "row", readsExec, "k0", "t1")
		p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		p.Parameters = append(p.Parameters, password(old))
		p.ParameterCount = len(p.Parameters)
		db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		req := liveExecute("k1", "t1")
		lp := req.Message.(*mysql.StmtExecutePacket)
		lp.Parameters = append(lp.Parameters, password(secret))
		lp.ParameterCount = len(lp.Parameters)
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = readsExec
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
		_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, "", schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the EXECUTE (%v)", err)
		}
		check(t, miss, "body.parameters.0.value")
	})
	// A statement that reads the attribute: one that does not leaves its
	// attributes out of what strict compares.
	const reads = "SELECT mysql_query_attribute_string('password') AS v"
	t.Run("a COM_QUERY", func(t *testing.T) {
		const sql = reads
		m := readbackMock("q1", sql, "row", zeroTime())
		m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = []mysql.Parameter{password(old)}
		req := comQueryReq(sql)
		req.PacketBundle.Message.(*mysql.QueryPacket).Parameters = []mysql.Parameter{password(secret)}
		db := &fakeMockDb{session: []*models.Mock{m}}
		_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), req, db, newDecodeCtx(), "", schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the COM_QUERY (%v)", err)
		}
		check(t, miss, "")
	})
	// Either side's name masks the position, and the side a field is absent
	// from stays empty: a recorded parameter sent without a name, an
	// attribute the call no longer sends, one it sends anew beside one that
	// drifted, and a value in an envelope.
	enveloped := func(v string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: []byte(v)}
	}
	for name, sides := range map[string][2][]mysql.Parameter{
		"a recording without the names": {{{Type: uint16(mysql.FieldTypeString), Value: old}}, {password(secret)}},
		"an attribute no longer sent":   {{password(old)}, nil},
		"an attribute sent anew":        {{password(old)}, {password(secret), {Type: uint16(mysql.FieldTypeString), Name: "token", Value: secret}}},
		"a value in an envelope":        {{enveloped(old)}, {enveloped(secret)}},
	} {
		t.Run(name, func(t *testing.T) {
			const sql = reads
			m := readbackMock("q1", sql, "row", zeroTime())
			m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = sides[0]
			req := comQueryReq(sql)
			req.PacketBundle.Message.(*mysql.QueryPacket).Parameters = sides[1]
			db := &fakeMockDb{session: []*models.Mock{m}}
			_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), req, db, newDecodeCtx(), "", schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
			if err != nil || ok {
				t.Fatalf("strict served the COM_QUERY (%v)", err)
			}
			check(t, miss, "")
		})
	}
}
