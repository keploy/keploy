package replayer

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const loopQuery = "SELECT ? * 10 AS v, CONCAT(?, '!') AS s"

// loopExecuteMock is a recorded EXECUTE of loopQuery binding n and s,
// answered with an OK whose affected-rows count is rows.
func loopExecuteMock(name string, at time.Time, n int32, s string, rows uint64) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks", "connID": "0"}
	m.Spec.ReqTimestampMock = at
	req := mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 30}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 1, IterationCount: 1, ParameterCount: 2, NewParamsBindFlag: 1,
			NullBitmap: []byte{0}, Parameters: []mysql.Parameter{
				{Type: uint16(mysql.FieldTypeLong), Value: n},
				{Type: uint16(mysql.FieldTypeVarString), Value: s},
			}},
	}
	req.SetStoredQuery(loopQuery)
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: req}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: "OK"},
		Message: &mysql.OKPacket{Header: mysql.OK, AffectedRows: rows, StatusFlags: 2},
	}}}
	return m
}

func loopExecutePayload(stmtID uint32, bound bool, n int32, s string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = append(p, 0x00)
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeLong), 0x00, byte(mysql.FieldTypeVarString), 0x00)
	} else {
		p = append(p, 0x00)
	}
	p = binary.LittleEndian.AppendUint32(p, uint32(n))
	return append(append(p, byte(len(s))), s...)
}

// A replayed client re-executes a statement without its parameter types
// (new_params_bind_flag 0), as Connector/J does: the replayer reads the
// command with the types the statement was last sent with, and answers it
// with the mock recorded for its values, plain and strict, the recording as
// this keploy makes it (the EXECUTEs sent without types recorded with the
// statement's types). It used to fail to decode it, and the connection was
// dropped.
func TestReplay_AnExecuteWithoutTypesIsMatchedOnItsValues(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "strict"}[strict], func(t *testing.T) {
			at := stmtT0.Add(time.Second)
			prepare := preparedMock("mock-prepare", "0", 1, loopQuery, stmtT0)
			unbound := func(m *models.Mock) *models.Mock {
				m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).NewParamsBindFlag = 0
				return m
			}
			db := &fakeMockDb{session: []*models.Mock{
				nativeHandshake(),
				prepare,
				loopExecuteMock("mock-execute-1", at, 1001, "item-1", 1),
				unbound(loopExecuteMock("mock-execute-2", at.Add(time.Millisecond), 1002, "item-2", 2)),
				unbound(loopExecuteMock("mock-execute-3", at.Add(2*time.Millisecond), 1003, "item-3", 3)),
			}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
			client, done := replayWith(t, db, models.OutgoingOptions{MockNoiseStrict: strict})
			client.greetAndAnswer("app", mysql.Native)
			if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
				t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
			}
			client.send(0, append([]byte{mysql.COM_STMT_PREPARE}, loopQuery...))
			prepareOK := client.read()
			if len(prepareOK) < 5 || prepareOK[0] != 0x00 {
				t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", prepareOK)
			}
			stmtID := binary.LittleEndian.Uint32(prepareOK[1:5])

			for i, tc := range []struct {
				bound bool
				n     int32
				s     string
			}{{true, 1001, "item-1"}, {false, 1003, "item-3"}, {false, 1002, "item-2"}} {
				client.send(0, loopExecutePayload(stmtID, tc.bound, tc.n, tc.s))
				ok := client.read()
				if len(ok) < 2 || ok[0] != mysql.OK || ok[1] != byte(tc.n-1000) {
					t.Fatalf("EXECUTE %d (%d, %s, types sent: %v) was answered with %x, want the OK recorded for its values (%d affected rows)", i, tc.n, tc.s, tc.bound, ok, tc.n-1000)
				}
			}
			_ = client.c.Close()
			<-done
		})
	}
}

// replayWith is replay with the replay's options.
func replayWith(t *testing.T, db integrations.MockMemDb, opts models.OutgoingOptions) (mysqlClient, <-chan error) {
	t.Helper()
	client, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
	ret := make(chan error, 1)
	go func() {
		ret <- Replay(context.Background(), zap.NewNop(), agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, opts)
		_ = agentEnd.Close()
	}()
	return mysqlClient{t: t, c: client}, ret
}

