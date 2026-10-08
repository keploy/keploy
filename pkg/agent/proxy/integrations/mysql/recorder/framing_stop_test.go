package recorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The recorder logs that a connection's recording stops where it decides it
// (warnFramingLost), and stamps the stop there, before it logs: the span of
// what the connection carries from then on opens as the stop is stamped
// (Session.OnStop). Stamped only once the error had reached leaveOutInFlight,
// after the WARN, a test case checked while the WARN was written overlapped no
// span. With no logger it stamps all the same. A response that cannot be
// framed, whose exchange is left out alone, stamps no stop
// (TestRecordV2_AResponseLeftOutAloneIsCountedAndStampsNoStop).
func TestWarnFramingLostStampsTheStopBeforeItWarns(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	stops, logged := 0, -1
	sess := &supervisor.Session{ClientConnID: "1", OnStop: func(time.Time) {
		stops++
		logged = logs.Len()
	}}
	warnFramingLost(zap.New(core), sess, "V2: mysql framing lost (TestWarnFramingLostStampsTheStopBeforeItWarns)", ErrFramingLost)
	if stops != 1 {
		t.Fatalf("the stop was stamped %d times, want once", stops)
	}
	if logged != 0 {
		t.Fatalf("the stop was stamped after %d log lines, want before the first", logged)
	}
	if logs.Len() != 1 {
		t.Fatalf("%d log lines, want the framing loss's", logs.Len())
	}

	quiet := &supervisor.Session{ClientConnID: "2"}
	quietStops := 0
	quiet.OnStop = func(time.Time) { quietStops++ }
	warnFramingLost(nil, quiet, "V2: mysql framing lost (TestWarnFramingLostStampsTheStopBeforeItWarns)", ErrFramingLost)
	if quietStops != 1 {
		t.Fatalf("with no logger the stop was stamped %d times, want once", quietStops)
	}
}

// The framing loss's WARN takes the place of the dispatcher's "parser retired"
// one (the error wraps supervisor.ErrReported), so it follows that WARN's rule
// (Session.RecordingStopping). It says that every test case recorded while the
// connection carries traffic is left out, and as the recording itself stops
// none is: the connection is torn down with it, and no span opens. So as the
// recording stops the loss is logged at DEBUG, after the stop is stamped, and
// the limit is not spent on it: a loss on a connection whose recording runs,
// right after, is still said at WARN. A session not told when its recording
// stops (no RecordingDone) warns, as before. Before, the WARN went out
// whatever the recording did, and the dispatcher, told the parser reported its
// stop, logged nothing in its place: one WARN followed another rule than the
// rest of its family.
func TestWarnFramingLostWarnsOnlyWhileTheRecordingRuns(t *testing.T) {
	const msg = "V2: mysql framing lost (TestWarnFramingLostWarnsOnlyWhileTheRecordingRuns)"
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	stopped := make(chan struct{})
	close(stopped)

	stops, logged := 0, -1
	stopping := &supervisor.Session{ClientConnID: "1", RecordingDone: stopped, OnStop: func(time.Time) {
		stops++
		logged = logs.Len()
	}}
	warnFramingLost(logger, stopping, msg, ErrFramingLost)
	if stops != 1 || logged != 0 {
		t.Fatalf("the stop was stamped %d times, after %d log lines; want once, before the first", stops, logged)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARNs for a framing loss as the recording stops, want none: it says every test case recorded from then on is left out, and none is", n)
	}
	debug := logs.FilterMessage(msg).FilterLevelExact(zapcore.DebugLevel).All()
	if len(debug) != 1 || debug[0].ContextMap()["recordingStopping"] != true {
		t.Fatalf("the loss as the recording stops was logged %v, want once at DEBUG, saying the recording was stopping", debug)
	}

	running := &supervisor.Session{ClientConnID: "2", RecordingDone: make(chan struct{})}
	warnFramingLost(logger, running, msg, ErrFramingLost)
	warns := logs.FilterMessage(msg).FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("%d WARNs for a framing loss while the recording runs, after one as it stopped; want one: the limit was spent on a line that was not a WARN", len(warns))
	}
	if held, ok := warns[0].ContextMap()["sameWarningsHeldBack"]; ok {
		t.Fatalf("the WARN says %v were held back, want none: the loss as the recording stopped was not a WARN held back", held)
	}

	const untold = "V2: mysql framing lost (TestWarnFramingLostWarnsOnlyWhileTheRecordingRuns, untold)"
	warnFramingLost(logger, &supervisor.Session{ClientConnID: "3"}, untold, ErrFramingLost)
	if n := logs.FilterMessage(untold).FilterLevelExact(zapcore.WarnLevel).Len(); n != 1 {
		t.Fatalf("%d WARNs for a session not told when its recording stops, want one", n)
	}
}

