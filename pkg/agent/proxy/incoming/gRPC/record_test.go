package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
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
	parseFramesFromChan(context.Background(), logger, clientCh, sm, false, func([]func()) {})

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
	parseFramesFromChan(context.Background(), logger, destCh, sm, true, func([]func()) {})

	streams := sm.GetCompleteStreams()
	require.Len(t, streams, 1)
	assert.Equal(t, requestAt, streams[0].GRPCReq.Timestamp)
	assert.Equal(t, responseAt, streams[0].GRPCResp.Timestamp)
}

func TestForwardAndTeeStampsResponsesAfterClientWrite(t *testing.T) {
	logger := zap.NewNop()
	srcRead, srcWrite := net.Pipe()
	dstRead, dstWrite := net.Pipe()
	for _, c := range []net.Conn{srcRead, srcWrite, dstRead, dstWrite} {
		require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
		defer c.Close()
	}

	ch := make(chan frameChunk, 1)
	go forwardAndTee(srcRead, dstWrite, ch, logger, "app→client", true, nil)

	payload := []byte("response-frame")
	writeDone := make(chan error, 1)
	go func() {
		_, err := srcWrite.Write(payload)
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("source write did not complete")
	}

	time.Sleep(100 * time.Millisecond)
	cutoff := time.Now()

	buf := make([]byte, len(payload))
	_, err := io.ReadFull(dstRead, buf)
	require.NoError(t, err)
	assert.Equal(t, payload, buf)

	select {
	case chunk := <-ch:
		assert.Equal(t, payload, chunk.data)
		assert.False(t, chunk.timestamp.Before(cutoff),
			"response timestamp should be captured after the proxy writes to the client")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for teed frame chunk")
	}
}

// caughtUp is a capture whose parsers are past every time: what a stop learns
// of the backlog is the manager's own.
type caughtUp struct{}

func (caughtUp) Settled(string, string, time.Time, time.Time) bool { return true }
func (caughtUp) PendingBefore(time.Time) bool                      { return false }

// pingCall is one unary gRPC call: the client's preface and request, and the
// server's response with its trailers.
func pingCall(t *testing.T) (req, resp []byte) {
	t.Helper()
	req = append([]byte(http2.ClientPreface),
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
	)
	resp = append(
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
	)
	return req, resp
}

// A gRPC call that ended before a recording's stop is the agent's until the
// ingress has captured it into the recorder's queue: a stop that asks before
// then hears it is pending, not "clear", or it ends the stream and the test
// case is lost with the recording reported complete. Once it is captured and
// taken, nothing of it is pending.
func TestRecordIncoming_ACallFromBeforeTheStopIsPendingUntilItIsCaptured(t *testing.T) {
	m := syncMock.New(nil)
	defer m.AddWatermark(caughtUp{})()
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), m))
	defer cancel()

	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	defer cliApp.Close()
	defer appApp.Close()
	tcCh := make(chan *models.TestCase) // not read until after the look
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", false, false)
	}()

	req, resp := pingCall(t)
	go func() { _, _ = cliApp.Write(req) }()
	_, err := io.ReadFull(appApp, make([]byte, len(req)))
	require.NoError(t, err)
	go func() { _, _ = appApp.Write(resp) }()
	_, err = io.ReadFull(cliApp, make([]byte, len(resp)))
	require.NoError(t, err)

	// The call is over for its client; the stop comes after it. (The
	// response is stamped once the forwarder's write to the client returns,
	// which can be just after the client has read it.)
	before := time.Now().Add(time.Second)
	st := m.BacklogBefore(before)
	require.True(t, st.Known)
	require.True(t, st.Pending, "the agent said it held nothing from before the stop (%+v) while a gRPC call from before it was still in the ingress", st)

	var tc *models.TestCase
	select {
	case tc = <-tcCh:
	case <-time.After(30 * time.Second):
		t.Fatal("no test case came of the call")
	}
	require.False(t, tc.GrpcResp.Timestamp.After(before), "setup: the call is from before the stop")

	// Taken: the emitter releases what it built it of.
	deadline := time.Now().Add(30 * time.Second)
	for m.BacklogBefore(before).Pending {
		if time.Now().After(deadline) {
			t.Fatalf("still pending once the call was captured and taken: %+v", m.BacklogBefore(before))
		}
		time.Sleep(time.Millisecond)
	}

	// A connection that ends leaves nothing pending either.
	cancel()
	<-recorded
	require.False(t, m.BacklogBefore(time.Now()).Pending, "pending once the connection ended")
}

