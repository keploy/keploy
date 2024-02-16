package mysql

import (
	"bytes"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/pkg/models"
)

type StmtPrepareOk struct {
	Status       byte               `json:"status,omitempty" yaml:"status,omitempty,flow"`
	StatementID  uint32             `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	NumColumns   uint16             `json:"num_columns,omitempty" yaml:"num_columns,omitempty,flow"`
	NumParams    uint16             `json:"num_params,omitempty" yaml:"num_params,omitempty,flow"`
	WarningCount uint16             `json:"warning_count,omitempty" yaml:"warning_count,omitempty,flow"`
	ColumnDefs   []ColumnDefinition `json:"column_definitions,omitempty" yaml:"column_definitions,omitempty,flow"`
	ParamDefs    []ColumnDefinition `json:"param_definitions,omitempty" yaml:"param_definitions,omitempty,flow"`
}

func decodeComStmtPrepareOk(data []byte) (*StmtPrepareOk, error) {
	if len(data) < 12 {
		return nil, errors.New("data length is not enough for COM_STMT_PREPARE_OK")
	}

	response := &StmtPrepareOk{
		Status:       data[0],
		StatementID:  binary.LittleEndian.Uint32(data[1:5]),
		NumColumns:   binary.LittleEndian.Uint16(data[5:7]),
		NumParams:    binary.LittleEndian.Uint16(data[7:9]),
		WarningCount: binary.LittleEndian.Uint16(data[10:12]),
	}

	offset := 12

	if response.NumParams > 0 {
		for i := uint16(0); i < response.NumParams; i++ {
			columnDef := ColumnDefinition{}
			columnHeader := PacketHeader{
				PacketLength:     data[offset],
				PacketSequenceID: data[offset+3],
			}
			columnDef.PacketHeader = columnHeader
			offset += 4 //Header of packet
			var err error
			columnDef.Catalog, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Schema, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Table, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.OrgTable, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Name, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.OrgName, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			offset++ //filler
			columnDef.CharacterSet = binary.LittleEndian.Uint16(data[offset : offset+2])
			columnDef.ColumnLength = binary.LittleEndian.Uint32(data[offset+2 : offset+6])
			columnDef.ColumnType = data[offset+6]
			columnDef.Flags = binary.LittleEndian.Uint16(data[offset+7 : offset+9])
			columnDef.Decimals = data[offset+9]
			offset += 10
			offset += 2 // filler
			response.ParamDefs = append(response.ParamDefs, columnDef)
		}
		offset += 9 //skip EOF packet for Parameter Definition
	}

	if response.NumColumns > 0 {
		for i := uint16(0); i < response.NumColumns; i++ {
			columnDef := ColumnDefinition{}
			columnHeader := PacketHeader{
				PacketLength:     data[offset],
				PacketSequenceID: data[offset+3],
			}
			columnDef.PacketHeader = columnHeader
			offset += 4
			var err error
			columnDef.Catalog, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Schema, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Table, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.OrgTable, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.Name, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			columnDef.OrgName, err = readLengthEncodedString(data, &offset)
			if err != nil {
				return nil, err
			}
			offset++ //filler
			columnDef.CharacterSet = binary.LittleEndian.Uint16(data[offset : offset+2])
			columnDef.ColumnLength = binary.LittleEndian.Uint32(data[offset+2 : offset+6])
			columnDef.ColumnType = data[offset+6]
			columnDef.Flags = binary.LittleEndian.Uint16(data[offset+7 : offset+9])
			columnDef.Decimals = data[offset+9]
			offset += 10
			offset += 2 // filler
			response.ColumnDefs = append(response.ColumnDefs, columnDef)
		}
		offset += 9 //skip EOF packet for Column Definitions
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
	for i := uint16(0); i < packet.NumParams; i++ {
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
