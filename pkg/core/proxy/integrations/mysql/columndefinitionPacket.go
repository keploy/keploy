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

	//catalog
	catalog, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Catalog = string(catalog)
	pos += n

	//schema
	schema, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Schema = string(schema)
	pos += n

	//table
	table, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Table = string(table)
	pos += n

	//org_table
	orgTable, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.OrgTable = string(orgTable)
	pos += n

	//name
	name, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Name = string(name)
	pos += n

	//org_name
	orgName, _, n, err := readLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.OrgName = string(orgName)
	pos += n

	// skip [0x0c] (length of fixed-length fields)
	pos++

	//character_set
	packet.CharacterSet = binary.LittleEndian.Uint16(b[pos:])
	pos += 2

	//column_length
	packet.ColumnLength = binary.LittleEndian.Uint32(b[pos:])
	pos += 4

	//type
	packet.ColumnType = b[pos]
	pos++

	// flag
	packet.Flags = binary.LittleEndian.Uint16(b[pos:])
	pos += 2

	// decimals 1
	packet.Decimals = b[pos]
	pos++

	//filler [0x00][0x00]
	pos += 2

	//if more data, command was field list
	if len(b) > pos {
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

	return packet, pos, nil
}
