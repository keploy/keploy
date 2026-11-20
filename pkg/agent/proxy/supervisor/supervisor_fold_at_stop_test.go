package supervisor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// readBoth reads sess's two streams to their end, the client's first.
func readBoth(sess *Session) {
	for _, fc := range []*fakeconn.FakeConn{sess.ClientStream, sess.DestStream} {
		for {
			if _, err := fc.ReadChunk(); err != nil {
				break
			}
		}
	}
}

// A parser that has read both its streams to their end at the recording's
// stop and is still recording what it read hands over every record: the
// supervisor waits for it while it makes progress, on the process's own
// clock, past the fold window, and takes its return. Before, the supervisor
// aborted it as soon as it had read both its streams to their end, and Run
// returned with the connection retired while the parser was still folding:
// the records it had not handed over by then were lost.
func TestRun_AtTheStopAParserFoldingWhatItReadHandsOverEveryRecord(t *testing.T) {
	t.Parallel()
	const n = 150
	const gap = 10 * time.Millisecond // n gaps run well past the fold window
	client, dest := newBehindProducer(), newBehindProducer()
	go client.run([]string{"request"})
	go dest.run([]string{"answer"})

	aborted := make(chan struct{})
	var abortOnce sync.Once
	s := New(Config{Logger: zap.NewNop(), HangBudget: time.Hour})
	s.SessionOnAbort = func() { abortOnce.Do(func() { close(aborted) }) }
	mocks := make(chan *models.Mock, n)
	sess := &Session{ClientStream: client.fc, DestStream: dest.fc, Mocks: mocks}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := s.Run(ctx, func(context.Context, *Session) error {
		cancel() // the stop
		readBoth(sess)
		for i := 0; i < n; i++ {
			time.Sleep(gap)
			if err := sess.EmitMockOnShutdown(&models.Mock{Name: "folded"}); err != nil {
				return err
			}
		}
		return nil
	}, sess)
	handed := len(mocks)

	select {
	case <-aborted:
		t.Fatalf("the supervisor aborted a parser folding what it had read (status %s, %d of %d records handed over as Run returned)", res.Status, handed, n)
	default:
	}
	if handed != n {
		t.Fatalf("%d of %d records handed over as Run returned", handed, n)
	}
	if res.Status != StatusCanceled || res.FallthroughToPassthrough || res.Err != nil {
		t.Fatalf("result %+v, want canceled with the parser's own return, not retired", res)
	}
}

// foldRig runs a parser under a supervisor on a ManualClock. At the stop the
// parser reads its client stream to its end, runs beforeEnd, reads its server
// stream to its end, then runs each op the test hands it (do), and returns
// once the test closes ops.
type foldRig struct {
	t       *testing.T
	clk     *stall.ManualClock
	sess    *Session
	ops     chan func()
	did     chan struct{}
	resc    chan Result
	aborted chan struct{}
}

