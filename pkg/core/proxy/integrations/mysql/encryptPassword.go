package mysql

import (
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
