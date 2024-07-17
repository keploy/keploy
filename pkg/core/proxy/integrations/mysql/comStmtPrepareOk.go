//go:build linux

package mysql

import (
	"bytes"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtPrepareOk(data []byte) (*models.MySQLStmtPrepareOk, error) {
	if len(data) < 12 {
		return nil, errors.New("data length is not enough for COM_STMT_PREPARE_OK")
	}

	offset := 0

	response := &models.MySQLStmtPrepareOk{
		Status:      data[offset],
		StatementID: binary.LittleEndian.Uint32(data[offset+1 : offset+5]),
		NumColumns:  binary.LittleEndian.Uint32(data[offset+5 : offset+7]),
		NumParams:   binary.LittleEndian.Uint32(data[offset+7 : offset+9]),
		//data[10] is reserved byte ([00] filler)
		WarningCount: binary.LittleEndian.Uint16(data[offset+10 : offset+12]),
	}

	offset += 12
	data = data[offset:]

	if response.NumParams > 0 {
		offset = 0
		for i := uint32(0); i < response.NumParams; i++ {
			column, n, err := parseColumnDefinitionPacket(data)
			if err != nil {
				return nil, err
			}
			response.ParamDefs = append(response.ParamDefs, *column)
			offset += n
		}
		offset += 9 //skip EOF packet for Parameter Definition
		data = data[offset:]
	}

	if response.NumColumns > 0 {
		offset = 0
		for i := uint32(0); i < response.NumColumns; i++ {
			column, n, err := parseColumnDefinitionPacket(data)
			if err != nil {
				return nil, err
			}
			response.ColumnDefs = append(response.ColumnDefs, *column)
			offset += n
		}
		// offset += 9 //skip EOF packet for Column Definitions
		// data = data[offset:]
	}

	return response, nil
}

func encodeStmtPrepareOk(packet *models.MySQLStmtPrepareOk) ([]byte, error) {
	buf := &bytes.Buffer{}
	buf.Write([]byte{0x0C, 0x00, 0x00, 0x01})
	// Encode the Status field
	if err := binary.Write(buf, binary.LittleEndian, uint8(packet.Status)); err != nil {
		return nil, err
	}

	// Encode the StatementID field
	if err := binary.Write(buf, binary.LittleEndian, packet.StatementID); err != nil {
		return nil, err
	}

	// Encode the NumColumns field
	if err := binary.Write(buf, binary.LittleEndian, uint16(packet.NumColumns)); err != nil {
		return nil, err
	}

	// Encode the NumParams field
	if err := binary.Write(buf, binary.LittleEndian, uint16(packet.NumParams)); err != nil {
		return nil, err
	}

	// Encode the WarningCount field
	if err := binary.Write(buf, binary.LittleEndian, uint16(packet.WarningCount)); err != nil {
		return nil, err
	}

	buf.WriteByte(0x00) // Reserved byte

	seqNum := byte(2)
	for i := uint32(0); i < packet.NumParams; i++ {
		param := packet.ParamDefs[i]
		if err := encodeColumnDefinition(buf, &param, &seqNum); err != nil {
			return nil, err
		}
	}
	if packet.NumParams > 0 {
		// Write EOF marker for parameter definitions
		buf.Write([]byte{5, 0, 0, seqNum, 0xFE, 0x00, 0x00, 0x02, 0x00})
		seqNum++
	}

	// Encode column definitions
	for _, col := range packet.ColumnDefs {
		if err := encodeColumnDefinition(buf, &col, &seqNum); err != nil {
			return nil, err
		}
	}

	if packet.NumColumns > 0 {
		// Write EOF marker for column definitions
		buf.Write([]byte{5, 0, 0, seqNum, 0xFE, 0x00, 0x00, 0x02, 0x00})
		seqNum++
	}

	return buf.Bytes(), nil
}

func encodeColumnDefinition(buf *bytes.Buffer, column *models.ColumnDefinition, seqNum *byte) error {
	tmpBuf := &bytes.Buffer{}
	writeLengthEncodedString(tmpBuf, column.Catalog)
	writeLengthEncodedString(tmpBuf, column.Schema)
	writeLengthEncodedString(tmpBuf, column.Table)
	writeLengthEncodedString(tmpBuf, column.OrgTable)
	writeLengthEncodedString(tmpBuf, column.Name)
	writeLengthEncodedString(tmpBuf, column.OrgName)
	tmpBuf.WriteByte(0x0C)
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.CharacterSet); err != nil {
		return err
	}
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.ColumnLength); err != nil {
		return err
	}
	tmpBuf.WriteByte(column.ColumnType)
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.Flags); err != nil {
		return err
	}
	tmpBuf.WriteByte(column.Decimals)
	tmpBuf.Write([]byte{0x00, 0x00})

	colData := tmpBuf.Bytes()
	length := len(colData)

	// Write packet header with length and sequence number
	buf.WriteByte(byte(length))
	buf.WriteByte(byte(length >> 8))
	buf.WriteByte(byte(length >> 16))
	buf.WriteByte(*seqNum)
	*seqNum++

	// Write column definition data
	buf.Write(colData)

	return nil
}
