package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
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

// grpcIngress is RecordIncoming on pipes, on a manager of its own whose
// capture is caught up, with an unbuffered test-case channel nothing reads
// until the test does. The test waits for RecordIncoming to return.
type grpcIngress struct {
	m              *syncMock.SyncMockManager
	cliApp, appApp net.Conn
	tcCh           chan *models.TestCase
	recorded       chan struct{}
	// stop is the recording's stop, as a CLI asks about it: one time, the
	// same at every look, after every call the test makes.
	stop time.Time
}

func startGRPCIngress(t *testing.T) *grpcIngress {
	t.Helper()
	g := &grpcIngress{m: syncMock.New(nil), tcCh: make(chan *models.TestCase), recorded: make(chan struct{}),
		stop: time.Now().Add(time.Hour)}
	remove := g.m.AddWatermark(caughtUp{})
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), g.m))
	var cliProxy, appProxy net.Conn
	g.cliApp, cliProxy = net.Pipe()
	appProxy, g.appApp = net.Pipe()
	go func() {
		defer close(g.recorded)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, g.tcCh, 50051, "", false, false)
	}()
	t.Cleanup(func() {
		cancel()
		_ = g.cliApp.Close()
		_ = g.appApp.Close()
		for {
			select {
			case <-g.tcCh: // a capture blocked on the channel
				continue
			case <-g.recorded:
			}
			break
		}
		remove()
	})
	return g
}

// backlog is what the stop learns asking now: whether something from before
// it is pending, and how many of the chunks noted before it are let go.
func (g *grpcIngress) backlog() syncMock.BacklogState { return g.m.BacklogBefore(g.stop) }

// waitFor waits for cond to hold of the backlog; the bound only catches a
// pipeline that never gets there.
func (g *grpcIngress) waitFor(t *testing.T, what string, cond func(syncMock.BacklogState) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for st := g.backlog(); !cond(st); st = g.backlog() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never came: %+v", what, st)
		}
		time.Sleep(time.Millisecond)
	}
}

// call sends one unary call through the ingress: the request is forwarded and
// its chunk let go before the response is sent (one chunk each way, as a
// pipe's single write is read whole), so what is pending afterwards is the
// response's.
func (g *grpcIngress) call(t *testing.T) {
	t.Helper()
	req, resp := pingCall(t)
	go func() { _, _ = g.cliApp.Write(req) }()
	_, err := io.ReadFull(g.appApp, make([]byte, len(req)))
	require.NoError(t, err)
	g.waitFor(t, "the request chunk's release", func(st syncMock.BacklogState) bool { return st.Progress >= 1 && !st.Pending })
	go func() { _, _ = g.appApp.Write(resp) }()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp)))
	require.NoError(t, err)
}

// A gRPC call that ended before a recording's stop is the agent's until the
// ingress has captured it into the recorder's queue: a stop that asks before
// then hears it is pending, not "clear", or it ends the stream and the test
// case is lost with the recording reported complete. Once it is captured and
// taken, nothing of it is pending.
func TestRecordIncoming_ACallFromBeforeTheStopIsPendingUntilItIsCaptured(t *testing.T) {
	g := startGRPCIngress(t)
	g.call(t)

	// The capture waits on the recorder's queue, which nothing reads yet: the
	// stop hears the call is pending, and goes on hearing it.
	g.waitFor(t, "pending for the call in the ingress", func(st syncMock.BacklogState) bool { return st.Pending })
	require.True(t, g.backlog().Pending)

	var tc *models.TestCase
	select {
	case tc = <-g.tcCh:
	case <-time.After(30 * time.Second):
		t.Fatal("no test case came of the call")
	}
	require.False(t, tc.GrpcResp.Timestamp.IsZero() || tc.GrpcResp.Timestamp.After(g.stop), "setup: the call is from before the stop")

	// Taken: the emitter releases what it built it of.
	g.waitFor(t, "clear once the call was taken", func(st syncMock.BacklogState) bool { return !st.Pending && st.Progress >= 2 })
}