const concatQuery = "SELECT CONCAT(?, '-', ?) AS s"

// concatExecuteMock is a recorded EXECUTE of concatQuery binding a and b with
// the type typ, sent with its types or not (bound), answered with an OK whose
// affected-rows count is rows.
func concatExecuteMock(name string, at time.Time, lt models.Lifetime, bound bool, typ mysql.FieldType, a, b string, rows uint64) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = lt
	m.Spec.Metadata = map[string]string{"type": "mocks", "connID": "0"}
	m.Spec.ReqTimestampMock = at
	var flag byte
	if bound {
		flag = 1
	}
	req := mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 30}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 1, IterationCount: 1, ParameterCount: 2, NewParamsBindFlag: flag,
			NullBitmap: []byte{0}, Parameters: []mysql.Parameter{{Type: uint16(typ), Value: a}, {Type: uint16(typ), Value: b}}},
	}
	req.SetStoredQuery(concatQuery)
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: req}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: "OK"},
		Message: &mysql.OKPacket{Header: mysql.OK, AffectedRows: rows, StatusFlags: 2},
	}}}
	return m
}

func concatExecutePayload(stmtID uint32, bound bool, a, b string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = append(p, 0x00)
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeVarString), 0x00, byte(mysql.FieldTypeVarString), 0x00)
	} else {
		p = append(p, 0x00)
	}
	p = append(append(p, byte(len(a))), a...)
	return append(append(p, byte(len(b))), b...)
}

// A released keploy recorded an EXECUTE sent without its types
// (new_params_bind_flag 0) whose parameters were all strings: reading every
// value as a string read them right. It stored them with the type
// FieldTypeString (254), which was never on the wire, while this replayer
// reads the live one with the statement's types (VAR_STRING, 253). Types a
// packet did not send are not compared, so each EXECUTE is still answered
// with the mock recorded for its values, in any order, in either tier, and
// strict matching takes the types for no drift. Comparing them, the session
// tier answered every such EXECUTE with the first mock of its query, the
// per-test tier answered them in recorded order, and strict matching
// rejected every one.
func TestReplay_AnOlderKeploysRecordingOfExecutesWithoutTypesIsMatchedOnItsValues(t *testing.T) {
	type exec struct {
		bound bool
		a, b  string
		rows  byte
	}
	orders := map[string][]exec{
		"recorded order":                  {{true, "a1", "b1", 1}, {false, "a2", "b2", 2}, {false, "a3", "b3", 3}},
		"another order":                   {{true, "a1", "b1", 1}, {false, "a3", "b3", 3}, {false, "a2", "b2", 2}},
		"types sent by the live one only": {{true, "a3", "b3", 3}, {true, "a2", "b2", 2}, {false, "a1", "b1", 1}},
	}
	for _, tier := range []models.Lifetime{models.LifetimeSession, models.LifetimePerTest} {
		for _, strict := range []bool{false, true} {
			for name, order := range orders {
				t.Run(tier.String()+map[bool]string{false: "", true: "/strict"}[strict]+"/"+name, func(t *testing.T) {
					at := stmtT0.Add(time.Second)
					execs := []*models.Mock{
						concatExecuteMock("mock-execute-1", at, tier, true, mysql.FieldTypeVarString, "a1", "b1", 1),
						concatExecuteMock("mock-execute-2", at.Add(time.Millisecond), tier, false, mysql.FieldTypeString, "a2", "b2", 2),
						concatExecuteMock("mock-execute-3", at.Add(2*time.Millisecond), tier, false, mysql.FieldTypeString, "a3", "b3", 3),
					}
					db := &fakeMockDb{session: []*models.Mock{nativeHandshake(), preparedMock("mock-prepare", "0", 1, concatQuery, stmtT0)},
						winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
					if tier == models.LifetimePerTest {
						db.perTest = execs
					} else {
						db.session = append(db.session, execs...)
					}
					client, done := replayWith(t, db, models.OutgoingOptions{MockNoiseStrict: strict})
					client.greetAndAnswer("app", mysql.Native)
					if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
						t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
					}
					client.send(0, append([]byte{mysql.COM_STMT_PREPARE}, concatQuery...))
					prepareOK := client.read()
					if len(prepareOK) < 5 || prepareOK[0] != 0x00 {
						t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", prepareOK)
					}
					stmtID := binary.LittleEndian.Uint32(prepareOK[1:5])
					for i, e := range order {
						client.send(0, concatExecutePayload(stmtID, e.bound, e.a, e.b))
						ok := client.read()
						if len(ok) < 2 || ok[0] != mysql.OK || ok[1] != e.rows {
							t.Fatalf("EXECUTE %d (%s, %s, types sent: %v) was answered with %x, want the OK recorded for its values (%d affected rows)", i, e.a, e.b, e.bound, ok, e.rows)
						}
					}
					_ = client.c.Close()
					<-done
				})
			}
		}
	}
}

