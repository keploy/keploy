package agent

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// Givens (POST /agent/scope/given, models.GivenReq): a value the open test
// gave the application. In a record it is kept with the test's window
// (models.ScopeWindow.Given), which keploy writes into the test's mapping. In
// a replay it is also bound as a given variable of the test's scope
// (variables.Table.Give), with the value its recording gave under the same
// variable (SetScopeGiven): the learn step follows it into the calls that
// carry it as a use-only variable, and the replay checks that each such call
// carries the value the test gives now.
//
// What keploy holds of them: in a replay, a test's givens only while it is
// open (its scope's bindings, the learn step's, its window's), at most
// MaxGivensPerTest of at most models.MaxGivenValue bytes, and the set's
// recorded givens with its table for the whole replay of the set, as the
// table itself, until the next set's replace them (SetScopeGiven). In a
// record, each test's window keeps its givens (keploy writes them into the
// test's mapping when it reads the windows at the run's end) until the next
// session begins (resetSession), at most MaxGivenBytesPerRecord in a
// session.

// MaxGivensPerTest bounds how many values one test may give.
const MaxGivensPerTest = 1024

// MaxGivenBytesPerRecord bounds the bytes of the values (names, values and
// sources) the tests of one record session may give: a record holds each
// test's with its window until the next session begins.
const MaxGivenBytesPerRecord = 64 << 20

// SetScopeGiven installs, for a replay, the values each test gave when it was
// recorded (models.ScopeTableReq.Given), by test ID.
func (a *Agent) SetScopeGiven(_ context.Context, given map[string][]models.GivenValue) error {
	var held map[string][]models.GivenValue
	for test, vs := range given {
		if len(vs) == 0 {
			continue
		}
		if held == nil {
			held = make(map[string][]models.GivenValue, len(given))
		}
		held[test] = slices.Clone(vs)
	}
	a.scopeMu.Lock()
	a.testGiven = held
	a.scopeMu.Unlock()
	return nil
}

// ProvenGive takes a value the open test gave (models.GivenReq): the n-th
// value a test gives under one name is variable models.GivenVariable(name,
// n). It is refused when no test is open (not_open), and when the name or
// the value is not one keploy can hold (bad_request).
func (a *Agent) ProvenGive(ctx context.Context, req models.GivenReq) (models.GivenResp, *models.ScopeError) {
	if !models.ValidGivenName(req.Name) {
		return models.GivenResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("a given's name is 1 to %d letters, digits, '_' or '-', not %q", models.MaxGivenName, req.Name),
			fmt.Sprintf("name it with 1 to %d letters, digits, '_' or '-'", models.MaxGivenName))
	}
	if req.Value == "" || len(req.Value) > models.MaxGivenValue {
		return models.GivenResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("given %s's value is empty or longer than %d bytes: keploy cannot follow it into a call", req.Name, models.MaxGivenValue),
			"give the value the test hands the application, as the application sends it on")
	}
	if len(req.Source) > models.MaxGivenSource {
		return models.GivenResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("given %s's source is longer than %d bytes", req.Name, models.MaxGivenSource),
			"say where the test gave the value as file:line")
	}
	// One step with begin and end: a given never lands between a test's end
	// and the next one's begin.
	if err := a.wire.lock(ctx); err != nil {
		return models.GivenResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	a.provenMu.Lock()
	closed, id := a.proven.closed, a.proven.open
	a.provenMu.Unlock()
	if closed {
		return models.GivenResp{}, errClosed()
	}
	if id == "" {
		return models.GivenResp{}, givenNotOpen(req)
	}
	k := scopeKey{pid: 0, name: id}
	a.scopeMu.Lock()
	if _, open := a.workerOpen[k]; !open {
		a.scopeMu.Unlock()
		return models.GivenResp{}, givenNotOpen(req)
	}
	meta := a.scopeMeta[k]
	if len(meta.given) >= MaxGivensPerTest {
		a.scopeMu.Unlock()
		return models.GivenResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("%s gave more than %d values", id, MaxGivensPerTest),
			"give only the values the test hands the application and expects it to send on")
	}
	variable := models.GivenVariable(req.Name, givenCount(meta.given, req.Name)+1)
	v := models.GivenValue{Name: variable, Value: req.Value, Source: req.Source}
	if !a.replaying() && a.givenBytes+givenSize(v) > MaxGivenBytesPerRecord {
		a.scopeMu.Unlock()
		return models.GivenResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("the tests of this record gave more than %d MiB of values: keploy holds each test's until the next record session begins", MaxGivenBytesPerRecord>>20),
			"give only the values the test hands the application and expects it to send on, or record fewer tests in one run")
	}
	recorded := ""
	for _, r := range a.testGiven[id] {
		if r.Name == variable {
			recorded = r.Value
			break
		}
	}
	a.scopeMu.Unlock()

	if a.replaying() {
		es, err := a.exactServer()
		if err != nil {
			return models.GivenResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy cannot check "+variable+" in "+id+": "+err.Error(), "run the keploy agent of this keploy version")
		}
		g, ok := es.(coreAgent.ExactTestGiver)
		if !ok {
			return models.GivenResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy cannot check "+variable+" in "+id+": this keploy agent's proxy keeps no test variables", "run the keploy agent of this keploy version")
		}
		b := models.VariableBinding{Name: variable, Value: req.Value, Recorded: recorded, Mock: "test " + id, Place: req.Source, Given: true}
		if err := g.GiveExactTest(id, b); err != nil {
			return models.GivenResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy could not hold "+variable+" for "+id+": "+err.Error(), "check the agent's log")
		}
	}
	a.scopeMu.Lock()
	if _, open := a.workerOpen[k]; open {
		meta := a.scopeMeta[k]
		meta.given = append(meta.given, v)
		if !a.replaying() {
			a.givenBytes += givenSize(v)
		}
		if a.scopeMeta == nil {
			a.scopeMeta = make(map[scopeKey]scopeMeta)
		}
		a.scopeMeta[k] = meta
	}
	a.scopeMu.Unlock()
	return models.GivenResp{V: models.ScopeProtocolVersion, Status: "ok", Variable: variable}, nil
}

