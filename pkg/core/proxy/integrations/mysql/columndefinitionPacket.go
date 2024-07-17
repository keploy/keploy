package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

// Each column definition is a separte packet
func parseColumnDefinitionPacket(b []byte) (*models.ColumnDefinition, int, error) {
	packet := &models.ColumnDefinition{}
	if len(b) < 4 {
		return nil, 0, fmt.Errorf("invalid column definition packet")
	}
	var pos = 0
	// Read packet header
	packet.PacketHeader.PacketLength = uint8(readUint24(b[pos : pos+3]))
	packet.PacketHeader.PacketSequenceID = uint8(b[pos+3])
	pos += 4

	fmt.Printf("Starting column packet with:%x %x %x %x:", b[pos], b[pos+1], b[pos+2], b[pos+3])
	fmt.Println()

	//catalog
	catalog, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("catalog", catalog)
	packet.Catalog = string(catalog)
	pos += n

	//schema
	schema, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("schema", schema)
	packet.Schema = string(schema)
	pos += n

	//table
	table, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("table", table)
	packet.Table = string(table)
	pos += n

	//org_table
	orgTable, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("org_table", orgTable)
	packet.OrgTable = string(orgTable)
	pos += n

	//name
	name, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("name", name)
	packet.Name = string(name)
	pos += n

	//org_name
	orgName, _, n, err := readLengthEncodedStrings(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	printByteArray("org_name", orgName)
	packet.OrgName = string(orgName)
	pos += n

	// skip [0x0c] (length of fixed-length fields)
	pos++

	//character_set
	packet.CharacterSet = binary.LittleEndian.Uint16(b[pos:])
	printByteArray("character_set", b[pos:pos+2])
	pos += 2

	//column_length
	packet.ColumnLength = binary.LittleEndian.Uint32(b[pos:])
	printByteArray("column_length", b[pos:pos+4])
	pos += 4

	//type
	packet.ColumnType = b[pos]
	printByteArray("column_type", b[pos:pos+1])
	pos++

	// flag
	packet.Flags = binary.LittleEndian.Uint16(b[pos:])
	printByteArray("flags", b[pos:pos+2])
	pos += 2

	// decimals 1
	packet.Decimals = b[pos]
	printByteArray("decimals", b[pos:pos+1])
	pos++

	//filler [0x00][0x00]
	pos += 2

	println("Length of column packet after parsing:", pos)

	fmt.Printf("before getting default values are:%x %x %x %x", b[pos-4], b[pos-3], b[pos-2], b[pos-1])
	fmt.Println()

	//if more data, command was field list
	if len(b) > pos {
		println("More data in column packet")
		//length of default value lenenc-int
		defaultValueLength, _, n := readLengthEncodedInteger(b[pos:])
		pos += n

		if pos+int(defaultValueLength) > len(b) {

			return nil, pos, fmt.Errorf("malformed packet: %v", err)
		}

		//default value string[$len]
		packet.DefaultValue = string(b[pos:(pos + int(defaultValueLength))])
		pos--
	}

	fmt.Printf("the last four bytes of the column packet are:%x %x %x %x", b[pos-4], b[pos-3], b[pos-2], b[pos-1])
	fmt.Println()
	return packet, pos, nil
}
