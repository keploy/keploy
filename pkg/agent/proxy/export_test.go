package proxy

import (
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// StopCutWarnMsg is the WARN the record path logs when the recording's stop
// cuts a parser short (warnStopCut), for the rows of package proxy_test.
const StopCutWarnMsg = stopCutWarnMsg

// CutAParserStuckAtTheStop runs, for a row of package proxy_test, a
// connection whose parser never reads the request it carries, on the record
// path as the agent runs it: on the process's manager (syncMock.Get), with no
// manager on its context, the one the record route checks its test cases
// against. The recording then stops, and the hang watchdog cuts the parser
// (its clock runs 100 times as fast as the wall clock: its minute in under a
// second). It returns once the record path has, with when the app sent the
// request and when the upstream had it: the connection carried it in
// between.
func CutAParserStuckAtTheStop(t *testing.T, logger *zap.Logger) (sent, arrived time.Time) {
	t.Helper()
	parser := behindAtTheStopParser{stuck: stuckUntilTheEnd(t), lost: &atomic.Int64{}, out: make(chan int, 1)}
	h := newStopHarnessOn(t, parser, logger, newScaledClock(100), nil)
	r := h.send(t, "GET /stuck HTTP/1.1\r\n\r\n")
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the hang watchdog never cut the parser stuck at the stop")
	}
	return r.sent, r.arrived
}
