package replayer

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func executeBundle(flag byte, params ...mysql.Parameter) mysql.PacketBundle {
	return mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 30}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 1, IterationCount: 1, ParameterCount: len(params),
			NewParamsBindFlag: flag, NullBitmap: []byte{0}, Parameters: params},
	}
}

// Under --mock-noise-strict a parameter's type, unsigned flag and name are
// compared only when both EXECUTEs sent them; its value always is, and the
// miss report names only what was compared.
func TestStrictGate_ExecuteTypesAreComparedOnlyWhenBothPacketsSentThem(t *testing.T) {
	type param = mysql.Parameter
	for _, tc := range []struct {
		name      string
		rec, live mysql.PacketBundle
		allowed   bool
		drift     string // the one path the report names, when rejected
	}{
		{"filled-in types differ", executeBundle(0, param{Type: 254, Value: "a"}), executeBundle(0, param{Type: 253, Value: "a"}), true, ""},
		{"filled-in unsigned flags differ", executeBundle(0, param{Type: 3, Unsigned: true, Value: int64(5)}), executeBundle(1, param{Type: 3, Value: int64(5)}), true, ""},
		{"filled-in names differ", executeBundle(1, param{Type: 253, Name: "n", Value: "a"}), executeBundle(0, param{Type: 253, Value: "a"}), true, ""},
		{"a value drifted", executeBundle(0, param{Type: 254, Value: "a"}), executeBundle(0, param{Type: 253, Value: "z"}), false, "body.parameters.0.value"},
		{"a value changed JSON type", executeBundle(0, param{Type: 254, Value: "5"}), executeBundle(0, param{Type: 3, Value: int32(5)}), false, "body.parameters.0.value"},
		{"both sent other types", executeBundle(1, param{Type: 254, Value: "a"}), executeBundle(1, param{Type: 253, Value: "a"}), false, "body.parameters.0.type"},
	} {
		live := tc.live
		gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, nil)
		rec := &models.Mock{Name: "rec", Kind: models.MySQL}
		rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: tc.rec}}
		if got := gate.allows(rec); got != tc.allowed {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.allowed)
			continue
		}
		if !tc.allowed && (len(gate.fieldDiffs) != 1 || gate.fieldDiffs[0].Path != tc.drift) {
			t.Errorf("%s: the report names %+v, want only %s", tc.name, gate.fieldDiffs, tc.drift)
		}
	}
}

// learnedNoiseDb keeps the request-body noise a match learned.
type learnedNoiseDb struct {
	*fakeMockDb
	learned map[string][]string
}

func (d *learnedNoiseDb) UpdateUnFilteredMock(_ *models.Mock, updated *models.Mock) bool {
	d.learned = updated.Spec.ReqBodyNoise
	return true
}

// An auto-replay learns no noise from types a packet did not send: a released
// recording's FieldTypeString is not drift from the live statement's types.
func TestMatchCommand_LearnsNoNoiseFromTypesAPacketDidNotSend(t *testing.T) {
	at := stmtT0.Add(time.Second)
	rec := executedMock("mock-execute-legacy", 1, at, "row", ordersInsert, "k1", "t1")
	msg := rec.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
	msg.NewParamsBindFlag = 0
	for i := range msg.Parameters {
		msg.Parameters[i].Type = uint16(mysql.FieldTypeString)
	}
	db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{rec}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}}
	req := liveExecute("k1", "t1")
	req.Message.(*mysql.StmtExecutePacket).NewParamsBindFlag = 0
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, mocknoise.New(mysqlNoiseAdapter{}, true, false), nil)
	if err != nil || !ok || resp.Payload != "row" {
		t.Fatalf("served %v (ok %v, %v), want the recorded row", resp, ok, err)
	}
	for path := range db.learned {
		if strings.HasSuffix(path, ".type") || strings.HasSuffix(path, ".unsigned") || strings.HasSuffix(path, ".name") {
			t.Errorf("learned %s as noise: a type the packets did not both send", path)
		}
	}
}

// An auto-replay learns no noise from a released recording of a small
// integer, whose garbage it reads the live packet as that keploy read it to
// match (StmtExecutePacket.LegacyParameters): the values are not drift.
func TestMatchCommand_LearnsNoNoiseFromAnOlderKeploysReading(t *testing.T) {
	at := stmtT0.Add(time.Second)
	// What that keploy recorded for INT 2 then "small-2": the INT's first byte
	// a length of 2, its next two bytes the first value, and the next byte a
	// length of 0 the second.
	rec := executedMock("mock-execute-legacy", 1, at, "row", ordersInsert, "\x00\x00", "")
	msg := rec.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
	msg.NewParamsBindFlag = 0
	for i := range msg.Parameters {
		msg.Parameters[i].Type = uint16(mysql.FieldTypeString)
	}
	db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{rec}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}}
	req := liveExecute()
	live := req.Message.(*mysql.StmtExecutePacket)
	live.NewParamsBindFlag, live.ParameterCount = 0, 2
	live.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeLong), Value: int32(2)}, {Type: 253, Value: "small-2"}}
	live.LegacyParameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "\x00\x00"}, {Type: uint16(mysql.FieldTypeString), Value: ""}}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, mocknoise.New(mysqlNoiseAdapter{}, true, false), nil)
	if err != nil || !ok || resp.Payload != "row" {
		t.Fatalf("served %v (ok %v, %v), want the recorded row", resp, ok, err)
	}
	if len(db.learned) != 0 {
		t.Errorf("learned %v as noise from a recording read as its keploy read it", db.learned)
	}
}
