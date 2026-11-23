//go:build darwin

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// darwinPairOn is darwinPair on p, paired as application connection appSeq,
// with nothing proven listed by its accept.
func darwinPairOn(t *testing.T, p *Proxy, appSeq uint64) (app, proxySide *net.TCPConn, reader *trackedConn) {
	t.Helper()
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
	p.paired.add(proxySide, appSeq, 0)
	reader = p.inflight.track(proxySide)
	reader.beforeClose = func() { p.pairClosing(context.Background(), proxySide, reader) }
	// Its use counted as the proxy's accept counts it, and used once by
	// the test open now: a close that cannot tell what was on its way is
	// that test's (pairUntold).
	reader.pairUse = p.paired.useOf(proxySide)
	reader.pairUse.Add(1)
	t.Cleanup(func() { p.inflight.forget(reader) })
	return app, proxySide, reader
}

// A close whose read of macOS's TCP table fails cannot tell what was on its
// way: before, it recorded nothing and answered nothing, and a wait judged
// the connection by what was written when it began. It is a loss of the test
// open now, said in one line naming the connection, the read and its error.
func TestACloseWhoseTableReadFailsIsUnknownOnDarwin(t *testing.T) {
	p, app, proxySide, reader := darwinPair(t)
	p.settleReads = true
	core, logs := observer.New(zapcore.WarnLevel)
	p.logger = zap.New(core)
	was := readXNUTable
	readXNUTable = func() ([]xnuPCB, error) {
		return nil, errors.New("sysctl net.inet.tcp.pcblist64: operation not permitted")
	}
	t.Cleanup(func() { readXNUTable = was })
	_ = reader.Close()
	wantOneCloseUnknownLine(t, p, logs, "operation not permitted")
	lines := logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines say the close cannot tell what was written, want one: %+v", len(lines), logs.All())
	}
	f := lines[0].ContextMap()
	if want := app.LocalAddr().String() + " -> " + proxySide.LocalAddr().String(); f["connection"] != want {
		t.Errorf("the line names the connection %v, want %s", f["connection"], want)
	}
	if e, _ := f["error"].(string); !strings.Contains(e, "macOS's TCP table cannot be read") || !strings.Contains(e, "operation not permitted") {
		t.Errorf("the line's error is %q, want it to name the table's read and its error", e)
	}
}

// A close that cannot tell what was on its way leaves no other wait without
// its answer: a later wait on another connection ends once that connection's
// bytes are in. Before, the first such close was every later wait's error, so
// one connection the agent could not account for (a client that closed as it
// read a miss's 502) left every later test without a verdict.
func TestACloseThatCannotTellLeavesLaterWaitsTheirOwnAnswerOnDarwin(t *testing.T) {
	p, _, _, reader := darwinPair(t)
	app2, proxySide2, reader2 := darwinPairOn(t, p, 2)
	p.settleReads = true
	core, logs := observer.New(zapcore.WarnLevel)
	p.logger = zap.New(core)
	was := readXNUTable
	t.Cleanup(func() { readXNUTable = was })
	readXNUTable = func() ([]xnuPCB, error) { return nil, errors.New("sysctl: operation not permitted") }
	_ = reader.Close()
	readXNUTable = was
	wantOneCloseUnknownLine(t, p, logs, "operation not permitted")
	if _, err := app2.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide2, 5)
	if _, err := io.ReadFull(reader2, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.WaitPairedBytes(ctx, 2); err != nil {
		t.Fatalf("a wait on a connection whose bytes are in ended with %v, want nil: another connection's close that could not tell is not its answer", err)
	}
}

// A close that reads the application's socket from its holder is the first
// such read as a wait's is: a later wait on a connection whose socket no
// process the agent may read holds waits for its FIN or reset, as it does
// once a wait read one, not unknown.
func TestACloseReadFromTheHolderProvesTheHoldersReadOnDarwin(t *testing.T) {
	// Connection 2 closes first; the wait is on connection 1 alone.
	p, app, _, _ := heldPair(t)
	app2, proxySide2, reader2 := darwinPairOn(t, p, 2)
	hideFromTable(t, app2.LocalAddr())
	if _, err := app2.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide2, 5)
	if _, err := io.ReadFull(reader2, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	untilSendBufferEmpty(t, pairedConnOf(t, p, 2))
	_ = reader2.Close()
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v after a close read from the holder with nothing on its way, want nil", err)
	}
	hideSockets(t, app.LocalAddr())
	if _, err := app.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.WaitPairedBytes(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the wait ended with %v, want it to wait for the FIN of a socket no process it may read holds", err)
	}
}