// givensNotRecorded is, in a replay, the reason's sentence for the values
// the open test id gave under a variable its recording does not hold: it
// was recorded before the test gave them, so each binds with no recorded
// value and checks nothing (ProvenGive), and a call that carries one misses
// as a plain call. "" when the recording holds every value the test gave.
func (a *Agent) givensNotRecorded(id string) string {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	recorded := a.testGiven[id]
	var lack []string
	for _, g := range a.scopeMeta[scopeKey{pid: 0, name: id}].given {
		if !slices.ContainsFunc(recorded, func(r models.GivenValue) bool { return r.Name == g.Name }) {
			lack = append(lack, g.Name)
		}
	}
	return notRecordedText(lack)
}

// notRecordedText is givensNotRecorded's sentence for the given variables
// names, the first few by name.
func notRecordedText(names []string) string {
	const most = 5
	switch {
	case len(names) == 0:
		return ""
	case len(names) == 1:
		return "the test gave " + names[0] + ", which its recording does not hold (recorded before the test gave it): re-record the test"
	case len(names) > most:
		names = append(slices.Clone(names[:most]), fmt.Sprintf("%d more", len(names)-most))
	}
	return "the test gave " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] +
		", which its recording does not hold (recorded before the test gave them): re-record the test"
}

// givenSize is the bytes of g a record holds.
func givenSize(g models.GivenValue) int {
	return len(g.Name) + len(g.Value) + len(g.Source)
}

// givenCount is how many of given were given under name.
func givenCount(given []models.GivenValue, name string) int {
	n := 0
	for _, g := range given {
		if g.Name == models.GivenPrefix+name || strings.HasPrefix(g.Name, models.GivenPrefix+name+"#") {
			n++
		}
	}
	return n
}

func givenNotOpen(req models.GivenReq) *models.ScopeError {
	where := ""
	if req.Source != "" {
		where = " (" + req.Source + ")"
	}
	return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotOpen,
		"a value was given as "+req.Name+where+" while no test was open: keploy holds a given value for the test that gave it",
		"give it inside a test, after its keploytest line; a value a fixture makes is given by the test that sends it")
}
