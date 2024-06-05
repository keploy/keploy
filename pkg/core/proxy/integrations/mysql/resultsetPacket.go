package mysql

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	// "github.com/zmap/zlint/v3/formattedoutput"
	"go.keploy.io/server/v2/pkg/models"
)

type RowColumnDefinition struct {
	Type  models.FieldType `yaml:"type"`
	Name  string           `yaml:"name"`
	Value interface{}      `yaml:"value"`
}
type RowHeader struct {
	PacketLength int   `yaml:"packet_length"`
	SequenceID   uint8 `yaml:"sequence_id"`
}

func parseResultSet(b []byte) (*models.MySQLResultSet, error) {
	columns := make([]*models.ColumnDefinition, 0)
	rows := make([]*models.Row, 0)
	var err error
	var eofPresent, paddingPresent, eofFinal, paddingFinal, optionalPadding bool
	var optionalEOFBytes []byte
	var eofAfterColumns []byte
	// Parse the column count packet
	columnCount, _, n := readLengthEncodedInteger(b)
	if n == 0 {
		return nil, errors.New("invalid column count")
	}
	b = b[n:]

	// Parse the columns
	for i := uint64(0); i < columnCount; i++ {
		var columnPacket *models.ColumnDefinition
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
		var row *models.Row
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

	resultSet := &models.MySQLResultSet{
		Columns:             columns,
		Rows:                rows,
		EOFPresent:          eofPresent,
		PaddingPresent:      paddingPresent,
		EOFPresentFinal:     eofFinal,
		PaddingPresentFinal: paddingFinal,
		OptionalPadding:     optionalPadding,
		OptionalEOFBytes:    optionalEOFBytes,
		EOFAfterColumns:     eofAfterColumns,
	}

	return resultSet, err
}

func parseColumnDefinitionPacket(b []byte) (*models.ColumnDefinition, []byte, error) {
	packet := &models.ColumnDefinition{}
	var n int
	var m int
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("invalid column definition packet")
	}
	// Read packet header
	packet.PacketHeader.PacketLength = uint8(readUint24(b[:3]))
	packet.PacketHeader.PacketSequenceID = uint8(b[3])
	b = b[4:]

	packet.Catalog, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}
	packet.Schema, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}
	packet.Table, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}
	packet.OrgTable, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}
	packet.Name, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}
	packet.OrgName, n = readLengthEncodedStrings(b)
	if len(b) > n {
		b = b[n:]
	}

	if len(b) > 1 {
		b = b[1:] // Skip the next byte (length of the fixed-length fields)
	}
	packet.CharacterSet = binary.LittleEndian.Uint16(b)
	if len(b) > 2 {
		b = b[2:]
	}
	packet.ColumnLength = binary.LittleEndian.Uint32(b)
	if len(b) > 4 {
		b = b[4:]
	}
	packet.ColumnType = b[0]
	if len(b) > 1 {
		b = b[1:]
	}
	packet.Flags = binary.LittleEndian.Uint16(b)
	if len(b) > 2 {
		b = b[2:]
	}
	if len(b) > 0 {
		packet.Decimals = b[0]
	}
	if len(b) > 2 {
		b = b[2:] // Skip filler
	}

	if len(b) > 0 {
		packet.DefaultValue, m = readLengthEncodedStrings(b)
		b = b[m:]
	}

	return packet, b, nil
}

var optionalPadding bool

