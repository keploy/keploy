package wire

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// executePacket is a COM_STMT_EXECUTE of stmtID binding an INT (LONG) and a
// VAR_STRING, framed as a packet, with the parameter types on the wire when
// bound is true (new_params_bind_flag 1) and without them otherwise.
func executePacket(stmtID uint32, bound bool, n int32, s string) []byte {
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
	p = append(append(p, byte(len(s))), s...)
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
}

// executeStringInt is executePacket with the parameters the other way round:
// a VAR_STRING, then an INT.
func executeStringInt(stmtID uint32, bound bool, s string, n int32) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = append(p, 0x00)
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeVarString), 0x00, byte(mysql.FieldTypeLong), 0x00)
	} else {
		p = append(p, 0x00)
	}
	p = append(append(p, byte(len(s))), s...)
	p = binary.LittleEndian.AppendUint32(p, uint32(n))
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
}

func closePacket(stmtID uint32) []byte {
	p := binary.LittleEndian.AppendUint32([]byte{mysql.COM_STMT_CLOSE}, stmtID)
	return append([]byte{byte(len(p)), 0, 0, 0}, p...)
}

// A connection's EXECUTEs that send no parameter types are decoded with the
// types its statement's last EXECUTE that sent them carried, per statement,
// until a COM_STMT_CLOSE ends the statement. Recording and replay decode a
// connection's commands this way alike.
func TestDecodePayload_AnExecuteWithoutTypesTakesItsStatementsLast(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			client, peer := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
			decodeCtx := &DecodeContext{
				Mode: mode, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(),
				PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}, 8: {StatementID: 8, NumParams: 2}},
			}
			decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
			decode := func(packet []byte) (*mysql.PacketBundle, error) {
				decodeCtx.LastOp.Store(client, RESET)
				return DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
			}
			params := func(packet []byte) []mysql.Parameter {
				t.Helper()
				b, err := decode(packet)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return b.Message.(*mysql.StmtExecutePacket).Parameters
			}

			params(executePacket(7, true, 1001, "item-1"))
			// Statement 8 sends other types, the other way round.
			params(executeStringInt(8, true, "key-1", 5))
			if got := params(executePacket(7, false, 1002, "item-2")); got[0].Value != int32(1002) || got[1].Value != "item-2" {
				t.Fatalf("statement 7 without types decoded %+v, want 1002 and item-2", got)
			}
			if got := params(executeStringInt(8, false, "key-2", 6)); got[0].Value != "key-2" || got[1].Value != int32(6) {
				t.Fatalf("statement 8 without types decoded %+v, want key-2 and 6", got)
			}

			if _, err := decode(closePacket(7)); err != nil {
				t.Fatal(err)
			}
			if _, err := decode(executePacket(7, false, 1003, "item-3")); err == nil || !strings.Contains(err.Error(), "statement 7") {
				t.Fatalf("after its COM_STMT_CLOSE, statement 7 without types decoded (%v); its types end with it", err)
			}
		})
	}
}

// executeWithAttribute is executePacket framed for CLIENT_QUERY_ATTRIBUTES:
// a parameter count, and a name after each type. With attr, a query
// attribute (OpenTelemetry's traceparent) follows the statement's two
// placeholders, as its third parameter; Connector/J sends its types whenever
// it sends one. attrNull sends that attribute as a NULL.
func executeWithAttribute(stmtID uint32, bound, attr, attrNull bool, n int32, s string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, mysql.PARAMETER_COUNT_AVAILABLE)
	p = binary.LittleEndian.AppendUint32(p, 1)
	count, bitmap := byte(2), byte(0)
	if attr {
		count = 3
		if attrNull {
			bitmap = 0x04
		}
	}
	p = append(p, count, bitmap)
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeLong), 0x00, 0x00, byte(mysql.FieldTypeVarString), 0x00, 0x00)
		if attr {
			p = append(append(p, byte(mysql.FieldTypeVarString), 0x00, 11), "traceparent"...)
		}
	} else {
		p = append(p, 0x00)
	}
	p = binary.LittleEndian.AppendUint32(p, uint32(n))
	p = append(append(p, byte(len(s))), s...)
	if attr && !attrNull {
		p = append(append(p, 3), "abc"...)
	}
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
}

