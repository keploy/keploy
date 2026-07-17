package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
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

	// Bounds guard for the fixed-length metadata block that follows. From the
	// current pos we read, at FIXED offsets: the 0x0c length byte (1) +
	// character_set (2) + column_length (4) + type (1) + flags (2) +
	// decimals (1) + filler (2) = 13 bytes. Without this guard a short or
	// misrouted buffer (e.g. a non-column packet decoded here because an
	// upstream column-count was wrong, or a genuinely truncated packet) would
	// index past the slice and PANIC — crashing the async decoder goroutine and
	// tearing down the whole connection's recording (observed as
	// "Recovered from panic ... closing active connections"). Returning an error
	// instead lets the caller degrade gracefully (it logs and resets to
	// stateExpectCommand) so the rest of the session still records.
	if pos+13 > len(b) {
		return nil, pos, fmt.Errorf("short column-definition packet: need 13 bytes for fixed-field block at pos %d, buffer len %d", pos, len(b))
	}

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
		// COM_FIELD_LIST response carries a length-encoded default value here.
		// Guard every read the same way the fixed-field block above is guarded:
		// the lenenc prefix must be present, the decode must consume at least
		// one byte, and the declared length must fit the remaining buffer. Any
		// violation returns an error (the caller logs and resets to
		// stateExpectCommand) instead of indexing past the slice and panicking.
		if pos >= len(b) {
			return nil, pos, fmt.Errorf("short column-definition packet: field-list default-value length prefix missing at pos %d, buffer len %d", pos, len(b))
		}
		//length of default value lenenc-int
		defaultValueLength, _, n := utils.ReadLengthEncodedInteger(b[pos:])
		if n <= 0 {
			return nil, pos, fmt.Errorf("malformed column-definition packet: unreadable field-list default-value length prefix at pos %d, buffer len %d", pos, len(b))
		}
		pos += n

		if defaultValueLength > uint64(len(b)-pos) {
			return nil, pos, fmt.Errorf("malformed column-definition packet: field-list default-value length %d overflows buffer (pos %d, buffer len %d)", defaultValueLength, pos, len(b))
		}

		//default value string[$len]
		packet.DefaultValue = string(b[pos:(pos + int(defaultValueLength))])
		pos--
	}

	return packet, pos, nil
}

func EncodeColumn(_ context.Context, _ *zap.Logger, packet *mysql.ColumnDefinition41) ([]byte, error) {
	// Build the body first
	body := new(bytes.Buffer)

	// Catalog, Schema, Table, OrgTable, Name, OrgName
	if err := utils.WriteLengthEncodedString(body, packet.Catalog); err != nil {
		return nil, fmt.Errorf("failed to write Catalog: %w", err)
	}
	if err := utils.WriteLengthEncodedString(body, packet.Schema); err != nil {
		return nil, fmt.Errorf("failed to write Schema: %w", err)
	}
	if err := utils.WriteLengthEncodedString(body, packet.Table); err != nil {
		return nil, fmt.Errorf("failed to write Table: %w", err)
	}
	if err := utils.WriteLengthEncodedString(body, packet.OrgTable); err != nil {
		return nil, fmt.Errorf("failed to write OrgTable: %w", err)
	}
	if err := utils.WriteLengthEncodedString(body, packet.Name); err != nil {
		return nil, fmt.Errorf("failed to write Name: %w", err)
	}
	if err := utils.WriteLengthEncodedString(body, packet.OrgName); err != nil {
		return nil, fmt.Errorf("failed to write OrgName: %w", err)
	}

	// Fixed-length fields length (always 0x0c)
	if err := body.WriteByte(packet.FixedLength); err != nil {
		return nil, fmt.Errorf("failed to write FixedLength: %w", err)
	}

	// CharacterSet, ColumnLength, Type, Flags, Decimals
	if err := binary.Write(body, binary.LittleEndian, packet.CharacterSet); err != nil {
		return nil, fmt.Errorf("failed to write CharacterSet: %w", err)
	}
	if err := binary.Write(body, binary.LittleEndian, packet.ColumnLength); err != nil {
		return nil, fmt.Errorf("failed to write ColumnLength: %w", err)
	}
	if err := body.WriteByte(packet.Type); err != nil {
		return nil, fmt.Errorf("failed to write Type: %w", err)
	}
	if err := binary.Write(body, binary.LittleEndian, packet.Flags); err != nil {
		return nil, fmt.Errorf("failed to write Flags: %w", err)
	}
	if err := body.WriteByte(packet.Decimals); err != nil {
		return nil, fmt.Errorf("failed to write Decimals: %w", err)
	}

	// Filler (2 bytes)
	filler := packet.Filler
	if len(filler) != 2 {
		filler = []byte{0x00, 0x00}
	}
	if _, err := body.Write(filler); err != nil {
		return nil, fmt.Errorf("failed to write Filler: %w", err)
	}

	// Optional DefaultValue (COM_FIELD_LIST case)
	if packet.DefaultValue != "" {
		if err := utils.WriteLengthEncodedString(body, packet.DefaultValue); err != nil {
			return nil, fmt.Errorf("failed to write DefaultValue: %w", err)
		}
	}

	// Prepend header with computed payload length
	out := new(bytes.Buffer)
	if err := utils.WriteUint24(out, uint32(body.Len())); err != nil {
		return nil, fmt.Errorf("failed to write PayloadLength: %w", err)
	}
	if err := out.WriteByte(packet.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}
	if _, err := out.Write(body.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
