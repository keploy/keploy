//go:build linux

package generic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

// ref:https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_ok_packet.html

func DecodeOk(_ context.Context, data []byte, capabilities uint32) (*mysql.OKPacket, error) {
	if len(data) < 7 {
		return nil, fmt.Errorf("OK packet too short")
	}

	packet := &mysql.OKPacket{
		Header: data[0],
	}

	var n int
	var pos = 1

	packet.AffectedRows, _, n = utils.ReadLengthEncodedInteger(data[pos:])
	pos += n
	packet.LastInsertID, _, n = utils.ReadLengthEncodedInteger(data[pos:])
	pos += n

	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		packet.StatusFlags = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
		packet.Warnings = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
	} else if capabilities&uint32(mysql.CLIENT_TRANSACTIONS) > 0 {
		packet.StatusFlags = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
	}

	// new ok package will check CLIENT_SESSION_TRACK too, but it is not supported in this version

	if pos < len(data) {
		packet.Info = string(data[pos:])
	}
	fmt.Printf("from decode func", packet)
	return packet, nil
}
func EncodeOk(_ context.Context, packet *mysql.OKPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write Header: %w", err)
	}

	// Write Affected Rows
	if err := utils.WriteLengthEncodedInteger(buf, packet.AffectedRows); err != nil {
		return nil, fmt.Errorf("failed to write AffectedRows: %w", err)
	}

	// Write Last Insert ID
	if err := utils.WriteLengthEncodedInteger(buf, packet.LastInsertID); err != nil {
		return nil, fmt.Errorf("failed to write LastInsertID: %w", err)
	}

	// Write Status Flags and Warnings
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write StatusFlags: %w", err)
		}
		if err := binary.Write(buf, binary.LittleEndian, packet.Warnings); err != nil {
			return nil, fmt.Errorf("failed to write Warnings: %w", err)
		}
	} else if capabilities&uint32(mysql.CLIENT_TRANSACTIONS) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write StatusFlags: %w", err)
		}
	}

	// Write Info
	if packet.Info != "" {
		if _, err := buf.WriteString(packet.Info); err != nil {
			return nil, fmt.Errorf("failed to write Info: %w", err)
		}
	}

	return buf.Bytes(), nil
}
