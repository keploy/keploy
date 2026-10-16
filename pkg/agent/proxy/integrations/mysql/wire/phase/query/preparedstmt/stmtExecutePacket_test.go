package preparedstmt

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestDecodeStmtExecute_ParameterCountFix(t *testing.T) {
	// Create a mock prepared statement with 3 parameters
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 945,
		NumParams:   3,
		NumColumns:  10,
	}

	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{
		945: stmtPrepOk,
	}

	// Mock COM_STMT_EXECUTE packet data (39 bytes as seen in the logs)
	// This represents a packet with statement ID 945, but with 3 NULL parameters
	data := []byte{
		0x17,                   // COM_STMT_EXECUTE command
		0xb1, 0x03, 0x00, 0x00, // Statement ID (945)
		0x00,                   // Flags
		0x01, 0x00, 0x00, 0x00, // Iteration count (1)
		0x07, // NULL bitmap (1 byte for 3 params, all NULL: bits 0-2)
		0x00, // New params bind flag (0 = reuse previous types)
		// No parameter type data since new_params_bind_flag = 0
		// No parameter value data since all parameters are NULL
	}

	// Pad to 39 bytes total to match the logs
	for len(data) < 39 {
		data = append(data, 0x00)
	}

	logger := zap.NewNop()
	ctx := context.Background()

	// Test with CLIENT_QUERY_ATTRIBUTES disabled (common case)
	clientCapabilities := uint32(0)

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities, nil, nil)

	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}

	// Verify that ParameterCount is set correctly from the prepared statement
	if packet.ParameterCount != 3 {
		t.Errorf("Expected ParameterCount to be 3, got %d", packet.ParameterCount)
	}

	// Verify other fields
	if packet.StatementID != 945 {
		t.Errorf("Expected StatementID to be 945, got %d", packet.StatementID)
	}

	if packet.Status != mysql.COM_STMT_EXECUTE {
		t.Errorf("Expected Status to be COM_STMT_EXECUTE (%d), got %d", mysql.COM_STMT_EXECUTE, packet.Status)
	}
}

func TestDecodeStmtExecute_NoParameters(t *testing.T) {
	// Create a mock prepared statement with 0 parameters
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 946,
		NumParams:   0,
		NumColumns:  10,
	}

	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{
		946: stmtPrepOk,
	}

	// Mock COM_STMT_EXECUTE packet data with no parameters
	data := []byte{
		0x17,                   // COM_STMT_EXECUTE command
		0xb2, 0x03, 0x00, 0x00, // Statement ID (946)
		0x00,                   // Flags
		0x01, 0x00, 0x00, 0x00, // Iteration count (1)
		// No NULL bitmap since no parameters
		// No new params bind flag since no parameters
	}

	logger := zap.NewNop()
	ctx := context.Background()
	clientCapabilities := uint32(0)

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities, nil, nil)

	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}

	// Verify that ParameterCount is 0 and the function returns early
	if packet.ParameterCount != 0 {
		t.Errorf("Expected ParameterCount to be 0, got %d", packet.ParameterCount)
	}

	if packet.StatementID != 946 {
		t.Errorf("Expected StatementID to be 946, got %d", packet.StatementID)
	}
}

func TestDecodeStmtExecute_DateTimeThenString(t *testing.T) {
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 12345,
		NumParams:   2,
	}

	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{
		12345: stmtPrepOk,
	}

	data := []byte{
		0x17,                   // COM_STMT_EXECUTE command
		0x39, 0x30, 0x00, 0x00, // Statement ID (12345)
		0x00,                   // Flags
		0x01, 0x00, 0x00, 0x00, // Iteration count (1)
		0x00,       // NULL bitmap (2 params, none NULL)
		0x01,       // New params bind flag
		0x0c, 0x00, // FieldTypeDateTime
		0xfd, 0x00, // FieldTypeVarString
		0x07, 0xea, 0x07, 0x01, 0x1a, 0x12, 0x32, 0x2d, // 2026-01-26 18:50:45
		0x0d, 'U', 'p', 'd', 'a', 't', 'e', 'd', ' ', 'T', 'i', 't', 'l', 'e',
	}

	logger := zap.NewNop()
	ctx := context.Background()
	clientCapabilities := uint32(0)

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities, nil, nil)
	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}

	if packet.ParameterCount != 2 {
		t.Fatalf("Expected ParameterCount to be 2, got %d", packet.ParameterCount)
	}

	if len(packet.Parameters) != 2 {
		t.Fatalf("Expected 2 parameters, got %d", len(packet.Parameters))
	}

	if packet.Parameters[0].Value != "2026-01-26 18:50:45" {
		t.Fatalf("Expected datetime param value, got %#v", packet.Parameters[0].Value)
	}

	if packet.Parameters[1].Value != "Updated Title" {
		t.Fatalf("Expected string param value, got %#v", packet.Parameters[1].Value)
	}
}

