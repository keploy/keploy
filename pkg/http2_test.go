package pkg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
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

// A stream says whether recording was paused as its request's first frame
// was read, and nothing read after that frame changes it: the request's later
// frames, and its response, read during a pause or not. That read is what puts
// a gRPC call in a pause of the memory guard (hooks/conn SkipGRPCUnderPressure).
func TestStreamManagerSaysWhetherRecordingWasPausedAsTheRequestsFirstFrameWasRead(t *testing.T) {
	for _, c := range []struct {
		what                          string
		headers, data, response, want bool
	}{
		{what: "nothing read during a pause"},
		{what: "every frame read during a pause", headers: true, data: true, response: true, want: true},
		{what: "the first frame read during a pause, the rest after it", headers: true, want: true},
		{what: "the first frame read before a pause, the rest during it", data: true, response: true},
	} {
		sm := NewStreamManager(zap.NewNop())
		at := time.Unix(100, 0)
		require.NoError(t, sm.HandleFrameRead(headersFrame(t, 1, []hpack.HeaderField{
			{Name: ":method", Value: "POST"},
			{Name: ":scheme", Value: "http"},
			{Name: ":path", Value: "/svc.Echo/Ping"},
			{Name: ":authority", Value: "localhost:50051"},
			{Name: "content-type", Value: "application/grpc"},
		}, false), false, at, c.headers))
		require.NoError(t, sm.HandleFrameRead(dataFrame(t, 1, true, grpcPayload([]byte{0x0a, 0x04, 'p', 'i', 'n', 'g'})), false, at, c.data))
		require.NoError(t, sm.HandleFrameRead(headersFrame(t, 1, []hpack.HeaderField{
			{Name: ":status", Value: "200"},
			{Name: "content-type", Value: "application/grpc"},
		}, false), true, at, c.response))
		require.NoError(t, sm.HandleFrameRead(dataFrame(t, 1, false, grpcPayload([]byte{0x0a, 0x04, 'p', 'o', 'n', 'g'})), true, at, c.response))
		require.NoError(t, sm.HandleFrameRead(headersFrame(t, 1, []hpack.HeaderField{
			{Name: "grpc-status", Value: "0"},
		}, true), true, at, c.response))

		streams := sm.GetCompleteStreams()
		require.Len(t, streams, 1, c.what)
		assert.Equal(t, c.want, streams[0].RequestPaused, c.what)
	}
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

// splitBlock is the frames of one header block on stream id, encoded with
// enc (one encoder for every block of a direction, as a peer has), cut into
// parts pieces of about one size: a HEADERS frame, then CONTINUATION frames.
func splitBlock(t *testing.T, enc *hpack.Encoder, out *bytes.Buffer, id uint32, endStream bool, parts int, fields ...hpack.HeaderField) []byte {
	t.Helper()
	out.Reset()
	for _, f := range fields {
		require.NoError(t, enc.WriteField(f))
	}
	block := append([]byte(nil), out.Bytes()...)
	var frames bytes.Buffer
	fr := http2.NewFramer(&frames, nil)
	for i := 0; i < parts; i++ {
		piece := block[i*len(block)/parts : (i+1)*len(block)/parts]
		last := i == parts-1
		if i == 0 {
			require.NoError(t, fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: piece, EndStream: endStream, EndHeaders: last}))
		} else {
			require.NoError(t, fr.WriteContinuation(id, last, piece))
		}
	}
	return frames.Bytes()
}

// frameList cuts b, whole frames, into its frames.
func frameList(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 9)
		size := 9 + (int(b[0])<<16 | int(b[1])<<8 | int(b[2]))
		out = append(out, b[:size])
		b = b[size:]
	}
	return out
}