// The server keeps the types of a statement's placeholders, not of the query
// attributes an EXECUTE sent after them, and reads an EXECUTE without types
// with those: so does the decoder, which used to take the attribute's too and
// then refuse an EXECUTE that sent fewer parameters. A flag-0 EXECUTE sends
// no names either, so it is decoded with none. An attribute on a flag-0
// EXECUTE has no type: the server takes it only as a NULL, and so does the
// decoder.
func TestDecodePayload_AnExecuteWithoutTypesTakesItsPlaceholdersTypesOnly(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	decodeCtx := &DecodeContext{
		Mode: models.MODE_RECORD, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(),
		ClientCapabilities: mysql.CLIENT_QUERY_ATTRIBUTES | mysql.CLIENT_PROTOCOL_41,
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}},
	}
	decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
	decode := func(packet []byte) (*mysql.StmtExecutePacket, error) {
		decodeCtx.LastOp.Store(client, RESET)
		b, err := DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
		if err != nil {
			return nil, err
		}
		return b.Message.(*mysql.StmtExecutePacket), nil
	}

	first, err := decode(executeWithAttribute(7, true, true, false, 1001, "item-1"))
	if err != nil || len(first.Parameters) != 3 || first.Parameters[2].Name != "traceparent" {
		t.Fatalf("the EXECUTE with its types and an attribute decoded %+v (%v)", first, err)
	}
	for name, tc := range map[string]struct {
		attr, attrNull bool
		want           int
	}{
		"no attribute":     {false, false, 2},
		"a NULL attribute": {true, true, 3},
	} {
		got, err := decode(executeWithAttribute(7, false, tc.attr, tc.attrNull, 1002, "item-2"))
		if err != nil {
			t.Fatalf("%s: the EXECUTE without types, which the server reads with LONG and VAR_STRING, failed to decode: %v", name, err)
		}
		if p := got.Parameters; len(p) != tc.want || p[0].Type != uint16(mysql.FieldTypeLong) || p[0].Value != int32(1002) ||
			p[1].Type != uint16(mysql.FieldTypeVarString) || p[1].Value != "item-2" || (tc.attr && p[2].Value != nil) {
			t.Fatalf("%s: decoded %+v, want LONG 1002, VAR_STRING item-2 and a NULL attribute when sent", name, p)
		}
		for i, p := range got.Parameters {
			if p.Name != "" {
				t.Errorf("%s: parameter %d carries the name %q, which an EXECUTE without types does not send", name, i, p.Name)
			}
		}
	}
	if _, err := decode(executeWithAttribute(7, false, true, false, 1003, "item-3")); err == nil || !strings.Contains(err.Error(), "parameter 2") {
		t.Fatalf("an attribute with a value on an EXECUTE without types decoded (%v); it has no type to read it with", err)
	}
}

// The reading a released keploy made of an EXECUTE sent without its types is
// kept at replay only, to match what it recorded; a recording never carries
// it.
func TestDecodePayload_AnOlderKeploysReadingIsKeptAtReplayOnly(t *testing.T) {
	for mode, want := range map[models.Mode]bool{models.MODE_RECORD: false, models.MODE_TEST: true} {
		client, peer := net.Pipe()
		decodeCtx := &DecodeContext{
			Mode: mode, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(),
			PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}},
		}
		decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
		var got *mysql.StmtExecutePacket
		for _, packet := range [][]byte{executePacket(7, true, 1001, "item-1"), executePacket(7, false, 3, "q")} {
			decodeCtx.LastOp.Store(client, RESET)
			b, err := DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			got = b.Message.(*mysql.StmtExecutePacket)
		}
		if (got.LegacyParameters != nil) != want {
			t.Errorf("%s: the older keploy's reading is %+v; want it kept: %v", mode, got.LegacyParameters, want)
		}
		_ = client.Close()
		_ = peer.Close()
	}
}

