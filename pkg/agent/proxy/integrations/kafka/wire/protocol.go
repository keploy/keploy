package wire

import (
	"encoding/binary"
	"fmt"
	"io"

	"go.keploy.io/server/v3/pkg/models/kafka"
)

// Packet represents a Kafka wire protocol packet
type Packet struct {
	Length  int32
	Payload []byte
}

// ReadPacket reads a complete Kafka packet from a reader
func ReadPacket(r io.Reader) (*Packet, error) {
	var length int32
	err := binary.Read(r, binary.BigEndian, &length)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, length)
	_, err = io.ReadFull(r, payload)
	if err != nil {
		return nil, err
	}

	return &Packet{
		Length:  length,
		Payload: payload,
	}, nil
}

// ReadPacketWithHeader reads a complete Kafka packet including the length header as raw bytes
func ReadPacketWithHeader(r io.Reader) ([]byte, error) {
	// Read 4-byte length header
	lengthBytes := make([]byte, 4)
	_, err := io.ReadFull(r, lengthBytes)
	if err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(lengthBytes)

	// Read the payload
	payload := make([]byte, length)
	_, err = io.ReadFull(r, payload)
	if err != nil {
		return nil, err
	}

	// Return complete message (length + payload)
	return append(lengthBytes, payload...), nil
}

// WritePacket writes a Kafka packet to a writer
func WritePacket(w io.Writer, p *Packet) error {
	err := binary.Write(w, binary.BigEndian, p.Length)
	if err != nil {
		return err
	}
	_, err = w.Write(p.Payload)
	return err
}

// WriteRawPacket writes raw bytes (including length header) to a writer
func WriteRawPacket(w io.Writer, data []byte) error {
	_, err := w.Write(data)
	return err
}

// RequestHeader represents a Kafka request header (for internal use during parsing)
type RequestHeader struct {
	ApiKey        kafka.ApiKey
	ApiVersion    int16
	CorrelationID int32
	ClientID      string
}

// ParseRequestHeader parses a Kafka request header from payload bytes
// Returns the header and the number of bytes consumed
func ParseRequestHeader(payload []byte) (*RequestHeader, int, error) {
	if len(payload) < 8 {
		return nil, 0, fmt.Errorf("payload too short for header: need at least 8 bytes, got %d", len(payload))
	}

	header := &RequestHeader{}
	header.ApiKey = kafka.ApiKey(binary.BigEndian.Uint16(payload[0:2]))
	header.ApiVersion = int16(binary.BigEndian.Uint16(payload[2:4]))
	header.CorrelationID = int32(binary.BigEndian.Uint32(payload[4:8]))

	offset := 8

	// ClientID is a nullable string. In Kafka protocol, length is int16.
	// If length is -1, it is null.
	if len(payload) < offset+2 {
		return header, offset, nil
	}

	clientIDLen := int16(binary.BigEndian.Uint16(payload[offset : offset+2]))
	offset += 2

	if clientIDLen > 0 {
		if len(payload) < offset+int(clientIDLen) {
			return nil, 0, fmt.Errorf("payload too short for client ID: need %d more bytes", int(clientIDLen)-(len(payload)-offset))
		}
		header.ClientID = string(payload[offset : offset+int(clientIDLen)])
		offset += int(clientIDLen)
	}

	return header, offset, nil
}

// ResponseHeader represents a Kafka response header
type ResponseHeader struct {
	CorrelationID int32
}

// ParseResponseHeader parses a Kafka response header from payload bytes
// Returns the header and the number of bytes consumed
func ParseResponseHeader(payload []byte) (*ResponseHeader, int, error) {
	if len(payload) < 4 {
		return nil, 0, fmt.Errorf("payload too short for response header: need at least 4 bytes, got %d", len(payload))
	}

	header := &ResponseHeader{
		CorrelationID: int32(binary.BigEndian.Uint32(payload[0:4])),
	}

	return header, 4, nil
}
