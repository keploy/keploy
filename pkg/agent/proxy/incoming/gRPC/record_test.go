package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
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

// The frame that ends a call's stream (its trailers) is stamped before its
// client can see it: the bytes before its last byte are written, then it is
// stamped and handed to the parser, and its last byte goes to the client once
// the clock reads past the stamp. Stamped after its write, as it was, the
// stamp came after the client had the trailers and could send its next call.
func TestForwardAndTeeStampsAStreamsEndBeforeItsClientCanSeeIt(t *testing.T) {
	for _, tick := range []time.Duration{0, windowsTick, time.Millisecond} {
		t.Run(clockName(tick), func(t *testing.T) {
			if tick != 0 {
				stubQuantizedClock(t, tick)
			}
			srcRead, srcWrite := net.Pipe()
			dstRead, dstWrite := net.Pipe()
			for _, c := range []net.Conn{srcRead, srcWrite, dstRead, dstWrite} {
				require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
				defer c.Close()
			}
			ch := make(chan frameChunk, 1)
			go forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "app→client", true, nil)

			_, resp := pingCall(t)
			awaitTickStart(tick)
			sentAt := clockNow()
			go func() { _, _ = srcWrite.Write(resp) }()

			// The client has every byte of the response but the last.
			_, err := io.ReadFull(dstRead, make([]byte, len(resp)-1))
			require.NoError(t, err)
			var chunk frameChunk
			select {
			case chunk = <-ch:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream's end was not stamped before its client had its last byte")
			}
			assert.Equal(t, resp, chunk.data, "the stamped chunk is not the response up to its end")
			assert.False(t, chunk.timestamp.Before(sentAt), "stamped at %v, before the app sent it (%v)", chunk.timestamp, sentAt)

			_, err = io.ReadFull(dstRead, make([]byte, 1))
			require.NoError(t, err)
			saw := clockNow()
			assert.True(t, chunk.timestamp.Before(saw), "stamped at %v, not before the clock its client saw the end by (%v)", chunk.timestamp, saw)
		})
	}
}

// Several calls end in one read, beside the frames of a call still running
// (one connection carries them all). The read is stamped once, before its
// client can see the first of those ends, and handed to the parser whole: an
// end stamped after the one before it had reached the client could be
// stamped after the client acted on that one
// (TestRecordIncoming_ASecondEndInAReadIsStampedBeforeTheFirstReachesItsClient).
// The client gets every byte, in order.
func TestForwardAndTeeStampsAReadOfSeveralEndsOnce(t *testing.T) {
	srcRead, srcWrite := net.Pipe()
	dstRead, dstWrite := net.Pipe()
	for _, c := range []net.Conn{srcRead, srcWrite, dstRead, dstWrite} {
		require.NoError(t, c.SetDeadline(time.Now().Add(10*time.Second)))
		defer c.Close()
	}
	ch := make(chan frameChunk, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "app→client", true, nil)
	}()
	_, resp1 := echoCall(t, 1, "one")
	_, resp3 := echoCall(t, 3, "two")
	running := headersFrameBytes(t, 5, []hpack.HeaderField{{Name: ":status", Value: "200"}}, false)
	read := append(append(append([]byte{}, resp1...), resp3...), running...)
	go func() { _, _ = srcWrite.Write(read) }()

	got := make([]byte, 0, len(read))
	for len(got) < len(read) {
		b := make([]byte, 1)
		_, err := io.ReadFull(dstRead, b)
		require.NoError(t, err)
		got = append(got, b...)
		if len(got) == len(resp1) {
			require.Len(t, ch, 1, "the first end reached its client before the read was stamped")
		}
	}
	srcWrite.Close()
	<-done
	close(ch)
	var teed []frameChunk
	for c := range ch {
		teed = append(teed, c)
	}
	require.Len(t, teed, 1, "the read was not stamped once")
	assert.Equal(t, read, teed[0].data)
	assert.Equal(t, read, got)
}