func parseRow(b []byte, columnDefinitions []*models.ColumnDefinition) (*models.Row, []byte, bool, bool, bool, []byte, error) {
	var eofFinal, paddingFinal bool
	var optionalEOFBytes []byte

	row := &models.Row{}
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
	packetLength := uint8(b[0])
	sequenceID := b[3]
	rowHeader := models.RowHeader{
		PacketLength:     packetLength,
		PacketSequenceID: sequenceID,
	}
	b = b[4:]
	if len(b) >= 2 && b[0] == 0x00 && b[1] == 0x00 {
		optionalPadding = true
		b = b[2:] // Skip padding
	}
	// b = b[2:]
	for _, columnDef := range columnDefinitions {
		var colValue models.RowColumnDefinition
		var length int
		dataLength := int(b[0])
		// Check the column type
		switch models.FieldType(columnDef.ColumnType) {
		case models.FieldTypeTimestamp:
			if b[0] == byte(0xfb) {
				colValue.Type = models.FieldTypeTimestamp
				colValue.Value = ""
				length = 1
			} else {
				b = b[1:] // Advance the buffer to the start of the encoded timestamp data
				// Check if the timestamp is null
				if dataLength < 4 || len(b) < dataLength {
					return nil, nil, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, fmt.Errorf("invalid timestamp data length")
				}
				dateStr := string(b[:dataLength])
				layout := "2006-01-02 15:04:05"
				t, err := time.Parse(layout, dateStr)
				if err != nil {
					return nil, nil, eofFinal, paddingFinal, optionalPadding, optionalEOFBytes, fmt.Errorf("failed to parse the time string")
				}
				year, month, day := t.Date()
				hour, minute, second := t.Clock()
				colValue.Type = models.FieldTypeTimestamp
				colValue.Value = fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, int(month), day, hour, minute, second)
				length = dataLength // Including the initial byte for dataLength
			}

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
		// Check if the converted value is actually correct.
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
			buf.WriteByte(column.PacketHeader.PacketLength)
			// TODO: Find out an appropriate type for Packet Length (since the packet structure has three bytes for packet length
			buf.WriteByte(0x00)
			buf.WriteByte(0x00)
			buf.WriteByte(sequenceID)

			writeLengthEncodedString(buf, column.Catalog)
			writeLengthEncodedString(buf, column.Schema)
			writeLengthEncodedString(buf, column.Table)
			writeLengthEncodedString(buf, column.OrgTable)
			writeLengthEncodedString(buf, column.Name)
			writeLengthEncodedString(buf, column.OrgName)
			buf.WriteByte(0x0c) // Length of the fixed-length fields (12 bytes)
			err := binary.Write(buf, binary.LittleEndian, column.CharacterSet)
			if err != nil {
				return nil, err
			}
			err = binary.Write(buf, binary.LittleEndian, column.ColumnLength)
			if err != nil {
				return nil, err
			}
			buf.WriteByte(column.ColumnType)
			err = binary.Write(buf, binary.LittleEndian, column.Flags)
			if err != nil {
				return nil, err
			}
			buf.WriteByte(column.Decimals)
			buf.Write([]byte{0x00, 0x00}) // Filler
		}
	}

	// Write EOF packet header
	if resultSet.EOFPresent {
		sequenceID++
		buf.Write(resultSet.EOFAfterColumns)
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
		b, _ := encodeRow(row, row.Columns)
		buf.Write(b)
	}
	//sequenceID++
	// Write EOF packet header again
	// buf.Write([]byte{5, 0, 0, sequenceID})
	buf.Write(resultSet.OptionalEOFBytes)
	if resultSet.PaddingPresentFinal {
		buf.Write([]byte{0x00, 0x00}) // Add padding bytes
	}
	return buf.Bytes(), nil
}

func encodeRow(_ *models.Row, columnValues []models.RowColumnDefinition) ([]byte, error) {
	var buf bytes.Buffer

	// Write the header
	//binary.Write(&buf, binary.LittleEndian, uint32(row.Header.PacketLength))
	//buf.WriteByte(row.Header.PacketSequenceID)

	for _, column := range columnValues {
		value := column.Value
		switch models.FieldType(column.Type) {
		case models.FieldTypeTimestamp:
			timestamp, ok := value.(string)
			// Check if the timestamp is base64 encoded.
			if !ok {
				return nil, errors.New("could not convert value to string")
			}
			t, err := time.Parse("2006-01-02 15:04:05", timestamp)
			if err != nil {
				return nil, errors.New("could not parse timestamp value")
			}

			buf.WriteByte(0x13) // Length of the following encoded data
			yearBytes := make([]byte, 2)
			binary.LittleEndian.PutUint16(yearBytes, uint16(t.Year()))
			// buf.Write(fmt.Sprintf("%04d",yearBytes))
			formattedTime := t.Format("2006-01-02 15:04:05")
			buf.WriteString(formattedTime)
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

//func encodeInt32(val int32) []byte {
//	buf := make([]byte, 4)
//	binary.LittleEndian.PutUint32(buf, uint32(val))
//	return buf
//}
//
//func encodeFloat32(val float32) []byte {
//	buf := make([]byte, 4)
//	binary.LittleEndian.PutUint32(buf, math.Float32bits(val))
//	return buf
//}
//
//func encodeFloat64(val float64) []byte {
//	buf := make([]byte, 8)
//	binary.LittleEndian.PutUint64(buf, math.Float64bits(val))
//	return buf
//}
