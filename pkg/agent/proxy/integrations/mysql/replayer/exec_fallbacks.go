package replayer

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// execFallback is a candidate of a live COM_STMT_EXECUTE that one pass of
// matchCommand's walk met: a recording of the command's statement made with
// other parameters (queryExact: the kind the FIFO candidates are), or one
// that scored (score), which the strict gate decides on before it may answer;
// or a definitive match (definitive), which the walk gated as it met it and
// which is kept only so that a miss's diagnostics replay in walk order.
type execFallback struct {
	mock       *models.Mock
	score      int
	queryExact bool
	definitive bool
	// own is testsOwn's answer on mock (0 not read yet, 1 the open test's
	// own, -1 not), verdict the gate's (0 not asked yet, 1 admitted, -1
	// refused).
	own, verdict int8
}

// execFallbacks are the fallbacks of a live COM_STMT_EXECUTE that one pass
// of the walk met, in walk order, and the three picks the strict gate makes
// among them: the open test's FIFO candidate (window), the FIFO candidate of
// the rest (any), each the first in walk order the gate admits, and the
// score candidate (scored), the first the gate admits of the highest score.
//
// The walk does not gate them: it only lists them. A pick is gated when the
// selection asks for it, in its rank (selectExec), so a command answered by
// a recording with its parameters, or by the first of its statement's other
// recordings, compares that one with the live request, not every recording
// of its statement. Gated as the walk met them (eager, the reference a test
// checks the deferred gate against), each was compared: n recordings of one
// statement cost n comparisons a command, n² a replay.
//
// The picks are the same either way. The gate's verdict on a candidate
// depends only on the candidate and the live command, so the first admitted
// in walk order is the same whatever order the verdicts are read in; and the
// eager walk's rule for the score (a later candidate replaces the current
// one only with a strictly higher score) keeps the first admitted of the
// highest score. So are a miss's diagnostics: replayed in walk order from
// the verdicts (diagnose), the gate counts, names and diffs what the eager
// walk did.
type execFallbacks struct {
	gate              *strictGate
	sameStatementOnly bool
	testsOwn          func(*models.Mock) bool
	eager             bool
	list              []execFallback

	window, anyTier, scored *models.Mock
	maxScore                int
	windowRead, anyTierRead bool
	scoredRead              bool
}

// reset starts a pass over: the walk's full scan after its first pass.
func (f *execFallbacks) reset() {
	f.list = f.list[:0]
	f.window, f.anyTier, f.scored, f.maxScore = nil, nil, nil, 0
	f.windowRead, f.anyTierRead, f.scoredRead = false, false, false
}

// addDefinitive notes a definitive match the walk gated as it met it.
func (f *execFallbacks) addDefinitive(mock *models.Mock) {
	f.list = append(f.list, execFallback{mock: mock, definitive: true})
}

// add lists a fallback the walk met. Eager, it is gated now and the picks
// follow at once, as the walk did before it deferred the gate.
func (f *execFallbacks) add(mock *models.Mock, score int, queryExact bool) {
	f.list = append(f.list, execFallback{mock: mock, score: score, queryExact: queryExact})
	if !f.eager {
		return
	}
	e := &f.list[len(f.list)-1]
	if !f.walkGate(e, f.maxScore) {
		return
	}
	if queryExact {
		if f.testsOwn(mock) {
			if f.window == nil {
				f.window = mock
			}
		} else if f.anyTier == nil {
			f.anyTier = mock
		}
	}
	if score > f.maxScore {
		f.maxScore, f.scored = score, mock
	}
}

// walkGate is the gate's calls on e as the eager walk made them, maxScore the
// highest score it admitted before e: a candidate that does not raise it and
// is not query-exact is not gated (it could not be picked). It reports
// whether the gate admitted e. A recording of the statement made with other
// parameters may become a FIFO or score candidate only when the drifting
// parameters (body.parameters.N.value) are covered by learned or user noise
// (allows), and under exact serving only when every parameter that differs
// is noise, strict enforcement on or not (allowsDrift).
func (f *execFallbacks) walkGate(e *execFallback, maxScore int) bool {
	if !e.queryExact && e.score <= maxScore {
		return false
	}
	if !f.gate.allows(e.mock) {
		return false
	}
	return !e.queryExact || !f.sameStatementOnly || f.gate.allowsDrift(e.mock)
}

// admit is the gate's verdict on e, read once.
func (f *execFallbacks) admit(e *execFallback) bool {
	if e.verdict == 0 {
		e.verdict = -1
		if f.walkGate(e, 0) {
			e.verdict = 1
		}
	}
	return e.verdict > 0
}

// owned is testsOwn's answer on e, read once.
func (f *execFallbacks) owned(e *execFallback) bool {
	if e.own == 0 {
		e.own = -1
		if f.testsOwn(e.mock) {
			e.own = 1
		}
	}
	return e.own > 0
}

// fifo is the first query-exact fallback in walk order, of the open test's
// own (own) or of the rest, the gate admits; nil when it admits none.
func (f *execFallbacks) fifo(own bool) *models.Mock {
	for i := range f.list {
		e := &f.list[i]
		if e.definitive || !e.queryExact || f.owned(e) != own {
			continue
		}
		if f.admit(e) {
			return e.mock
		}
	}
	return nil
}

// windowPick is the open test's FIFO candidate (fifoExecMockWindow).
func (f *execFallbacks) windowPick() *models.Mock {
	if !f.eager && !f.windowRead {
		f.windowRead, f.window = true, f.fifo(true)
	}
	return f.window
}

// anyPick is the FIFO candidate of the rest (fifoExecMock).
func (f *execFallbacks) anyPick() *models.Mock {
	if !f.eager && !f.anyTierRead {
		f.anyTierRead, f.anyTier = true, f.fifo(false)
	}
	return f.anyTier
}

// scorePick is the score candidate: the first in walk order the gate admits
// of the highest score; nil when it admits none that scored.
func (f *execFallbacks) scorePick() *models.Mock {
	if f.eager || f.scoredRead {
		return f.scored
	}
	f.scoredRead = true
	var order []int
	for i := range f.list {
		if e := &f.list[i]; !e.definitive && e.score > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return f.list[order[a]].score > f.list[order[b]].score })
	for _, i := range order {
		if e := &f.list[i]; f.admit(e) {
			f.scored, f.maxScore = e.mock, e.score
			break
		}
	}
	return f.scored
}

// diagnose gives the gate's rejection diagnostics (rejected, closestMock,
// fieldDiffs, exactRejected) the values the eager walk left them with, for a
// miss's report: it replays the eager walk's gate calls in walk order. The
// verdicts are cached, so it compares no candidate a second time. Eager, the
// walk left them so.
func (f *execFallbacks) diagnose() {
	if f.eager {
		return
	}
	f.gate.resetDiagnostics()
	maxScore := 0
	for i := range f.list {
		e := &f.list[i]
		if e.definitive {
			f.gate.allows(e.mock)
			continue
		}
		if f.walkGate(e, maxScore) && e.score > maxScore {
			maxScore = e.score
		}
	}
}

// recordedResponse is the answer mock recorded, nil for no mock.
func recordedResponse(mock *models.Mock) *mysql.Response {
	if mock == nil {
		return nil
	}
	return &mock.Spec.MySQLResponses[0]
}