func longDataPacket(stmtID uint32, paramID uint16, chunk []byte) []byte {
	p := []byte{mysql.COM_STMT_SEND_LONG_DATA}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = binary.LittleEndian.AppendUint16(p, paramID)
	p = append(p, chunk...)
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
}

// executeBlobAndString frames a COM_STMT_EXECUTE for a 2-parameter (BLOB,
// VAR_STRING) statement. When blobInline is false, param 0 is omitted from the
// wire payload because it was streamed ahead of time via COM_STMT_SEND_LONG_DATA.
func executeBlobAndString(stmtID uint32, bound, blobInline bool, blob, s string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = append(p, 0x00) // null bitmap: neither param is NULL
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeBLOB), 0x00, byte(mysql.FieldTypeVarString), 0x00)
	} else {
		p = append(p, 0x00)
	}
	if blobInline {
		p = append(append(p, byte(len(blob))), blob...)
	}
	p = append(append(p, byte(len(s))), s...)
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
}

// MySQL discards buffered COM_STMT_SEND_LONG_DATA parameters once the statement
// executes or closes. If the decoder keeps stale LongDataParams entries across
// executions or after COM_STMT_CLOSE (when a statement ID is reused), the next
// inline EXECUTE skips the parameter bytes and mis-parses the remaining fields.
func TestDecodePayload_LongDataParamsClearedOnExecuteAndClose(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			client, peer := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
			decodeCtx := &DecodeContext{
				Mode: mode, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(),
				PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{
					7: {StatementID: 7, NumParams: 2},
					8: {StatementID: 8, NumParams: 2},
				},
			}
			decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
			decode := func(packet []byte) (*mysql.PacketBundle, error) {
				decodeCtx.LastOp.Store(client, RESET)
				return DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
			}

			// Stream long data on both statements 7 and 8.
			if _, err := decode(longDataPacket(7, 0, []byte("streamed-7"))); err != nil {
				t.Fatalf("send long data stmt 7: %v", err)
			}
			if _, err := decode(longDataPacket(8, 0, []byte("streamed-8"))); err != nil {
				t.Fatalf("send long data stmt 8: %v", err)
			}

			// First execute on stmt 7 consumes its buffered long data and must
			// leave stmt 8's buffered long data intact.
			b1, err := decode(executeBlobAndString(7, true, false, "", "run-1"))
			if err != nil {
				t.Fatalf("first execute stmt 7: %v", err)
			}
			if p := b1.Message.(*mysql.StmtExecutePacket).Parameters; p[0].Value != nil || p[1].Value != "run-1" {
				t.Fatalf("first execute stmt 7 decoded %+v, want [nil, run-1]", p)
			}
			if _, ok := decodeCtx.LongDataParams[7]; ok {
				t.Fatalf("stmt 7 LongDataParams remained after COM_STMT_EXECUTE: %+v", decodeCtx.LongDataParams[7])
			}
			if !decodeCtx.LongDataParams[8][0] {
				t.Fatalf("stmt 8 LongDataParams was cleared by stmt 7 execute")
			}

			// Second execute on stmt 7 sends both parameters inline without
			// rebinding types; both must decode accurately.
			b2, err := decode(executeBlobAndString(7, false, true, "inline-blob", "run-2"))
			if err != nil {
				t.Fatalf("second execute stmt 7: %v", err)
			}
			if p := b2.Message.(*mysql.StmtExecutePacket).Parameters; p[0].Value != "inline-blob" || p[1].Value != "run-2" {
				t.Fatalf("second execute stmt 7 decoded %+v, want [inline-blob, run-2]", p)
			}

			// Stream long data for stmt 7 again, then close stmt 7 without
			// executing. Closing must clear stmt 7's LongDataParams without
			// touching stmt 8, so a subsequent execute reusing ID 7 reads
			// inline parameters properly.
			if _, err := decode(longDataPacket(7, 0, []byte("abandoned"))); err != nil {
				t.Fatalf("send long data before close: %v", err)
			}
			if _, err := decode(closePacket(7)); err != nil {
				t.Fatalf("close stmt 7: %v", err)
			}
			if _, ok := decodeCtx.LongDataParams[7]; ok {
				t.Fatalf("stmt 7 LongDataParams remained after COM_STMT_CLOSE: %+v", decodeCtx.LongDataParams[7])
			}
			if !decodeCtx.LongDataParams[8][0] {
				t.Fatalf("stmt 8 LongDataParams was cleared by stmt 7 close")
			}

			b3, err := decode(executeBlobAndString(7, true, true, "reused-blob", "run-3"))
			if err != nil {
				t.Fatalf("execute on reused stmt 7: %v", err)
			}
			if p := b3.Message.(*mysql.StmtExecutePacket).Parameters; p[0].Value != "reused-blob" || p[1].Value != "run-3" {
				t.Fatalf("reused stmt 7 decoded %+v, want [reused-blob, run-3]", p)
			}

			// A COM_STMT_EXECUTE that fails to decode (e.g. an unsupported
			// parameter type on param 1) must still clear stmt 7's buffered
			// long data so the next decodable inline EXECUTE on the same
			// connection is not poisoned.
			if _, err := decode(longDataPacket(7, 0, []byte("streamed-before-err"))); err != nil {
				t.Fatalf("send long data before failing execute: %v", err)
			}
			badExec := []byte{mysql.COM_STMT_EXECUTE}
			badExec = binary.LittleEndian.AppendUint32(badExec, 7)
			badExec = append(badExec, 0x00)
			badExec = binary.LittleEndian.AppendUint32(badExec, 1)
			badExec = append(badExec, 0x00) // null bitmap
			badExec = append(badExec, 0x01, byte(mysql.FieldTypeBLOB), 0x00, byte(mysql.FieldTypeNewDecimal), 0x00)
			badExec = append(badExec, 0x01, '1')
			badExec = append([]byte{byte(len(badExec)), byte(len(badExec) >> 8), byte(len(badExec) >> 16), 0}, badExec...)
			if _, err := decode(badExec); err == nil {
				t.Fatalf("expected unsupported parameter type execute to fail decode")
			}
			if _, ok := decodeCtx.LongDataParams[7]; ok {
				t.Fatalf("stmt 7 LongDataParams remained after failing COM_STMT_EXECUTE: %+v", decodeCtx.LongDataParams[7])
			}
			b4, err := decode(executeBlobAndString(7, true, true, "after-err-blob", "run-4"))
			if err != nil {
				t.Fatalf("inline execute after failing execute: %v", err)
			}
			if p := b4.Message.(*mysql.StmtExecutePacket).Parameters; p[0].Value != "after-err-blob" || p[1].Value != "run-4" {
				t.Fatalf("after-err stmt 7 decoded %+v, want [after-err-blob, run-4]", p)
			}

			// Confirm stmt 8's buffered long data remained usable end-to-end.
			b8, err := decode(executeBlobAndString(8, true, false, "", "stmt8-run"))
			if err != nil {
				t.Fatalf("execute stmt 8: %v", err)
			}
			if p := b8.Message.(*mysql.StmtExecutePacket).Parameters; p[0].Value != nil || p[1].Value != "stmt8-run" {
				t.Fatalf("stmt 8 decoded %+v, want [nil, stmt8-run]", p)
			}
			if _, ok := decodeCtx.LongDataParams[8]; ok {
				t.Fatalf("stmt 8 LongDataParams remained after COM_STMT_EXECUTE: %+v", decodeCtx.LongDataParams[8])
			}
		})
	}
}