// Every chunk the parser is handed is released exactly once, whatever becomes
// of it: through the emitter when its frames completed any (it releases it
// once it has decided what they completed), and at once when they completed
// none, or the parser never took it. A chunk released before the emitter
// decided its calls would let a stop end before they are captured; one never
// released would hold every stop until it gave up, failing a recording that
// had lost nothing.
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

	t.Run("a chunk that completes no frame is released at once", func(t *testing.T) {
		got := run(t, [][]byte{req[:split], req[split:]})
		assert.Equal(t, []int{1}, got.toEmitter, "the chunk that completes the frames")
		assert.Equal(t, []int{0}, got.direct, "the chunk that ends mid-frame")
	})
	t.Run("chunks left when the parser gives up are released", func(t *testing.T) {
		bad := []byte{0, 0, 0, 0x0, 0, 0, 0, 0, 0} // a DATA frame on stream 0: a protocol error
		got := run(t, [][]byte{req, bad, req})
		assert.Equal(t, []int{0}, got.toEmitter, "the good chunk")
		assert.Equal(t, []int{1, 2}, got.direct, "the one it stopped in, which completed no frame, and the one it never took")
	})
	t.Run("a chunk it stops in after some of its frames goes to the emitter", func(t *testing.T) {
		bad := []byte{0, 0, 0, 0x0, 0, 0, 0, 0, 0}
		got := run(t, [][]byte{append(append([]byte{}, req...), bad...)})
		assert.Equal(t, []int{0}, got.toEmitter)
		assert.Empty(t, got.direct)
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
		want   int32 // chunks let go before ch closes
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
			var released atomic.Int32
			parserDone := make(chan struct{})
			go func() {
				defer close(parserDone)
				parseFramesFromChan(context.Background(), zap.NewNop(), ch, pkg.NewStreamManager(zap.NewNop()), false, func(done []func()) {
					for _, d := range done {
						d()
					}
				})
			}()
			for i := range tc.chunks {
				c := tc.chunks[i]
				if !c.teeEnded {
					c.timestamp, c.built = time.Now(), func() { released.Add(1) }
				}
				ch <- c
			}
			deadline := time.Now().Add(30 * time.Second) // bounds only a parser that never lets go
			for released.Load() < tc.want {
				if time.Now().After(deadline) {
					t.Fatalf("%d of %d chunks let go while the connection is open", released.Load(), tc.want)
				}
				time.Sleep(time.Millisecond)
			}
			// What comes after is let go as it comes.
			ch <- frameChunk{data: req, timestamp: time.Now(), built: func() { released.Add(1) }}
			close(ch)
			<-parserDone
			assert.Equal(t, tc.want+1, released.Load())
		})
	}
}

// A client that stops reading in the middle of a response: the forwarder
// waits in its write of the next chunk, which is never noted, and the chunk
// before it ends mid-frame. That one is let go once its frames are handled:
// the frame it ends in is stamped by the chunk that completes it, after its
// write, so nothing it holds can be from before the stop. Held, it would keep
// the stop waiting on a client that never reads again.
func TestRecordIncoming_AClientThatStopsReadingMidFrameHoldsNoStop(t *testing.T) {
	g := startGRPCIngress(t)
	req, _ := pingCall(t)
	go func() { _, _ = g.cliApp.Write(req) }()
	_, err := io.ReadFull(g.appApp, make([]byte, len(req)))
	require.NoError(t, err)
	g.waitFor(t, "the request chunk's release", func(st syncMock.BacklogState) bool { return st.Progress >= 1 && !st.Pending })

	// The response: its headers, then a DATA frame the first chunk cuts.
	head := headersFrameBytes(t, 1, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "application/grpc"},
	}, false)
	data := dataFrameBytes(t, 1, false, grpcPayloadBytes(make([]byte, 64)))
	first := append(append([]byte{}, head...), data[:20]...)
	second := data[20:]

	// The client reads the first chunk, then stops reading.
	go func() { _, _ = g.appApp.Write(first) }()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(first)))
	require.NoError(t, err)
	// The forwarder has read the second chunk once this returns, and waits
	// in its write to the client, which never reads it.
	wrote := make(chan struct{})
	go func() { _, _ = g.appApp.Write(second); close(wrote) }()
	<-wrote

	g.waitFor(t, "clear while the client does not read", func(st syncMock.BacklogState) bool { return !st.Pending && st.Progress >= 2 })
}