// Parameter types, unsigned flags and names are compared only when both
// packets sent them (new_params_bind_flag 1): a packet sent without them
// carries filled-in ones.
func TestMatchExecute_TypesAreComparedOnlyWhenBothPacketsSentThem(t *testing.T) {
	const q = "SELECT CONCAT(?, '-', ?) AS s"
	bundle := func(flag byte, p mysql.Parameter) mysql.PacketBundle {
		return mysql.PacketBundle{
			Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 16}, Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{Status: 0x17, IterationCount: 1, ParameterCount: 1, NewParamsBindFlag: flag,
				NullBitmap: []byte{0}, Parameters: []mysql.Parameter{p}},
		}
	}
	str := func(typ mysql.FieldType) mysql.Parameter { return mysql.Parameter{Type: uint16(typ), Value: "x"} }
	for _, tc := range []struct {
		name             string
		expFlag, actFlag byte
		exp, act         mysql.Parameter
		wantDefinitive   bool
	}{
		{"both sent the same type", 1, 1, str(mysql.FieldTypeVarString), str(mysql.FieldTypeVarString), true},
		{"both sent, other types", 1, 1, str(mysql.FieldTypeString), str(mysql.FieldTypeVarString), false},
		{"both sent, other unsigned flags", 1, 1, mysql.Parameter{Type: 253, Unsigned: true, Value: "x"}, str(mysql.FieldTypeVarString), false},
		{"both sent, other names", 1, 1, mysql.Parameter{Type: 253, Name: "n", Value: "x"}, str(mysql.FieldTypeVarString), false},
		{"neither sent", 0, 0, str(mysql.FieldTypeString), str(mysql.FieldTypeVarString), true},
		{"the recorded one did not", 0, 1, str(mysql.FieldTypeString), str(mysql.FieldTypeVarString), true},
		{"the live one did not", 1, 0, str(mysql.FieldTypeVarString), str(mysql.FieldTypeString), true},
		{"the live one sent no unsigned flag", 1, 0, mysql.Parameter{Type: 253, Unsigned: true, Value: "x"}, str(mysql.FieldTypeVarString), true},
		{"the live one sent no name", 1, 0, mysql.Parameter{Type: 253, Name: "n", Value: "x"}, str(mysql.FieldTypeVarString), true},
		{"neither sent, other values", 0, 0, str(mysql.FieldTypeString), mysql.Parameter{Type: 253, Value: "y"}, false},
	} {
		def, _, _ := matchStmtExecutePacketQueryAware(zap.NewNop(), bundle(tc.expFlag, tc.exp), bundle(tc.actFlag, tc.act), q, q, "m", util.NewNoiseChecker(nil))
		if def != tc.wantDefinitive {
			t.Errorf("%s: definitive = %v, want %v", tc.name, def, tc.wantDefinitive)
		}
	}
}