// TestDecodeStmtExecute_QueryAttrsExtension reproduces the mysql2 (Node.js)
// wire format against MySQL 8: CLIENT_QUERY_ATTRIBUTES is negotiated and
// PARAMETER_COUNT_AVAILABLE (0x08) is set in `flags`. In that mode the wire
// carries an extra length-encoded `parameter_count` after `iteration_count`
// and each declared parameter type is followed by a length-encoded
// parameter_name string. The old decoder skipped both, mis-reading
// parameter_count as null_bitmap[0] and corrupting every captured bind
// value (the `value: /Q==` / `value: null` bug).
func TestDecodeStmtExecute_QueryAttrsExtension(t *testing.T) {
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 4,
		NumParams:   1,
	}
	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{4: stmtPrepOk}

	// Bytes captured from mysql2/promise (Node.js) running against MySQL 8.0:
	//   SELECT id FROM users WHERE username = ? LIMIT 1  bound to 'admin'.
	data := []byte{
		0x17,                   // COM_STMT_EXECUTE
		0x04, 0x00, 0x00, 0x00, // statement_id = 4
		0x08,                   // flags = PARAMETER_COUNT_AVAILABLE
		0x01, 0x00, 0x00, 0x00, // iteration_count = 1
		0x01,       // length-encoded parameter_count = 1
		0x00,       // null_bitmap (param 0 NOT null)
		0x01,       // new_params_bind_flag = 1
		0xfd, 0x00, // type = VAR_STRING
		0x00,                         // parameter_name length = 0
		0x05,                         // value length = 5
		0x61, 0x64, 0x6d, 0x69, 0x6e, // "admin"
	}

	clientCapabilities := mysql.CLIENT_QUERY_ATTRIBUTES

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities, nil, nil)
	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}

	if packet.ParameterCount != 1 {
		t.Fatalf("ParameterCount: expected 1, got %d", packet.ParameterCount)
	}
	if len(packet.NullBitmap) != 1 || packet.NullBitmap[0] != 0x00 {
		t.Fatalf("NullBitmap: expected [0x00], got %v", packet.NullBitmap)
	}
	if packet.NewParamsBindFlag != 1 {
		t.Fatalf("NewParamsBindFlag: expected 1, got %d", packet.NewParamsBindFlag)
	}
	if len(packet.Parameters) != 1 {
		t.Fatalf("Parameters: expected len 1, got %d", len(packet.Parameters))
	}
	if packet.Parameters[0].Type != uint16(mysql.FieldTypeVarString) {
		t.Fatalf("Parameters[0].Type: expected VAR_STRING, got %d", packet.Parameters[0].Type)
	}
	if packet.Parameters[0].Value != "admin" {
		t.Fatalf("Parameters[0].Value: expected \"admin\", got %#v", packet.Parameters[0].Value)
	}
}

