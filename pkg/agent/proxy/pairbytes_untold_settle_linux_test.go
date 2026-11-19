//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// A close keploy cannot account for leaves NOT JUDGED only the tests that
// used the connection: a test that never used it is untouched, whatever test
// is open at the close. The rows below have the test open at the close never
// use the connection, and want it uncharged; the control has it use it.

// The ending test's last call is on its way as its end begins: the bytes wait
// marks the connection, then the rest of the end's settle has the handler
// take that call and answer it. Every byte moved on the connection is the
// ending test's: once the settle is done (PairedBytesSettled) the next test,
// which never uses the connection, is not charged with its untold close.
func TestTheCallsAnEndsSettleAnswersAreTheEndingTests(t *testing.T) {
	p, app, tracked := untoldPair(t)
	use(t, app, tracked) // TestStock's call, answered
	if _, err := app.Write([]byte("bye..")); err != nil {
		t.Fatal(err)
	}
	waitHeld(t, pairedConnOf(t, p, 1), 5) // on its way as TestStock's end begins
	endWait(t, p, "TestStock")            // the bytes wait: it marks, and answers for the bytes received
	// The rest of TestStock's settle: its handler takes the call and answers it.
	if _, err := io.ReadFull(tracked, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	p.PairedBytesSettled(0) // TestStock's settle is done
	// TestClock opens, and never uses the connection. The proxy closes it, untold.
	emptyDiagFree(t)
	failLookupWritten(t)
	_ = tracked.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 0 {
		t.Errorf("the test open at the close, which never used the connection, is charged %d untold close(s), want 0", l)
	}
}

// What the proxy moves on a connection after an end's settle is done is the
// next test's: a settle done marks again only the connections its end's wait
// marked, of its own worker, once.
func TestAMoveAfterAnEndsSettleIsTheNextTests(t *testing.T) {
	t.Run("a move after the settle", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		endWait(t, p, "TestStock")
		p.PairedBytesSettled(0)
		use(t, app, tracked) // TestClock's call
		if !pairedConnOf(t, p, 1).usedSinceEnd() {
			t.Fatal("a call after the end's settle was done is not the open test's use")
		}
	})
	t.Run("a settle done twice", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		endWait(t, p, "TestStock")
		p.PairedBytesSettled(0)
		use(t, app, tracked)
		p.PairedBytesSettled(0) // no end's wait marked it again
		if !pairedConnOf(t, p, 1).usedSinceEnd() {
			t.Fatal("a settle no end's wait began took the open test's use")
		}
	})
	t.Run("another worker's settle", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		pc := pairedConnOf(t, p, 1)
		pc.instance = 1
		use(t, app, tracked)
		p.paired.markEnd(1, 1)
		use(t, app, tracked)
		p.PairedBytesSettled(2)
		if !pc.usedSinceEnd() {
			t.Fatal("worker 2's settle took worker 1's connection's use")
		}
		p.PairedBytesSettled(1)
		if pc.usedSinceEnd() {
			t.Fatal("worker 1's settle did not mark its own connection")
		}
	})
}

// stallAfterWrite is a socket whose write returns only once released, after
// its bytes went out: the proxy's goroutine descheduled between the write's
// syscall and what follows it in trackedConn.Write.
type stallAfterWrite struct {
	net.Conn
	wrote, release chan struct{}
}

func (s *stallAfterWrite) Write(b []byte) (int, error) {
	n, err := s.Conn.Write(b)
	close(s.wrote)
	<-s.release
	return n, err
}

