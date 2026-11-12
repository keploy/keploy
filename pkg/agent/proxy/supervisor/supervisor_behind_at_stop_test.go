package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// behindProducer stands in for the relay's tee: it hands chunks to its
// stream's channel one at a time, gives up on a reader that was closed (the
// tee's select on FakeConn.Done: what it still held is lost), and ends the
// stream once it has handed on all it was given, as the relay's teardown does.
type behindProducer struct {
	ch     chan fakeconn.Chunk
	fc     *fakeconn.FakeConn
	handed chan int
}

func newBehindProducer() *behindProducer {
	ch := make(chan fakeconn.Chunk) // unbuffered: the reader takes each one
	return &behindProducer{ch: ch, fc: fakeconn.New(ch, nil, nil), handed: make(chan int, 1)}
}

func (p *behindProducer) run(msgs []string) {
	n := 0
	defer func() { p.handed <- n }()
	for i, m := range msgs {
		select {
		case p.ch <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: uint32(i + 1), Bytes: []byte(m), ReadAt: time.Now()}:
			n++
		case <-p.fc.Done():
			return // the reader was closed under it: the rest is lost
		}
	}
	close(p.ch)
}

// cancelSeenCore closes seen once the supervisor has taken the outer ctx's
// cancel (it logs that first).
type cancelSeenCore struct {
	zapcore.Core
	once *sync.Once
	seen chan struct{}
}

func (c cancelSeenCore) Enabled(zapcore.Level) bool        { return true }
func (c cancelSeenCore) With([]zapcore.Field) zapcore.Core { return c }
func (c cancelSeenCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == "supervisor: outer ctx cancelled" {
		c.once.Do(func() { close(c.seen) })
	}
	return ce
}

// A parser behind its capture at the recording's stop still holds what the
// relay captured for it before the stop: the supervisor waits for it to read
// that, and aborts it on no clock. The parser is held (a hook) while the stop
// comes, and released only once the supervisor has taken the stop and a wait
// far past the grace it had has passed. Before, the supervisor aborted it 50 ms
// after the stop: its streams were closed under it, and every exchange the
// relay still held for it was lost.
func TestRun_AtTheStopAParserBehindItsCaptureReadsAllOfIt(t *testing.T) {
	t.Parallel()
	msgs := []string{"one", "two", "three"}
	client, dest := newBehindProducer(), newBehindProducer()
	go client.run(msgs)
	go dest.run(nil)

	seen := make(chan struct{})
	aborted := make(chan struct{})
	var abortOnce sync.Once
	s := New(Config{
		Logger:     zap.New(cancelSeenCore{Core: zapcore.NewNopCore(), once: &sync.Once{}, seen: seen}),
		HangBudget: time.Hour,
	})
	s.SessionOnAbort = func() {
		abortOnce.Do(func() { close(aborted) })
		_ = client.fc.Close()
		_ = dest.fc.Close()
	}
	sess := &Session{ClientStream: client.fc, DestStream: dest.fc}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	go func() {
		<-seen
		select {
		case <-aborted:
		case <-time.After(time.Second):
		}
		close(release)
	}()

	var read []string
	res := s.Run(ctx, func(context.Context, *Session) error {
		cancel() // the stop comes while the parser is held
		<-release
		for {
			c, err := sess.ClientStream.ReadChunk()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			read = append(read, string(c.Bytes))
		}
	}, sess)

	select {
	case <-aborted:
		t.Fatalf("the supervisor aborted the parser while it was behind its capture at the stop (status %s, read %q of %q)", res.Status, read, msgs)
	default:
	}
	if fmt.Sprint(read) != fmt.Sprint(msgs) {
		t.Fatalf("the parser read %q, want every exchange captured before the stop %q", read, msgs)
	}
	if res.Status != StatusCanceled || res.FallthroughToPassthrough {
		t.Fatalf("result %+v, want canceled, not retired: the parser returned on its own", res)
	}
	if n := <-client.handed; n != len(msgs) {
		t.Fatalf("the producer handed on %d of %d chunks", n, len(msgs))
	}
}