// TestDecodeStmtExecute_QueryAttrsExtensionTwoStrings exercises the
// CLIENT_QUERY_ATTRIBUTES path with multiple parameters where the recorded
// names are empty (mysql2's positional binding). With the old decoder these
// would alias and disambiguation would be lost.
func TestDecodeStmtExecute_QueryAttrsExtensionTwoStrings(t *testing.T) {
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 2,
		NumParams:   2,
	}
	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{2: stmtPrepOk}

	// Captured from mysql2: register lookup
	//   SELECT id FROM users WHERE username = ? OR email = ? LIMIT 1
	// bound to ('carol', 'carol@taskhub.local').
	data := []byte{
		0x17,
		0x02, 0x00, 0x00, 0x00, // statement_id = 2
		0x08,                   // flags = PARAMETER_COUNT_AVAILABLE
		0x01, 0x00, 0x00, 0x00, // iteration_count = 1
		0x02,             // length-encoded parameter_count = 2
		0x00,             // null_bitmap (neither null)
		0x01,             // new_params_bind_flag = 1
		0xfd, 0x00, 0x00, // type[0] = VAR_STRING, name length 0
		0xfd, 0x00, 0x00, // type[1] = VAR_STRING, name length 0
		0x05, 'c', 'a', 'r', 'o', 'l',
		0x13, 'c', 'a', 'r', 'o', 'l', '@', 't', 'a', 's', 'k', 'h', 'u', 'b', '.', 'l', 'o', 'c', 'a', 'l',
	}

	clientCapabilities := mysql.CLIENT_QUERY_ATTRIBUTES

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities, nil, nil)
	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}

	if packet.ParameterCount != 2 {
		t.Fatalf("ParameterCount: expected 2, got %d", packet.ParameterCount)
	}
	if packet.NewParamsBindFlag != 1 {
		t.Fatalf("NewParamsBindFlag: expected 1, got %d", packet.NewParamsBindFlag)
	}
	if packet.Parameters[0].Value != "carol" {
		t.Fatalf("Parameters[0].Value: expected \"carol\", got %#v", packet.Parameters[0].Value)
	}
	if packet.Parameters[1].Value != "carol@taskhub.local" {
		t.Fatalf("Parameters[1].Value: expected email, got %#v", packet.Parameters[1].Value)
	}
}

// TestDecodeStmtExecute_IntegerAsDouble pins the FieldTypeDouble decode
// to math.Float64frombits — the wire bytes ARE the float's IEEE-754 bit
// pattern, NOT a numeric value to cast. mysql2 (Node.js) binds integer
// IDs as FieldTypeDouble (type 5); the pre-fix decoder did
// `float64(uint64(...))` which converted the bit pattern as a number,
// turning the integer 6 into ~4.618e+18. The captured YAML then carried
// nonsense values and the matcher couldn't tell IDs apart at
// COM_STMT_EXECUTE replay time.
//
// Two cases:
//   - whole-number bind (6.0) — proves the integer-ID surface.
//   - genuinely fractional bind (3.14) — proves the path is generic,
//     not specifically a whole-number short-circuit.
//
// Both exercise the realistic mysql2 wire shape: CLIENT_QUERY_ATTRIBUTES
// negotiated + PARAMETER_COUNT_AVAILABLE set + length-encoded
// parameter_count + per-parameter name. FieldTypeDouble = 5 (iota=5 in
// pkg/models/mysql/const.go).
func TestDecodeStmtExecute_IntegerAsDouble(t *testing.T) {
	cases := []struct {
		name string
		// bits is the little-endian byte representation of the float's
		// IEEE-754 64-bit encoding, as it appears on the MySQL wire.
		bits [8]byte
		want float64
	}{
		{
			// 6.0 → IEEE-754 bits 0x4018000000000000 (mysql2 integer-ID surface)
			name: "whole_number_6",
			bits: [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x40},
			want: 6.0,
		},
		{
			// 3.14 → IEEE-754 bits 0x40091EB851EB851F (genuinely fractional)
			name: "fractional_pi_approx",
			bits: [8]byte{0x1F, 0x85, 0xEB, 0x51, 0xB8, 0x1E, 0x09, 0x40},
			want: 3.14,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmtPrepOk := &mysql.StmtPrepareOkPacket{
				StatementID: 7,
				NumParams:   1,
			}
			preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{7: stmtPrepOk}

			// Wire shape (mysql2 against MySQL 8 w/ CLIENT_QUERY_ATTRIBUTES):
			data := []byte{
				0x17,                   // COM_STMT_EXECUTE
				0x07, 0x00, 0x00, 0x00, // statement_id = 7
				0x08,                   // flags = PARAMETER_COUNT_AVAILABLE
				0x01, 0x00, 0x00, 0x00, // iteration_count = 1
				0x01, // length-encoded parameter_count = 1
				0x00, // null_bitmap (param 0 NOT null)
				0x01, // new_params_bind_flag = 1
				// type[0] = FieldTypeDouble (5), unsigned-flag byte = 0,
				// followed by length-encoded parameter_name = 0.
				byte(mysql.FieldTypeDouble), 0x00,
				0x00,
			}
			// Append the 8-byte IEEE-754 value bytes.
			data = append(data, tc.bits[:]...)

			clientCapabilities := mysql.CLIENT_QUERY_ATTRIBUTES

			packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities, nil, nil)
			if err != nil {
				t.Fatalf("DecodeStmtExecute failed: %v", err)
			}
			if packet.ParameterCount != 1 {
				t.Fatalf("ParameterCount: expected 1, got %d", packet.ParameterCount)
			}
			if len(packet.Parameters) != 1 {
				t.Fatalf("Parameters: expected len 1, got %d", len(packet.Parameters))
			}
			if packet.Parameters[0].Type != uint16(mysql.FieldTypeDouble) {
				t.Fatalf("Parameters[0].Type: expected FieldTypeDouble (%d), got %d",
					mysql.FieldTypeDouble, packet.Parameters[0].Type)
			}
			got, ok := packet.Parameters[0].Value.(float64)
			if !ok {
				t.Fatalf("Parameters[0].Value: expected float64, got %T (%v)",
					packet.Parameters[0].Value, packet.Parameters[0].Value)
			}
			// Exact equality is safe here: both 6.0 and 3.14's IEEE-754
			// constant survive a Go float-literal round trip byte-for-byte.
			// If the decoder ever regresses to `float64(uint64(...))` the
			// observed value would jump to ~4.6e+18 for 6.0 and ~4.6e+18
			// for 3.14 — orders of magnitude off, NOT a precision blip.
			if got != tc.want {
				t.Fatalf("Parameters[0].Value: expected %v, got %v (regression: decoder cast bit pattern as number?)",
					tc.want, got)
			}
		})
	}
}

