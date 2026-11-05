package supervisor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// capturedStream is a stream whose capture is already in the FakeConn's
// channel, n chunks, as the relay's tee leaves it for a parser behind it. It
// is not ended: the test ends it (close) where it needs to.
func capturedStream(dir fakeconn.Direction, n int) (chan fakeconn.Chunk, *fakeconn.FakeConn) {
	ch := make(chan fakeconn.Chunk, n)
	for i := 0; i < n; i++ {
		ch <- fakeconn.Chunk{Dir: dir, ConnSeq: uint32(i + 1), Bytes: []byte("exchange"), ReadAt: time.Now()}
	}
	return ch, fakeconn.New(ch, nil, nil)
}

// stopSeenLogger closes seen once the supervisor has taken the outer ctx's
// cancel.
func stopSeenLogger() (*zap.Logger, chan struct{}) {
	seen := make(chan struct{})
	return zap.New(cancelSeenCore{Core: zapcore.NewNopCore(), once: &sync.Once{}, seen: seen}), seen
}

// At the stop, the watchdog measures what the parser still has to read, not
// the pending flag a recorded mock clears. The parser reads its first
// exchange, records its mock (the pending flag cleared), and stalls outside a
// read as the stop comes, two more exchanges captured for it unread. It is
// hung: the watchdog fires, and Run retires it, so that what it never read is
// left out and reported (what it hands over from here is left out: retire),
// not waited on for as long as it stalls. Before, the recorded mock had
// disarmed the watchdog, and the stop waited on the parser silently: in proxy
// mode the stop has no deadline of its own.
func TestRun_AtTheStopAParserStalledWithCapturedBytesUnreadIsHung(t *testing.T) {
	t.Parallel()
	_, client := capturedStream(fakeconn.FromClient, 3)
	_, dest := capturedStream(fakeconn.FromDest, 3)
	logger, seen := stopSeenLogger()
	s := New(Config{Logger: logger, HangBudget: 50 * time.Millisecond})
	aborted := make(chan struct{})
	var abortOnce sync.Once
	s.SessionOnAbort = func() {
		abortOnce.Do(func() { close(aborted) })
		_ = client.Close()
		_ = dest.Close()
	}
	sess := &Session{ClientStream: client, DestStream: dest}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unpark := make(chan struct{})
	defer close(unpark)
	resc := make(chan Result, 1)
	go func() {
		resc <- s.Run(ctx, func(context.Context, *Session) error {
			if _, err := sess.ClientStream.ReadChunk(); err != nil {
				return err
			}
			if _, err := sess.DestStream.ReadChunk(); err != nil {
				return err
			}
			s.MarkPendingWork()
			s.ClearPendingWork() // its mock is recorded
			cancel()
			<-seen
			<-unpark // stalls outside a read, two exchanges unread
			return nil
		}, sess)
	}()

	var res Result
	select {
	case res = <-resc:
	case <-time.After(20 * time.Second):
		t.Fatal("the stop waited on a parser stalled with captured bytes unread: the watchdog never fired")
	}
	if res.Status != StatusHung || !res.FallthroughToPassthrough {
		t.Fatalf("result %+v, want hung and retired", res)
	}
	select {
	case <-aborted:
	default:
		t.Fatal("Run returned on the hang without aborting the parser")
	}
	if reason, ok := sess.TakeMockIncomplete(); !ok || !strings.HasPrefix(reason, "parser_hung") {
		t.Fatalf("the session's mark is %q (%v), want the hang's: what the parser hands over from here is left out and reported", reason, ok)
	}
}

