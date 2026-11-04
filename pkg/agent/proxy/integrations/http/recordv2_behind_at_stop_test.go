//go:build linux

package http

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zaptest"
)

// behindAtStopReq is the request of the parser-behind rows' exchange n, and
// behindAtStopAnswer its whole answer.
func behindAtStopReq(n int) string {
	return fmt.Sprintf("GET /behind/%d HTTP/1.1\r\nHost: upstream\r\n\r\n", n)
}

func behindAtStopAnswer(n int) string {
	body := fmt.Sprintf("answer %d", n)
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// behindLeftOut is each reason c's session left a mock out for.
func behindLeftOut(c *relayedConn) []string {
	var got []string
	for _, e := range c.logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			r, _ := e.ContextMap()["reason"].(string)
			got = append(got, r)
		}
	}
	return got
}

// stopBehind captures exchanges whole on c with its parser not reading them
// yet (behind its capture: a starved agent's parser, or one held in its
// delivery of an earlier mock), and then the request of a cut one with only
// part of its answer when cut is set; then stops the recording as the
// dispatcher's stop does: the session's context ends, and with it the relay's
// (recordViaSupervisor derives both from the recording's). The parser starts
// only then, on the context that has ended.
func stopBehind(t *testing.T, c *relayedConn, whole int, cut string, viaSyncMock bool) {
	t.Helper()
	c.startRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.sess.Ctx = ctx
	c.sess.RecordingDone = ctx.Done()
	if viaSyncMock {
		c.mgr.SetOutputChannel(c.mocks)
		c.sess.RouteMocksViaSyncMock = true
	}
	for n := 1; n <= whole; n++ {
		write(t, c.app, c.src, behindAtStopReq(n))
		relayed(t, c.dest, behindAtStopReq(n))
		write(t, c.dest, c.dst, behindAtStopAnswer(n))
		relayed(t, c.app, behindAtStopAnswer(n))
	}
	if cut != "" {
		write(t, c.app, c.src, behindAtStopReq(whole+1))
		relayed(t, c.dest, behindAtStopReq(whole+1))
		write(t, c.dest, c.dst, cut)
		relayed(t, c.app, cut)
	}
	cancel()
	c.stopRelay()
	go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
}

// collectBehind waits for recordV2 to return, and takes the mocks c has by
// then.
func collectBehind(t *testing.T, c *relayedConn) ([]*models.Mock, error) {
	t.Helper()
	var err error
	select {
	case err = <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s of the stop")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks, err
		}
	}
}

// awaitBehindMocks reads c's mocks until it has want of them, or none comes
// for 10s (a lost mock is then a failure, not a hang).
func awaitBehindMocks(c *relayedConn, mocks []*models.Mock, want int) []*models.Mock {
	for len(mocks) < want {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		case <-time.After(10 * time.Second):
			return mocks
		}
	}
	return mocks
}

// checkBehindMocks fails t unless mocks are the whole exchanges 1..whole, in
// order, each with its own answer.
func checkBehindMocks(t *testing.T, mocks []*models.Mock, whole int) {
	t.Helper()
	if len(mocks) != whole {
		t.Fatalf("recorded %d mock(s), want the %d exchanges captured whole before the stop", len(mocks), whole)
	}
	for i, m := range mocks {
		n := i + 1
		if m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
			t.Fatalf("mock %d has no request or response: %+v", n, m.Spec)
		}
		if !strings.HasSuffix(m.Spec.HTTPReq.URL, fmt.Sprintf("/behind/%d", n)) {
			t.Fatalf("mock %d is of %q, want /behind/%d", n, m.Spec.HTTPReq.URL, n)
		}
		if want := fmt.Sprintf("answer %d", n); m.Spec.HTTPResp.Body != want || m.Spec.HTTPResp.StatusCode != 200 {
			t.Fatalf("mock %d answers %d %q, want 200 %q", n, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body, want)
		}
		if m.Spec.Fault != nil {
			t.Fatalf("mock %d records a fault %v, want its whole answer", n, m.Spec.Fault)
		}
	}
}