// The scanner finds the end of every frame that ends a stream, wherever the
// reads cut the frames: in a header, in a payload, at a frame's edge.
func TestEndScannerFindsEveryEndWhereverTheReadsCut(t *testing.T) {
	_, resp1 := echoCall(t, 1, "one")
	emptyEnd := dataFrameBytes(t, 3, true, nil) // a DATA frame with no payload that ends its stream
	padded := frameBytes(t, func(fr *http2.Framer) error {
		return fr.WriteDataPadded(5, true, []byte("x"), make([]byte, 7))
	})
	notEnd := dataFrameBytes(t, 7, false, []byte("more"))
	// Flag 0x1 is ACK on these: not an end.
	settingsAck := frameBytes(t, func(fr *http2.Framer) error { return fr.WriteSettingsAck() })
	pingAck := frameBytes(t, func(fr *http2.Framer) error { return fr.WritePing(true, [8]byte{}) })
	stream := append(append(append(append(append(append([]byte{}, resp1...), emptyEnd...), settingsAck...), notEnd...), pingAck...), padded...)
	want := []int{len(resp1), len(resp1) + len(emptyEnd), len(stream)}
	for cut := 0; cut <= len(stream); cut++ {
		var s endScanner
		var got []int
		for _, e := range s.scan(stream[:cut]) {
			got = append(got, e)
		}
		for _, e := range s.scan(stream[cut:]) {
			got = append(got, cut+e)
		}
		require.Equal(t, want, got, "reads cut at %d", cut)
	}
	var s endScanner
	var got []int
	for i := range stream {
		if e := s.scan(stream[i : i+1]); len(e) > 0 {
			got = append(got, i+e[0])
		}
	}
	assert.Equal(t, want, got, "a byte a read")

	// A frame longer than 64 KiB: its length's first byte counts.
	big := append([]byte{0x01, 0x11, 0x70, byte(http2.FrameData), byte(http2.FlagDataEndStream), 0, 0, 0, 9}, make([]byte, 0x011170)...)
	big = append(big, resp1...)
	for _, cut := range []int{0, 2, 9, 30000, len(big) - len(resp1) - 1, len(big) - len(resp1), len(big) - 1} {
		var s endScanner
		var got []int
		for _, e := range s.scan(big[:cut]) {
			got = append(got, e)
		}
		for _, e := range s.scan(big[cut:]) {
			got = append(got, cut+e)
		}
		require.Equal(t, []int{len(big) - len(resp1), len(big)}, got, "a frame over 64 KiB, reads cut at %d", cut)
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
	return startGRPCIngressOn(t, nil)
}

// startGRPCIngressOn is startGRPCIngress with the ingress's end of the client
// connection wrapped by wrap, when it is not nil.
func startGRPCIngressOn(t *testing.T, wrap func(net.Conn) net.Conn) *grpcIngress {
	t.Helper()
	g := &grpcIngress{m: syncMock.New(nil), tcCh: make(chan *models.TestCase), recorded: make(chan struct{}),
		stop: time.Now().Add(time.Hour)}
	remove := g.m.AddWatermark(caughtUp{})
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), g.m))
	var cliProxy, appProxy net.Conn
	g.cliApp, cliProxy = net.Pipe()
	appProxy, g.appApp = net.Pipe()
	if wrap != nil {
		cliProxy = wrap(cliProxy)
	}
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
// write (a frame that ends no stream), so nothing it holds can be from before
// the stop. Held, it would keep the stop waiting on a client that never reads
// again.
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