// TestDecodeStmtExecute_QueryAttrsExtensionZeroParams verifies the
// length-encoded `parameter_count = 0` trailer that mysql2 still emits when
// the prepared statement has no parameters. Before the fix this `0x00` byte
// was left in the buffer and confused subsequent packets on the same conn.
func TestDecodeStmtExecute_QueryAttrsExtensionZeroParams(t *testing.T) {
	stmtPrepOk := &mysql.StmtPrepareOkPacket{
		StatementID: 1,
		NumParams:   0,
	}
	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{1: stmtPrepOk}

	data := []byte{
		0x17,
		0x01, 0x00, 0x00, 0x00, // statement_id = 1
		0x08,                   // flags = PARAMETER_COUNT_AVAILABLE
		0x01, 0x00, 0x00, 0x00, // iteration_count = 1
		0x00, // length-encoded parameter_count = 0
	}

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, mysql.CLIENT_QUERY_ATTRIBUTES, nil, nil)
	if err != nil {
		t.Fatalf("DecodeStmtExecute failed: %v", err)
	}
	if packet.ParameterCount != 0 {
		t.Fatalf("ParameterCount: expected 0, got %d", packet.ParameterCount)
	}
	if len(packet.Parameters) != 0 {
		t.Fatalf("Parameters: expected len 0, got %d", len(packet.Parameters))
	}
}