// A parser behind its capture at the recording's stop records every exchange
// its capture holds whole: the stop ends its context before it has read them,
// and it reads them all the same, off the streams the relay ends at its
// teardown once it has handed on what it captured. It returned at its loop's
// check of the context instead, and lost all three with nothing recording,
// leaving out or reporting them. Under both switches, and both ways a mock is
// handed on (the dispatcher's sync-mock manager, and the session's channel).
func TestRecordV2_BehindAtTheStopRecordsEveryExchangeCapturedWhole(t *testing.T) {
	const whole = 3
	for _, proven := range []bool{false, true} {
		for _, viaSyncMock := range []bool{false, true} {
			name := fmt.Sprintf("proven=%v, sync-mock=%v", proven, viaSyncMock)
			t.Run(name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				stopBehind(t, c, whole, "", viaSyncMock)
				mocks, err := collectBehind(t, c)
				mocks = awaitBehindMocks(c, mocks, whole)
				if err != nil {
					t.Fatalf("recordV2 = %v, want nil: the streams' end ends it at the stop", err)
				}
				checkBehindMocks(t, mocks, whole)
				if got := behindLeftOut(c); len(got) != 0 {
					t.Fatalf("left out %q, want nothing: every exchange was captured whole", got)
				}
			})
		}
	}
}

// The exchange the stop cut, with its request and part of its answer
// captured, is left out with its reason, and the whole ones before it are
// still recorded: none of them vanishes.
func TestRecordV2_BehindAtTheStopLeavesOutTheExchangeTheStopCut(t *testing.T) {
	const whole = 2
	cut := "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			stopBehind(t, c, whole, cut, true)
			mocks, err := collectBehind(t, c)
			mocks = awaitBehindMocks(c, mocks, whole)
			if err != nil && strings.Contains(err.Error(), context.Canceled.Error()) {
				t.Fatalf("recordV2 = %v: it returned on the context's end", err)
			}
			checkBehindMocks(t, mocks, whole)
			got := behindLeftOut(c)
			if len(got) != 1 || got[0] == "" {
				t.Fatalf("left out %q, want the exchange the stop cut, once, with its reason", got)
			}
			if spans := c.spans.all(); len(spans) != 1 {
				t.Fatalf("recorded %d span(s) %v, want the cut exchange's", len(spans), spans)
			}
		})
	}
}

// TestRecordV2_BehindAtTheStopRecordsAFaultCapturedBeforeIt pins that a call
// whose upstream closed with no answer before the stop, read by a parser
// behind its capture after the stop, is recorded with its fault, as before the
// stop: the mock goes to the shutdown emit, which the stop does not refuse. An
// emit that took the live path was refused once the context had ended, and the
// call was lost.
func TestRecordV2_BehindAtTheStopRecordsAFaultCapturedBeforeIt(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			c.startRelay(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			c.sess.Ctx = ctx
			c.sess.RecordingDone = ctx.Done()
			c.mgr.SetOutputChannel(c.mocks)
			c.sess.RouteMocksViaSyncMock = true
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			_ = c.dest.Close()
			// The application reads the upstream's close: the relay has
			// captured it, before the stop.
			if got, rerr := appRead(t, c.app); got != "" || rerr != io.EOF {
				t.Fatalf("the application read %q, %v; want nothing, then EOF, as the upstream gave", got, rerr)
			}
			cancel()
			c.stopRelay()
			go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
			mocks, err := collectBehind(t, c)
			mocks = awaitBehindMocks(c, mocks, 1)
			wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultEOF})
			if got := behindLeftOut(c); len(got) != 0 {
				t.Fatalf("left out %q, want nothing: the call was captured whole, with its fault", got)
			}
		})
	}
}
