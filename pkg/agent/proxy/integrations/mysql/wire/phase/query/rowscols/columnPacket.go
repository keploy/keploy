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

// columnFixedTailLen is the length of a column definition's fixed-length
// fields after its six length-encoded strings.
const columnFixedTailLen = 13

// DecodeColumn decodes the column definition packet b (with its 4-byte
// header) as the connection frames it: with MariaDB's extended type
// information after org_name when framing says so.
func DecodeColumn(_ context.Context, _ *zap.Logger, b []byte, framing mysql.MetadataFraming) (*mysql.ColumnDefinition41, int, error) {
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

	if framing.ExtendedMetadata() {
		ext, n, err := decodeExtendedMetadata(b[pos:])
		if err != nil {
			return nil, pos, err
		}
		packet.ExtendedMetadata = ext
		pos += n
	}

	// The fixed-length tail: [0x0c], character_set(2), column_length(4),
	// type(1), flags(2), decimals(1), filler(2). A column definition cut
	// short (misframed, or a capture hole) must fail here, not panic below:
	// a panic kills the recorder for the rest of its connection.
	if len(b)-pos < columnFixedTailLen {
		return nil, pos, fmt.Errorf("column definition truncated: %d of its %d fixed-length bytes present", max(len(b)-pos, 0), columnFixedTailLen)
	}

	// [0x0c], the length of the fixed-length fields. Anything else means the
	// fields before it were not where this decoder read them (a MariaDB
	// column definition read without its extended type information, or the
	// reverse): decoding on would shift every field after it.
	if b[pos] != 0x0c {
		return nil, pos, fmt.Errorf("column definition's fixed-length fields are not where they belong: 0x%02x where their length 0x0c is", b[pos])
	}
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

	// If more data, the command was COM_FIELD_LIST: a default value follows.
	// pos counts the 4-byte header, PayloadLength does not.
	if packet.Header.PayloadLength > uint32(pos-4) {
		//length of default value lenenc-int
		defaultValueLength, _, n := utils.ReadLengthEncodedInteger(b[pos:])
		if n == 0 {
			return nil, pos, fmt.Errorf("column definition truncated before its default value's length")
		}
		pos += n

		if defaultValueLength > uint64(len(b)-pos) {
			return nil, pos, fmt.Errorf("column definition's default value (%d bytes) overruns the packet", defaultValueLength)
		}

		//default value string[$len]
		packet.DefaultValue = string(b[pos:(pos + int(defaultValueLength))])
		pos += int(defaultValueLength)
	}

	return packet, pos, nil
}

// decodeExtendedMetadata decodes a MariaDB column definition's extended type
// information at the start of b: a length-encoded length, then that many
// bytes of entries, each a one-byte kind and a length-encoded string. It
// returns the entries and the bytes read.
func decodeExtendedMetadata(b []byte) ([]mysql.ColumnExtendedMetadata, int, error) {
	size, _, n := utils.ReadLengthEncodedInteger(b)
	if n == 0 {
		return nil, 0, fmt.Errorf("column definition truncated before its extended type information")
	}
	if size > uint64(len(b)-n) {
		return nil, 0, fmt.Errorf("column definition's extended type information (%d bytes) overruns the packet", size)
	}
	block := b[n : n+int(size)]
	var ext []mysql.ColumnExtendedMetadata
	for len(block) > 0 {
		kind := block[0]
		value, _, m, err := utils.ReadLengthEncodedString(block[1:])
		if err != nil {
			return nil, 0, fmt.Errorf("column definition's extended type information does not decode: %w", err)
		}
		ext = append(ext, mysql.ColumnExtendedMetadata{Kind: kind, Value: string(value)})
		block = block[1+m:]
	}
	return ext, n + int(size), nil
}

// EncodeColumn encodes packet as a column definition packet (with its 4-byte
// header) framed as the connection it is sent on negotiated: with its MariaDB
// extended type information when framing says so (an empty one where it has
// none), without it otherwise.
func EncodeColumn(_ context.Context, _ *zap.Logger, packet *mysql.ColumnDefinition41, framing mysql.MetadataFraming) ([]byte, error) {
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

	if framing.ExtendedMetadata() {
		ext := new(bytes.Buffer)
		for _, e := range packet.ExtendedMetadata {
			ext.WriteByte(e.Kind)
			if err := utils.WriteLengthEncodedString(ext, e.Value); err != nil {
				return nil, fmt.Errorf("failed to write extended type information: %w", err)
			}
		}
		if err := utils.WriteLengthEncodedInteger(body, uint64(ext.Len())); err != nil {
			return nil, fmt.Errorf("failed to write extended type information's length: %w", err)
		}
		body.Write(ext.Bytes())
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