// A parameter whose value was streamed ahead with COM_STMT_SEND_LONG_DATA is
// absent from the COM_STMT_EXECUTE payload, but the null bitmap still reports
// it as NOT NULL — the server already holds the bytes. Reading a value for it
// walks off the end of the packet, so the command is rejected and never
// recorded, which is what dropped every streamed-BLOB EXECUTE from the trace
// and made replay fail with "Can not read response from server" (#4262).
func TestDecodeStmtExecute_ParameterSentAsLongData(t *testing.T) {
	const stmtID = uint32(7)
	preparedStmts := map[uint32]*mysql.StmtPrepareOkPacket{
		stmtID: {StatementID: stmtID, NumParams: 1},
	}

	// One parameter, not NULL, types bound — and no value bytes, because the
	// value travelled in a preceding COM_STMT_SEND_LONG_DATA.
	data := []byte{
		0x17,                   // COM_STMT_EXECUTE
		0x07, 0x00, 0x00, 0x00, // statement id
		0x00,                   // flags
		0x01, 0x00, 0x00, 0x00, // iteration count
		0x00,       // NULL bitmap: parameter 0 is NOT null
		0x01,       // new params bind flag
		0xfc, 0x00, // type = MYSQL_TYPE_BLOB, unsigned = false
	}

	logger := zap.NewNop()

	// Without the long-data hint the decoder looks for a value that is not
	// there and rejects the whole command.
	if _, err := DecodeStmtExecute(context.Background(), logger, data, preparedStmts, 0, nil, nil); err == nil {
		t.Fatal("premise broken: the payload is supposed to be short by one parameter value")
	}

	longData := map[uint16]bool{0: true}
	packet, err := DecodeStmtExecute(context.Background(), logger, data, preparedStmts, 0, longData, nil)
	if err != nil {
		t.Fatalf("a parameter sent via COM_STMT_SEND_LONG_DATA must not make the EXECUTE "+
			"undecodable — the command is then dropped and replay has no mock for it: %v", err)
	}
	if packet.StatementID != stmtID {
		t.Errorf("StatementID = %d, want %d", packet.StatementID, stmtID)
	}
	if len(packet.Parameters) != 1 {
		t.Fatalf("got %d parameters, want 1", len(packet.Parameters))
	}
	if packet.Parameters[0].Value != nil {
		t.Errorf("the long-sent parameter carries %v; its bytes live in the SEND_LONG_DATA "+
			"mock, not here", packet.Parameters[0].Value)
	}
}

// executeIntString is a COM_STMT_EXECUTE of stmtID binding an INT (LONG) and
// a VAR_STRING, with their types on the wire when bound is true
// (new_params_bind_flag 1) and without them otherwise (flag 0).
func executeIntString(stmtID uint32, bound bool, n int32, s string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, stmtID)
	p = append(p, 0x00)                        // flags: no cursor
	p = binary.LittleEndian.AppendUint32(p, 1) // iteration count
	p = append(p, 0x00)                        // NULL bitmap: no NULL
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeLong), 0x00, byte(mysql.FieldTypeVarString), 0x00)
	} else {
		p = append(p, 0x00)
	}
	p = binary.LittleEndian.AppendUint32(p, uint32(n))
	return append(append(p, byte(len(s))), s...)
}

// An EXECUTE that sends no types (new_params_bind_flag 0) is read with the
// types the statement's last EXECUTE that sent them carried, as the server
// reads it. Connector/J sends it so when it re-executes a server-prepared
// statement without rebinding, and Connector/J 5.1 whenever the types did
// not change.
func TestDecodeStmtExecute_UnsentTypesAreTheLastSentOnes(t *testing.T) {
	prepared := map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}}
	first, err := DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, true, 1001, "item-1"), prepared, 0, nil, nil)
	if err != nil {
		t.Fatalf("the EXECUTE that sends its types: %v", err)
	}
	next, err := DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 1002, "item-2"), prepared, 0, nil, first.Parameters)
	if err != nil {
		t.Fatalf("the EXECUTE that sends none: %v", err)
	}
	if next.NewParamsBindFlag != 0 {
		t.Errorf("new_params_bind_flag = %d, want 0 as sent", next.NewParamsBindFlag)
	}
	if got := next.Parameters; len(got) != 2 || got[0].Type != uint16(mysql.FieldTypeLong) || got[0].Value != int32(1002) ||
		got[1].Type != uint16(mysql.FieldTypeVarString) || got[1].Value != "item-2" {
		t.Fatalf("decoded %+v, want LONG 1002 and VAR_STRING item-2", got)
	}

	// An unsigned type stays unsigned. A name is not taken: the EXECUTE
	// sends none.
	unsigned := []mysql.Parameter{{Type: uint16(mysql.FieldTypeLong), Unsigned: true, Name: "n"}, {Type: uint16(mysql.FieldTypeVarString), Name: "s"}}
	big, err := DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, -1, "x"), prepared, 0, nil, unsigned)
	if err != nil || big.Parameters[0].Value != uint32(0xffffffff) || !big.Parameters[0].Unsigned {
		t.Fatalf("decoded %+v (%v), want the unsigned 4294967295", big, err)
	}
	if big.Parameters[0].Name != "" || big.Parameters[1].Name != "" {
		t.Fatalf("decoded the names %q and %q, which the EXECUTE did not send", big.Parameters[0].Name, big.Parameters[1].Name)
	}
}

