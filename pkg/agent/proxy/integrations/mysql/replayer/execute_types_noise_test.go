package replayer

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
		gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, "", nil)
		rec := &models.Mock{Name: "rec", Kind: models.MySQL}
		rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: tc.rec}}
		if got := gate.allows(rec); got != tc.allowed {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.allowed)
			continue
		}
		if !tc.allowed {
			diffs := gate.reportDiffs(gate.closestPlaces)
			if len(diffs) != 1 || diffs[0].Path != tc.drift {
				t.Errorf("%s: the report names %+v, want only %s", tc.name, diffs, tc.drift)
			}
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

// TestAStrictCommandComparesEachRecordingWithItsOwnReading: under strict
// enforcement, a live EXECUTE that an older keploy read otherwise
// (LegacyParameters) is compared with this keploy's recording as the packet
// reads now, and with that keploy's recording as that keploy read it: one
// body for each reading (strictGate.bodyDocFor). The eager gate compares
// each recording as the walk meets it: this keploy's recording, made with
// other values, first, which it refuses; then the older keploy's recording
// of the same call, which answers.
func TestAStrictCommandComparesEachRecordingWithItsOwnReading(t *testing.T) {
	at := stmtT0.Add(time.Second)
	// What that keploy recorded for INT 2 then "small-2" (see above).
	legacy := executedMock("mock-execute-legacy", 1, at, "row", ordersInsert, "\x00\x00", "")
	msg := legacy.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
	msg.NewParamsBindFlag = 0
	for i := range msg.Parameters {
		msg.Parameters[i].Type = uint16(mysql.FieldTypeString)
	}
	current := executedMock("mock-execute-current", 1, at.Add(-time.Millisecond), "other-row", ordersInsert)
	cur := current.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
	cur.ParameterCount = 2
	cur.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeLong), Value: int32(3)}, {Type: 253, Value: "small-3"}}
	if legacyRecordedMock(current) || !legacyRecordedMock(legacy) {
		t.Fatal("the row needs one recording of this keploy and one of an older keploy")
	}
	db := &fakeMockDb{session: []*models.Mock{current, legacy}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	req := liveExecute()
	live := req.Message.(*mysql.StmtExecutePacket)
	live.NewParamsBindFlag, live.ParameterCount = 0, 2
	live.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeLong), Value: int32(2)}, {Type: 253, Value: "small-2"}}
	live.LegacyParameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "\x00\x00"}, {Type: uint16(mysql.FieldTypeString), Value: ""}}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	var compared int
	var docs []*mocknoise.JSONBody
	resp, ok, _, err := matchCommandWith(ctx, zap.NewNop(), req, db, dctx, mocknoise.New(mysqlNoiseAdapter{}, false, true), nil,
		matchOptions{compared: &compared, docsMade: &docs, eagerGate: true, wholeWalk: true})
	if err != nil || !ok || resp.Payload != "row" {
		t.Fatalf("served %v (ok %v, %v), want the older keploy's recording's row", resp, ok, err)
	}
	if compared != 2 || len(docs) != 2 {
		t.Errorf("the gate compared %d recordings with %d bodies, want 2 with 2 (one for each reading)", compared, len(docs))
	}
}

