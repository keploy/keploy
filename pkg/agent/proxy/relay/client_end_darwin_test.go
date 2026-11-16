package relay

import (
	"context"
	"io"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A reset the client's forwarder reads is the client's FIN and then a reset
// when an answer before it said the FIN was in, or when its counts say so:
// rcv_nxt moved on, since the last answer that found the connection open, by
// one more than the bytes counted in less the duplicates, on a connection
// that never had a segment come out of order. Anything else is a reset, as
// before.
func TestXNUTellsAFINBeforeAResetByItsCounts(t *testing.T) {
	base := received{rcvNxt: ^uint32(0) - 1, rxBytes: 1000, dup: 10} // rcv_nxt 2 before the wrap
	open := darwinTCPInfo{state: tcpsEstablished, received: base}
	reset := func(moved uint32, in, dup uint64) darwinTCPInfo {
		return darwinTCPInfo{state: tcpsClosed, received: received{rcvNxt: base.rcvNxt + moved, rxBytes: base.rxBytes + in, dup: base.dup + dup}}
	}
	withOOO := reset(301, 300, 0)
	withOOO.ooo = 1
	wentBack, dupBack := reset(1, 0, 0), reset(1, 0, 0)
	wentBack.rxBytes, dupBack.dup = base.rxBytes-1, base.dup-1
	notReset := reset(1, 0, 0)
	notReset.state = tcpsEstablished
	moved := open
	moved.rcvNxt, moved.rxBytes = base.rcvNxt+100, base.rxBytes+100
	for _, row := range []struct {
		name   string
		script []darwinTCPInfo // the looks before the reset, then the answer at it
		want   bool
	}{
		{name: "the FIN alone, across the wrap", script: []darwinTCPInfo{open, reset(1, 0, 0)}, want: true},
		{name: "bytes, then the FIN", script: []darwinTCPInfo{open, reset(301, 300, 0)}, want: true},
		{name: "bytes, some resent, then the FIN", script: []darwinTCPInfo{open, reset(301, 340, 40)}, want: true},
		{name: "from the last open answer", script: []darwinTCPInfo{open, moved, reset(101, 100, 0)}, want: true},
		{name: "a look found the FIN in", script: []darwinTCPInfo{open, {state: tcpsCloseWait}, reset(0, 0, 0)}, want: true},
		{name: "a look found the FIN in, the proxy's FIN sent", script: []darwinTCPInfo{open, {state: tcpsLastAck}, reset(0, 0, 0)}, want: true},
		{name: "a look found both FINs crossed", script: []darwinTCPInfo{open, {state: tcpsClosing}, reset(0, 0, 0)}, want: true},
		{name: "a look found the FIN after the proxy's", script: []darwinTCPInfo{open, {state: tcpsTimeWait}, reset(0, 0, 0)}, want: true},
		{name: "a look after the reset, the FIN before it", script: []darwinTCPInfo{open, reset(1, 0, 0), reset(1, 0, 0)}, want: true},
		{name: "no FIN", script: []darwinTCPInfo{open, reset(0, 0, 0)}, want: false},
		{name: "bytes, no FIN", script: []darwinTCPInfo{open, reset(300, 300, 0)}, want: false},
		{name: "bytes, some resent, no FIN", script: []darwinTCPInfo{open, reset(300, 340, 40)}, want: false},
		{name: "moved on more than counted", script: []darwinTCPInfo{open, reset(302, 300, 0)}, want: false},
		{name: "a segment came out of order", script: []darwinTCPInfo{open, withOOO}, want: false},
		{name: "more duplicates than bytes", script: []darwinTCPInfo{open, reset(1, 0, 5)}, want: false},
		{name: "bytes counted went back", script: []darwinTCPInfo{open, wentBack}, want: false},
		{name: "duplicates counted went back", script: []darwinTCPInfo{open, dupBack}, want: false},
		{name: "the answer at the reset is no reset", script: []darwinTCPInfo{open, notReset}, want: false},
		{name: "no answer before the reset", script: []darwinTCPInfo{reset(1, 0, 0)}, want: false},
		{name: "no answer before the reset, counts from zero", script: []darwinTCPInfo{{state: tcpsClosed, received: received{rcvNxt: 1}}}, want: false},
		{name: "only a closed answer before", script: []darwinTCPInfo{reset(1, 0, 0), reset(1, 0, 0)}, want: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			x := &scriptedXNU{infos: row.script}
			s := x.socket()
			for range row.script[:len(row.script)-1] {
				_ = s.look()
			}
			if got := s.finBeforeReset(); got != row.want {
				t.Fatalf("finBeforeReset = %v, want %v", got, row.want)
			}
		})
	}
}

// Counts no kernel gives, which the arithmetic would wrap into the FIN's
// sum, say no: duplicates that went back, bytes that went back, and more
// duplicates than bytes.
func TestXNUCountsThatWrapIntoTheFINsSumSayNoFIN(t *testing.T) {
	top := ^uint64(0)
	for _, row := range []struct {
		name      string
		base, now received
	}{
		{name: "duplicates went back, bytes to the top", base: received{rxBytes: 0, dup: 10}, now: received{rcvNxt: 1, rxBytes: top, dup: 9}},
		{name: "bytes went back", base: received{rxBytes: 10}, now: received{rcvNxt: 2, rxBytes: 9, dup: top - 1}},
		{name: "more duplicates than bytes", base: received{}, now: received{rcvNxt: 2, dup: top}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if finCameFirst(row.base, &darwinTCPInfo{state: tcpsClosed, received: row.now}) {
				t.Fatalf("finCameFirst(%+v, %+v) = true, want false", row.base, row.now)
			}
		})
	}
}

