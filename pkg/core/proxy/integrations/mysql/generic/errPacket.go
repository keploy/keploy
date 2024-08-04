//go:build linux

package generic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_err_packet.html

func DecodeErr(_ context.Context, data []byte, capabilities uint32) (*mysql.ERRPacket, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("ERR packet too short")
	}

	packet := &mysql.ERRPacket{
		Header: data[0],
	}

	pos := 1
	packet.ErrorCode = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2

	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if data[pos] != '#' {
			return nil, fmt.Errorf("invalid SQL state marker: %c", data[pos])
		}
		packet.SQLStateMarker = string(data[pos])

		pos++
		packet.SQLState = string(data[pos : pos+5])
		pos += 5
	}

	packet.ErrorMessage = string(data[pos:])
	return packet, nil
}

func EncodeErr(_ context.Context, packet *mysql.ERRPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}

	// Write the error code
	if err := binary.Write(buf, binary.LittleEndian, packet.ErrorCode); err != nil {
		return nil, fmt.Errorf("failed to write error code: %w", err)
	}

	// Write the SQL state marker and SQL state if CLIENT_PROTOCOL_41 is set
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if len(packet.SQLStateMarker) != 1 || len(packet.SQLState) != 5 {
			return nil, fmt.Errorf("invalid SQL state marker or SQL state length")
		}

		if err := buf.WriteByte(packet.SQLStateMarker[0]); err != nil {
			return nil, fmt.Errorf("failed to write SQL state marker: %w", err)
		}

		if _, err := buf.WriteString(packet.SQLState); err != nil {
			return nil, fmt.Errorf("failed to write SQL state: %w", err)
		}
	}

	// Write the error message
	if _, err := buf.WriteString(packet.ErrorMessage); err != nil {
		return nil, fmt.Errorf("failed to write error message: %w", err)
	}

	return buf.Bytes(), nil
}