// TestStrictGate_LearnedFieldMissingOrRetypedNamesFieldAndKind pins that
// MySQL's strict gate mismatch diffs also treat learned ReqBodyNoise as
// type-strict, naming a missing or retyped learned parameter rather than
// leaving fieldDiffs empty.
func TestStrictGate_LearnedFieldMissingOrRetypedNamesFieldAndKind(t *testing.T) {
	type param = mysql.Parameter
	for _, mode := range []struct {
		name string
		env  string
	}{
		{name: "proven_unset", env: ""},
		{name: "proven_1", env: "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode.env)

			cases := []struct {
				name         string
				rec          mysql.PacketBundle
				live         mysql.PacketBundle
				learned      map[string][]string
				wantPath     string
				wantKind     models.MockFieldDiffKind
				wantExpected string
				wantActual   string
			}{
				{
					name:         "learned parameter missing in live",
					rec:          executeBundle(0, param{Type: 254, Value: "a"}, param{Type: 3, Value: int32(1)}),
					live:         executeBundle(0, param{Type: 254, Value: "a"}),
					learned:      map[string][]string{"body.parameters.1.value": {}},
					wantPath:     "body.parameters.1.value",
					wantKind:     models.DiffKindMissingInLive,
					wantExpected: "1",
					wantActual:   "",
				},
				{
					name:         "learned parameter changed JSON type",
					rec:          executeBundle(0, param{Type: 3, Value: int32(5)}),
					live:         executeBundle(0, param{Type: 254, Value: "5"}),
					learned:      map[string][]string{"body.parameters.0.value": {}},
					wantPath:     "body.parameters.0.value",
					wantKind:     models.DiffKindTypeChanged,
					wantExpected: "number: 5",
					wantActual:   "string: 5",
				},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					live := tc.live
					gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, "", nil)
					rec := &models.Mock{Name: "rec-learned", Kind: models.MySQL}
					rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: tc.rec}}
					rec.Spec.ReqBodyNoise = tc.learned

					if gate.allows(rec) {
						t.Fatalf("expected strict rejection for %s", tc.name)
					}
					diffs := gate.reportDiffs(gate.closestPlaces)
					var found bool
					for _, d := range diffs {
						if d.Path == tc.wantPath && d.Kind == tc.wantKind {
							if d.Expected != tc.wantExpected || d.Actual != tc.wantActual {
								t.Fatalf("got Expected=%q Actual=%q; want Expected=%q Actual=%q (diff=%+v)", d.Expected, d.Actual, tc.wantExpected, tc.wantActual, d)
							}
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("expected gate.reportDiffs to contain path=%q kind=%q; got %+v", tc.wantPath, tc.wantKind, diffs)
					}
				})
			}

			t.Run("redacted learned parameter never leaks through reportDiffs", func(t *testing.T) {
				const recSecret = "MYSQL_SECRET_PLACEHOLDER_77"
				const liveSecret = "MYSQL_LIVE_SECRET_88"
				rec := &models.Mock{Name: "rec-redacted", Kind: models.MySQL}
				rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: executeBundle(0,
					param{Type: 254, Value: "a"},
					param{Type: 254, Value: recSecret},
					param{Type: 3, Value: int32(99)},
				)}}
				rec.Spec.ReqBodyNoise = map[string][]string{
					"body.parameters.1.value": {},
					"body.parameters.2.value": {},
				}
				rec.Noise = []string{"^" + recSecret + "$", "^99$"}

				// Missing redacted learned parameter (with param 0 also drifting so rejection is guaranteed).
				liveMissing := executeBundle(0, param{Type: 254, Value: "b"})
				gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &liveMissing, -1, "", nil)
				if gate.allows(rec) {
					t.Fatal("expected strict rejection when param 0 drifted and param 1 omitted")
				}
				diffs := gate.reportDiffs(gate.closestPlaces)
				var sawMissing bool
				for _, d := range diffs {
					if strings.Contains(d.Expected, recSecret) || strings.Contains(d.Actual, recSecret) {
						t.Fatalf("recorded secret leaked in reportDiffs: %+v", d)
					}
					if d.Path == "body.parameters.1.value" && d.Kind == models.DiffKindMissingInLive {
						if d.Expected != valueRedacted || d.Actual != "" {
							t.Fatalf("want Expected=%q Actual=\"\"; got %+v", valueRedacted, d)
						}
						sawMissing = true
					}
				}
				if !sawMissing {
					t.Fatalf("expected redacted body.parameters.1.value missing_in_live; got %+v", diffs)
				}

				// Retyped redacted learned parameters: param 1 string -> number, param 2 number -> live secret string.
				liveRetyped := executeBundle(0,
					param{Type: 254, Value: "b"},
					param{Type: 3, Value: int32(42)},
					param{Type: 254, Value: liveSecret},
				)
				gate2 := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &liveRetyped, -1, "", nil)
				if gate2.allows(rec) {
					t.Fatal("expected strict rejection when param 0 drifted and params 1 & 2 retyped")
				}
				// Confirm raw diffs carry liveSecret before reportDiffs redacts it.
				var rawHasLiveSecret bool
				for _, d := range gate2.rawFieldDiffs() {
					if strings.Contains(d.Actual, liveSecret) {
						rawHasLiveSecret = true
					}
				}
				if !rawHasLiveSecret {
					t.Fatalf("precondition: expected rawFieldDiffs to contain liveSecret before redaction; got %+v", gate2.rawFieldDiffs())
				}
				diffs2 := gate2.reportDiffs(gate2.closestPlaces)
				var sawRetyped1, sawRetyped2 bool
				for _, d := range diffs2 {
					if strings.Contains(d.Expected, recSecret) || strings.Contains(d.Actual, "42") || strings.Contains(d.Actual, liveSecret) || strings.Contains(d.Expected, "99") {
						t.Fatalf("secret or unredacted value leaked at redacted param position: %+v", d)
					}
					if d.Path == "body.parameters.1.value" && d.Kind == models.DiffKindTypeChanged {
						if d.Expected != valueRedacted || d.Actual != valueRedacted {
							t.Fatalf("want both sides redacted on param 1; got %+v", d)
						}
						sawRetyped1 = true
					}
					if d.Path == "body.parameters.2.value" && d.Kind == models.DiffKindTypeChanged {
						if d.Expected != valueRedacted || d.Actual != valueRedacted {
							t.Fatalf("want both sides redacted on param 2; got %+v", d)
						}
						sawRetyped2 = true
					}
				}
				if !sawRetyped1 || !sawRetyped2 {
					t.Fatalf("expected redacted type_changed on params 1 and 2; got %+v", diffs2)
				}
			})

			t.Run("allowsDrift with nil gate engine uses strict compare engine noise for report", func(t *testing.T) {
				rec := &models.Mock{Name: "rec-drift", Kind: models.MySQL}
				rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: executeBundle(0, param{Type: 254, Value: "a"}, param{Type: 3, Value: int32(5)})}}
				rec.Spec.ReqBodyNoise = map[string][]string{
					"body.parameters.0.value": {},
					"body.parameters.1.value": {},
				}
				live := executeBundle(0, param{Type: 254, Value: "b"}, param{Type: 254, Value: "5"})
				// nil engine simulates noise flags off while allowsDrift still runs exact serving.
				gate := newStrictGate(nil, zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, "", nil)
				if gate.allowsDrift(rec) {
					t.Fatal("expected allowsDrift to reject retyped learned param 1")
				}
				diffs := gate.reportDiffs(gate.closestPlaces)
				if len(diffs) != 1 || diffs[0].Path != "body.parameters.1.value" ||
					diffs[0].Kind != models.DiffKindTypeChanged ||
					diffs[0].Expected != "number: 5" || diffs[0].Actual != "string: 5" {
					t.Fatalf("expected only retyped body.parameters.1.value (ignoring same-type learned param 0 drift); got %+v", diffs)
				}
				// resetDiagnostics clears the lazy diff input too.
				gate.resetDiagnostics()
				if cleared := gate.reportDiffs(gate.closestPlaces); len(cleared) != 0 {
					t.Fatalf("expected empty reportDiffs after resetDiagnostics; got %+v", cleared)
				}
			})
		})
	}
}
