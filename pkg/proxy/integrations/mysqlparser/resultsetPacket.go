package mysqlparser

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	// "math"
	"time"

	"go.keploy.io/server/pkg/models"
)

type ResultSet struct {
	Columns             []*ColumnDefinition `json:"columns,omitempty" yaml:"columns,omitempty,flow"`
	Rows                []*Row              `json:"rows,omitempty" yaml:"rows,omitempty,flow"`
	EOFPresent          bool                `json:"eofPresent,omitempty" yaml:"eofPresent,omitempty,flow"`
	PaddingPresent      bool                `json:"paddingPresent,omitempty" yaml:"paddingPresent,omitempty,flow"`
	EOFPresentFinal     bool                `json:"eofPresentFinal,omitempty" yaml:"eofPresentFinal,omitempty,flow"`
	PaddingPresentFinal bool                `json:"paddingPresentFinal,omitempty" yaml:"paddingPresentFinal,omitempty,flow"`
	OptionalPadding     bool                `json:"optionalPadding,omitempty" yaml:"optionalPadding,omitempty,flow"`
	OptionalEOFBytes    string              `json:"optionalEOFBytes,omitempty" yaml:"optionalEOFBytes,omitempty,flow"`
	EOFAfterColumns     string              `json:"eofAfterColumns,omitempty" yaml:"eofAfterColumns,omitempty,flow"`
}

type Row struct {
	Header  RowHeader             `json:"header,omitempty" yaml:"header,omitempty,flow"`
	Columns []RowColumnDefinition `json:"columns,omitempty" yaml:"row_column_definition,omitempty,flow"`
}

type RowColumnDefinition struct {
	Type  models.FieldType `json:"type,omitempty" yaml:"type,omitempty,flow"`
	Name  string           `json:"name,omitempty" yaml:"name,omitempty,flow"`
	Value interface{}      `json:"value,omitempty" yaml:"value,omitempty,flow"`
}

type RowHeader struct {
	PacketLength int   `json:"packet_length,omitempty" yaml:"packet_length,omitempty,flow"`
	SequenceID   uint8 `json:"sequence_id,omitempty" yaml:"sequence_id,omitempty,flow"`
}

