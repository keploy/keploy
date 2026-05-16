package preparedstmt

import (
	"context"
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
		0x01, // NULL bitmap (1 byte for 3 params, all NULL)
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

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities)

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

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities)

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

	packet, err := DecodeStmtExecute(ctx, logger, data, preparedStmts, clientCapabilities)
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

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities)
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

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities)
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

			packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, clientCapabilities)
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

	packet, err := DecodeStmtExecute(context.Background(), zap.NewNop(), data, preparedStmts, mysql.CLIENT_QUERY_ATTRIBUTES)
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