func newFoldRig(t *testing.T, window time.Duration, beforeEnd func(*Session)) *foldRig {
	t.Helper()
	client, dest := newBehindProducer(), newBehindProducer()
	go client.run([]string{"request"})
	go dest.run([]string{"answer"})
	r := &foldRig{
		t:       t,
		clk:     &stall.ManualClock{},
		ops:     make(chan func()),
		did:     make(chan struct{}),
		resc:    make(chan Result, 1),
		aborted: make(chan struct{}),
	}
	var abortOnce sync.Once
	s := New(Config{Logger: zap.NewNop(), HangBudget: time.Hour, FoldWindow: window, Clock: r.clk})
	s.SessionOnAbort = func() { abortOnce.Do(func() { close(r.aborted) }) }
	r.sess = &Session{ClientStream: client.fc, DestStream: dest.fc, Mocks: make(chan *models.Mock, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		r.resc <- s.Run(ctx, func(context.Context, *Session) error {
			cancel() // the stop
			for {
				if _, err := r.sess.ClientStream.ReadChunk(); err != nil {
					break
				}
			}
			beforeEnd(r.sess)
			for {
				if _, err := r.sess.DestStream.ReadChunk(); err != nil {
					break
				}
			}
			for op := range r.ops {
				op()
				r.did <- struct{}{}
			}
			return nil
		}, r.sess)
	}()
	return r
}

// watching waits until both the watchdog and the fold watch have armed their
// next look, and fails if Run returned instead.
func (r *foldRig) watching(while string) {
	r.t.Helper()
	bound := time.Now().Add(time.Minute) // only turns a hang into a failure
	for r.clk.Pending() != 2 {
		select {
		case res := <-r.resc:
			r.t.Fatalf("Run returned %+v %s", res, while)
		default:
		}
		if time.Now().After(bound) {
			r.t.Fatalf("the fold watch never armed its next look %s", while)
		}
		time.Sleep(time.Millisecond)
	}
}

// do has the parser run op, and waits until it has.
func (r *foldRig) do(op func()) {
	r.ops <- op
	<-r.did
}

// ended waits for Run's result.
func (r *foldRig) ended(what string) Result {
	r.t.Helper()
	select {
	case res := <-r.resc:
		return res
	case <-time.After(time.Minute): // only turns a hang into a failure
		r.t.Fatalf("Run did not return %s", what)
		return Result{}
	}
}

// Each kind of progress a parser makes on what it read keeps it from being
// aborted once it has read both its streams to their end: a mock handed over
// (EmitMock refused as the stop has come, EmitMockOnShutdown), an exchange or
// a part of one reported left out, a mock held, bytes folded. Each comes three
// quarters of the fold window after the one before, six times over: the
// parser runs four and a half windows past its streams' end, and returns on
// its own.
func TestRun_AtTheStopEachKindOfProgressKeepsAParserFoldingWhatItRead(t *testing.T) {
	t.Parallel()
	const window = time.Second
	for _, tc := range []struct {
		name string
		op   func(*Session)
	}{
		{"EmitMock", func(s *Session) { _ = s.EmitMock(&models.Mock{}) }},
		{"EmitMockOnShutdown", func(s *Session) { _ = s.EmitMockOnShutdown(&models.Mock{}) }},
		{"ReportLeftOut", func(s *Session) { s.ReportLeftOut(&models.Mock{}, "folded at the stop") }},
		{"ReportLeftOutPart", func(s *Session) { s.ReportLeftOutPart(time.Time{}, time.Time{}) }},
		{"MockHeld", func(s *Session) { s.MockHeld() }},
		{"Folded", func(s *Session) { s.Folded() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFoldRig(t, window, func(*Session) {})
			r.watching("once the parser had read both its streams to their end")
			for i := 0; i < 6; i++ {
				r.do(func() { tc.op(r.sess) })
				r.clk.Advance(window * 3 / 4)
				r.watching(fmt.Sprintf("while the parser made progress (%s) every three quarters of its fold window, look %d", tc.name, i+1))
			}
			close(r.ops)
			res := r.ended("once the parser returned")
			select {
			case <-r.aborted:
				t.Fatalf("the supervisor aborted a parser making progress (%s)", tc.name)
			default:
			}
			if res.Status != StatusCanceled || res.FallthroughToPassthrough || res.Err != nil {
				t.Fatalf("result %+v, want canceled with the parser's own return, not retired", res)
			}
			if n := r.clk.Pending(); n != 0 {
				t.Fatalf("%d timers left armed once Run returned", n)
			}
		})
	}
}

// A parser that has read both its streams to their end and then makes no
// progress is aborted once its fold window has passed with none, and not
// before: a fold window of the process's run time after the watch began, and
// no later than the look after it. Progress it made before it read its server
// stream to its end does not count.
func TestRun_AtTheStopAParserIdleOnceItReadItAllIsAbortedOnceItsWindowPasses(t *testing.T) {
	t.Parallel()
	const window = time.Second
	r := newFoldRig(t, window, func(s *Session) { s.Folded() })
	r.watching("once the parser had read both its streams to their end")
	r.clk.Advance(window)
	r.watching("a fold window into its idleness, not past it")
	select {
	case <-r.aborted:
		t.Fatal("the supervisor aborted the parser before its fold window had passed")
	default:
	}
	r.clk.Advance(time.Nanosecond)
	res := r.ended("once the parser's fold window had passed with no progress")
	select {
	case <-r.aborted:
	default:
		t.Fatalf("Run returned %+v without aborting the parser", res)
	}
	if res.Status != StatusCanceled || !res.FallthroughToPassthrough {
		t.Fatalf("result %+v, want canceled and retired", res)
	}
	close(r.ops)
}

// A parser still making progress on what it read when the stop's deadline
// passes is aborted at the deadline, and a WARN says the records it had not
// handed over then are lost, naming its connection.
func TestRun_AtTheStopDeadlineCutsAParserStillFoldingAndSaysSo(t *testing.T) {
	t.Parallel()
	client, dest := newBehindProducer(), newBehindProducer()
	go client.run([]string{"request"})
	go dest.run([]string{"answer"})
	aborted := make(chan struct{})
	var abortOnce sync.Once
	core, logs := observer.New(zapcore.DebugLevel)
	s := New(Config{Logger: zap.New(core), HangBudget: time.Hour, FoldWindow: time.Hour})
	s.SessionOnAbort = func() { abortOnce.Do(func() { close(aborted) }) }
	deadline := make(chan struct{})
	s.StopDeadline = deadline
	sess := &Session{ClientStream: client.fc, DestStream: dest.fc, ClientConnID: "41", Mocks: make(chan *models.Mock, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	folding := make(chan struct{})
	unpark := make(chan struct{})
	defer close(unpark)
	resc := make(chan Result, 1)
	go func() {
		resc <- s.Run(ctx, func(context.Context, *Session) error {
			cancel()
			readBoth(sess)
			close(folding)
			for {
				select {
				case <-unpark:
					return nil
				case <-time.After(time.Millisecond):
					sess.Folded()
				}
			}
		}, sess)
	}()
	<-folding
	close(deadline)
	var res Result
	select {
	case res = <-resc:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("Run did not return once the stop's deadline had passed")
	}
	select {
	case <-aborted:
	default:
		t.Fatalf("Run returned %+v without aborting the parser", res)
	}
	if res.Status != StatusCanceled || !res.FallthroughToPassthrough {
		t.Fatalf("result %+v, want canceled and retired", res)
	}
	warns := logs.FilterMessage(FoldCutWarnMsg).AllUntimed()
	if len(warns) != 1 || warns[0].Level != zapcore.WarnLevel || warns[0].ContextMap()["clientConnID"] != "41" {
		t.Fatalf("WARNs of the cut %+v, want one, naming the connection", warns)
	}
}

// The process clock moves only while held: the fold watch holds it while it
// watches (the watchdog, with nothing unread, does not), and lets it go once
// Run returns.
// Not parallel: it swaps holdProcessClock, which only New reads.
func TestRun_TheFoldWatchHoldsTheProcessClockWhileItWatches(t *testing.T) {
	var holds, releases atomic.Int32
	saved := holdProcessClock
	holdProcessClock = func() func() {
		holds.Add(1)
		return func() { releases.Add(1) }
	}
	t.Cleanup(func() { holdProcessClock = saved })
	client, dest := newBehindProducer(), newBehindProducer()
	go client.run([]string{"request"})
	go dest.run([]string{"answer"})
	// An hour's budget: the watchdog's next look, which would hold the
	// clock for what the parser had not read yet, comes after the test.
	s := New(Config{Logger: zap.NewNop(), HangBudget: time.Hour})
	sess := &Session{ClientStream: client.fc, DestStream: dest.fc}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := s.Run(ctx, func(context.Context, *Session) error {
		cancel()
		readBoth(sess)
		bound := time.Now().Add(time.Minute) // only turns a hang into a failure
		for holds.Load() != 1 {
			if time.Now().After(bound) {
				return fmt.Errorf("the fold watch did not hold the process clock (%d holds)", holds.Load())
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}, sess)
	if res.Status != StatusCanceled || res.Err != nil || res.FallthroughToPassthrough {
		t.Fatalf("result %+v", res)
	}
	if h, r := holds.Load(), releases.Load(); h != 1 || r != 1 {
		t.Fatalf("%d holds and %d releases of the process clock once Run returned, want 1 and 1", h, r)
	}
}

// New gives a Config without a fold window the default one.
func TestNew_DefaultsTheFoldWindow(t *testing.T) {
	t.Parallel()
	s := New(Config{})
	defer s.Close()
	if s.cfg.FoldWindow != defaultFoldWindow {
		t.Fatalf("fold window %s, want %s", s.cfg.FoldWindow, defaultFoldWindow)
	}
}

// The hand-overs that note progress stay nil-safe.
func TestSession_ProgressOnANilSessionIsANoOp(t *testing.T) {
	t.Parallel()
	var s *Session
	for name, op := range map[string]func(){
		"Folded":            s.Folded,
		"MockHeld":          s.MockHeld,
		"ReportLeftOutPart": func() { s.ReportLeftOutPart(time.Now(), time.Now()) },
		"ReportLeftOut":     func() { s.ReportLeftOut(nil, "x") },
		"EmitMock":          func() { _ = s.EmitMock(&models.Mock{}) },
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s on a nil Session panicked: %v", name, r)
				}
			}()
			op()
		}()
	}
}
