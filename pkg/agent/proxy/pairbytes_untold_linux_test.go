//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// untoldPair is a paired connection the proxy accepts as it accepts any
// (acceptedConn: its tracked reads and writes count as the proxy's use of
// it), for a proxy that holds the paired bytes.
func untoldPair(t *testing.T) (p *Proxy, app *net.TCPConn, tracked *trackedConn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop(), settleReads: true}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
	t.Cleanup(func() { _ = s.Close() })
	p.paired.add(s.(*net.TCPConn), 1, 0)
	_, tracked = p.acceptedConn(context.Background(), s)
	if tracked == nil || tracked.pairUse == nil {
		t.Fatal("premise: the proxy's accept keeps a paired connection, its use counted")
	}
	t.Cleanup(func() { p.inflight.forget(tracked) })
	return p, app, tracked
}

// use is a call on the connection the proxy reads and answers.
func use(t *testing.T, app *net.TCPConn, tracked *trackedConn) {
	t.Helper()
	if _, err := app.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(tracked, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
}

// endWait is a test's end waiting for the bytes on the connection.
func endWait(t *testing.T, p *Proxy, test string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(agent.WithEndOf(context.Background(), test), 10*time.Second)
	defer cancel()
	if err := p.WaitPairedBytes(ctx, 1); err != nil {
		t.Fatalf("the end of %s: %v", test, err)
	}
}

// A close keploy cannot account for leaves without a verdict only the tests
// the connection could have served, never as a judged loss: the test open at
// the close when the proxy moved bytes on the connection since the last end
// that waited for them. A test that never used it is untouched, whatever
// test is open at the close: the macOS m5-miss race closed a connection used
// in the miss test after that test ended, and the test open then (TestClock,
// which never used it) came out failed, then not judged.
func TestAnUntoldCloseIsOnlyTheTestsThatUsedTheConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		// ended: the test that used it ended before the close; usedAfter:
		// the test open at the close used it too.
		ended, usedAfter bool
		charged          uint64
	}{
		{"the close in the test that used it", false, false, 1},
		{"the close after the test that used it ended, in one that did not", true, false, 0},
		{"the close after the test that used it ended, in one that used it too", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, app, tracked := untoldPair(t)
			lines := closeUnknownLines(p)
			use(t, app, tracked)
			if tc.ended {
				endWait(t, p, "TestStock")
			}
			if tc.usedAfter {
				use(t, app, tracked)
			}
			emptyDiagFree(t)
			failLookupWritten(t)
			_ = tracked.Close()
			if err := p.PairedBytesErr(); err != nil {
				t.Fatalf("PairedBytesErr = %v, want nil: no later wait is answered by this close", err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != tc.charged {
				t.Errorf("%d untold closes charged to the open test, want %d", l, tc.charged)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Errorf("%d judged bytes losses, want none: keploy cannot tell a byte was lost", l)
			}
			if owned := p.TakeOwnedLosses(); len(owned) != 0 {
				t.Errorf("owned %+v, want none: no end was cut short", owned)
			}
			said := lines.FilterMessageSnippet("keploy cannot tell what the application had written").All()
			if len(said) != 1 {
				t.Fatalf("%d lines say the close cannot tell what was written, want one: %+v", len(said), lines.All())
			}
			if tc.charged == 0 && said[0].Level != zap.InfoLevel {
				t.Errorf("a close charged to no test is said at %v, want info", said[0].Level)
			}
			if tc.charged == 1 && said[0].Level != zap.WarnLevel {
				t.Errorf("a close charged to a test is said at %v, want warn", said[0].Level)
			}
		})
	}
}

// A test whose end was cut short with bytes on the connection unsettled owns
// a close keploy cannot account for that comes later: it is left without a
// verdict whichever test is open then. The open test is charged too only
// when it used the connection itself.
func TestAnUntoldCloseIsOwnedByATestCutShortOnTheConnection(t *testing.T) {
	for _, usedAfter := range []bool{false, true} {
		name := "the open test did not use it"
		if usedAfter {
			name = "the open test used it too"
		}
		t.Run(name, func(t *testing.T) {
			p, app, tracked := untoldPair(t)
			use(t, app, tracked)
			// TestStock's end began its wait (what the proxy had moved is
			// its), and was cut short with 5 bytes written unsettled.
			p.paired.markEnd(1, 0)
			pairedConnOf(t, p, 1).cutShort("TestStock", 5)
			if usedAfter {
				use(t, app, tracked)
			}
			emptyDiagFree(t)
			failLookupWritten(t)
			_ = tracked.Close()
			owned := p.TakeOwnedLosses()
			if len(owned) != 1 || owned[0].Test != "TestStock" || owned[0].Loss.Kind != models.CaptureLossAppBytesUntold || owned[0].Loss.Count != 1 {
				t.Fatalf("owned %+v, want one untold close of TestStock's, whose end was cut short on the connection", owned)
			}
			want := uint64(0)
			if usedAfter {
				want = 1
			}
			if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != want {
				t.Errorf("%d untold closes charged to the open test, want %d", l, want)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Errorf("%d judged bytes losses, want none", l)
			}
		})
	}
}

// The end of a test worker's test marks only that worker's connections: a
// connection of another worker, used by that worker's open test, is still
// that test's when it closes untold.
func TestAnEndMarksOnlyItsOwnWorkersConnections(t *testing.T) {
	p, app, tracked := untoldPair(t)
	pairedConnOf(t, p, 1).instance = 7
	use(t, app, tracked)
	p.paired.markEnd(1, 3)
	if !pairedConnOf(t, p, 1).usedSinceEnd() {
		t.Fatal("worker 3's end marked worker 7's connection: its open test's use of it would be lost")
	}
	p.paired.markEnd(1, 7)
	if pairedConnOf(t, p, 1).usedSinceEnd() {
		t.Fatal("worker 7's end did not mark its own connection")
	}
	use(t, app, tracked)
	p.paired.markEnd(0, 0)
	if !pairedConnOf(t, p, 1).usedSinceEnd() {
		t.Fatal("an end that holds no connection numbered 1 marked it")
	}
	p.paired.markEnd(1, 0)
	if pairedConnOf(t, p, 1).usedSinceEnd() {
		t.Fatal("the end of a run of one test process did not mark its connection")
	}
}