// Without the types it is not read at all: a value's width depends on its
// type, and every value read as a string read the INT's first byte as a
// string length, so the EXECUTE failed to decode or decoded to garbage.
func TestDecodeStmtExecute_UnsentTypesNobodySentAreAnError(t *testing.T) {
	prepared := map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}}
	for name, bound := range map[string][]mysql.Parameter{
		"no earlier EXECUTE sent them":   nil,
		"types known for one of the two": {{Type: uint16(mysql.FieldTypeLong)}},
	} {
		if p, err := DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 3, "abc"), prepared, 0, nil, bound); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, p)
		} else if !strings.Contains(err.Error(), "statement 7") {
			t.Errorf("%s: the error does not name the statement: %v", name, err)
		}
	}
}

// An EXECUTE sent without its types is also read as a released keploy read
// it (LegacyParameters): every value a length-encoded string, typed
// FieldTypeString, so a recording that keploy made of it can still be
// matched. Nil when that keploy could not read it.
func TestDecodeStmtExecute_UnsentTypesAreAlsoReadAsAReleasedKeployReadThem(t *testing.T) {
	prepared := map[uint32]*mysql.StmtPrepareOkPacket{7: {StatementID: 7, NumParams: 2}}
	bound := []mysql.Parameter{{Type: uint16(mysql.FieldTypeLong)}, {Type: uint16(mysql.FieldTypeVarString)}}
	str := uint16(mysql.FieldTypeString)

	p, err := DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 3, "q"), prepared, 0, nil, bound)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.LegacyParameters; len(got) != 2 || got[0] != (mysql.Parameter{Type: str, Value: "\x00\x00\x00"}) || got[1] != (mysql.Parameter{Type: str, Value: "q"}) {
		t.Fatalf("read as a released keploy did: %+v, want \"\\x00\\x00\\x00\" and \"q\", typed FieldTypeString", got)
	}
	if p.Parameters[0].Value != int32(3) || p.Parameters[1].Value != "q" {
		t.Fatalf("read: %+v, want 3 and q", p.Parameters)
	}

	// 1001's first byte (0xe9) is a length past the packet: that keploy
	// could not read it, and recorded nothing.
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 1001, "q"), prepared, 0, nil, bound)
	if err != nil || p.LegacyParameters != nil {
		t.Fatalf("decoded %+v (%v), want no legacy reading", p, err)
	}
	one := map[uint32]*mysql.StmtPrepareOkPacket{8: {StatementID: 8, NumParams: 1}}
	lone := []byte{mysql.COM_STMT_EXECUTE, 8, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x00, 0xe9, 0x03, 0x00, 0x00}
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), lone, one, 0, nil, bound[:1])
	if err != nil || p.Parameters[0].Value != int32(1001) || p.LegacyParameters != nil {
		t.Fatalf("decoded %+v (%v), want 1001 and no legacy reading of a lone INT 1001", p, err)
	}

	// Bytes that are not ASCII were kept base64-encoded: 32769's 0x80.
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 32769, "q"), prepared, 0, nil, bound)
	if err != nil || len(p.LegacyParameters) != 2 || p.LegacyParameters[0].Value != base64.StdEncoding.EncodeToString([]byte{0x80}) || p.LegacyParameters[1].Value != "" {
		t.Fatalf("decoded %+v (%v), want the base64 of 0x80, then an empty string", p, err)
	}

	// A NULL and a value sent as long data are not read.
	nullFirst := []byte{mysql.COM_STMT_EXECUTE, 7, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x01, 0x00, 1, 'z'}
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), nullFirst, prepared, 0, nil, bound)
	if err != nil || len(p.LegacyParameters) != 2 || p.LegacyParameters[0].Value != nil || p.LegacyParameters[1].Value != "z" {
		t.Fatalf("decoded %+v (%v), want a NULL and z", p, err)
	}
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), []byte{mysql.COM_STMT_EXECUTE, 7, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x00, 1, 'z'}, prepared, 0, map[uint16]bool{0: true}, bound)
	if err != nil || len(p.LegacyParameters) != 2 || p.LegacyParameters[0].Value != nil || p.LegacyParameters[1].Value != "z" {
		t.Fatalf("decoded %+v (%v), want long data and z", p, err)
	}

	// INT 254 (fe 00 00 00) then a string whose 4th byte is not ASCII: read
	// as a length-encoded string, 0xfe announces an 8-byte length past
	// MaxInt64. That keploy panicked on it, and recorded nothing.
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, false, 254, "abc\u00e9"), prepared, 0, nil, bound)
	if err != nil || p.Parameters[0].Value != int32(254) || p.LegacyParameters != nil {
		t.Fatalf("decoded %+v (%v), want 254 and no legacy reading", p, err)
	}

	// INT 4's misread takes every byte after it, so the TINYINT that follows
	// has none: that keploy's read failed.
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), []byte{mysql.COM_STMT_EXECUTE, 7, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x00, 4, 0, 0, 0, 9}, prepared, 0, nil,
		[]mysql.Parameter{{Type: uint16(mysql.FieldTypeLong)}, {Type: uint16(mysql.FieldTypeTiny)}})
	if err != nil || p.Parameters[1].Value != int8(9) || p.LegacyParameters != nil {
		t.Fatalf("decoded %+v (%v), want 4 and 9 and no legacy reading", p, err)
	}

	// One sent with its types has none.
	p, err = DecodeStmtExecute(context.Background(), zap.NewNop(), executeIntString(7, true, 3, "q"), prepared, 0, nil, nil)
	if err != nil || p.LegacyParameters != nil {
		t.Fatalf("decoded %+v (%v), want no legacy reading of an EXECUTE that sent its types", p, err)
	}
}

