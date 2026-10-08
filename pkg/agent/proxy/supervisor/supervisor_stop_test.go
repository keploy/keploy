package supervisor

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// stopEvents records, in order, the session's stop (OnStop), the abort
// (SessionOnAbort), the log line that says why the parser was retired, and the
// panic report.
type stopEvents struct {
	mu     sync.Mutex
	events []string
	// cancelSeen is closed once the supervisor has taken the outer ctx's
	// cancel: it logs that first.
	cancelSeen chan struct{}
	cancelOnce sync.Once
}

func (e *stopEvents) add(ev string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *stopEvents) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// retirementLogCore records the supervisor's lines that say why it retired a
// parser.
type retirementLogCore struct {
	zapcore.Core
	ev *stopEvents
}

func (c retirementLogCore) Enabled(zapcore.Level) bool        { return true }
func (c retirementLogCore) With([]zapcore.Field) zapcore.Core { return c }
func (c retirementLogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	switch e.Message {
	case "parser panicked", "parser hang detected; aborting":
		c.ev.add("log")
	case "supervisor: outer ctx cancelled":
		c.ev.cancelOnce.Do(func() { close(c.ev.cancelSeen) })
	}
	return ce
}

// A parser that panics or returns an error is gone as it does: what its
// connection carries from then on is captured for no one. So the supervisor
// stamps the session's stop where it learns it, in the parser's goroutine, and
// aborts (SessionOnAbort pauses the capture) before it logs why or reports a
// panic. A hung parser is aborted before the hang is logged. Before, a panic
// was logged, with its stack, and reported before the abort, and nothing
// stamped the stop until the abort: what the connection carried while the
// stack was written was in no span.
func TestRun_StopsAndAbortsBeforeItLogsWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cancel bool // the outer ctx is cancelled first
		hang   bool
		fn     func(*stopEvents) ParserFunc
		want   []string
	}{
		{
			name: "a panic",
			fn: func(*stopEvents) ParserFunc {
				return func(context.Context, *Session) error { panic("boom") }
			},
			want: []string{"stop", "abort", "log", "report"},
		},
		{
			name: "an error",
			fn: func(*stopEvents) ParserFunc {
				return func(context.Context, *Session) error { return errors.New("a parser error") }
			},
			want: []string{"stop", "abort"},
		},
		{
			name:   "a panic as the recording stops",
			cancel: true,
			// It panics once the supervisor has taken the cancel, within its
			// grace for the parser's return (but see graceRanOut below).
			fn: func(ev *stopEvents) ParserFunc {
				return func(context.Context, *Session) error {
					<-ev.cancelSeen
					panic("boom")
				}
			},
			want: []string{"stop", "abort", "report"},
		},
		{
			name: "a hang",
			hang: true,
			fn: func(*stopEvents) ParserFunc {
				return func(ctx context.Context, _ *Session) error {
					<-ctx.Done()
					return ctx.Err()
				}
			},
			// The hung parser returns once it is cancelled, after the
			// supervisor has given up on it: the stop it stamps then may
			// come after Run returns.
			want: []string{"abort", "log"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := &stopEvents{cancelSeen: make(chan struct{})}
			s := New(Config{
				Logger:        zap.New(retirementLogCore{Core: zapcore.NewNopCore(), ev: ev}),
				HangBudget:    20 * time.Millisecond,
				PanicReporter: func(any, []byte) { ev.add("report") },
			})
			s.SessionOnAbort = func() { ev.add("abort") }
			sess := &Session{OnStop: func(time.Time) { ev.add("stop") }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if tc.hang {
				s.MarkPendingWork()
			}
			res := s.Run(ctx, tc.fn(ev), sess)
			if !res.FallthroughToPassthrough {
				t.Fatalf("status %s: the parser was not retired", res.Status)
			}
			got := ev.get()
			if tc.cancel && res.Status == StatusCanceled {
				// graceRanOut: on a starved runner the parser can take longer
				// than the supervisor's grace to panic after the cancel. Then
				// it is aborted while still running, and its panic, later, is
				// reported by no one: the abort comes first all the same.
				if len(got) == 0 || got[0] != "abort" {
					t.Fatalf("events %v, want the abort first when the grace runs out", got)
				}
				for _, e := range got {
					if e == "report" {
						t.Fatalf("events %v: a panic after the grace ran out was reported", got)
					}
				}
				t.Logf("the grace ran out before the parser panicked: events %v", got)
				return
			}
			if tc.hang && len(got) > len(tc.want) {
				got = got[:len(tc.want)]
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events %v, want %v: the stop is stamped and the capture paused before the supervisor logs or reports why", got, tc.want)
			}
		})
	}
}

// A parser that returns nil has recorded what it read: the supervisor does not
// stamp a stop for it.
func TestRun_DoesNotStopAParserThatReturnsNil(t *testing.T) {
	t.Parallel()
	stopped := false
	s := New(Config{Logger: zap.NewNop()})
	res := s.Run(context.Background(), func(context.Context, *Session) error { return nil },
		&Session{OnStop: func(time.Time) { stopped = true }})
	if res.Status != StatusOK {
		t.Fatalf("status %s, want ok", res.Status)
	}
	if stopped {
		t.Fatal("the supervisor stamped a stop for a parser that returned nil")
	}
}

// RecordingStopping is the one rule for a stop as the recording itself stops,
// and it reads only the recording's own stop: false while RecordingDone is
// open, true once it is closed, for good. A session not told when its
// recording stops (no RecordingDone), and a nil one, are never stopping.
func TestRecordingStoppingIsTheRecordingsStop(t *testing.T) {
	recording, cancel := context.WithCancel(context.Background())
	s := &Session{RecordingDone: recording.Done()}
	if s.RecordingStopping() {
		t.Fatal("stopping while the recording runs")
	}
	cancel()
	if !s.RecordingStopping() || !s.RecordingStopping() {
		t.Fatal("not stopping once the recording's context is done")
	}
	if (&Session{}).RecordingStopping() {
		t.Fatal("a session with no RecordingDone is stopping")
	}
	var none *Session
	if none.RecordingStopping() {
		t.Fatal("a nil session is stopping")
	}
}
