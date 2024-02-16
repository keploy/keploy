package mysql

import (
	"encoding/binary"
	"errors"
)

type PasswordData struct {
	PayloadLength uint32 `json:"payload_length,omitempty" yaml:"payload_length,omitempty,flow"`
	SequenceID    byte   `json:"sequence_id,omitempty" yaml:"sequence_id,omitempty,flow"`
	Payload       []byte `json:"payload,omitempty" yaml:"payload,omitempty,flow"`
}

func decodeEncryptPassword(data []byte) (string, *PasswordData, error) {
	var packetType = "ENCRYPT_PASSWORD"

	if len(data) < 4 {
		return packetType, nil, errors.New("data too short for MySQL packet")
	}

	payloadLength := Uint24(data[:3])
	sequenceID := data[3]

	if len(data) < 4+int(payloadLength) {
		return packetType, nil, errors.New("payload length mismatch")
	}
	return packetType, &PasswordData{
		PayloadLength: payloadLength,
		SequenceID:    sequenceID,
		Payload:       data[4:],
	}, nil
}

func encodeEncryptPassword(packet *PasswordData) ([]byte, error) {
	if packet == nil {
		return nil, errors.New("nil packet provided for encoding")
	}

	// Convert PayloadLength from uint32 to 3-byte representation
	lengthBytes := make([]byte, 3)
	binary.LittleEndian.PutUint32(lengthBytes, packet.PayloadLength)
	lengthBytes = lengthBytes[:3]

	// Construct the encoded packet
	encodedPacket := make([]byte, 0, 4+len(packet.Payload))
	encodedPacket = append(encodedPacket, lengthBytes...)
	encodedPacket = append(encodedPacket, packet.SequenceID)
	encodedPacket = append(encodedPacket, packet.Payload...)

	return encodedPacket, nil
}