// The recorder runs behind the traffic, so it can find a framing loss that
// stops the connection after the recording stopped, in bytes the connection
// carried before: here a client packet out of sequence. RecordV2's own context
// is left running, as it is for a recorder that read the bytes before the stop
// and checks its context no more before it finds the loss. The exchange the
// loss is in was captured and is not recorded, so it is reported as before:
// counted, said at WARN, and spanned. The connection's stop is not said at
// WARN: the connection is torn down with the recording. While the recording
// runs, both are.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_AFramingLossFoundAsTheRecordingStopsIsNotWarnedOf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stopping bool
	}{{"recording stopped", true}, {"recording runs", false}} {
		t.Run(tc.name, func(t *testing.T) {
			resetWarnLimiters()
			core, logs := observer.New(zapcore.DebugLevel)
			logger := zap.New(core)
			h := newV2Harness(t)
			h.sess.Logger = logger
			mgr := syncMock.New(nil)
			h.sess.Mgr = mgr
			spans := &orphanSpans{}
			h.sess.Orphans = spans
			recording := make(chan struct{})
			h.sess.RecordingDone = recording
			if tc.stopping {
				close(recording)
			}

			base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			greetingBuf := cannedHandshakeV10(t)
			greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
			if err != nil {
				t.Fatal(err)
			}
			h.pushDest(greetingBuf, base)
			h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(time.Millisecond))
			h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(2*time.Millisecond))
			// A client packet out of sequence: 5 where a command (0) must
			// start.
			h.pushClient(cannedCOMQuery(t, 5, "SELECT 1"), base.Add(3*time.Millisecond))
			h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(4*time.Millisecond))
			h.closeStreams()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := RecordV2(ctx, logger, h.sess); !errors.Is(err, ErrFramingLost) {
				t.Fatalf("RecordV2 = %v, want ErrFramingLost", err)
			}

			stopWarns := logs.FilterMessageSnippet("the connection is no longer recorded").FilterLevelExact(zapcore.WarnLevel).Len()
			if want := map[bool]int{true: 0, false: 1}[tc.stopping]; stopWarns != want {
				t.Fatalf("%d WARNs of the connection's stop, want %d", stopWarns, want)
			}
			if n := len(leftOutWarnings(logs)); n != 1 || mgr.MocksLeftOut() != 1 || spans.count() != 1 {
				t.Fatalf("the exchange the loss is in: %d left-out WARNs, %d counted, %d spans; want it reported once", n, mgr.MocksLeftOut(), spans.count())
			}
		})
	}
}

// A response the recorder cannot frame costs its exchange alone, and the
// connection's recording goes on from the client's next command (realign).
// That exchange is counted as every mock left out is (Session.ReportLeftOut),
// for the recording's summary (mocks_left_out), with the MySQL kind, what was
// found, and that the connection's recording goes on. The count is beside the
// recorder's own line for the exchange (reportFault), which is logged once,
// with the message and the next_step it had before the exchange was counted:
// a recording is checked by that line. Nothing stamps the connection's stop
// for it (Session.StoppedAt): a stop stamped there opens the span of what the
// connection carries next, and every test case recorded while a pooled
// connection carries traffic is left out. Before, the exchange was spanned
// and logged, and nothing counted it.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_AResponseLeftOutAloneIsCountedAndStampsNoStop(t *testing.T) {
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7}, false)
	// An empty packet where the column count should be.
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	rec := recordWire(t, xs, 16384, true, time.Minute)
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 9-1)
	if n := rec.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d mocks left out counted, want the 1 it could not frame: the recording's summary (mocks_left_out) misses it", n)
	}
	if rec.stops != 0 {
		t.Fatalf("the connection's stop was stamped %d times for an exchange left out alone, want never", rec.stops)
	}

	const line = "V2: failed to decode mysql response head; the exchange is left out, and the connection's recording goes on from its next command"
	const nextStep = "user traffic is unaffected. The test cases recorded while this exchange ran are left out of the recording rather than saved without its mock; the connection's later queries are recorded"
	own := rec.logs.FilterMessage(line).FilterLevelExact(zapcore.WarnLevel).All()
	if len(own) != 1 {
		t.Fatalf("%d WARN lines %q, want 1", len(own), line)
	}
	if got := own[0].ContextMap()["next_step"]; got != nextStep {
		t.Fatalf("the recorder's line for the exchange left out has next_step %q, want it unchanged: %q", got, nextStep)
	}

	w := leftOutWarnings(rec.logs)
	if len(w) != 1 {
		t.Fatalf("%d left-out WARNs for the exchange, want 1", len(w))
	}
	fields := w[0].ContextMap()
	reason, _ := fields["reason"].(string)
	if fields["kind"] != string(models.MySQL) || !strings.HasPrefix(reason, "mysql exchange not recorded: its response cannot be framed (") ||
		!strings.Contains(reason, "first packet does not decode") || !strings.Contains(reason, "the connection's recording goes on from its next command") {
		t.Fatalf("the left-out WARN says %v, want the MySQL kind, what was found, and that the connection's recording goes on", fields)
	}
	if strings.Contains(reason, ErrFramingLost.Error()) {
		t.Fatalf("the left-out WARN's reason says the connection's framing is lost: %q; the exchange alone is left out", reason)
	}
}