// Once the parser has read both its streams to their end, nothing of the
// capture is left for it: the supervisor aborts one that still has not
// returned (it waits on something else) then, and only then. One that has read
// only one stream to its end is aborted only once the stop's own deadline
// passes.
func TestRun_AtTheStopAbortsOnceBothStreamsAreReadToTheirEndOrTheDeadlinePasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		readDest bool
	}{
		{name: "both streams read to their end", readDest: true},
		{name: "one stream read to its end, then the deadline", readDest: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, dest := newBehindProducer(), newBehindProducer()
			go client.run([]string{"one"})
			go dest.run([]string{"answer"})

			aborted := make(chan struct{})
			var abortOnce sync.Once
			s := New(Config{Logger: zap.NewNop(), HangBudget: time.Hour})
			s.SessionOnAbort = func() { abortOnce.Do(func() { close(aborted) }) }
			deadline := make(chan struct{})
			s.StopDeadline = deadline
			sess := &Session{ClientStream: client.fc, DestStream: dest.fc}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parked := make(chan struct{})
			unpark := make(chan struct{})
			defer close(unpark)
			resc := make(chan Result, 1)
			go func() {
				resc <- s.Run(ctx, func(context.Context, *Session) error {
					cancel()
					streams := []*fakeconn.FakeConn{sess.ClientStream}
					if tc.readDest {
						streams = append(streams, sess.DestStream)
					}
					for _, fc := range streams {
						for {
							if _, err := fc.ReadChunk(); err != nil {
								break
							}
						}
					}
					close(parked)
					<-unpark // waits on something other than its streams
					return nil
				}, sess)
			}()
			<-parked
			if !tc.readDest {
				// Nothing ends the wait but the deadline: no abort comes
				// within a window far longer than an abort on the client
				// stream's end alone would take.
				select {
				case <-aborted:
					t.Fatal("the supervisor aborted a parser that had not read its server stream to its end, before the stop's deadline")
				case res := <-resc:
					t.Fatalf("Run returned %+v before the stop's deadline", res)
				case <-time.After(300 * time.Millisecond):
				}
				close(deadline)
			}
			var res Result
			select {
			case res = <-resc:
			case <-time.After(20 * time.Second):
				t.Fatal("Run did not return once the parser had read both its streams to their end, or the stop's deadline had passed")
			}
			select {
			case <-aborted:
			default:
				t.Fatalf("Run returned %+v without aborting the parser", res)
			}
			if res.Status != StatusCanceled || !res.FallthroughToPassthrough {
				t.Fatalf("result %+v, want canceled and retired", res)
			}
		})
	}
}

// TestRun_AParserThatHangsAfterTheStopIsAbortedAsHung pins that the stop's wait
// still honours the hang watchdog: a parser that, after the stop, holds a
// request awaiting its answer and makes no progress is aborted as hung, even
// with neither of its streams ended and no deadline to the stop. A wait that
// left the watchdog out held the connection until the agent's end.
func TestRun_AParserThatHangsAfterTheStopIsAbortedAsHung(t *testing.T) {
	t.Parallel()
	client, dest := newBehindProducer(), newBehindProducer() // never ended

	seen := make(chan struct{})
	aborted := make(chan struct{})
	var abortOnce sync.Once
	s := New(Config{
		Logger:     zap.New(cancelSeenCore{Core: zapcore.NewNopCore(), once: &sync.Once{}, seen: seen}),
		HangBudget: 50 * time.Millisecond,
	})
	s.SessionOnAbort = func() {
		abortOnce.Do(func() { close(aborted) })
		_ = client.fc.Close()
		_ = dest.fc.Close()
	}
	sess := &Session{ClientStream: client.fc, DestStream: dest.fc}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unpark := make(chan struct{})
	defer close(unpark)
	resc := make(chan Result, 1)
	go func() {
		resc <- s.Run(ctx, func(context.Context, *Session) error {
			cancel()
			// The supervisor has taken the stop: from here only its stop's
			// wait can see the hang.
			<-seen
			s.MarkPendingWork() // a request awaits its answer, and none comes
			<-unpark
			return nil
		}, sess)
	}()

	var res Result
	select {
	case res = <-resc:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not abort a parser that hung after the stop")
	}
	if res.Status != StatusHung || !res.FallthroughToPassthrough {
		t.Fatalf("result %+v, want hung and retired", res)
	}
	select {
	case <-aborted:
	default:
		t.Fatal("Run returned on the hang without aborting the parser")
	}
}

// TestRun_AtTheStopASessionWithoutBothStreamsWaitsForItsParser pins that a
// session with only one stream has nothing to say its parser read both to
// their end: the stop's wait ends on the parser's return, and the supervisor
// asks nothing of the stream the session lacks.
func TestRun_AtTheStopASessionWithoutBothStreamsWaitsForItsParser(t *testing.T) {
	t.Parallel()
	client := newBehindProducer()
	go client.run([]string{"one"})

	aborted := make(chan struct{})
	var abortOnce sync.Once
	s := New(Config{Logger: zap.NewNop(), HangBudget: time.Hour})
	s.SessionOnAbort = func() {
		abortOnce.Do(func() { close(aborted) })
		_ = client.fc.Close()
	}
	sess := &Session{ClientStream: client.fc}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var read int
	res, panicked := func() (res Result, panicked any) {
		defer func() { panicked = recover() }()
		return s.Run(ctx, func(context.Context, *Session) error {
			cancel()
			for {
				if _, err := sess.ClientStream.ReadChunk(); err != nil {
					return nil
				}
				read++
			}
		}, sess), nil
	}()
	if panicked != nil {
		t.Fatalf("Run panicked at the stop for a session without a server stream: %v", panicked)
	}
	if read != 1 || res.Status != StatusCanceled || res.FallthroughToPassthrough {
		t.Fatalf("read %d, result %+v: want the one exchange read and canceled, not retired", read, res)
	}
	select {
	case <-aborted:
		t.Fatal("the supervisor aborted a parser that returned on its own")
	default:
	}
}