func parseResultSet(b []byte) (*ResultSet, error) {
	columns := make([]*ColumnDefinition, 0)
	rows := make([]*Row, 0)
	var err error
	var eofPresent, paddingPresent, eofFinal, paddingFinal, optionalPadding bool
	var optionalEOFBytes []byte
	var eofAfterColumns []byte
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

	// Check for EOF packet after columns
	if len(b) > 4 && bytes.Contains(b[4:9], []byte{0xfe, 0x00, 0x00}) {
		eofPresent = true
		eofAfterColumns = b[:9]
		b = b[9:] // Skip the EOF packet
		if len(b) >= 2 && b[0] == 0x00 && b[1] == 0x00 {
			paddingPresent = true
			b = b[2:] // Skip padding
		}
	}

	// Parse the rows
	// fmt.Println(!bytes.Equal(b[:4], []byte{0xfe, 0x00, 0x00, 0x02, 0x00}))
	for len(b) > 5 {
		// fmt.Println(b)
		var row *Row
		row, b, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, err = parseRow(b, columns)
		if err != nil {
			return nil, err
		}
		if row != nil {
			rows = append(rows, row)
		}
	}

	// Remove EOF packet of the rows
	// b = b[9:]

	resultSet := &ResultSet{
		Columns:             columns,
		Rows:                rows,
		EOFPresent:          eofPresent,
		PaddingPresent:      paddingPresent,
		EOFPresentFinal:     eofFinal,
		PaddingPresentFinal: paddingFinal,
		OptionalPadding:     optionalPadding,
		OptionalEOFBytes:    base64.StdEncoding.EncodeToString(optionalEOFBytes),
		EOFAfterColumns:     base64.StdEncoding.EncodeToString(eofAfterColumns),
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

var optionalPadding bool

func parseRow(b []byte, columnDefinitions []*ColumnDefinition) (*Row, []byte, bool, bool, bool, []byte, error) {
	var eofFinal, paddingFinal bool
	var optionalEOFBytes []byte

	row := &Row{}
	if b[4] == 0xfe {
		eofFinal = true
		optionalEOFBytes = b[:9]
		b = b[9:]
		if len(b) >= 2 && b[0] == 0x00 && b[1] == 0x00 {
			paddingFinal = true
			b = b[2:] // Skip padding
		}
		if len(b) < 14 {
			return nil, nil, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, nil

		}
	}
	packetLength := int(b[0])
	sequenceID := b[3]
	rowHeader := RowHeader{
		PacketLength: packetLength,
		SequenceID:   sequenceID,
	}
	b = b[4:]
	if len(b) >= 2 && b[0] == 0x00 && b[1] == 0x00 {
		optionalPadding = true
		b = b[2:] // Skip padding
	}
	// b = b[2:]
	for _, columnDef := range columnDefinitions {
		var colValue RowColumnDefinition
		var length int
		// if b[0] == 0x00 {
		// 	b = b[1:]
		// }
		dataLength := int(b[0])

		// Check the column type
		switch models.FieldType(columnDef.ColumnType) {
		case models.FieldTypeTimestamp:
			b = b[1:] // Advance the buffer to the start of the encoded timestamp data

			if dataLength < 4 || len(b) < dataLength {
				return nil, nil, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, fmt.Errorf("invalid timestamp data length")
			}

			// Decode the year, month, day, hour, minute, second
			year := binary.LittleEndian.Uint16(b[:2])
			month := uint8(b[2])
			day := uint8(b[3])
			hour := uint8(b[4])
			minute := uint8(b[5])
			second := uint8(b[6])

			colValue.Type = models.FieldTypeTimestamp
			colValue.Value = fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second)
			length = dataLength // Including the initial byte for dataLength

		// case models.FieldTypeInt24, models.FieldTypeLong:
		// 	colValue.Type = models.FieldType(columnDef.ColumnType)
		// 	colValue.Value = int32(binary.LittleEndian.Uint32(b[:4]))
		// 	length = 4
		// case models.FieldTypeLongLong:
		// 	colValue.Type = models.FieldTypeLongLong
		// 	var longLongBytes []byte = b[1 : dataLength+1]
		// 	colValue.Value = longLongBytes
		// 	length = dataLength
		// 	b = b[1:]
		// case models.FieldTypeFloat:
		// 	colValue.Type = models.FieldTypeFloat
		// 	colValue.Value = math.Float32frombits(binary.LittleEndian.Uint32(b[:4]))
		// 	length = 4
		// case models.FieldTypeDouble:
		// 	colValue.Type = models.FieldTypeDouble
		// 	colValue.Value = math.Float64frombits(binary.LittleEndian.Uint64(b[:8]))
		// 	length = 8
		default:
			// Read a length-encoded integer
			stringLength, _, n := readLengthEncodedInteger(b)
			length = int(stringLength) + n

			// Extract the string
			str := string(b[n : n+int(stringLength)])

			// Remove non-printable characters
			// re := regexp.MustCompile(`[^[:print:]\t\r\n]`)
			// cleanedStr := re.ReplaceAllString(str, "")

			colValue.Type = models.FieldType(columnDef.ColumnType)
			colValue.Value = str
		}

		colValue.Name = columnDef.Name
		row.Columns = append(row.Columns, colValue)
		b = b[length:]
	}
	row.Header = rowHeader
	return row, b, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, nil
}

func encodeMySQLResultSet(resultSet *models.MySQLResultSet) ([]byte, error) {
	buf := new(bytes.Buffer)
	sequenceID := byte(1)
	buf.Write([]byte{0x01, 0x00, 0x00, 0x01})
	// Write column count
	lengthColumns := uint64(len(resultSet.Columns))
	writeLengthEncodedInteger(buf, &lengthColumns)

	if len(resultSet.Columns) > 0 {
		for _, column := range resultSet.Columns {
			sequenceID++
			buf.WriteByte(byte(column.PacketHeader.PacketLength))
			// buf.WriteByte(byte(column.PacketHeader.PacketLength >> 8))
			// buf.WriteByte(byte(column.PacketHeader.PacketLength >> 16))
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

	// Write EOF packet header
	if resultSet.EOFPresent {
		sequenceID++
		EOFAfterColumnsValue, _ := base64.StdEncoding.DecodeString(resultSet.EOFAfterColumns)
		buf.Write(EOFAfterColumnsValue)
		if resultSet.PaddingPresent {
			buf.Write([]byte{0x00, 0x00}) // Add padding bytes
		}
	}

	// Write rows
	for _, row := range resultSet.Rows {
		sequenceID++
		//buf.WriteByte(byte(row.Header.PacketLength))
		buf.WriteByte(row.Header.PacketLength)
		buf.Write([]byte{0x00, 0x00}) // two extra bytes after header
		buf.WriteByte(sequenceID)
		if resultSet.OptionalPadding {
			buf.Write([]byte{0x00, 0x00}) // Add padding bytes
		}
		bytes, _ := encodeRow(row, row.Columns)
		buf.Write(bytes)
	}
	sequenceID++
	// Write EOF packet header again
	// buf.Write([]byte{5, 0, 0, sequenceID})
	OptionalEOFBytesValue, _ := base64.StdEncoding.DecodeString(resultSet.OptionalEOFBytes)
	buf.Write(OptionalEOFBytesValue)
	if resultSet.PaddingPresentFinal {
		buf.Write([]byte{0x00, 0x00}) // Add padding bytes
	}
	return buf.Bytes(), nil
}

func encodeRow(row *models.Row, columnValues []models.RowColumnDefinition) ([]byte, error) {
	var buf bytes.Buffer

	// Write the header
	//binary.Write(&buf, binary.LittleEndian, uint32(row.Header.PacketLength))
	//buf.WriteByte(row.Header.PacketSequenceId)

	for _, column := range columnValues {
		value := column.Value
		switch models.FieldType(column.Type) {
		case models.FieldTypeTimestamp:
			timestamp, ok := value.(string)
			if !ok {
				return nil, errors.New("could not convert value to string")
			}
			t, err := time.Parse("2006-01-02 15:04:05", timestamp)
			if err != nil {
				return nil, errors.New("could not parse timestamp value")
			}

			buf.WriteByte(7) // Length of the following encoded data
			yearBytes := make([]byte, 2)
			binary.LittleEndian.PutUint16(yearBytes, uint16(t.Year()))
			buf.Write(yearBytes)            // Year
			buf.WriteByte(byte(t.Month()))  // Month
			buf.WriteByte(byte(t.Day()))    // Day
			buf.WriteByte(byte(t.Hour()))   // Hour
			buf.WriteByte(byte(t.Minute())) // Minute
			buf.WriteByte(byte(t.Second())) // Second
			// case models.FieldTypeLongLong:
			// 	longLongSlice, ok := column.Value.([]interface{})
			// 	numElements := len(longLongSlice)
			// 	buf.WriteByte(byte(numElements))
			// 	if !ok {
			// 		return nil, errors.New("invalid type for FieldTypeLongLong, expected []interface{}")
			// 	}

			// 	for _, v := range longLongSlice {
			// 		intVal, ok := v.(int)
			// 		if !ok {
			// 			return nil, errors.New("invalid int value for FieldTypeLongLong in slice")
			// 		}

			// 		// Check that the int value is within the range of a single byte
			// 		if intVal < 0 || intVal > 255 {
			// 			return nil, errors.New("int value for FieldTypeLongLong out of byte range")
			// 		}

			// 		// Convert int to a single byte
			// 		buf.WriteByte(byte(intVal))
			// 	}
		default:
			strValue, ok := value.(string)
			if !ok {
				return nil, errors.New("could not convert value to string")
			}

			if strValue == "" {
				// Write 0xFB to represent NULL
				buf.WriteByte(0xFB)
			} else {
				length := uint64(len(strValue))
				// Now pass a pointer to length
				writeLengthEncodedInteger(&buf, &length)
				// Write the string value
				buf.WriteString(strValue)
			}
		}

	}

	return buf.Bytes(), nil
}

// func encodeInt32(val int32) []byte {
// 	buf := make([]byte, 4)
// 	binary.LittleEndian.PutUint32(buf, uint32(val))
// 	return buf
// }

// func encodeFloat32(val float32) []byte {
// 	buf := make([]byte, 4)
// 	binary.LittleEndian.PutUint32(buf, math.Float32bits(val))
// 	return buf
// }

// func encodeFloat64(val float64) []byte {
// 	buf := make([]byte, 8)
// 	binary.LittleEndian.PutUint64(buf, math.Float64bits(val))
// 	return buf
// }
