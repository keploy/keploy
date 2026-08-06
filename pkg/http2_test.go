package pkg

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestStreamManagerUsesIncomingFrameTimeForGRPCRequestTimestamp(t *testing.T) {
	sm := NewStreamManager(zap.NewNop())
	requestAt := time.Unix(100, 0).UTC()
	responseAt := time.Unix(105, 0).UTC()

	// Process response frames first to model the two parser goroutines racing.
	require.NoError(t, sm.HandleFrame(headersFrame(t, 1, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "application/grpc"},
	}, false), true, responseAt))
	require.NoError(t, sm.HandleFrame(dataFrame(t, 1, false, grpcPayload([]byte{0x0a, 0x04, 'p', 'o', 'n', 'g'})), true, responseAt.Add(time.Millisecond)))
	require.NoError(t, sm.HandleFrame(headersFrame(t, 1, []hpack.HeaderField{
		{Name: "grpc-status", Value: "0"},
		{Name: "grpc-message", Value: ""},
	}, true), true, responseAt.Add(2*time.Millisecond)))

	require.NoError(t, sm.HandleFrame(headersFrame(t, 1, []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/svc.Echo/Ping"},
		{Name: ":authority", Value: "localhost:50051"},
		{Name: "content-type", Value: "application/grpc"},
	}, false), false, requestAt))
	require.NoError(t, sm.HandleFrame(dataFrame(t, 1, true, grpcPayload([]byte{0x0a, 0x04, 'p', 'i', 'n', 'g'})), false, requestAt.Add(time.Millisecond)))

	streams := sm.GetCompleteStreams()
	require.Len(t, streams, 1)
	assert.Equal(t, requestAt, streams[0].GRPCReq.Timestamp)
	assert.Equal(t, responseAt.Add(2*time.Millisecond), streams[0].GRPCResp.Timestamp)
}

func headersFrame(t *testing.T, streamID uint32, fields []hpack.HeaderField, endStream bool) http2.Frame {
	t.Helper()

	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, field := range fields {
		require.NoError(t, enc.WriteField(field))
	}

	return readFrame(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: block.Bytes(),
			EndHeaders:    true,
			EndStream:     endStream,
		})
	})
}

func dataFrame(t *testing.T, streamID uint32, endStream bool, data []byte) http2.Frame {
	t.Helper()

	return readFrame(t, func(fr *http2.Framer) error {
		return fr.WriteData(streamID, endStream, data)
	})
}

func readFrame(t *testing.T, write func(*http2.Framer) error) http2.Frame {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, write(http2.NewFramer(&buf, nil)))
	frame, err := http2.NewFramer(nil, &buf).ReadFrame()
	require.NoError(t, err)
	return frame
}

func grpcPayload(msg []byte) []byte {
	payload := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(msg)))
	copy(payload[5:], msg)
	return payload
}

// TestReadGRPCFrameRejectsOversizedLength pins the allocation bound on the
// 4-byte, wire-supplied length prefix. Two distinct failures are covered:
//
//  1. A merely huge length (here 2 GiB) used to be handed straight to
//     make([]byte, msgLen) — the proxy commits the memory before io.ReadFull
//     gets a chance to notice the peer never sends that many bytes.
//  2. 0xFFFFFFFF additionally wrapped the follow-on `5+msgLen` uint32
//     arithmetic to 4, so the function panicked in copy(frame[5:], msgBuf)
//     with "slice bounds out of range [5:4]".
//
// Both must now come back as an error, and a legitimate frame must still
// round-trip.
func TestReadGRPCFrameRejectsOversizedLength(t *testing.T) {
	frameWithLength := func(n uint32) []byte {
		buf := make([]byte, 5)
		buf[0] = 0 // compression flag
		binary.BigEndian.PutUint32(buf[1:5], n)
		return buf
	}

	for _, tc := range []struct {
		name   string
		msgLen uint32
	}{
		{"two_gib", 2 << 30},
		{"uint32_max_wraps_the_frame_length", ^uint32(0)},
		{"just_over_the_bound", MaxGRPCFrameLength + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadGRPCFrame(bytes.NewReader(frameWithLength(tc.msgLen)))
			require.Error(t, err, "oversized length %d must be rejected before allocating", tc.msgLen)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "exceeds")
		})
	}

	// A real frame still reads back byte-for-byte.
	payload := []byte("hello grpc")
	raw := append(frameWithLength(uint32(len(payload))), payload...)
	got, err := ReadGRPCFrame(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, raw, got)
}
