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
