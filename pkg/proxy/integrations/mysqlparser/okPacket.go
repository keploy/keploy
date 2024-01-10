package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/pkg/models"
)

type OKPacket struct {
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows,omitempty,flow"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id,omitempty,flow"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags,omitempty,flow"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings,omitempty,flow"`
	Info         string `json:"info,omitempty" yaml:"info,omitempty,flow"`
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

func encodeMySQLOK(packet *models.MySQLOKPacket, header *models.MySQLPacketHeader) ([]byte, error) {
	buf := new(bytes.Buffer)
	// payload (without the header)
	payload := new(bytes.Buffer)
	// header (0x00)
	payload.WriteByte(0x00)
	//  affected rows
	payload.Write(encodeLengthEncodedInteger(packet.AffectedRows))
	//last insert ID
	payload.Write(encodeLengthEncodedInteger(packet.LastInsertID))
	// status flags
	binary.Write(payload, binary.LittleEndian, packet.StatusFlags)
	// warnings
	binary.Write(payload, binary.LittleEndian, packet.Warnings)
	// info
	if len(packet.Info) > 0 {
		payload.WriteString(packet.Info)
	}

	// header bytes
	//  packet length (3 bytes)
	packetLength := uint32(payload.Len())
	buf.WriteByte(byte(packetLength))
	buf.WriteByte(byte(packetLength >> 8))
	buf.WriteByte(byte(packetLength >> 16))
	// Write packet sequence number (1 byte)
	buf.WriteByte(header.PacketNumber)

	// Write payload
	buf.Write(payload.Bytes())

	return buf.Bytes(), nil
}

func encodeMySQLOKConnectionPhase(packet interface{}, operation string, sequence int) ([]byte, error) {
	innerPacket, ok := packet.(*interface{})
	if ok {
		packet = *innerPacket
	}
	p, ok := packet.(*models.MySQLOKPacket)
	if !ok {
		return nil, fmt.Errorf("invalid packet type for HandshakeResponse: expected *HandshakeResponse, got %T", packet)
	}
	buf := new(bytes.Buffer)
	payload := new(bytes.Buffer)
	// header (0x00)
	payload.WriteByte(0x00)
	// affected rows
	payload.Write(encodeLengthEncodedInteger(p.AffectedRows))
	//last insert ID
	payload.Write(encodeLengthEncodedInteger(p.LastInsertID))
	//status flags
	binary.Write(payload, binary.LittleEndian, p.StatusFlags)
	// warnings
	binary.Write(payload, binary.LittleEndian, p.Warnings)
	// info
	if len(p.Info) > 0 {
		payload.Write([]byte{0})
		payload.WriteString(p.Info)
	}
	// header bytes
	// packet length (3 bytes)
	packetLength := uint32(payload.Len())
	buf.WriteByte(byte(packetLength))
	buf.WriteByte(byte(packetLength >> 8))
	buf.WriteByte(byte(packetLength >> 16))
	buf.WriteByte(byte(sequence))
	buf.Write(payload.Bytes())

	return buf.Bytes(), nil
}