// From the stop, a parser that reads is not hung, however long its capture
// takes it to read, and neither is one blocked in a read with nothing handed
// to it: it waits on the relay, which ends the stream once it has handed on
// what it captured. Only a parser that neither reads nor waits in a read, with
// bytes captured unread, is hung. The clock is the test's: each look of the
// watchdog comes as the test moves it, three quarters of the budget a time.
func TestRun_AtTheStopTheWatchdogMeasuresTheParsersReads(t *testing.T) {
	t.Parallel()
	const budget = 40 * time.Millisecond
	clk := &stall.ManualClock{}
	// The process could run for many budgets before the stop: what the
	// watchdog measures from the stop starts at the stop, not at the
	// clock's zero, so the looks below see under a budget each.
	clk.Advance(10 * budget)
	clientCh, _ := capturedStream(fakeconn.FromClient, 5)
	clientCh = func() chan fakeconn.Chunk { // room for one more request
		c := make(chan fakeconn.Chunk, 6)
		for len(clientCh) > 0 {
			c <- <-clientCh
		}
		return c
	}()
	client := fakeconn.New(clientCh, nil, nil)
	_, dest := capturedStream(fakeconn.FromDest, 2)
	logger, seen := stopSeenLogger()
	s := New(Config{Logger: logger, HangBudget: budget, Clock: clk})
	s.SessionOnAbort = func() {
		_ = client.Close()
		_ = dest.Close()
	}
	sess := &Session{ClientStream: client, DestStream: dest}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := make(chan func())
	did := make(chan struct{})
	unpark := make(chan struct{})
	defer close(unpark)
	resc := make(chan Result, 1)
	go func() {
		resc <- s.Run(ctx, func(context.Context, *Session) error {
			if _, err := sess.ClientStream.ReadChunk(); err != nil {
				return err
			}
			if _, err := sess.DestStream.ReadChunk(); err != nil {
				return err
			}
			s.MarkPendingWork()
			s.ClearPendingWork() // its mock is recorded
			cancel()
			<-seen
			for {
				select {
				case op := <-ops:
					op()
					did <- struct{}{}
				case <-unpark:
					return nil
				}
			}
		}, sess)
	}()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for s.stop.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Run never began its stop wait")
		}
		time.Sleep(time.Millisecond)
	}
	// armed waits until the watchdog has armed its next look, and fails at
	// once if the parser was ended instead: a parser retired arms none.
	armed := func(while string) {
		t.Helper()
		bound := time.Now().Add(time.Minute) // only turns a hang into a failure
		for clk.Pending() == 0 {
			select {
			case r := <-resc:
				t.Fatalf("the parser was ended %s %s", r.Status, while)
			default:
			}
			if time.Now().After(bound) {
				t.Fatal("the watchdog never armed its next look")
			}
			time.Sleep(time.Millisecond)
		}
	}
	look := func(while string) {
		t.Helper()
		armed(while)
		clk.Advance(budget * 3 / 4)
		armed(while) // the look is done, and armed the next
		select {
		case r := <-resc:
			t.Fatalf("the parser was ended %s %s", r.Status, while)
		default:
		}
	}
	do := func(op func()) {
		ops <- op
		<-did
	}

	look("before its first read after the stop, three quarters of its budget in")
	for i := 0; i < 4; i++ { // the client's capture, read to its last request
		do(func() { _, _ = sess.ClientStream.ReadChunk() })
		look("while it reads its capture, one exchange each three quarters of its budget")
	}
	// Blocked in a read of the client's stream, which holds nothing more and
	// is not ended, with an answer of the server's unread.
	blocked := func() {
		t.Helper()
		ops <- func() { _, _ = sess.ClientStream.ReadChunk() }
		for w, _ := client.Waiting(); !w; w, _ = client.Waiting() {
			if time.Now().After(deadline) {
				t.Fatal("the parser never blocked in its read of the client's stream")
			}
			time.Sleep(time.Millisecond)
		}
	}
	blocked()
	for i := 0; i < 3; i++ {
		look("while it waits in a read for the relay")
	}
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: 6, Bytes: []byte("request"), ReadAt: time.Now()}
	<-did
	look("once it read the request it waited for")
	// Blocked in a read again, which the relay's end of the stream ends, all
	// between two looks: that it waited in a read is all the next look sees.
	blocked()
	close(clientCh)
	<-did
	look("once its wait for the relay ended between two looks")
	// Now it stalls outside a read, the server's last answer unread.
	look("three quarters of its budget into its stall")
	waitPending(t, clk)
	clk.Advance(budget * 3 / 4)
	select {
	case r := <-resc:
		if r.Status != StatusHung || !r.FallthroughToPassthrough {
			t.Fatalf("result %+v, want hung and retired", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a parser stalled for its budget with captured bytes unread was not declared hung")
	}
}

// A session without both streams keeps the pending flag at the stop: nothing
// says what its parser still has to read, and a request pending with no
// answer coming is a hang, as before the stop.
func TestRun_AtTheStopASessionWithoutStreamsKeepsItsPendingWatchdog(t *testing.T) {
	t.Parallel()
	logger, seen := stopSeenLogger()
	s := New(Config{Logger: logger, HangBudget: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unpark := make(chan struct{})
	defer close(unpark)
	resc := make(chan Result, 1)
	go func() {
		resc <- s.Run(ctx, func(context.Context, *Session) error {
			cancel()
			<-seen
			s.MarkPendingWork() // a request awaits its answer, and none comes
			<-unpark
			return nil
		}, &Session{})
	}()
	select {
	case res := <-resc:
		if res.Status != StatusHung {
			t.Fatalf("result %+v, want hung", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stop waited on a parser of a session without streams that hung with a request pending")
	}
}

// From the stop, the watchdog is armed while captured bytes are unread, a
// suspended one's too (its parser waits in a read for what the network holds,
// and a parser blocked in a read is not hung), and it is disarmed and judges
// no hang once the parser has read both its streams to their end.
func TestTheStopsWatchdogIsArmedWhileCapturedBytesAreUnread(t *testing.T) {
	t.Parallel()
	const budget = 40 * time.Millisecond
	clk := &stall.ManualClock{}
	s := New(Config{Logger: zap.NewNop(), HangBudget: budget, Clock: clk})
	defer s.Close()
	clientCh, client := capturedStream(fakeconn.FromClient, 2)
	destCh, dest := capturedStream(fakeconn.FromDest, 2)
	_, _ = client.ReadChunk() // what it read before the stop is no read after it
	_, _ = dest.ReadChunk()
	s.SuspendWatchdog()
	s.watchTheStop(&Session{ClientStream: client, DestStream: dest})
	if !s.armed() {
		t.Fatal("a suspended watchdog is not armed at the stop with captured bytes unread")
	}
	clk.Advance(2 * budget)
	if !s.hangExceeded() {
		t.Fatal("a parser that read nothing for twice its budget, captured bytes unread, is not hung")
	}
	for _, f := range []struct {
		ch chan fakeconn.Chunk
		fc *fakeconn.FakeConn
	}{{clientCh, client}, {destCh, dest}} {
		close(f.ch)
		for {
			if _, err := f.fc.ReadChunk(); err != nil {
				break
			}
		}
	}
	_ = s.hangExceeded() // sees the reads
	clk.Advance(2 * budget)
	if s.armed() {
		t.Error("the watchdog is armed once both streams are read to their end")
	}
	if s.hangExceeded() {
		t.Error("a parser that read both streams to their end is judged hung")
	}
}