// Every chunk the parser is handed is released exactly once, whatever becomes
// of it: handed to the emitter once all of it is taken, or as the parser
// returns when it took only part of it, or released at once when the parser
// gave up before taking it. A chunk released early would let a stop end
// before the call it completes is captured; one never released would hold
// every stop until it gave up, failing a recording that lost nothing.
func TestParseFramesFromChan_ReleasesEachChunkOnce(t *testing.T) {
	req, _ := pingCall(t)
	split := len(http2.ClientPreface) + 20 // inside the request's HEADERS frame

	// toEmitter and direct are the chunks released through parsed (by the
	// emitter, in RecordIncoming) and released by the parser itself.
	type counts struct{ toEmitter, direct []int }
	run := func(t *testing.T, chunks [][]byte) counts {
		t.Helper()
		var got counts
		viaEmitter := false
		ch := make(chan frameChunk, len(chunks))
		for i, c := range chunks {
			i := i
			ch <- frameChunk{data: c, timestamp: time.Now(), built: func() {
				if viaEmitter {
					got.toEmitter = append(got.toEmitter, i)
				} else {
					got.direct = append(got.direct, i)
				}
			}}
		}
		close(ch)
		sm := pkg.NewStreamManager(zap.NewNop())
		parseFramesFromChan(context.Background(), zap.NewNop(), ch, sm, false, func(done []func()) {
			viaEmitter = true
			for _, d := range done {
				d()
			}
			viaEmitter = false
		})
		return got
	}

	t.Run("a frame split across chunks waits for its end", func(t *testing.T) {
		got := run(t, [][]byte{req[:split], req[split:]})
		assert.Equal(t, []int{0, 1}, got.toEmitter)
		assert.Empty(t, got.direct)
	})
	t.Run("a stream that ends mid-frame hands its part-taken chunk on", func(t *testing.T) {
		got := run(t, [][]byte{req[:split]})
		assert.Equal(t, []int{0}, got.toEmitter)
		assert.Empty(t, got.direct)
	})
	t.Run("chunks left when the parser gives up are released", func(t *testing.T) {
		bad := []byte{0, 0, 0, 0x0, 0, 0, 0, 0, 0} // a DATA frame on stream 0: a protocol error
		got := run(t, [][]byte{req, bad, req})
		assert.Equal(t, []int{0, 1}, got.toEmitter, "the good chunk, and the one it stopped in")
		assert.Equal(t, []int{2}, got.direct, "the chunk it never took")
	})
	t.Run("an empty chunk is released", func(t *testing.T) {
		got := run(t, [][]byte{{}, req})
		assert.Equal(t, []int{1}, got.toEmitter)
		assert.Equal(t, []int{0}, got.direct)
	})
}

