package manager

import (
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// warnNoRequestTime says a test's mock that its parser handed over with no
// request time (Spec.ReqTimestampMock), naming the parser by the mock's kind.
//
// No test case's window can be shown not to hold such a mock, so it is
// treated as needed by every test case of its recording (servedToAnyTest):
// its loss leaves out every later test case, and in the --sync or static-dedup
// buffer the next resolve's stale cutoff drops it, since a missing request
// time is older than every cutoff. Every parser's recorder stamps the request
// time (the row over every registered parser in the proxy's tests), so one
// that arrives without it is a recorder's bug: said here, as it is handed
// over, so the leave-outs it costs are traced to their parser. A mock any
// test may be served needs no request time, and an answer the client never
// read serves no test: the rule the loss reads (attributedReusable). That is
// a session's or a connection's mock, but not one reusable only by its
// recorder's statement (a MySQL SET, BEGIN or COMMIT), nor, under the proven
// switch, one outside its protocol's phase (IsProtocolPhase), a test's mock
// there: those are said.
//
// The first of each kind and every 1024th after it are said at WARN, the rest
// at Debug. It runs before AddMock takes mu.
func (m *SyncMockManager) warnNoRequestTime(mock *models.Mock) {
	if !mock.Spec.ReqTimestampMock.IsZero() || mock.IsUndelivered() || attributedReusable(mock) {
		return
	}
	c, _ := m.noRequestTime.LoadOrStore(mock.Kind, new(atomic.Uint64))
	n := c.(*atomic.Uint64).Add(1)
	lg := m.dropLogger()
	say := lg.Debug
	if n == 1 || n%1024 == 0 {
		say = lg.Warn
	}
	say("a parser handed over a test's mock with no request time: no test case can be shown not to need it, so "+
		"losing it leaves out every later test case of the recording",
		zap.String("parser", string(mock.Kind)),
		zap.String("mock", mock.Name),
		zap.String("connection", mock.ConnectionID),
		zap.Uint64("of_this_parser", n),
		zap.String("next_step", "the "+string(mock.Kind)+" parser's recorder must stamp each mock's request time "+
			"(ReqTimestampMock) with its request's first byte; report it to keploy with this line"))
}
