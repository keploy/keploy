package wire

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/pkg/models/kafka"
)

func TestParseRequestHeader(t *testing.T) {
	// Create a mock Kafka request header
	// ApiKey: 1 (Fetch)
	// ApiVersion: 11
	// CorrelationID: 12345
	// ClientID: "test-client"

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, int16(1))       // ApiKey
	binary.Write(buf, binary.BigEndian, int16(11))      // ApiVersion
	binary.Write(buf, binary.BigEndian, int32(12345))   // CorrelationID
	binary.Write(buf, binary.BigEndian, int16(11))      // ClientID length
	buf.WriteString("test-client")                      // ClientID
	header, _, err := ParseRequestHeader(buf.Bytes())
	assert.NoError(t, err)
	assert.NotNil(t, header)
	assert.Equal(t, kafka.Fetch, header.ApiKey)
	assert.Equal(t, int16(11), header.ApiVersion)
	assert.Equal(t, int32(12345), header.CorrelationID)
	assert.Equal(t, "test-client", header.ClientID)
}

func TestWriteAndReadPacket(t *testing.T) {
	payload := []byte("payload")
	pkt := &Packet{
		Length:  int32(len(payload)),
		Payload: payload,
	}

	buf := new(bytes.Buffer)
	err := WritePacket(buf, pkt)
	assert.NoError(t, err)

	pktRead, err := ReadPacket(buf)
	assert.NoError(t, err)
	assert.Equal(t, pkt.Length, pktRead.Length)
	assert.Equal(t, pkt.Payload, pktRead.Payload)
}