// eachFrame reads the frames in b, in order, with r.
func eachFrame(t *testing.T, r *FrameReader, b []byte, fn func(http2.Frame) error) error {
	t.Helper()
	for _, raw := range frameList(t, b) {
		f, err := r.ReadFrame(raw)
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

// A FrameReader reads a header block a peer split across a HEADERS frame and
// CONTINUATION frames, and refuses what RFC 9113 §6.10 forbids between them:
// another frame, or a CONTINUATION of another stream. A CONTINUATION read by
// a Framer that has not seen its HEADERS frame (each frame read alone, as the
// ingress did) is refused.
func TestFrameReaderReadsAHeaderBlockSplitAcrossFrames(t *testing.T) {
	var out bytes.Buffer
	enc := hpack.NewEncoder(&out)
	block := splitBlock(t, enc, &out, 1, true, 3, hpack.HeaderField{Name: "grpc-status", Value: "0"}, hpack.HeaderField{Name: "x-detail", Value: strings.Repeat("d", 200)})
	frames := frameList(t, block)
	require.Len(t, frames, 3)

	var types []http2.FrameType
	require.NoError(t, eachFrame(t, NewFrameReader(), block, func(f http2.Frame) error {
		types = append(types, f.Header().Type)
		return nil
	}))
	assert.Equal(t, []http2.FrameType{http2.FrameHeaders, http2.FrameContinuation, http2.FrameContinuation}, types)

	_, err := NewFrameReader().ReadFrame(frames[1])
	assert.Error(t, err, "a CONTINUATION read without its HEADERS frame")

	var data bytes.Buffer
	require.NoError(t, http2.NewFramer(&data, nil).WriteData(1, false, []byte("x")))
	interleaved := append(append([]byte{}, frames[0]...), data.Bytes()...)
	assert.Error(t, eachFrame(t, NewFrameReader(), interleaved, func(http2.Frame) error { return nil }), "a DATA frame inside a header block")

	other := append([]byte{}, block...)
	other[len(frames[0])+8] = 3 // the first CONTINUATION, on stream 3
	assert.Error(t, eachFrame(t, NewFrameReader(), other, func(http2.Frame) error { return nil }), "a CONTINUATION of another stream")

	_, err = NewFrameReader().ReadFrame(append(append([]byte{}, data.Bytes()...), data.Bytes()...))
	assert.Error(t, err, "two frames given as one")
}

// at is the n-th instant of a test's frames: distinct, in order.
func at(n int) time.Time { return time.Unix(1000, 0).Add(time.Duration(n) * time.Millisecond) }

// The stream manager decodes a split header block once, joined, with its
// direction's HPACK decoder: a block cut inside a field decodes whole, and the
// next block, which refers to that block's fields by their dynamic-table
// index, decodes too. A HEADERS frame's END_STREAM ends its stream at its
// block's end (RFC 9113 §8.1): nothing completes while the block is open, the
// trailers are the call's own, not its headers copied, and the call's end is
// the time of the frame that ends the block.
func TestStreamManagerReadsAHeaderBlockSplitAcrossFramesWhole(t *testing.T) {
	sm := NewStreamManager(zap.NewNop())
	var reqOut, respOut bytes.Buffer
	reqEnc, respEnc := hpack.NewEncoder(&reqOut), hpack.NewEncoder(&respOut)
	in, out := NewFrameReader(), NewFrameReader()
	big := strings.Repeat("b", 300)
	n := 0
	feed := func(r *FrameReader, raw []byte, outgoing bool) time.Time {
		t.Helper()
		f, err := r.ReadFrame(raw)
		require.NoError(t, err)
		n++
		require.NoError(t, sm.HandleFrame(f, outgoing, at(n)))
		return at(n)
	}
	feedAll := func(r *FrameReader, b []byte, outgoing bool) {
		t.Helper()
		for _, raw := range frameList(t, b) {
			feed(r, raw, outgoing)
		}
	}
	ends := map[uint32]time.Time{}
	call := func(id uint32, parts int) {
		t.Helper()
		feedAll(in, splitBlock(t, reqEnc, &reqOut, id, false, parts,
			hpack.HeaderField{Name: ":method", Value: "POST"},
			hpack.HeaderField{Name: ":path", Value: fmt.Sprintf("/svc.Echo/Call%d", id)},
			hpack.HeaderField{Name: "authorization", Value: "Bearer " + big}), false)
		var data bytes.Buffer
		require.NoError(t, http2.NewFramer(&data, nil).WriteData(id, true, []byte{0, 0, 0, 0, 1, 'q'}))
		feedAll(in, data.Bytes(), false)
		feedAll(out, splitBlock(t, respEnc, &respOut, id, false, 1,
			hpack.HeaderField{Name: ":status", Value: "200"},
			hpack.HeaderField{Name: "content-type", Value: "application/grpc"}), true)
		data.Reset()
		require.NoError(t, http2.NewFramer(&data, nil).WriteData(id, false, []byte{0, 0, 0, 0, 1, 'x'}))
		feedAll(out, data.Bytes(), true)
		trailers := frameList(t, splitBlock(t, respEnc, &respOut, id, true, parts,
			hpack.HeaderField{Name: "grpc-status", Value: "13"},
			hpack.HeaderField{Name: "grpc-message", Value: big}))
		for i, raw := range trailers {
			ends[id] = feed(out, raw, true)
			if i < len(trailers)-1 {
				for _, st := range sm.GetCompleteStreams() {
					require.NotEqual(t, id, st.ID, "stream %d completed inside its trailers block, after frame %d of %d", id, i+1, len(trailers))
				}
			}
		}
	}
	call(1, 3)
	call(3, 1) // refers to call 1's fields by index

	streams := sm.GetCompleteStreams()
	require.Len(t, streams, 2, "a call whose blocks were split did not complete")
	for _, st := range streams {
		assert.Equal(t, "Bearer "+big, st.GRPCReq.Headers.OrdinaryHeaders["authorization"], "stream %d's request headers", st.ID)
		assert.Equal(t, "200", st.GRPCResp.Headers.PseudoHeaders[":status"], "stream %d's headers", st.ID)
		assert.Equal(t, "13", st.GRPCResp.Trailers.OrdinaryHeaders["grpc-status"], "stream %d's trailers: %+v", st.ID, st.GRPCResp.Trailers)
		assert.Equal(t, big, st.GRPCResp.Trailers.OrdinaryHeaders["grpc-message"], "stream %d's trailers", st.ID)
		assert.Empty(t, st.GRPCResp.Trailers.PseudoHeaders, "stream %d's headers were taken for its trailers", st.ID)
		assert.Equal(t, ends[st.ID], st.GRPCResp.Timestamp, "stream %d ends at %v, not at the frame that ended its trailers block", st.ID, st.GRPCResp.Timestamp)
	}
}

// A bodyless request (END_STREAM on its HEADERS frame) whose header block
// continues in a CONTINUATION ends at that block's end, and completes its
// call then, even when the call's response was read before it (the two
// directions are parsed apart). Before, its END_STREAM took effect at the
// HEADERS frame, before its headers were decoded, and nothing checked the call
// again: it never completed.
func TestStreamManagerCompletesASplitBodylessRequestWhoseResponseCameFirst(t *testing.T) {
	sm := NewStreamManager(zap.NewNop())
	var reqOut, respOut bytes.Buffer
	reqEnc, respEnc := hpack.NewEncoder(&reqOut), hpack.NewEncoder(&respOut)
	in, out := NewFrameReader(), NewFrameReader()
	feedAll := func(r *FrameReader, b []byte, outgoing bool) {
		t.Helper()
		require.NoError(t, eachFrame(t, r, b, func(f http2.Frame) error { return sm.HandleFrame(f, outgoing, time.Now()) }))
	}
	feedAll(out, splitBlock(t, respEnc, &respOut, 1, true, 1,
		hpack.HeaderField{Name: ":status", Value: "200"},
		hpack.HeaderField{Name: "grpc-status", Value: "0"}), true)
	request := frameList(t, splitBlock(t, reqEnc, &reqOut, 1, true, 2,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc.Echo/Ping"},
		hpack.HeaderField{Name: "x-meta", Value: strings.Repeat("m", 100)}))
	require.Len(t, request, 2)
	feedAll(in, request[0], false)
	assert.Empty(t, sm.GetCompleteStreams(), "complete before its request's headers were read")
	feedAll(in, request[1], false)
	streams := sm.GetCompleteStreams()
	require.Len(t, streams, 1, "the call did not complete at its request's end")
	assert.Equal(t, "/svc.Echo/Ping", streams[0].GRPCReq.Headers.PseudoHeaders[":path"])
	assert.Equal(t, "0", streams[0].GRPCResp.Trailers.OrdinaryHeaders["grpc-status"])
}

// A gRPC test of an app port published on one other host port is dialled
// there, which PublishedPort names; without it, it is dialled at its app port.
func TestAGRPCTestOfAnAppPortPublishedOnOneHostPortIsDialledThere(t *testing.T) {
	var dialled atomic.Int32
	addr := serveEachConn(t, func(c net.Conn) { dialled.Add(1); _ = c.Close() })
	_, hp, _ := net.SplitHostPort(addr)
	hostPort, _ := strconv.Atoi(hp)
	appPort := refusedPort(t)
	tc := func() *models.TestCase {
		tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
		tc.AppPort = appPort
		return tc
	}

	_, err := SimulateGRPC(context.Background(), tc(), "set", zap.NewNop(), SimulationConfig{APITimeout: 5, AppPortReachability: unreachableAt{}})
	if !IsUnreachableAppPort(err) || dialled.Load() != 0 {
		t.Fatalf("without PublishedPort: err %v, %d connections to host port %d; want its app port %d refused", err, dialled.Load(), hostPort, appPort)
	}
	published := func(_ context.Context, _ string, port uint16) uint16 {
		if port == appPort {
			return uint16(hostPort)
		}
		return 0
	}
	_, _ = SimulateGRPC(context.Background(), tc(), "set", zap.NewNop(), SimulationConfig{APITimeout: 5, AppPortReachability: unreachableAt{}, PublishedPort: published})
	if dialled.Load() == 0 {
		t.Fatalf("with PublishedPort naming host port %d, nothing dialled it", hostPort)
	}

	// A replaceWith that matches the recorded authority and names the port
	// is the user's choice: it is asked of the recorded authority, which the
	// resolved one no longer contains.
	before := dialled.Load()
	_, err = SimulateGRPC(context.Background(), tc(), "set", zap.NewNop(), SimulationConfig{APITimeout: 5, AppPortReachability: unreachableAt{},
		URLReplacements: map[string]string{addr: fmt.Sprintf("localhost:%d", appPort)}, PublishedPort: published})
	if !IsUnreachableAppPort(err) || dialled.Load() != before {
		t.Fatalf("a matching replaceWith naming the port: err %v, %d more connections to host port %d; want its app port %d refused", err, dialled.Load()-before, hostPort, appPort)
	}
}

// A recorded answer of trailers alone, kept as one empty message, is complete
// with no message when its status, as a gRPC client reads it, is an error: a
// keepalive after none is one beyond it. A Trailers-Only recording kept as
// headers alone (enterprise's low-latency recorder before its fix) has its
// status in that one header block; trailers, when there are any, are read
// for it instead.
func TestRecordedMessagesCompleteReadsTheStatusAsAClientDoes(t *testing.T) {
	block := func(kv ...string) models.GrpcHeaders {
		h := models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
		for i := 0; i < len(kv); i += 2 {
			h.OrdinaryHeaders[kv[i]] = kv[i+1]
		}
		return h
	}
	for _, c := range []struct {
		name string
		resp models.GrpcResp
		want int
	}{
		{"an error in its trailers", models.GrpcResp{Headers: block("content-type", "application/grpc"), Trailers: block("grpc-status", "5")}, 0},
		{"OK in its trailers", models.GrpcResp{Headers: block("content-type", "application/grpc"), Trailers: block("grpc-status", "0")}, 1},
		{"a Trailers-Only error kept as headers alone", models.GrpcResp{Headers: block("content-type", "application/grpc", "grpc-status", "5"), Trailers: block()}, 0},
		{"a Trailers-Only OK kept as headers alone", models.GrpcResp{Headers: block("content-type", "application/grpc", "grpc-status", "0"), Trailers: block()}, 1},
		{"trailers without a status, an error in its headers", models.GrpcResp{Headers: block("grpc-status", "5"), Trailers: block("x-done", "yes")}, 1},
	} {
		if got := recordedMessagesComplete(c.resp); got != c.want {
			t.Errorf("%s: complete with %d message(s), want %d", c.name, got, c.want)
		}
	}
}