// A client that stops reading inside the frame that ends a call's stream holds
// no stop, wherever it stops. Before the frame's last byte, the forwarder
// waits in the write of the bytes before it, and nothing of the response is
// noted yet. At the last byte, the frame is stamped and handed to the parser
// before that byte's write, which waits on the client: the call is pending
// only until it is captured and taken, as any call is, never on the client.
func TestRecordIncoming_AClientThatStopsReadingInAStreamsEndHoldsNoStop(t *testing.T) {
	for name, unread := range map[string]int{"before the end's last byte": 5, "at the end's last byte": 1} {
		t.Run(name, func(t *testing.T) {
			g := startGRPCIngress(t)
			req, resp := pingCall(t)
			go func() { _, _ = g.cliApp.Write(req) }()
			_, err := io.ReadFull(g.appApp, make([]byte, len(req)))
			require.NoError(t, err)
			g.waitFor(t, "the request chunk's release", func(st syncMock.BacklogState) bool { return st.Progress >= 1 && !st.Pending })

			go func() { _, _ = g.appApp.Write(resp) }()
			_, err = io.ReadFull(g.cliApp, make([]byte, len(resp)-unread))
			require.NoError(t, err)
			// The client reads no more.
			if unread > 1 {
				g.waitFor(t, "clear while the client does not read", func(st syncMock.BacklogState) bool { return !st.Pending && st.Progress >= 1 })
				select {
				case tc := <-g.tcCh:
					t.Fatalf("a call was captured whose end its client never had: %v", tc.GrpcReq.Headers.PseudoHeaders[":path"])
				case <-time.After(50 * time.Millisecond):
				}
				return
			}
			select {
			case <-g.tcCh:
			case <-time.After(10 * time.Second):
				t.Fatal("the call whose end was stamped was not captured")
			}
			g.waitFor(t, "clear once the call was taken, though its last byte waits on the client", func(st syncMock.BacklogState) bool { return !st.Pending && st.Progress >= 2 })
		})
	}
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

// The forwarder notes each chunk it tees as being built before it reads the
// chunk's stamp (the note is in when the stamp is read), and releases it when
// it cannot hand it on. What it sends the client (toClient) is noted once the
// write that may wait on the client returns: a write blocked on a client that
// stopped reading would otherwise hold every stop. That holds for a read that
// ends a stream too: it is noted after the write of the bytes before the first
// end's last byte (TestRecordIncoming_AClientThatStopsReadingInAStreamsEndHoldsNoStop).
func TestForwardAndTeeNotesEachChunkFromBeforeItsStamp(t *testing.T) {
	_, end := pingCall(t)
	for _, tc := range []struct {
		name     string
		toClient bool
		payload  []byte
	}{
		{"toClient=false", false, []byte("frame")},
		{"toClient=true, a read that ends no stream", true, []byte("frame")},
		{"toClient=true, a read that ends a stream", true, end},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcRead, srcWrite := net.Pipe()
			dstRead, dstWrite := net.Pipe()
			defer srcWrite.Close()
			defer dstRead.Close()

			notes := make(chan time.Time, 4)
			var released atomic.Int32
			building := func(time.Time) func() {
				notes <- clockNow() // when the note is in
				return func() { released.Add(1) }
			}
			ch := make(chan frameChunk, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				forwardAndTee(srcRead, dstWrite, ch, zap.NewNop(), "test", tc.toClient, building)
			}()

			go func() { _, _ = srcWrite.Write(tc.payload) }()
			unblocked := clockNow() // the forwarder's write can return from here on
			_, err := io.ReadFull(dstRead, make([]byte, len(tc.payload)))
			require.NoError(t, err)
			chunk := <-ch
			notedAt := <-notes
			assert.False(t, notedAt.After(chunk.timestamp), "noted at %v, after its stamp %v", notedAt, chunk.timestamp)
			if tc.toClient {
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
			if tc.toClient {
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

// heldAfterEndConn is the ingress's end of a client connection whose write
// that hands the client the last byte of end returns only once gate opens: the
// forwarder descheduled just after it gave its client a whole response, as on
// a loaded host. A pipe's write returns once the client has read all of it.
type heldAfterEndConn struct {
	net.Conn
	end  []byte
	gate <-chan struct{}

	mu   sync.Mutex
	sent []byte
	held bool
}

func (c *heldAfterEndConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.sent = append(c.sent, p[:n]...)
	hold := !c.held && bytes.HasSuffix(c.sent, c.end)
	c.held = c.held || hold
	c.mu.Unlock()
	if hold {
		<-c.gate
	}
	return n, err
}

// echoCall is one unary call on stream id, without the connection preface: its
// request, and the response with its trailers.
func echoCall(t *testing.T, id uint32, msg string) (req, resp []byte) {
	t.Helper()
	req = append(headersFrameBytes(t, id, []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/svc.Echo/" + msg},
		{Name: ":authority", Value: "localhost:50051"},
		{Name: "content-type", Value: "application/grpc"},
	}, false), dataFrameBytes(t, id, true, grpcPayloadBytes([]byte(msg)))...)
	resp = append(append(headersFrameBytes(t, id, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "application/grpc"},
	}, false), dataFrameBytes(t, id, false, grpcPayloadBytes([]byte(msg)))...),
		headersFrameBytes(t, id, []hpack.HeaderField{{Name: "grpc-status", Value: "0"}}, true)...)
	return req, resp
}

// A client that sends its next call only once it has the whole of the
// previous response (its trailers), on the same connection, as gRPC clients
// do, is sequential, and so are the app's calls for the two. The forwarder
// stamped the frames it sends the client after its write had returned: a
// forwarder descheduled there, once its client had the trailers, stamped them
// late, and the call's window ([request, trailers]) ran into the next call's.
// Under the proven switch the record derives each test's calls from those
// windows (models.DeriveWindowMappings), and a call made while two windows are
// open goes to the one that opened first: the next call's dependency call went
// to the test before it.
//
// What ends a stream is stamped before the byte that ends it reaches the
// client, and the client gets that byte only once the clock reads past the
// stamp: the window holds every call the app made for the call, and nothing
// its client does on the answer.
func TestRecordIncoming_AResponseEndsBeforeItsClientCanSeeIt(t *testing.T) {
	for _, tick := range []time.Duration{0, windowsTick, time.Millisecond} {
		t.Run(clockName(tick), func(t *testing.T) {
			if tick != 0 {
				stubQuantizedClock(t, tick)
			}
			aResponseEndsBeforeItsClientCanSeeIt(t, tick)
		})
	}
}

func aResponseEndsBeforeItsClientCanSeeIt(t *testing.T, tick time.Duration) {
	gate := make(chan struct{})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	defer open()
	req1, resp1 := echoCall(t, 1, "first")
	req2, resp2 := echoCall(t, 3, "next")
	g := startGRPCIngressOn(t, func(c net.Conn) net.Conn {
		return &heldAfterEndConn{Conn: c, end: resp1, gate: gate}
	})
	calls := map[string]time.Time{} // the app's dependency call for each, stamped as a parser stamps it
	serve := func(req, resp []byte, name string) {
		_, err := io.ReadFull(g.appApp, make([]byte, len(req)))
		require.NoError(t, err)
		calls[name] = clockNow()
		go func() { _, _ = g.appApp.Write(resp) }()
	}

	awaitTickStart(tick)
	go func() { _, _ = g.cliApp.Write(append([]byte(http2.ClientPreface), req1...)) }()
	_, err := io.ReadFull(g.appApp, make([]byte, len(http2.ClientPreface)))
	require.NoError(t, err)
	serve(req1, resp1, "first")
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp1)))
	require.NoError(t, err)
	sawEnd := clockNow()

	// The client has the first call's trailers: its next call.
	go func() { _, _ = g.cliApp.Write(req2) }()
	serve(req2, resp2, "next")
	open()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp2)))
	require.NoError(t, err)

	got := map[string]*models.TestCase{}
	for len(got) < 2 {
		select {
		case tc := <-g.tcCh:
			got[strings.TrimPrefix(tc.GrpcReq.Headers.PseudoHeaders[":path"], "/svc.Echo/")] = tc
		case <-time.After(10 * time.Second):
			t.Fatalf("captured %d of 2 calls", len(got))
		}
	}
	first, next := got["first"], got["next"]
	require.NotNil(t, first)
	require.NotNil(t, next)
	const ts = "15:04:05.000000"
	assert.True(t, first.GrpcResp.Timestamp.Before(sawEnd), "the first call's window ends at %s, not before its client had read its trailers (%s)",
		first.GrpcResp.Timestamp.Format(ts), sawEnd.Format(ts))
	assert.True(t, first.GrpcResp.Timestamp.Before(next.GrpcReq.Timestamp), "the first call's window ends at %s, not before the next call's begins (%s), though the next was sent only once the first was answered",
		first.GrpcResp.Timestamp.Format(ts), next.GrpcReq.Timestamp.Format(ts))

	// What a recording made without --sync maps each test to.
	derived := models.DeriveWindowMappings(
		[]models.TestWindow{models.RecordedWindow(withName(first, "first")), models.RecordedWindow(withName(next, "next"))},
		[]models.WindowedMock{
			{Name: "first", Kind: models.HTTP, At: calls["first"], Res: calls["first"], TestMock: true},
			{Name: "next", Kind: models.HTTP, At: calls["next"], Res: calls["next"], TestMock: true},
		})
	for _, tc := range derived.Tests {
		var owns []string
		for _, m := range tc.Mocks {
			owns = append(owns, m.Name)
		}
		assert.Equal(t, []string{tc.ID}, owns, "%s's test owns the calls %v, want only the one its own call made", tc.ID, owns)
	}
	assert.Empty(t, derived.Concurrent, "recorded as open together, though the client sent one call at a time")
}