// A parser that gives up on a connection that stays open (a frame it cannot
// read, the tee stopped by memory pressure mid-frame) lets go of what it holds
// at once: gRPC connections are pooled and live long, and a chunk held until
// the connection ends would keep a stop waiting on it until the stop gave up
// and failed a recording that had lost nothing.
func TestParseFramesFromChan_AParserThatGivesUpLetsGoAtOnce(t *testing.T) {
	req, _ := pingCall(t)
	split := len(http2.ClientPreface) + 20 // inside the request's HEADERS frame
	for name, tc := range map[string]struct {
		chunks []frameChunk
		want   int // chunks handed to the emitter before ch closes
	}{
		"a frame it cannot read": {
			chunks: []frameChunk{{data: req}, {data: []byte{0, 0, 0, 0x0, 0, 0, 0, 0, 0}}}, // DATA on stream 0
			want:   2,
		},
		"the tee stopped mid-frame": {
			chunks: []frameChunk{{data: req[:split]}, {teeEnded: true}},
			want:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ch := make(chan frameChunk) // open: the connection lives on
			handed := make(chan int, 8)
			var direct atomic.Int32
			parserDone := make(chan struct{})
			go func() {
				defer close(parserDone)
				parseFramesFromChan(context.Background(), zap.NewNop(), ch, pkg.NewStreamManager(zap.NewNop()), false, func(done []func()) {
					for _, d := range done {
						d()
					}
					handed <- len(done)
				})
			}()
			for i := range tc.chunks {
				c := tc.chunks[i]
				if !c.teeEnded {
					c.timestamp, c.built = time.Now(), func() {}
				}
				ch <- c
			}
			got := 0
			for got < tc.want {
				select {
				case n := <-handed:
					got += n
				case <-time.After(30 * time.Second):
					t.Fatalf("%d of %d chunks let go while the connection is open", got, tc.want)
				}
			}
			// What comes after is let go as it comes.
			ch <- frameChunk{data: req, timestamp: time.Now(), built: func() { direct.Add(1) }}
			close(ch)
			<-parserDone
			assert.Equal(t, int32(1), direct.Load(), "a chunk the parser never takes is released by it")
			assert.Equal(t, tc.want, got)
		})
	}
}

// The tee that memory pressure stops tells the parser so: the rest of the
// frame it may be in the middle of will never come.
func TestForwardAndTeeTellsTheParserWhenTheTeeStops(t *testing.T) {
	var paused atomic.Bool
	defer func(f func() bool) { recordingPaused = f }(recordingPaused)
	recordingPaused = paused.Load

	srcRead, srcWrite := net.Pipe()
	dstRead, dstWrite := net.Pipe()
	defer srcWrite.Close()
	defer dstRead.Close()
	var released atomic.Int32
	building := func(time.Time) func() { return func() { released.Add(1) } }
	ch := make(chan frameChunk, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "client→app", false, building)
	}()
	forward := func(b []byte) {
		t.Helper()
		go func() { _, _ = srcWrite.Write(b) }()
		_, err := io.ReadFull(dstRead, make([]byte, len(b)))
		require.NoError(t, err)
	}

	forward([]byte("teed"))
	first := <-ch
	require.Equal(t, []byte("teed"), first.data)
	paused.Store(true)
	forward([]byte("not teed"))
	ended := <-ch
	assert.True(t, ended.teeEnded, "the parser is not told the tee stopped")
	assert.Equal(t, int32(1), released.Load(), "the chunk read as the tee stopped is not released")
	forward([]byte("after"))
	paused.Store(false) // the tee does not come back mid-connection
	forward([]byte("later"))
	srcWrite.Close()
	<-done
	assert.Len(t, ch, 0, "chunks teed after the tee stopped")
}

// The forwarder notes each chunk it tees as being built from no later than
// the chunk's stamp, and releases it when it cannot hand it on.
func TestForwardAndTeeNotesEachChunkFromBeforeItsStamp(t *testing.T) {
	srcRead, srcWrite := net.Pipe()
	dstRead, dstWrite := net.Pipe()
	defer srcWrite.Close()
	defer dstRead.Close()

	var notedAt []time.Time
	released := 0
	building := func(at time.Time) func() {
		notedAt = append(notedAt, at)
		return func() { released++ }
	}
	ch := make(chan frameChunk, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "app→client", true, building)
	}()

	go func() { _, _ = srcWrite.Write([]byte("frame")) }()
	_, err := io.ReadFull(dstRead, make([]byte, 5))
	require.NoError(t, err)
	chunk := <-ch
	require.Len(t, notedAt, 1)
	assert.False(t, notedAt[0].After(chunk.timestamp), "noted at %v, after its stamp %v", notedAt[0], chunk.timestamp)
	require.NotNil(t, chunk.built)
	chunk.built()

	// The peer is gone: the chunk read next cannot be forwarded, and is
	// released as the forwarder gives up.
	dstRead.Close()
	go func() { _, _ = srcWrite.Write([]byte("lost")) }()
	<-done
	assert.Equal(t, 2, released)
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
