package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/pkg/models"
)

type OKPacket struct {
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings"`
	Info         string `json:"info,omitempty" yaml:"info"`
}

func decodeMySQLOK(data []byte) (*OKPacket, error) {
	if len(data) < 7 {
		return nil, fmt.Errorf("OK packet too short")
	}

	packet := &OKPacket{}
	var err error
	//identifier of ok packet
	var offset int = 1
	// Decode affected rows
	packet.AffectedRows, err = readLengthEncodedIntegerOff(data, &offset)
	if err != nil {
		return nil, fmt.Errorf("failed to decode info: %w", err)
	}
	// Decode last insert ID
	packet.LastInsertID, err = readLengthEncodedIntegerOff(data, &offset)
	if err != nil {
		return nil, fmt.Errorf("failed to decode info: %w", err)
	}

	if len(data) < offset+4 {
		return nil, fmt.Errorf("OK packet too short")
	}

	packet.StatusFlags = binary.LittleEndian.Uint16(data[offset:])
	offset += 2

	packet.Warnings = binary.LittleEndian.Uint16(data[offset:])
	offset += 2

	if offset < len(data) {
		packet.Info = string(data[offset:])
	}

	return packet, nil
}

func encodeMySQLOK(packet *models.MySQLOKPacket) ([]byte, error) {
	buf := new(bytes.Buffer)
	// Prepare the payload (without the header)
	payload := new(bytes.Buffer)
	// Write header (0x00)
	payload.WriteByte(0x00)
	// Write affected rows
	payload.Write(encodeLengthEncodedInteger(packet.AffectedRows))
	// Write last insert ID
	payload.Write(encodeLengthEncodedInteger(packet.LastInsertID))
	// Write status flags
	binary.Write(payload, binary.LittleEndian, packet.StatusFlags)
	// Write warnings
	binary.Write(payload, binary.LittleEndian, packet.Warnings)
	// Write info
	if len(packet.Info) > 0 {
		payload.Write([]byte{0})
		payload.WriteString(packet.Info)
	} else if payload.Bytes()[payload.Len()-1] == 0 {
		// Trim the extra 0 byte
		payload.Truncate(payload.Len() - 1)
	}

	// Write header bytes
	// Write packet length (3 bytes)
	packetLength := uint32(payload.Len())
	buf.WriteByte(byte(packetLength))
	buf.WriteByte(byte(packetLength >> 8))
	buf.WriteByte(byte(packetLength >> 16))
	// Write packet sequence number (1 byte)
	buf.WriteByte(1)

	// Write payload
	buf.Write(payload.Bytes())

	return buf.Bytes(), nil
}