// withName is tc named name.
func withName(tc *models.TestCase, name string) *models.TestCase {
	c := *tc
	c.Name = name
	return &c
}

// windowsTick is Windows' default timer tick: how often its time.Now moves on
// unless a process asks for a finer one.
const windowsTick = 15625 * time.Microsecond

// stubQuantizedClock has the forwarder, and the stamps a test takes with
// clockNow, read a clock of tick's resolution until the test ends: real time
// cut down to the tick, as Windows' time.Now is. The forwarder's waits are
// real sleeps on it.
func stubQuantizedClock(t *testing.T, tick time.Duration) {
	t.Helper()
	prev := stubbedClock.Swap(&forwardClock{
		now:   func() time.Time { return time.Now().Truncate(tick) },
		sleep: time.Sleep,
	})
	t.Cleanup(func() { stubbedClock.Store(prev) })
}

// awaitTickStart returns at the start of the clock's next tick: what a run
// does in its first few milliseconds then falls in one tick, as it does on a
// coarse clock unless the forwarder waits for the clock. At once for the
// system clock (tick 0).
func awaitTickStart(tick time.Duration) {
	if tick > 0 {
		time.Sleep(time.Until(time.Now().Truncate(tick).Add(tick)))
	}
}

// clockName names a run by the clock it runs on.
func clockName(tick time.Duration) string {
	if tick == 0 {
		return "on the system clock"
	}
	return "on a clock of " + tick.String() + " ticks"
}

