package routes

import (
	"fmt"

	"go.uber.org/zap"
)

// stopCutNamesMax bounds the test cases the stop cut's WARN names
// (stopCutLeftOut.say): one log line, however many it left out.
const stopCutNamesMax = 20

// stopCutLeftOutMsg is the WARN a recording session logs, once, as it ends,
// when the recording's stop cut a connection's parser short and that left out
// test cases (stopCutLeftOut).
const stopCutLeftOutMsg = "agent: the recording's stop left out test cases: a connection's parser made no progress after the stop on requests it had not read, so their mocks were never recorded"

// stopCutReason is the reason the stop cut's WARN gives.
const stopCutReason = "the parser made no progress after the stop"

// stopCutLeftOut is the test cases a recording session left out over a span
// of requests a connection's parser never read because the recording's stop
// cut it short (syncMock.SyncMockManager.WasStopCutInWindow): their count, and
// the names of the first stopCutNamesMax.
type stopCutLeftOut struct {
	names []string
	n     int
}

// add notes the test case named name left out by the stop's cut.
func (l *stopCutLeftOut) add(name string) {
	l.n++
	if len(l.names) < stopCutNamesMax {
		l.names = append(l.names, name)
	}
}

// listed is the names the WARN lists: the first stopCutNamesMax, and then
// "and N more" for the rest.
func (l *stopCutLeftOut) listed() []string {
	if l.n <= len(l.names) {
		return l.names
	}
	return append(append([]string(nil), l.names...), fmt.Sprintf("and %d more", l.n-len(l.names)))
}

// say logs the stop cut's WARN, once per session, when it left out any test
// case: the user sees which test cases the recording does not hold, not only
// the window of the requests the cut lost (the proxy's WARN of the cut, which
// names the connection).
func (l *stopCutLeftOut) say(logger *zap.Logger) {
	if l.n == 0 {
		return
	}
	logger.Warn(stopCutLeftOutMsg,
		zap.Int("test_cases_left_out", l.n),
		zap.Strings("test_cases", l.listed()),
		zap.String("reason", stopCutReason),
		zap.String("next_step", "the agent's WARN that the recording's stop cut short a parser names its connection: re-run with --debug for why the parser made no progress, and re-record to keep these test cases"))
}
