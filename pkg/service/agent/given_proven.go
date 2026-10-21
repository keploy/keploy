package agent

import (
	"context"
	"net/http"
	"slices"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A value the test makes itself (a session token it signs with a test key and
// installs as a cookie) is new in every run: no recorded answer gave it, so
// no call of the recording is where it came from, and the learn step refuses
// every call that carries it. The test's harness gives keploy each such value
// as the test installs it (POST /agent/given), named in the order the test
// installs it. A record keeps them with the test's recording (the window's
// Given, then mappings.yaml); a replay binds the value given under a name as
// the test's given variable of that name, recorded with the recording's value
// (as a template value of keploy test is bound): the learn step then follows
// it where a call carries it, whole or inside a longer value (a cookie
// header), and every later replay asserts each such call carries exactly this
// run's value.

// GivenBinder is the proxy's optional extension that binds a given variable
// in the open test's scope (proxy.Proxy.BindGiven).
type GivenBinder interface {
	BindGiven(b models.VariableBinding) (in models.VariableBinding, bound, ok bool)
}

// SetTestGiven installs, by test ID, what each test's harness gave when it was
// recorded (models.MappedTestCase.Given), and by claim key, what each key's
// harness gave while no test was open (models.Mapping.KeyGiven). The CLI
// installs them for a replay session, like the tests' begins; a new session
// clears them (resetScopeState).
func (a *Agent) SetTestGiven(_ context.Context, given, keys map[string][]models.GivenValue) error {
	a.scopeMu.Lock()
	a.testGiven, a.keyGiven = cloneGiven(given), cloneGiven(keys)
	a.scopeMu.Unlock()
	return nil
}

// cloneGiven is a copy of in without its empty entries; nil when none is left.
func cloneGiven(in map[string][]models.GivenValue) map[string][]models.GivenValue {
	var out map[string][]models.GivenValue
	for k, vs := range in {
		if len(vs) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]models.GivenValue{}
		}
		out[k] = slices.Clone(vs)
	}
	return out
}

// Give takes a value the harness gave (models.GivenReq): the open test's, or,
// while no test is open, the current claim key's (a group's beforeAll that
// signs a session for its tests and seeds with it). In a record it is kept, in
// order, with the test's window, or with the claim's (models.KeyGiven). In a
// replay it is bound as the given variable of its name, the test's or the
// key's (Outside: every test of the key reads it, and the key's calls outside
// its tests bind with it), recorded as the recording's value given under that
// name; a name the recording gave none under binds nothing (recorded before
// its harness gave values, or one more given now): a call that carries it
// then misses, as it did before. With no test open and no claim the value
// belongs to nothing, and is refused.
func (a *Agent) Give(ctx context.Context, req models.GivenReq) *models.ScopeError {
	if req.Name == "" || req.Value == "" {
		return scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest, "a given value has a name and a value", "send both")
	}
	if err := a.wire.lock(ctx); err != nil {
		return abandoned(err)
	}
	defer a.wire.unlock()
	a.provenMu.Lock()
	id, key := a.proven.open, a.proven.key
	a.provenMu.Unlock()
	if id == "" {
		if key == "" {
			return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotOpen,
				"the harness gave "+req.Name+" while no test was open and before any claim, so it belongs to nothing", "give a value after the test process claims the run")
		}
		return a.giveOutside(key, req)
	}
	k := scopeKey{name: id}
	if !a.replaying() {
		a.scopeMu.Lock()
		if a.scopeMeta == nil {
			a.scopeMeta = make(map[scopeKey]scopeMeta)
		}
		meta := a.scopeMeta[k]
		meta.given = append(meta.given, models.GivenValue{Name: req.Name, Value: req.Value})
		a.scopeMeta[k] = meta
		a.scopeMu.Unlock()
		a.logger.Debug("kept a value the test made, with its recording", zap.String("test", id), zap.String("name", req.Name))
		return nil
	}
	a.scopeMu.Lock()
	recorded, found := givenValue(a.testGiven[id], req.Name)
	a.scopeMu.Unlock()
	if !found {
		a.logger.Info("the test's harness gave a value its recording holds none of under that name; nothing is bound, and a call that carries it misses",
			zap.String("test", id), zap.String("name", req.Name),
			zap.String("next_step", "re-record the test with this keploy, so its recording keeps what the harness gives"))
		return nil
	}
	return a.bindGiven(id, models.VariableBinding{Name: req.Name, Value: req.Value, Recorded: recorded, Mock: "the test's harness", Place: "given", Given: true})
}

// giveOutside is Give while no test is open: the claim key's value. A record
// keeps it with the key's latest claim window; a replay binds it in the key's
// scope (Outside), which the key's next claim drops (variables.Table
// StartOutside: a test process run again runs its beforeAll again).
func (a *Agent) giveOutside(key string, req models.GivenReq) *models.ScopeError {
	if !a.replaying() {
		a.provenMu.Lock()
		kept, claims := false, len(a.proven.claims)
		for i := len(a.proven.claims) - 1; i >= 0; i-- {
			if a.proven.claims[i].Claim == key {
				a.proven.claims[i].Given = append(a.proven.claims[i].Given, models.GivenValue{Name: req.Name, Value: req.Value})
				kept = true
				break
			}
		}
		a.provenMu.Unlock()
		if !kept {
			return scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy holds no claim of "+key+" to keep "+req.Name+" with", "check the agent's log")
		}
		a.logger.Debug("kept a value the claim key's harness made outside every test, with its claim",
			zap.String("key", key), zap.String("name", req.Name), zap.Int("claims", claims))
		return nil
	}
	a.scopeMu.Lock()
	recorded, found := givenValue(a.keyGiven[key], req.Name)
	a.scopeMu.Unlock()
	if !found {
		a.logger.Info("the claim key's harness gave a value outside every test that its recording holds none of under that name; nothing is bound, and a call that carries it misses",
			zap.String("key", key), zap.String("name", req.Name),
			zap.String("next_step", "re-record the test with this keploy, so its recording keeps what the harness gives"))
		return nil
	}
	return a.bindGiven("the claim key "+key, models.VariableBinding{Name: req.Name, Value: req.Value, Recorded: recorded, Mock: "the claim key's harness", Place: "given", Given: true, Outside: true})
}

// bindGiven binds b through the proxy (GivenBinder), for whose (named in an
// error).
func (a *Agent) bindGiven(whose string, b models.VariableBinding) *models.ScopeError {
	binder, ok := a.Proxy.(GivenBinder)
	if !ok {
		return scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy cannot bind "+b.Name+" for "+whose+": this keploy agent's proxy keeps no test variables", "run the keploy agent of this keploy version")
	}
	in, bound, kept := binder.BindGiven(b)
	switch {
	case !kept:
		return scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy cannot bind "+b.Name+" for "+whose+": the proxy serves no replay yet", "check the agent's log")
	case !bound && in.Value != b.Value:
		// The harness gave the name twice with two values: the first holds
		// (names are per install, so this is a harness's bug).
		a.logger.Warn("the harness gave a name twice with two values; the first is kept",
			zap.String("for", whose), zap.String("name", b.Name))
	}
	a.logger.Info("bound a value the test made, given by its harness, as the test's variable",
		zap.String("for", whose), zap.String("name", b.Name), zap.Bool("claim_key", b.Outside))
	return nil
}

// givenValue is the value vs gives under name.
func givenValue(vs []models.GivenValue, name string) (string, bool) {
	for _, v := range vs {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}