// The m5-miss shape: the proxy reads the call and writes keploy's answer. The
// answer reaches the application, which reads it and ends its test, before
// the proxy's goroutine runs again after the write's syscall. The write was
// counted as it began, so the end's mark holds it, and the next test, which
// never uses the connection, is not charged with its untold close.
func TestAnAnswerIsCountedBeforeItGoesOut(t *testing.T) {
	p, app, tracked := untoldPair(t)
	if _, err := app.Write([]byte("GET /")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(tracked, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	st := &stallAfterWrite{Conn: tracked.Conn, wrote: make(chan struct{}), release: make(chan struct{})}
	tracked.Conn = st
	done := make(chan error, 1)
	go func() {
		_, err := tracked.Write([]byte("502"))
		done <- err
	}()
	<-st.wrote
	if _, err := io.ReadFull(app, make([]byte, 3)); err != nil { // the application has keploy's answer
		t.Fatal(err)
	}
	endWait(t, p, "TestStock") // the application answered its test; the test ends
	close(st.release)          // the proxy's goroutine runs again
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tracked.Conn = st.Conn
	emptyDiagFree(t)
	failLookupWritten(t)
	_ = tracked.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 0 {
		t.Errorf("the test open at the close, which never used the connection, is charged %d untold close(s), want 0", l)
	}
}

// The control: the test open at the close used the connection itself, and is
// charged.
func TestAnUntoldCloseChargesTheOpenTestThatUsedIt(t *testing.T) {
	p, app, tracked := untoldPair(t)
	use(t, app, tracked)
	endWait(t, p, "TestStock")
	p.PairedBytesSettled(0)
	use(t, app, tracked)
	emptyDiagFree(t)
	failLookupWritten(t)
	_ = tracked.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 1 {
		t.Errorf("%d untold closes charged to the open test that used it, want 1", l)
	}
}

// A close's read of what the socket held that fails, after the close read
// what the application had written (wroteAtClose), every byte of it owned by
// a test whose end was cut short: that test owns the untold close, and the
// open test, though the proxy wrote to the connection in it, is not charged.
func TestAnUnreadableCloseOfBytesAllOwnedByACutEndSparesTheOpenTest(t *testing.T) {
	p, app, tracked := untoldPair(t)
	use(t, app, tracked)
	p.paired.markEnd(1, 0)
	pc := pairedConnOf(t, p, 1)
	pc.cutShort("TestStock", 5)
	if _, err := tracked.Write([]byte("push")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	was := readUnreadAtClose
	readUnreadAtClose = func(*net.TCPConn) (int, error) { return 0, errors.New("bad file descriptor") }
	t.Cleanup(func() { readUnreadAtClose = was })
	emptyDiagFree(t)
	_ = tracked.Close()
	if w := pc.wroteAtClose.Load(); w == nil || w.want != 5 {
		t.Fatalf("premise: the close read what the application had written (%+v), want 5", w)
	}
	owned := p.TakeOwnedLosses()
	if len(owned) != 1 || owned[0].Test != "TestStock" || owned[0].Loss.Kind != models.CaptureLossAppBytesUntold {
		t.Fatalf("owned %+v, want one untold close of TestStock's", owned)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 0 {
		t.Errorf("%d untold closes charged to the open test, want none: every byte the application wrote is TestStock's", l)
	}
}

// A close that waited for the application's FIN and did not see it
// (closeUnseen) charges the open test though the proxy moved no bytes on the
// connection in it: bytes it wrote may be on their way unseen. One that
// cannot tell for another reason (closeUnknown) charges it only for a use the
// proxy saw, and says a close charged to no test once, at info level.
func TestAnUntoldCloseThatDidNotSeeTheFINChargesTheOpenTest(t *testing.T) {
	for _, unseen := range []bool{false, true} {
		name := "a close that cannot tell"
		if unseen {
			name = "a close that waited for the FIN and did not see it"
		}
		t.Run(name, func(t *testing.T) {
			p, app, tracked := untoldPair(t)
			lines := closeUnknownLines(p)
			use(t, app, tracked)
			endWait(t, p, "TestStock")
			p.PairedBytesSettled(0)
			pc := pairedConnOf(t, p, 1)
			for range 2 { // a second read of the same close says nothing more
				if unseen {
					p.closeUnseen(pc, errors.New("no FIN"))
				} else {
					p.closeUnknown(pc, errors.New("netlink: no buffer space"))
				}
			}
			want := uint64(0)
			level := zap.InfoLevel
			if unseen {
				want, level = 1, zap.WarnLevel
			}
			if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != want {
				t.Errorf("%d untold closes charged to the open test, want %d", l, want)
			}
			said := lines.FilterMessageSnippet("keploy cannot tell what the application had written").All()
			if len(said) != 1 || said[0].Level != level {
				t.Errorf("lines %+v, want one at %v", said, level)
			}
		})
	}
}

// A close of a test worker's connection charges the open test of that worker
// alone (chargeOpen): an owned loss naming the worker, in no count, so the
// agent never charges it to another worker's open test. A connection of no
// worker's is the run's count, as before, for a judged loss as for an untold
// close.
func TestACloseChargesTheOpenTestOfItsConnectionsWorker(t *testing.T) {
	for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
		for _, worker := range []uint32{0, 3} {
			t.Run(fmt.Sprintf("%s of worker %d", kind, worker), func(t *testing.T) {
				p, app, tracked := untoldPair(t)
				pc := pairedConnOf(t, p, 1)
				pc.instance = worker
				use(t, app, tracked)
				if kind == models.CaptureLossAppBytesUntold {
					p.closeUnknown(pc, errors.New("netlink: no buffer space"))
				} else {
					p.pairLost(pc, 1, 5, unreadLost)
				}
				owned := p.TakeOwnedLosses()
				if worker == 0 {
					if len(owned) != 0 || lossesOf(p, kind) != 1 {
						t.Fatalf("owned %+v, %d counted; want none owned and one counted", owned, lossesOf(p, kind))
					}
					return
				}
				want := agent.OwnedLoss{Worker: worker, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from " + pc.app.String()}}
				if len(owned) != 1 || !reflect.DeepEqual(owned[0], want) {
					t.Fatalf("owned %+v, want %+v", owned, want)
				}
				if l := lossesOf(p, kind); l != 0 {
					t.Errorf("%d counted, want none: a count charges every open test", l)
				}
			})
		}
	}
}
