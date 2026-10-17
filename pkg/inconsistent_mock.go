package pkg

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// AnsweredBeforeAskedMsg is the warning a replay logs for each mock it leaves
// out because its response is stamped before its request
// (WarnAnsweredBeforeAsked).
const AnsweredBeforeAskedMsg = "a mock is left out of the replay: its response is stamped before its request (an inconsistent recording)"

// answeredBeforeAsked holds the mocks WarnAnsweredBeforeAsked has warned of
// in this process: a filter runs again at every test of a set, the agent's at
// every staging, and each sees the same mock every time. It holds at most
// maxAnsweredBeforeAsked mocks: one that would hold more is emptied first, so
// a long-lived agent that replays set after set holds a bounded set, and a
// mock it forgot is said of once more if it is left out again.
var (
	answeredBeforeAskedMu sync.Mutex
	answeredBeforeAsked   = map[answeredBeforeAskedKey]struct{}{}
)

// maxAnsweredBeforeAsked bounds answeredBeforeAsked.
const maxAnsweredBeforeAsked = 4096

type answeredBeforeAskedKey struct {
	name     string
	kind     models.Kind
	req, res int64
}

// WarnAnsweredBeforeAsked warns, once for each mock in the process, that m is
// left out of the replay because its response is stamped before its request.
// A replayed call it recorded is then answered by another mock, or by none, so
// the drop must not be silent: the user would be left a miss or a wrong
// answer with nothing to point at the mock. The stamps, not the answer, are
// what is wrong: the recorder that wrote it stamped its request after its
// response, a recorder's bug.
func WarnAnsweredBeforeAsked(logger *zap.Logger, m *models.Mock) {
	if logger == nil || m == nil {
		return
	}
	req, res := m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
	key := answeredBeforeAskedKey{name: m.Name, kind: m.Kind, req: req.UnixNano(), res: res.UnixNano()}
	answeredBeforeAskedMu.Lock()
	_, said := answeredBeforeAsked[key]
	if !said {
		if len(answeredBeforeAsked) >= maxAnsweredBeforeAsked {
			answeredBeforeAsked = map[answeredBeforeAskedKey]struct{}{}
		}
		answeredBeforeAsked[key] = struct{}{}
	}
	answeredBeforeAskedMu.Unlock()
	if said {
		return
	}
	logger.Warn(AnsweredBeforeAskedMsg,
		zap.String("mock", m.Name),
		zap.String("kind", string(m.Kind)),
		zap.Time("request", req),
		zap.Time("response", res),
		zap.Duration("answeredBeforeAskedBy", req.Sub(res)),
		zap.String("next_step", "a call this mock recorded is answered by another mock or by none in this replay; record the test set again with a keploy whose recorder for this kind stamps its requests when they were read, and report the kind if a new recording has the same"))
}

// LeaveOutAnsweredBeforeAsked reports whether the replay leaves m out because
// its response is stamped before its request, and says so when it does
// (WarnAnsweredBeforeAsked). Its caller has checked that m has both stamps.
// Under the proven switch it never leaves one out: a test owns the mocks its
// mapping lists whatever their recorded times say, and a pair of stamps in the
// wrong order is a recorder's bug, not a reason to answer the call the mock
// recorded with another mock or with none.
func LeaveOutAnsweredBeforeAsked(logger *zap.Logger, m *models.Mock) bool {
	if m == nil || !m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) || provenswitch.On() {
		return false
	}
	WarnAnsweredBeforeAsked(logger, m)
	return true
}
