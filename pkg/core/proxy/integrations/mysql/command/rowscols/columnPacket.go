//go:build linux

package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset_column_definition.html

func DecodeColumn(_ context.Context, _ *zap.Logger, b []byte) (*mysql.ColumnDefinition41, int, error) {
	packet := &mysql.ColumnDefinition41{
		Header: mysql.Header{},
	}
	if len(b) < 4 {
		return nil, 0, fmt.Errorf("invalid column definition packet")
	}
	var pos = 0

	// Read packet header
	packet.Header.PayloadLength = utils.ReadUint24(b[pos : pos+3])
	packet.Header.SequenceID = uint8(b[pos+3])
	pos += 4

	//catalog
	catalog, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Catalog = string(catalog)
	pos += n

	//schema
	schema, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Schema = string(schema)
	pos += n

	//table
	table, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Table = string(table)
	pos += n

	//org_table
	orgTable, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.OrgTable = string(orgTable)
	pos += n

	//name
	name, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.Name = string(name)
	pos += n

	//org_name
	orgName, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		return nil, pos, err
	}
	packet.OrgName = string(orgName)
	pos += n

	// skip [0x0c] (length of fixed-length fields)
	packet.FixedLength = 0x0c
	pos++

	//character_set
	packet.CharacterSet = binary.LittleEndian.Uint16(b[pos:])
	pos += 2

	//column_length
	packet.ColumnLength = binary.LittleEndian.Uint32(b[pos:])
	pos += 4

	//type
	packet.Type = b[pos]
	pos++

	// flag
	packet.Flags = binary.LittleEndian.Uint16(b[pos:])
	pos += 2

	// decimals 1
	packet.Decimals = b[pos]
	pos++

	//filler [0x00][0x00]
	packet.Filler = b[pos : pos+2]
	pos += 2

	//if more data, command was field list
	if packet.Header.PayloadLength > uint32(pos) {
		//length of default value lenenc-int
		defaultValueLength, _, n := utils.ReadLengthEncodedInteger(b[pos:])
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

func EncodeColumn(_ context.Context, _ *zap.Logger, packet *mysql.ColumnDefinition41) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write packet header
	if err := utils.WriteUint24(buf, packet.Header.PayloadLength); err != nil {
		return nil, fmt.Errorf("failed to write PayloadLength: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing PayloadLength")

	fmt.Printf("SequenceID: %v\n", packet.Header.SequenceID)
	if err := buf.WriteByte(packet.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing SequenceID")

	// Write Catalog
	fmt.Printf("Catalog: %v\n", packet.Catalog)
	if err := utils.WriteLengthEncodedString(buf, packet.Catalog); err != nil {
		return nil, fmt.Errorf("failed to write Catalog: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Catalog")

	// Write Schema
	fmt.Printf("Schema: %v\n", packet.Schema)
	if err := utils.WriteLengthEncodedString(buf, packet.Schema); err != nil {
		return nil, fmt.Errorf("failed to write Schema: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Schema")

	// Write Table
	fmt.Printf("Table: %v\n", packet.Table)
	if err := utils.WriteLengthEncodedString(buf, packet.Table); err != nil {
		return nil, fmt.Errorf("failed to write Table: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Table")

	// Write OrgTable
	fmt.Printf("OrgTable: %v\n", packet.OrgTable)
	if err := utils.WriteLengthEncodedString(buf, packet.OrgTable); err != nil {
		return nil, fmt.Errorf("failed to write OrgTable: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing OrgTable")

	// Write Name
	fmt.Printf("Name: %v\n", packet.Name)
	if err := utils.WriteLengthEncodedString(buf, packet.Name); err != nil {
		return nil, fmt.Errorf("failed to write Name: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Name")

	// Write OrgName
	fmt.Printf("OrgName: %v\n", packet.OrgName)
	if err := utils.WriteLengthEncodedString(buf, packet.OrgName); err != nil {
		return nil, fmt.Errorf("failed to write OrgName: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing OrgName")

	// Write FixedLength (0x0c)
	fmt.Printf("FixedLength: %v\n", packet.FixedLength)
	if err := buf.WriteByte(packet.FixedLength); err != nil {
		return nil, fmt.Errorf("failed to write FixedLength: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing FixedLength")

	// Write CharacterSet
	fmt.Printf("CharacterSet: %v\n", packet.CharacterSet)
	if err := binary.Write(buf, binary.LittleEndian, packet.CharacterSet); err != nil {
		return nil, fmt.Errorf("failed to write CharacterSet: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing CharacterSet")

	// Write ColumnLength
	fmt.Printf("ColumnLength: %v\n", packet.ColumnLength)
	if err := binary.Write(buf, binary.LittleEndian, packet.ColumnLength); err != nil {
		return nil, fmt.Errorf("failed to write ColumnLength: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing ColumnLength")

	// Write Type
	fmt.Printf("Type: %v\n", packet.Type)
	if err := buf.WriteByte(packet.Type); err != nil {
		return nil, fmt.Errorf("failed to write Type: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Type")

	// Write Flags
	fmt.Printf("Flags: %v\n", packet.Flags)
	if err := binary.Write(buf, binary.LittleEndian, packet.Flags); err != nil {
		return nil, fmt.Errorf("failed to write Flags: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Flags")

	// Write Decimals
	fmt.Printf("Decimals: %v\n", packet.Decimals)
	if err := buf.WriteByte(packet.Decimals); err != nil {
		return nil, fmt.Errorf("failed to write Decimals: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Decimals")

	// Write Filler
	fmt.Printf("Filler: %v\n", packet.Filler)
	if _, err := buf.Write(packet.Filler); err != nil {
		return nil, fmt.Errorf("failed to write Filler: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Filler")

	// Write DefaultValue if it exists
	if packet.DefaultValue != "" {
		fmt.Printf("DefaultValue: %v\n", packet.DefaultValue)
		if err := utils.WriteLengthEncodedString(buf, packet.DefaultValue); err != nil {
			return nil, fmt.Errorf("failed to write DefaultValue: %w", err)
		}
		fmt.Println(buf.Bytes(), "After writing DefaultValue")
	}

	fmt.Println(buf.Bytes(), "Final buffer")

	return buf.Bytes(), nil
}