// The forwarder is descheduled after it reads the clock, before its next
// step, as on a loaded host; a stop asks meanwhile. A read that ends a stream
// is noted before its stamp is read: a stop that asks before the note is in
// finds the call ends after its own instant, and nothing of it is lost. Noted
// after its stamp, a stop with an instant after the stamp heard "clear" while
// the call was on its way, and the call came after the stream had ended.
func TestRecordIncoming_AStopBetweenAReadAndItsNoteLosesNothing(t *testing.T) {
	var armed atomic.Bool
	reached := make(chan struct{}, 1)
	gate := make(chan struct{})
	var opened sync.Once
	open := func() { opened.Do(func() { close(gate) }) }
	defer open()
	prev := stubbedClock.Swap(&forwardClock{now: func() time.Time {
		n := time.Now()
		if armed.CompareAndSwap(true, false) {
			reached <- struct{}{}
			<-gate
		}
		return n
	}, sleep: time.Sleep})
	defer stubbedClock.Store(prev)

	g := startGRPCIngress(t)
	req, resp := pingCall(t)
	go func() { _, _ = g.cliApp.Write(req) }()
	_, err := io.ReadFull(g.appApp, make([]byte, len(req)))
	require.NoError(t, err)
	g.waitFor(t, "the request chunk's release", func(st syncMock.BacklogState) bool { return st.Progress >= 1 && !st.Pending })

	// The forwarder reads the response, writes it up to its end's last byte,
	// and is held at its next read of the clock.
	armed.Store(true)
	go func() { _, _ = g.appApp.Write(resp) }()
	go func() { _, _ = io.ReadFull(g.cliApp, make([]byte, len(resp))) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarder never read its clock for the response")
	}
	g.stop = time.Now().Add(time.Millisecond)
	for !time.Now().After(g.stop) {
		time.Sleep(100 * time.Microsecond)
	}
	st := g.backlog()
	open()
	var tc *models.TestCase
	select {
	case tc = <-g.tcCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no test case came of the call")
	}
	require.False(t, st.Pending, "setup: the stop asked before the forwarder noted the response")
	assert.True(t, tc.GrpcResp.Timestamp.After(g.stop), "the stop heard clear, then a call that ended before it came (at %v, the stop %v): lost to a stream the stop ended",
		tc.GrpcResp.Timestamp, g.stop)
}