// The tee that memory pressure stops tells the parser so: the rest of the
// frame it may be in the middle of will never come.
func TestForwardAndTeeTellsTheParserWhenTheTeeStops(t *testing.T) {
	var paused atomic.Bool
	pausedNow := paused.Load
	teePaused.Store(&pausedNow)
	defer teePaused.Store(nil)

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
// the chunk's stamp, and releases it when it cannot hand it on. A chunk
// stamped after its write is noted once the write returns: a write blocked on
// a client that stopped reading would otherwise hold every stop, though what
// it carries is stamped after the write.
func TestForwardAndTeeNotesEachChunkFromBeforeItsStamp(t *testing.T) {
	for _, stampAfterWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("stampAfterWrite=%v", stampAfterWrite), func(t *testing.T) {
			srcRead, srcWrite := net.Pipe()
			dstRead, dstWrite := net.Pipe()
			defer srcWrite.Close()
			defer dstRead.Close()

			notes := make(chan time.Time, 4)
			var released atomic.Int32
			building := func(at time.Time) func() {
				notes <- at
				return func() { released.Add(1) }
			}
			ch := make(chan frameChunk, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "test", stampAfterWrite, building)
			}()

			go func() { _, _ = srcWrite.Write([]byte("frame")) }()
			unblocked := time.Now() // the forwarder's write can return from here on
			_, err := io.ReadFull(dstRead, make([]byte, 5))
			require.NoError(t, err)
			chunk := <-ch
			notedAt := <-notes
			assert.False(t, notedAt.After(chunk.timestamp), "noted at %v, after its stamp %v", notedAt, chunk.timestamp)
			if stampAfterWrite {
				assert.False(t, notedAt.Before(unblocked), "noted at %v, before its write could return (%v)", notedAt, unblocked)
			}
			require.NotNil(t, chunk.built)
			chunk.built()

			// The peer is gone: the chunk read next cannot be forwarded. One
			// noted before its write is released as the forwarder gives up;
			// one noted after it never was.
			dstRead.Close()
			go func() { _, _ = srcWrite.Write([]byte("lost")) }()
			<-done
			close(notes)
			extra := 0
			for range notes {
				extra++
			}
			if stampAfterWrite {
				assert.Equal(t, 0, extra, "a chunk whose write failed was noted")
				assert.Equal(t, int32(1), released.Load())
			} else {
				assert.Equal(t, 1, extra)
				assert.Equal(t, int32(2), released.Load(), "the chunk whose write failed is not released")
			}
		})
	}
}

// A call that completes while memory pressure pauses the recording is decided
// by the emitter pass that finds it (left out, as CaptureGRPC leaves out what
// completes under pressure), before the chunks it was built of are released:
// a stop told nothing is pending is never followed by a test case from before
// it. A pass that skipped it left it for a later pass, after the stream ended.
func TestRecordIncoming_ACallCompletedUnderPressureIsDecidedBeforeItsChunksGo(t *testing.T) {
	// The tee sees no pressure for the call's chunks; the capture does.
	notPaused := func() bool { return false }
	teePaused.Store(&notPaused)
	defer teePaused.Store(nil)
	restore := memoryguard.SetRecordingPausedForTest(true)
	defer restore()

	g := startGRPCIngress(t)
	g.call(t)
	// Both chunks let go: the stop hears "clear", and its stream ends.
	g.waitFor(t, "both chunks let go", func(st syncMock.BacklogState) bool { return !st.Pending && st.Progress >= 2 })

	// Pressure lifts and the connection ends: nothing from before the stop
	// may come now.
	restore()
	_ = g.cliApp.Close()
	_ = g.appApp.Close()
	select {
	case tc := <-g.tcCh:
		t.Fatalf("after clear, a test case from before the stop came (response at %v): lost, as the stream had ended", tc.GrpcResp.Timestamp)
	case <-g.recorded:
	}
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