// A string's length past MaxInt64 is a malformed packet: an error, not a
// panic (as an int it turns negative and passed the bounds check).
func TestDecodeStmtExecute_ALengthPastMaxInt64IsAnError(t *testing.T) {
	prepared := map[uint32]*mysql.StmtPrepareOkPacket{8: {StatementID: 8, NumParams: 1}}
	data := []byte{mysql.COM_STMT_EXECUTE, 8, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00,
		0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'}
	if p, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, prepared, 0, nil, nil); err == nil {
		t.Fatalf("decoded %+v, want an error", p)
	}
}

// A parameter_name's length past MaxInt64, and a parameter_count whose NULL
// bitmap's length overflows, are malformed packets too: an error, not a panic.
func TestDecodeStmtExecute_AQueryAttributeLengthPastMaxInt64IsAnError(t *testing.T) {
	prepared := map[uint32]*mysql.StmtPrepareOkPacket{9: {StatementID: 9, NumParams: 1}}
	for name, data := range map[string][]byte{
		"parameter_name length": {mysql.COM_STMT_EXECUTE, 9, 0, 0, 0, mysql.PARAMETER_COUNT_AVAILABLE, 1, 0, 0, 0, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00,
			0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'},
		"parameter_count": {mysql.COM_STMT_EXECUTE, 9, 0, 0, 0, mysql.PARAMETER_COUNT_AVAILABLE, 1, 0, 0, 0,
			0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00, 0x00, 'x'},
		"parameter_count past MaxInt64": {mysql.COM_STMT_EXECUTE, 9, 0, 0, 0, mysql.PARAMETER_COUNT_AVAILABLE, 1, 0, 0, 0,
			0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x00, 0x00, 'x'},
	} {
		if p, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, prepared, mysql.CLIENT_QUERY_ATTRIBUTES, nil, nil); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, p)
		}
	}
}