// Two calls in flight on one connection (streams 1 and 3); the app answers
// both, and the forwarder reads both answers in one read. The client reads the
// first answer and, on it, sends a third call, and the app makes the third
// call's dependency call, all before the client reads the second answer, so
// the forwarder is still in the write that carries it. The second call's
// window must not run past that dependency call: the app had answered it
// before the third call was sent. Stamped end by end, the second end was
// stamped only after the first had reached its client and the client had read
// on, and the second call's test took the third call's dependency call.
func TestRecordIncoming_ASecondEndInAReadIsStampedBeforeTheFirstReachesItsClient(t *testing.T) {
	gate := make(chan struct{})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	defer open()
	req1, resp1 := echoCall(t, 1, "first")
	req2, resp2 := echoCall(t, 3, "second")
	req3, resp3 := echoCall(t, 5, "third")
	g := startGRPCIngressOn(t, func(c net.Conn) net.Conn {
		return &heldAfterEndConn{Conn: c, end: resp1, gate: gate}
	})
	calls := map[string]time.Time{} // each call's dependency call, stamped as a parser stamps it
	go func() { _, _ = g.cliApp.Write(append(append([]byte(http2.ClientPreface), req1...), req2...)) }()
	_, err := io.ReadFull(g.appApp, make([]byte, len(http2.ClientPreface)+len(req1)+len(req2)))
	require.NoError(t, err)
	calls["first"], calls["second"] = clockNow(), clockNow()
	go func() { _, _ = g.appApp.Write(append(append([]byte{}, resp1...), resp2...)) }()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp1)))
	require.NoError(t, err)

	// On the first answer, the client's next call.
	go func() { _, _ = g.cliApp.Write(req3) }()
	_, err = io.ReadFull(g.appApp, make([]byte, len(req3)))
	require.NoError(t, err)
	calls["third"] = clockNow()
	open()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp2)))
	require.NoError(t, err)
	go func() { _, _ = g.appApp.Write(resp3) }()
	_, err = io.ReadFull(g.cliApp, make([]byte, len(resp3)))
	require.NoError(t, err)

	got := map[string]*models.TestCase{}
	for len(got) < 3 {
		select {
		case tc := <-g.tcCh:
			got[strings.TrimPrefix(tc.GrpcReq.Headers.PseudoHeaders[":path"], "/svc.Echo/")] = tc
		case <-time.After(10 * time.Second):
			t.Fatalf("captured %d of 3 calls", len(got))
		}
	}
	var windows []models.TestWindow
	var made []models.WindowedMock
	for _, name := range []string{"first", "second", "third"} {
		require.NotNil(t, got[name], name)
		windows = append(windows, models.RecordedWindow(withName(got[name], name)))
		made = append(made, models.WindowedMock{Name: name, Kind: models.HTTP, At: calls[name], Res: calls[name], TestMock: true})
	}
	// The first two were in flight together: which of them owns each of their
	// calls is decided by time alone (the record says so). The third's is its
	// own.
	for _, tc := range models.DeriveWindowMappings(windows, made).Tests {
		for _, m := range tc.Mocks {
			if m.Name == "third" {
				assert.Equal(t, "third", tc.ID, "the third call's dependency call went to the %s call's test", tc.ID)
			}
		}
	}
}
