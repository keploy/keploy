//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_EXECUTE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_execute.html

func DecodeStmtExecute(_ context.Context, _ *zap.Logger, data []byte, preparedStmts map[uint32]*mysql.StmtPrepareOkPacket) (*mysql.StmtExecutePacket, error) {
	if len(data) < 10 {
		return &mysql.StmtExecutePacket{}, errors.New("packet length too short for COM_STMT_EXECUTE")
	}

	pos := 0

	packet := &mysql.StmtExecutePacket{}

	// Read Status
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	//data[0] is COM_STMT_EXECUTE (0x17)
	packet.Status = data[pos]
	pos++

	// Read StatementID
	if pos+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.StatementID = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	numParams, ok := preparedStmts[packet.StatementID]
	if !ok && numParams == nil {
		return nil, fmt.Errorf("prepared statement with ID %d not found", packet.StatementID)
	}

	packet.ParameterCount = int(numParams.NumParams)

	// Read Flags
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.Flags = data[pos]
	pos++

	// Read IterationCount
	if pos+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.IterationCount = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	if packet.ParameterCount <= 0 {
		return packet, nil
	}

	// Read Parameters only if there are any

	// Read NULL bitmap
	nullBitmapLength := (packet.ParameterCount + 7) / 8
	if pos+nullBitmapLength > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.NullBitmap = data[pos : pos+nullBitmapLength]
	pos += int(nullBitmapLength)

	// Read NewParamsBindFlag
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.NewParamsBindFlag = data[pos]
	pos++

	// Read Parameters if NewParamsBindFlag is set
	if packet.NewParamsBindFlag == 1 {
		packet.Parameters = make([]mysql.Parameter, packet.ParameterCount)
		for i := 0; i < packet.ParameterCount; i++ {
			if pos+2 > len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			packet.Parameters[i].Type = binary.LittleEndian.Uint16(data[pos : pos+2])
			packet.Parameters[i].Unsigned = (data[pos+1] & 0x80) != 0 // Check if the highest bit is set
			pos += 2
		}
	}

	// Read Parameter Values
	for i := 0; i < packet.ParameterCount; i++ {
		if pos >= len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		length, _, n := utils.ReadLengthEncodedInteger(data[pos:])
		pos += n
		if pos+int(length) > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		packet.Parameters[i].Value = data[pos : pos+int(length)]
		pos += int(length)
	}

	return packet, nil
}
