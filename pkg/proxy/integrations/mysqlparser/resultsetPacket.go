package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"

	"go.keploy.io/server/pkg/models"
)

type ResultSet struct {
	Columns []*ColumnDefinition `yaml:"columns"`
	Rows    []*Row              `yaml:"rows"`
}

type Row struct {
	Header  RowHeader             `yaml:"header"`
	Columns []RowColumnDefinition `yaml:"row_column_definition"`
}
type RowColumnDefinition struct {
	Type  fieldType   `yaml:"type"`
	Name  string      `yaml:"name"`
	Value interface{} `yaml:"value"`
}

func parseResultSet(b []byte) (*ResultSet, error) {
	columns := make([]*ColumnDefinition, 0)
	rows := make([]*Row, 0)

	var err error

	// Parse the column count packet
	columnCount, _, n := readLengthEncodedInteger(b)
	b = b[n:]

	// Parse the columns
	for i := uint64(0); i < columnCount; i++ {
		var columnPacket *ColumnDefinition
		columnPacket, b, err = parseColumnDefinitionPacket(b)
		if err != nil {
			return nil, err
		}
		columns = append(columns, columnPacket)
	}

	// Parse the EOF packet after the columns
	b = b[9:]

	// Parse the rows
	fmt.Println(!bytes.Equal(b[:4], []byte{0xfe, 0x00, 0x00, 0x02, 0x00}))
	for len(b) > 5 && !bytes.Equal(b[4:], []byte{0xfe, 0x00, 0x00, 0x02, 0x00}) {
		var row *Row
		row, b, err = parseRow(b, columns)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}

	// Remove EOF packet of the rows
	b = b[9:]

	resultSet := &ResultSet{
		Columns: columns,
		Rows:    rows,
	}

	return resultSet, err
}

func parseColumnDefinitionPacket(b []byte) (*ColumnDefinition, []byte, error) {
	packet := &ColumnDefinition{}
	var n int
	var m int

	// Read packet header
	packet.PacketHeader.PacketLength = uint8(readUint24(b[:3]))
	packet.PacketHeader.PacketSequenceID = uint8(b[3])
	b = b[4:]

	packet.Catalog, n = readLengthEncodedStrings(b)
	b = b[n:]
	packet.Schema, n = readLengthEncodedStrings(b)
	b = b[n:]
	packet.Table, n = readLengthEncodedStrings(b)
	b = b[n:]
	packet.OrgTable, n = readLengthEncodedStrings(b)
	b = b[n:]
	packet.Name, n = readLengthEncodedStrings(b)
	b = b[n:]
	packet.OrgName, n = readLengthEncodedStrings(b)
	b = b[n:]
	b = b[1:] // Skip the next byte (length of the fixed-length fields)
	packet.CharacterSet = binary.LittleEndian.Uint16(b)
	b = b[2:]
	packet.ColumnLength = binary.LittleEndian.Uint32(b)
	b = b[4:]
	packet.ColumnType = b[0]
	b = b[1:]
	packet.Flags = binary.LittleEndian.Uint16(b)
	b = b[2:]
	packet.Decimals = b[0]
	b = b[2:] // Skip filler
	if len(b) > 0 {
		packet.DefaultValue, m = readLengthEncodedStrings(b)
		b = b[m:]
	}

	return packet, b, nil
}

