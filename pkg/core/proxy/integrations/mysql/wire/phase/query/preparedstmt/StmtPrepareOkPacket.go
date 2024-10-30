//go:build linux || windows

package preparedstmt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_PREPARE_OK: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

func DecodePrepareOk(_ context.Context, _ *zap.Logger, data []byte) (*mysql.StmtPrepareOkPacket, error) {
	if len(data) < 12 {
		return nil, errors.New("data length is not enough for COM_STMT_PREPARE_OK")
	}

	offset := 0

	response := &mysql.StmtPrepareOkPacket{}

	response.Status = data[offset]
	offset++

	response.StatementID = binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4

	response.NumColumns = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	response.NumParams = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	//data[10] is reserved byte ([00] filler)
	response.Filler = data[offset]
	offset++

	response.WarningCount = binary.LittleEndian.Uint16(data[offset : offset+2])
	// offset += 2

	return response, nil
}

func EncodePrepareOk(ctx context.Context, logger *zap.Logger, packet *mysql.StmtPrepareOkPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the Prepare OK packet
	prepareOkBytes, err := EncodePreparedStmtResponse(ctx, logger, packet)
	if err != nil {
		return nil, fmt.Errorf("failed to encode StmtPrepareOkPacket: %w", err)
	}
	if _, err := buf.Write(prepareOkBytes); err != nil {
		return nil, fmt.Errorf("failed to write StmtPrepareOkPacket: %w", err)
	}

	// Encode parameter definitions if present
	for _, paramDef := range packet.ParamDefs {
		paramBytes, err := rowscols.EncodeColumn(ctx, logger, paramDef)
		if err != nil {
			return nil, fmt.Errorf("failed to encode parameter definition: %w", err)
		}
		if _, err := buf.Write(paramBytes); err != nil {
			return nil, fmt.Errorf("failed to write parameter definition: %w", err)
		}
	}

	// Write EOF packet after parameter definitions
	if packet.NumParams > 0 {
		if _, err := buf.Write(packet.EOFAfterParamDefs); err != nil {
			return nil, fmt.Errorf("failed to write EOF packet after parameter definitions: %w", err)
		}
	}

	// Encode column definitions if present
	for _, columnDef := range packet.ColumnDefs {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, columnDef)
		if err != nil {
			return nil, fmt.Errorf("failed to encode column definition: %w", err)
		}
		if _, err := buf.Write(columnBytes); err != nil {
			return nil, fmt.Errorf("failed to write column definition: %w", err)
		}
	}

	// Write EOF packet after column definitions
	if packet.NumColumns > 0 {
		if _, err := buf.Write(packet.EOFAfterColumnDefs); err != nil {
			return nil, fmt.Errorf("failed to write EOF packet after column definitions: %w", err)
		}
	}

	return buf.Bytes(), nil
}

func EncodePreparedStmtResponse(_ context.Context, _ *zap.Logger, packet *mysql.StmtPrepareOkPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Status
	if err := buf.WriteByte(packet.Status); err != nil {
		return nil, fmt.Errorf("failed to write Status: %w", err)
	}

	// Write Statement ID
	if err := binary.Write(buf, binary.LittleEndian, packet.StatementID); err != nil {
		return nil, fmt.Errorf("failed to write StatementID: %w", err)
	}

	// Write Number of Columns
	if err := binary.Write(buf, binary.LittleEndian, packet.NumColumns); err != nil {
		return nil, fmt.Errorf("failed to write NumColumns: %w", err)
	}

	// Write Number of Parameters
	if err := binary.Write(buf, binary.LittleEndian, packet.NumParams); err != nil {
		return nil, fmt.Errorf("failed to write NumParams: %w", err)
	}

	// Write Filler
	if err := buf.WriteByte(packet.Filler); err != nil {
		return nil, fmt.Errorf("failed to write Filler: %w", err)
	}

	// Write Warning Count
	if err := binary.Write(buf, binary.LittleEndian, packet.WarningCount); err != nil {
		return nil, fmt.Errorf("failed to write WarningCount: %w", err)
	}

	return buf.Bytes(), nil
}
