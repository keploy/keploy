//go:build darwin

package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// darwinAcceptedPair sets up a real loopback connection wired through
// p.acceptedConn so reader carries the proxy's real appClosed watcher
// (unlike darwinPair, which constructs trackedConn directly without wiring
// appClosed).
func darwinAcceptedPair(t *testing.T) (p *Proxy, app, proxySide *net.TCPConn, reader *trackedConn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop()}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app = c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide = s.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	p.paired.add(proxySide, 1, 0)
	t.Cleanup(func() { p.paired.remove(proxySide) })
	_, reader = p.acceptedConn(context.Background(), proxySide)
	if reader == nil || reader.appClosed == nil {
		t.Fatal("premise: acceptedConn must wire appClosed on a paired darwin connection")
	}
	t.Cleanup(func() { p.inflight.forget(reader) })
	return p, app, proxySide, reader
}

// On macOS, a real loopback socket carries SS_ISCONNECTED and leaves
// SS_NOFDREF unset while open or half-closed (CloseWrite, which sets
// SS_CANTSENDMORE), then enters FIN_WAIT_2 with SS_NOFDREF set once the
// process closes its descriptor whole while the proxy holds CLOSE_WAIT.
// PairedBytesDrain keeps a half-closed hold open and ends a closed-whole hold
// with zero stuck losses.
func TestDarwinAppSocketClosedReadsRealSSNoFDRef(t *testing.T) {
	if err := xnuTableReadable(); err != nil {
		t.Fatalf("premise: xnuTableReadable failed: %v", err)
	}
	if err := xnuAppClosedReadable(); err != nil {
		t.Fatalf("premise: xnuAppClosedReadable failed: %v", err)
	}
	t.Run("live, half-closed, then closed whole", func(t *testing.T) {
		p, app, proxySide, _ := darwinAcceptedPair(t)
		pc := pairedConnOf(t, p, 1)

		if p.appClosedChecker()(pc) {
			t.Fatal("live socket reported as closed")
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, proxySide, peerEnded, "the application's FIN")
		if p.appClosedChecker()(pc) {
			t.Fatal("half-closed socket reported as closed while process still holds its descriptor")
		}
		table, err := readXNUTable()
		if err != nil {
			t.Fatalf("readXNUTable after CloseWrite: %v", err)
		}
		b, ok := findXNUPCB(table, pc.app, pc.proxy)
		if !ok || b.soState&xnuSSNoFDRef != 0 || b.soState&xnuSSCantSend == 0 {
			t.Fatalf("half-closed socket pcb = (%+v, %v); want listed with SS_CANTSENDMORE set and SS_NOFDREF unset", b, ok)
		}

		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			table, err = readXNUTable()
			if err != nil {
				t.Fatalf("readXNUTable after Close: %v", err)
			}
			b, ok = findXNUPCB(table, pc.app, pc.proxy)
			if ok && b.soState&xnuSSNoFDRef != 0 && (b.state == xnuTCPFinWait1 || b.state == xnuTCPFinWait2) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("closed socket pcb = (%+v, %v); want FIN_WAIT_1/FIN_WAIT_2 with SS_NOFDREF set while proxy holds CLOSE_WAIT", b, ok)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !p.appClosedChecker()(pc) {
			t.Fatal("appClosedChecker returned false after app.Close()")
		}
	})

	startHold := func(t *testing.T, p *Proxy, proxySide *net.TCPConn, reader *trackedConn) <-chan struct{} {
		t.Helper()
		ended := make(chan struct{})
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop) })
		ctx := integrations.WithRequestHolder(context.Background(), reader)
		go func() {
			defer close(ended)
			_, _ = io.Copy(io.Discard, reader)
			release := integrations.MarkIdle(ctx)
			select {
			case <-integrations.AppClosed(ctx):
			case <-stop:
				release()
				return
			}
			release()
			_ = reader.Close()
			p.paired.remove(proxySide)
		}()
		return ended
	}

	t.Run("drain ends a post-FIN hold once the app closes its socket whole", func(t *testing.T) {
		p, app, proxySide, reader := darwinAcceptedPair(t)
		pc := pairedConnOf(t, p, 1)
		ended := startHold(t, p, proxySide, reader)

		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, proxySide, peerEnded, "the application's FIN")
		if err := drainAnswers(t, drainReturns(context.Background(), p), "once no process held the application's socket"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Fatal("the handler did not exit after the drain signalled appClosed")
		}
		if !pc.appClosedSaid() {
			t.Fatal("the drain did not mark appClosed on the paired connection")
		}
		if ls := stuckLosses(t, p); len(ls) != 0 {
			t.Fatalf("the drain gave the close up as stuck: %+v", ls)
		}
	})

	t.Run("a half-closed application gets no word until it closes", func(t *testing.T) {
		p, app, proxySide, reader := darwinAcceptedPair(t)
		pc := pairedConnOf(t, p, 1)
		ended := startHold(t, p, proxySide, reader)

		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, proxySide, peerEnded, "the application's FIN")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := drainReturns(ctx, p)
		drainHolds(t, got, "while the half-closed application's process still held its socket")
		if pc.appClosedSaid() {
			t.Fatal("the drain marked appClosed while the process still held its descriptor")
		}
		select {
		case <-ended:
			t.Fatal("the handler ended while the process still held its descriptor")
		default:
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		if err := drainAnswers(t, got, "once the half-closed application closed its socket"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Fatal("the handler did not exit after the half-closed application closed its socket")
		}
		if ls := stuckLosses(t, p); len(ls) != 0 {
			t.Fatalf("the drain gave the close up as stuck: %+v", ls)
		}
	})

	t.Run("unlisted socket never sighted stays held even when another socket was visible", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		// Simulate another app socket having been sighted in the TCP table and
		// a holder table, while THIS pc has never been explicitly sighted.
		p.paired.visible.Store(true)
		p.paired.held.Store(true)
		pc := &pairedConn{
			app:   netip.MustParseAddrPort("127.0.0.1:59991"),
			proxy: netip.MustParseAddrPort("127.0.0.1:16789"),
		}
		var h heldSockets
		if p.appSocketClosedIn(nil, pc, &h, true) {
			t.Fatal("unlisted socket never explicitly sighted must not be treated as closed merely because another socket was visible")
		}
		// Once THIS socket was sighted in the table and then vanishes, it is closed.
		pc.seenInTable.Store(true)
		if !p.appSocketClosedIn(nil, pc, &h, true) {
			t.Fatal("socket explicitly sighted in table and now gone must be treated as closed")
		}
	})
}
