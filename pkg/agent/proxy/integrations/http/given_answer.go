package http

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// minAnswerGiven is the length, in bytes, of the shortest recorded value of a
// given that a later answer is answered with. A place there is found by the
// recorded value alone: the call's request does not carry the given, as it
// does where the learn step follows one (givenFor reads both runs' values).
// A short value, such as a count, a flag, a word or a clock read, stands
// whole in answers that never echoed what the test gave, and answering those
// would change what the dependency said. Sixteen is the length the learn
// step reads as a value no answer holds by chance (its minLateRun). The
// values tests give per run are longer: a cluster name that holds the run's
// clock is 21 characters or more, a signed session token hundreds. A shorter
// given is still followed in the requests that carry it.
const minAnswerGiven = 16

// givenAnswers is stub, the recording a call of the running test is answered
// with, with each place of its recorded answer that holds a value the test
// gave (a given: models.GivenReq, bound in the table the call binds in,
// varScope.of) answered with the value the test gave in this run, where the
// test gave another value than when stub was recorded. The dependency echoed
// what the test gave it: a list read after a create names the clusters the
// test created, with this run's names. The learn step answers a given in the
// answer of the call that carries it (learnPlan), which the variable pass
// served already; this is every other answer, whose request does not carry
// it.
//
// A place is one the learn step answers a given at (learn.WholeEcho): a JSON
// string, number or boolean of the body, or a header's one value, that is
// the given's recorded value whole. A value inside a longer one is no such
// place, as a request's is not (givenFor). Where the value the test gave
// cannot stand at a place (text where the recording holds a number), the
// answer is served as recorded, as before, and the log says why. So is a
// place of a given recorded shorter than minAnswerGiven. stub is never
// changed: an answer served with a given is a copy (renderAnswers).
func (h *HTTP) givenAnswers(stub *models.Mock, mockDb integrations.MockMemDb) *models.Mock {
	// The table as varScope.of picks it, read here with nothing allocated:
	// every call's serve asks it.
	table := variableTable(mockDb)
	if vs, ok := mockDb.(integrations.VariableScopes); ok {
		if ahead := vs.AheadVariables(); ahead != nil && vs.InAheadScope(stub) {
			table = ahead
		}
	}
	if table == nil {
		return stub
	}
	var moved []variables.Binding
	for _, g := range table.Given() {
		// A value given through keploy's SDKs (given()); a test case's
		// template key keploy test gave is answered as recorded.
		if models.IsGivenVariable(g.Name) && g.Value != g.Recorded {
			moved = append(moved, g)
		}
	}
	if len(moved) == 0 {
		return stub
	}
	loaded, err := withResponse(stub)
	if err != nil {
		// The serve loads it again and says why it cannot.
		return stub
	}
	leaves := respLeaves(loaded.Spec.HTTPResp)
	hit := &varHit{mock: loaded, local: make(map[string]variables.Binding, len(moved))}
	var places, short []string
	for _, g := range moved {
		if len(g.Recorded) < minAnswerGiven {
			for _, a := range leaves {
				if !learn.WholeEcho(learn.Leaf{Value: g.Recorded}, a) {
					continue
				}
				if place, ok := learn.VarPlace(a, true); ok {
					short = append(short, g.Name+" at "+place)
				}
			}
			continue
		}
		d := varDecl{name: g.Name, recorded: g.Recorded}
		for _, a := range leaves {
			if !learn.WholeEcho(learn.Leaf{Value: g.Recorded}, a) {
				continue
			}
			if place, ok := learn.VarPlace(a, true); ok {
				d.answer = append(d.answer, varPlace{raw: place})
				places = append(places, g.Name+" at "+place)
			}
		}
		hit.decls = append(hit.decls, d)
		hit.local[g.Name] = g
	}
	if len(short) > 0 {
		h.Logger.Warn("a recorded answer holds a value the test gave, and keploy serves it as recorded there: the value was recorded shorter than the shortest one keploy answers later answers with, and a value that short stands in answers that never echoed it; give a value of at least that many characters, such as a name that holds the run's clock, and record again",
			zap.String("mock", stub.Name), zap.Strings("places", short), zap.Int("shortest", minAnswerGiven))
	}
	if len(places) == 0 {
		return stub
	}
	rendered, problem := h.renderAnswers(loaded, hit, &variables.Table{})
	if problem != nil {
		h.Logger.Warn("a recorded answer holds a value the test gave, and keploy serves it as recorded: the value the test gives now cannot stand there",
			zap.String("mock", stub.Name), zap.String("given", problem.name), zap.String("place", problem.place), zap.String("why", problem.why))
		return stub
	}
	h.Logger.Info("answered a recorded answer with the values the test gave in this run",
		zap.String("mock", stub.Name), zap.Strings("places", places))
	return rendered
}