func parseRow(b []byte, columnDefinitions []*ColumnDefinition) (*Row, []byte, error) {
	row := &Row{}

	packetLength := int(b[0])
	sequenceID := b[3]
	rowHeader := RowHeader{
		PacketLength: packetLength,
		SequenceID:   sequenceID,
	}
	fmt.Println(rowHeader)
	b = b[4:]
	b = b[2:]
	for _, columnDef := range columnDefinitions {
		var colValue RowColumnDefinition
		var length int

		// Check the column type
		switch fieldType(columnDef.ColumnType) {
		case fieldTypeTimestamp:
			dataLength := int(b[0])
			b = b[1:] // Advance the buffer to the start of the encoded timestamp data

			if dataLength < 4 || len(b) < dataLength {
				return nil, nil, fmt.Errorf("invalid timestamp data length")
			}

			// Decode the year, month, day, hour, minute, second
			year := binary.LittleEndian.Uint16(b[:2])
			month := uint8(b[2])
			day := uint8(b[3])
			hour := uint8(b[4])
			minute := uint8(b[5])
			second := uint8(b[6])

			colValue.Type = fieldTypeTimestamp
			colValue.Value = fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second)
			length = dataLength // Including the initial byte for dataLength

		case fieldTypeInt24, fieldTypeLong:
			colValue.Type = fieldType(columnDef.ColumnType)
			colValue.Value = int32(binary.LittleEndian.Uint32(b[:4]))
			length = 4
		case fieldTypeLongLong:
			colValue.Type = fieldTypeLongLong
			colValue.Value = int64(binary.LittleEndian.Uint64(b[:8]))
			length = 8
		case fieldTypeFloat:
			colValue.Type = fieldTypeFloat
			colValue.Value = math.Float32frombits(binary.LittleEndian.Uint32(b[:4]))
			length = 4
		case fieldTypeDouble:
			colValue.Type = fieldTypeDouble
			colValue.Value = math.Float64frombits(binary.LittleEndian.Uint64(b[:8]))
			length = 8
		default:
			// Read a length-encoded integer
			stringLength, _, n := readLengthEncodedInteger(b)
			length = int(stringLength) + n

			// Extract the string
			str := string(b[n : n+int(stringLength)])

			// Remove non-printable characters
			re := regexp.MustCompile(`[^[:print:]\t\r\n]`)
			cleanedStr := re.ReplaceAllString(str, "")

			colValue.Type = fieldType(columnDef.ColumnType)
			colValue.Value = cleanedStr
		}

		colValue.Name = columnDef.Name
		row.Columns = append(row.Columns, colValue)
		b = b[length:]
	}
	row.Header = rowHeader
	return row, b, nil
}

func encodeMySQLResultSet(resultSet *models.MySQLResultSet) ([]byte, error) {
	buf := new(bytes.Buffer)
	sequenceID := byte(1)
	buf.Write([]byte{0x01, 0x00, 0x00, 0x01})

	// Write column count
	writeLengthEncodedInteger(buf, uint64(len(resultSet.Columns)))

	if len(resultSet.Columns) > 0 {
		for _, column := range resultSet.Columns {
			sequenceID++
			buf.WriteByte(byte(column.PacketHeader.PacketLength))
			buf.WriteByte(byte(column.PacketHeader.PacketLength >> 8))
			buf.WriteByte(byte(column.PacketHeader.PacketLength >> 16))
			buf.WriteByte(sequenceID)

			writeLengthEncodedString(buf, column.Catalog)
			writeLengthEncodedString(buf, column.Schema)
			writeLengthEncodedString(buf, column.Table)
			writeLengthEncodedString(buf, column.OrgTable)
			writeLengthEncodedString(buf, column.Name)
			writeLengthEncodedString(buf, column.OrgName)
			buf.WriteByte(0x0c) // Length of the fixed-length fields (12 bytes)
			binary.Write(buf, binary.LittleEndian, column.CharacterSet)
			binary.Write(buf, binary.LittleEndian, column.ColumnLength)
			buf.WriteByte(column.ColumnType)
			binary.Write(buf, binary.LittleEndian, column.Flags)
			buf.WriteByte(column.Decimals)
			buf.Write([]byte{0x00, 0x00}) // Filler
		}
	}
	sequenceID++
	// Write EOF packet header
	buf.Write([]byte{5, 0, 0, sequenceID, 0xFE, 0x00, 0x00, 0x02, 0x00})

	// Write rows
	for _, row := range resultSet.Rows {
		sequenceID++
		//buf.WriteByte(byte(row.Header.PacketLength))
		buf.WriteByte(row.Header.PacketLength)
		buf.Write([]byte{0x00, 0x00}) // two extra bytes after header
		buf.WriteByte(sequenceID)
		buf.Write([]byte{0x00, 0x00}) // two extra bytes after header

		bytes, _ := encodeRow(row, row.Columns)
		buf.Write(bytes)
	}
	sequenceID++
	// Write EOF packet header again
	buf.Write([]byte{5, 0, 0, sequenceID, 0xFE, 0x00, 0x00, 0x02, 0x00})

	return buf.Bytes(), nil
}