// A socket with no logger whose TCP_INFO cannot be read says nothing, and
// logs nothing.
func TestXNUASocketWithNoLoggerLogsNoFallback(t *testing.T) {
	_, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, nil).(*tcpClientSocket)
	s.fallback = &infoFallback{}
	s.getInfo = func(uintptr, *[tcpInfoBuf]byte) (int, error) { return tcpInfoNeeded - 1, nil }
	if l := s.look(); l.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", l)
	}
	s.getInfo = func(uintptr, *[tcpInfoBuf]byte) (int, error) { return 0, syscall.EINVAL }
	if l := s.look(); l.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", l)
	}
}

// A socket whose TCP_INFO cannot answer at the reset says no.
func TestXNUASocketThatCannotAnswerAtTheResetSaysNoFIN(t *testing.T) {
	answers := 0
	s := &tcpClientSocket{rc: noFD{}, askInfo: func(_ uintptr, _ *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
		answers++
		if answers > 1 {
			return false
		}
		*info = darwinTCPInfo{state: tcpsCloseWait}
		return true
	}}
	s.ask, s.count = s.lookAt, s.countAt
	_ = s.look()
	if s.finBeforeReset() {
		t.Fatal("finBeforeReset = true with no answer at the reset; want false")
	}
}

// On a real socket the counts a look reads are the bytes the client sent,
// and its FIN: rcv_nxt moves on by each byte and by one for the FIN, and
// tcpi_rxbytes by each byte, with nothing duplicated or out of order.
func TestXNUCountsTheBytesAndTheFINTheClientSent(t *testing.T) {
	app, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, zap.NewNop()).(*tcpClientSocket)
	ask := func() darwinTCPInfo {
		t.Helper()
		var info darwinTCPInfo
		ok := false
		if err := s.rc.Control(func(fd uintptr) { ok = s.askInfo(fd, &s.buf, &info) }); err != nil || !ok {
			t.Fatalf("TCP_INFO: %v %v", ok, err)
		}
		return info
	}
	base := ask()
	const sent = "eleven byte"
	if _, err := app.Write([]byte(sent)); err != nil {
		t.Fatal(err)
	}
	_ = proxyClient.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(proxyClient, make([]byte, len(sent))); err != nil {
		t.Fatal(err)
	}
	got := ask()
	if d, in := got.rcvNxt-base.rcvNxt, got.rxBytes-base.rxBytes; d != uint32(len(sent)) || in != uint64(len(sent)) || got.dup != 0 || got.ooo != 0 {
		t.Fatalf("after %d bytes: rcv_nxt moved %d, %d bytes counted, %d duplicate, %d out of order; want %d, %d, 0, 0",
			len(sent), d, in, got.dup, got.ooo, len(sent), len(sent))
	}
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN is in")
	got = ask()
	if d, in := got.rcvNxt-base.rcvNxt, got.rxBytes-base.rxBytes; d != uint32(len(sent))+1 || in != uint64(len(sent)) {
		t.Fatalf("after the FIN: rcv_nxt moved %d, %d bytes counted; want %d, %d", d, in, len(sent)+1, len(sent))
	}
}

// A TCP_INFO this macOS answers shorter than a look reads (getsockopt's
// optlen short of the end of the last field read), or a call that fails,
// says nothing: what the destination sends a closed client is teed as it is
// written, whether the client took it or not, as on Windows, and the relay
// says so once. (With the socket answering, this answer is refused:
// TestAfterTheClientClosed_WhatTheClientDidNotTakeIsNotTeed.)
func TestXNUATCPInfoThatCannotBeReadTeesEveryChunk(t *testing.T) {
	const request, answer = "request", "the answer, after the client's FIN"
	for _, row := range []struct {
		name string
		get  func(fd uintptr, buf *[tcpInfoBuf]byte) (int, error)
		said string
	}{
		{name: "an answer one byte short", said: "answers a TCP_INFO shorter than keploy reads", get: func(fd uintptr, buf *[tcpInfoBuf]byte) (int, error) {
			n, err := getTCPInfo(fd, buf)
			return min(n, tcpInfoNeeded-1), err
		}},
		{name: "a failing call", said: "TCP_INFO failed", get: func(uintptr, *[tcpInfoBuf]byte) (int, error) {
			return 0, syscall.EINVAL
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			core, logs := observer.New(zapcore.DebugLevel)
			r := New(Config{Logger: zap.New(core)}, proxyClient, connseq.NewUpstream(proxyServer))
			s := r.client.(*tcpClientSocket)
			s.getInfo, s.fallback = row.get, &infoFallback{}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			streams := make(chan string, 1)
			go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
			if _, err := app.Write([]byte(request)); err != nil {
				t.Fatal(err)
			}
			_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(server, make([]byte, len(request))); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
				t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
			}
			if _, err := server.Write([]byte(answer)); err != nil {
				t.Fatal(err)
			}
			awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the answer")
			_ = server.Close()
			select {
			case got := <-streams:
				if got != answer {
					t.Fatalf("the destination's stream carried %q, want %q: teed as written", got, answer)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the destination's stream did not end")
			}
			if n := notTakenLogged(t, logs); n != 0 {
				t.Fatalf("the relay logged %d bytes not taken, want none", n)
			}
			if n := logs.FilterMessageSnippet(row.said).Len(); n != 1 {
				t.Fatalf("the relay said %q %d times, want once", row.said, n)
			}
		})
	}
}
