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

func DecodeColumn(ctx context.Context, logger *zap.Logger, b []byte) (*mysql.ColumnDefinition41, int, error) {
	packet := &mysql.ColumnDefinition41{
		Header: mysql.Header{},
	}
	if len(b) < 4 {
		logger.Error("Invalid column definition packet: too short")
		return nil, 0, fmt.Errorf("invalid column definition packet")
	}
	var pos = 0

	// Read packet header
	packet.Header.PayloadLength = utils.ReadUint24(b[pos : pos+3])
	packet.Header.SequenceID = uint8(b[pos+3])
	pos += 4
	logger.Info("Decoded packet header", zap.Uint32("PayloadLength", packet.Header.PayloadLength), zap.Uint8("SequenceID", packet.Header.SequenceID))

	//catalog
	catalog, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading catalog", zap.Error(err))
		return nil, pos, err
	}
	packet.Catalog = string(catalog)
	pos += n
	logger.Info("Decoded catalog", zap.String("Catalog", packet.Catalog))

	//schema
	schema, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading schema", zap.Error(err))
		return nil, pos, err
	}
	packet.Schema = string(schema)
	pos += n
	logger.Info("Decoded schema", zap.String("Schema", packet.Schema))

	//table
	table, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading table", zap.Error(err))
		return nil, pos, err
	}
	packet.Table = string(table)
	pos += n
	logger.Info("Decoded table", zap.String("Table", packet.Table))

	//org_table
	orgTable, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading org_table", zap.Error(err))
		return nil, pos, err
	}
	packet.OrgTable = string(orgTable)
	pos += n
	logger.Info("Decoded org_table", zap.String("OrgTable", packet.OrgTable))

	//name
	name, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading name", zap.Error(err))
		return nil, pos, err
	}
	packet.Name = string(name)
	pos += n
	logger.Info("Decoded name", zap.String("Name", packet.Name))

	//org_name
	orgName, _, n, err := utils.ReadLengthEncodedString(b[pos:])
	if err != nil {
		logger.Error("Error reading org_name", zap.Error(err))
		return nil, pos, err
	}
	packet.OrgName = string(orgName)
	pos += n
	logger.Info("Decoded org_name", zap.String("OrgName", packet.OrgName))

	// skip [0x0c] (length of fixed-length fields)
	packet.FixedLength = b[pos]
	pos++
	logger.Info("Skipped fixed-length fields", zap.Uint8("FixedLength", packet.FixedLength))

	//character_set
	packet.CharacterSet = binary.LittleEndian.Uint16(b[pos:])
	pos += 2
	logger.Info("Decoded character_set", zap.Uint16("CharacterSet", packet.CharacterSet))

	//column_length
	packet.ColumnLength = binary.LittleEndian.Uint32(b[pos:])
	pos += 4
	logger.Info("Decoded column_length", zap.Uint32("ColumnLength", packet.ColumnLength))

	//type
	packet.Type = b[pos]
	pos++
	logger.Info("Decoded type", zap.Uint8("Type", packet.Type))

	// flag
	packet.Flags = binary.LittleEndian.Uint16(b[pos:])
	pos += 2
	logger.Info("Decoded flags", zap.Uint16("Flags", packet.Flags))

	// decimals 1
	packet.Decimals = b[pos]
	pos++
	logger.Info("Decoded decimals", zap.Uint8("Decimals", packet.Decimals))

	//filler [0x00][0x00]
	packet.Filler = b[pos : pos+2]
	pos += 2
	logger.Info("Decoded filler", zap.ByteString("Filler", packet.Filler))

	//if more data, command was field list
	if packet.Header.PayloadLength > uint32(pos) {
		//length of default value lenenc-int
		defaultValueLength, _, n := utils.ReadLengthEncodedInteger(b[pos:])
		pos += n
		logger.Info("Default value length", zap.Uint64("defaultValueLength", defaultValueLength))

		if pos+int(defaultValueLength) > len(b) {
			logger.Error("Malformed packet: insufficient data for default value")
			return nil, pos, fmt.Errorf("malformed packet: %v", err)
		}

		//default value string[$len]
		packet.DefaultValue = string(b[pos:(pos + int(defaultValueLength))])
		pos--
		logger.Info("Decoded default value", zap.String("DefaultValue", packet.DefaultValue))
	}

	logger.Info("Successfully decoded column packet", zap.Any("packet", packet))
	return packet, pos, nil
}

func writeLengthEncodedString(buf *bytes.Buffer, s string) error {
	length := len(s)
	switch {
	case length <= 250:
		if err := buf.WriteByte(byte(length)); err != nil {
			return err
		}
	case length <= 0xFFFF:
		if err := buf.WriteByte(0xFC); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint16(length)); err != nil {
			return err
		}
	case length <= 0xFFFFFF:
		if err := buf.WriteByte(0xFD); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint32(length)&0xFFFFFF); err != nil {
			return err
		}
	default:
		if err := buf.WriteByte(0xFE); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint64(length)); err != nil {
			return err
		}
	}
	if _, err := buf.WriteString(s); err != nil {
		return err
	}
	return nil
}

func EncodeColumn(_ context.Context, _ *zap.Logger, packet *mysql.ColumnDefinition41) ([]byte, error) {
	buf := new(bytes.Buffer)

	fmt.Println(buf.Bytes(), "Initial buffer")

	// Write packet header
	fmt.Printf("PayloadLength: %v\n", packet.Header.PayloadLength)
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
	if err := writeLengthEncodedString(buf, packet.Catalog); err != nil {
		return nil, fmt.Errorf("failed to write Catalog: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Catalog")

	// Write Schema
	fmt.Printf("Schema: %v\n", packet.Schema)
	if err := writeLengthEncodedString(buf, packet.Schema); err != nil {
		return nil, fmt.Errorf("failed to write Schema: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Schema")

	// Write Table
	fmt.Printf("Table: %v\n", packet.Table)
	if err := writeLengthEncodedString(buf, packet.Table); err != nil {
		return nil, fmt.Errorf("failed to write Table: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Table")

	// Write OrgTable
	fmt.Printf("OrgTable: %v\n", packet.OrgTable)
	if err := writeLengthEncodedString(buf, packet.OrgTable); err != nil {
		return nil, fmt.Errorf("failed to write OrgTable: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing OrgTable")

	// Write Name
	fmt.Printf("Name: %v\n", packet.Name)
	if err := writeLengthEncodedString(buf, packet.Name); err != nil {
		return nil, fmt.Errorf("failed to write Name: %w", err)
	}
	fmt.Println(buf.Bytes(), "After writing Name")

	// Write OrgName
	fmt.Printf("OrgName: %v\n", packet.OrgName)
	if err := writeLengthEncodedString(buf, packet.OrgName); err != nil {
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
		if err := writeLengthEncodedString(buf, packet.DefaultValue); err != nil {
			return nil, fmt.Errorf("failed to write DefaultValue: %w", err)
		}
		fmt.Println(buf.Bytes(), "After writing DefaultValue")
	}

	fmt.Println(buf.Bytes(), "Final buffer")

	return buf.Bytes(), nil
}
