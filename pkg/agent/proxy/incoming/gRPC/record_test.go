package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestParseFramesFromChanUsesChunkTimestamps(t *testing.T) {
	logger := zap.NewNop()
	sm := pkg.NewStreamManager(logger)
	requestAt := time.Unix(200, 0).UTC()
	responseAt := time.Unix(205, 0).UTC()

	clientCh := make(chan frameChunk, 1)
	clientCh <- frameChunk{
		data: append([]byte(http2.ClientPreface),
			append(
				headersFrameBytes(t, 1, []hpack.HeaderField{
					{Name: ":method", Value: "POST"},
					{Name: ":scheme", Value: "http"},
					{Name: ":path", Value: "/svc.Echo/Ping"},
					{Name: ":authority", Value: "localhost:50051"},
					{Name: "content-type", Value: "application/grpc"},
				}, false),
				dataFrameBytes(t, 1, true, grpcPayloadBytes([]byte{0x0a, 0x04, 'p', 'i', 'n', 'g'}))...,
			)...,
		),
		timestamp: requestAt,
	}
	close(clientCh)
	parseFramesFromChan(context.Background(), logger, clientCh, sm, false, func() {})

	destCh := make(chan frameChunk, 1)
	destCh <- frameChunk{
		data: append(
			append(
				headersFrameBytes(t, 1, []hpack.HeaderField{
					{Name: ":status", Value: "200"},
					{Name: "content-type", Value: "application/grpc"},
				}, false),
				dataFrameBytes(t, 1, false, grpcPayloadBytes([]byte{0x0a, 0x04, 'p', 'o', 'n', 'g'}))...,
			),
			headersFrameBytes(t, 1, []hpack.HeaderField{
				{Name: "grpc-status", Value: "0"},
				{Name: "grpc-message", Value: ""},
			}, true)...,
		),
		timestamp: responseAt,
	}
	close(destCh)
	parseFramesFromChan(context.Background(), logger, destCh, sm, true, func() {})

	streams := sm.GetCompleteStreams()
	require.Len(t, streams, 1)
	assert.Equal(t, requestAt, streams[0].GRPCReq.Timestamp)
	assert.Equal(t, responseAt, streams[0].GRPCResp.Timestamp)
}

func headersFrameBytes(t *testing.T, streamID uint32, fields []hpack.HeaderField, endStream bool) []byte {
	t.Helper()

	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, field := range fields {
		require.NoError(t, enc.WriteField(field))
	}

	return frameBytes(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: block.Bytes(),
			EndHeaders:    true,
			EndStream:     endStream,
		})
	})
}

func dataFrameBytes(t *testing.T, streamID uint32, endStream bool, data []byte) []byte {
	t.Helper()

	return frameBytes(t, func(fr *http2.Framer) error {
		return fr.WriteData(streamID, endStream, data)
	})
}

func frameBytes(t *testing.T, write func(*http2.Framer) error) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, write(http2.NewFramer(&buf, nil)))
	return buf.Bytes()
}

func grpcPayloadBytes(msg []byte) []byte {
	payload := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(msg)))
	copy(payload[5:], msg)
	return payload
}