// A released keploy read the values of an EXECUTE sent without its types as
// strings. A small integer (its first byte no more than the bytes after it)
// read so: INT 3 then "q" became "\x00\x00\x00" and "q", recorded typed
// FieldTypeString; its replayer read the live packet the same way, and
// matched. This replayer reads the live packet with the statement's types,
// and, for such a recording, as that keploy read it too
// (StmtExecutePacket.LegacyParameters): each EXECUTE is still answered with
// the mock recorded for it, in either tier, plain and strict.
func TestReplay_AnOlderKeploysReadingOfSmallIntegersIsMatchedAsItRead(t *testing.T) {
	type exec struct {
		bound bool
		n     int32
		s     string
		rows  byte
	}
	order := []exec{{true, 1001, "item-1", 1}, {false, 3, "q", 3}, {false, 3, "p", 2}}
	str := uint16(mysql.FieldTypeString)
	for _, tier := range []models.Lifetime{models.LifetimeSession, models.LifetimePerTest} {
		for _, strict := range []bool{false, true} {
			t.Run(tier.String()+map[bool]string{false: "", true: "/strict"}[strict], func(t *testing.T) {
				at := stmtT0.Add(time.Second)
				first := loopExecuteMock("mock-execute-1", at, 1001, "item-1", 1)
				first.TestModeInfo.Lifetime = tier
				garbled := func(name string, at time.Time, s string, rows uint64) *models.Mock {
					m := loopExecuteMock(name, at, 0, "", rows)
					m.TestModeInfo.Lifetime = tier
					msg := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
					msg.NewParamsBindFlag = 0
					msg.Parameters = []mysql.Parameter{{Type: str, Value: "\x00\x00\x00"}, {Type: str, Value: s}}
					return m
				}
				execs := []*models.Mock{
					first,
					garbled("mock-execute-2", at.Add(time.Millisecond), "p", 2),
					garbled("mock-execute-3", at.Add(2*time.Millisecond), "q", 3),
				}
				db := &fakeMockDb{session: []*models.Mock{nativeHandshake(), preparedMock("mock-prepare", "0", 1, loopQuery, stmtT0)},
					winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
				if tier == models.LifetimePerTest {
					db.perTest = execs
				} else {
					db.session = append(db.session, execs...)
				}
				client, done := replayWith(t, db, models.OutgoingOptions{MockNoiseStrict: strict})
				client.greetAndAnswer("app", mysql.Native)
				if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
					t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
				}
				client.send(0, append([]byte{mysql.COM_STMT_PREPARE}, loopQuery...))
				prepareOK := client.read()
				if len(prepareOK) < 5 || prepareOK[0] != 0x00 {
					t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", prepareOK)
				}
				stmtID := binary.LittleEndian.Uint32(prepareOK[1:5])
				for i, e := range order {
					client.send(0, loopExecutePayload(stmtID, e.bound, e.n, e.s))
					ok := client.read()
					if len(ok) < 2 || ok[0] != mysql.OK || ok[1] != e.rows {
						t.Fatalf("EXECUTE %d (%d, %s, types sent: %v) was answered with %x, want the OK recorded for it (%d affected rows)", i, e.n, e.s, e.bound, ok, e.rows)
					}
				}
				_ = client.c.Close()
				<-done
			})
		}
	}
}

// A recording this keploy made of an EXECUTE sent without its types carries
// the statement's types and the values read with them, so it is compared with
// the live packet as this keploy reads it, never as an older keploy did: a
// small integer read so would be garbage, and strict matching would reject
// the mock recorded for it.
func TestReplay_ThisKeploysRecordingIsNotReadAsAnOlderKeployWould(t *testing.T) {
	at := stmtT0.Add(time.Second)
	unbound := loopExecuteMock("mock-execute-2", at.Add(time.Millisecond), 3, "q", 2)
	unbound.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).NewParamsBindFlag = 0
	db := &fakeMockDb{session: []*models.Mock{
		nativeHandshake(),
		preparedMock("mock-prepare", "0", 1, loopQuery, stmtT0),
		loopExecuteMock("mock-execute-1", at, 2, "p", 1),
		unbound,
	}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	client, done := replayWith(t, db, models.OutgoingOptions{MockNoiseStrict: true})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_STMT_PREPARE}, loopQuery...))
	prepareOK := client.read()
	if len(prepareOK) < 5 || prepareOK[0] != 0x00 {
		t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", prepareOK)
	}
	stmtID := binary.LittleEndian.Uint32(prepareOK[1:5])
	for i, tc := range []struct {
		bound bool
		n     int32
		s     string
		rows  byte
	}{{true, 2, "p", 1}, {false, 3, "q", 2}} {
		client.send(0, loopExecutePayload(stmtID, tc.bound, tc.n, tc.s))
		if ok := client.read(); len(ok) < 2 || ok[0] != mysql.OK || ok[1] != tc.rows {
			t.Fatalf("EXECUTE %d (%d, %s, types sent: %v) was answered with %x, want the OK recorded for it (%d affected rows)", i, tc.n, tc.s, tc.bound, ok, tc.rows)
		}
	}
	_ = client.c.Close()
	<-done
}
