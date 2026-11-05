package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A learned value (models.MockVariable.Of) goes with its bind when two
// declarations of one variable in one call merge: taken with a bind the
// first lacks, kept where both bind naming one value, dropped where they
// name two or one names none.
func TestMergeVariable_OfGoesWithItsBind(t *testing.T) {
	const of, other = "6f9619ff-8b86-4011-b42d-00c04fc964ff", "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
	bound := models.MockVariable{Recorded: "4011", Bind: "body.id#2", Of: of}
	use := models.MockVariable{Recorded: "4011", Use: []string{"body.x"}}
	for _, c := range []struct {
		name string
		a, b models.MockVariable
		want string
	}{
		{"a bind, then a use", bound, use, of},
		{"a use, then a bind", use, bound, of},
		{"two binds naming one value", bound, bound, of},
		{"two binds naming two values", bound, models.MockVariable{Recorded: "4011", Bind: "body.id#2", Of: other}, ""},
		{"a bind, then one naming none", bound, models.MockVariable{Recorded: "4011", Bind: "body.id#2"}, ""},
	} {
		if got := MergeVariable(c.a, c.b).Of; got != c.want {
			t.Errorf("%s: Of %q, want %q", c.name, got, c.want)
		}
	}
}

// A word's learned value (Of) is the owning call's, the call that made the
// word: a later call that uses the variable declares where it is bound (a
// bind at an earlier recording, MockState.Binds) and names no value, so the
// value stays on the recording as the owning call learned it, whether that
// call's learning is in the same states or already on the recording. Where
// the owning call's own declarations name none or another, none stays, and
// a later call that names the bind elsewhere leaves none either.
func TestMergeLearned_ALaterUseLeavesTheOwningCallsLearnedValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const of, other, at = "b0743d2d-c52f-4d6b-8524-272b094a1c9c", "e8e0f266-2cf7-4ed4-8d6c-0250bdbf2acf", "body.docs.msgs.0.appId#2"
	own := models.MockVariable{Recorded: "4d6b", Bind: at, Of: of, Why: "made anew at mock-217"}
	later := func(bind string) models.MockState {
		return models.MockState{Name: "mock-225",
			Variables: models.MockVariables{"appId2": {Recorded: "4d6b", Use: []string{"body.docs.msgs.0.app._id#2"}}},
			Binds:     map[string]models.MockVariables{"mock-217": {"appId2": {Recorded: "4d6b", Bind: bind, Why: "bound at mock-217"}}}}
	}
	merged := func(onDisk *models.MockVariable, mine *models.MockVariable, use models.MockState) models.MockVariable {
		states := map[string]models.MockState{"mock-225": use}
		if mine != nil {
			states["mock-217"] = models.MockState{Name: "mock-217", Variables: models.MockVariables{"appId2": *mine}}
		}
		withLearning, binds := FoldLearned(states)
		m := &models.Mock{Name: "mock-217"}
		if onDisk != nil {
			m.Spec.Variables = models.MockVariables{"appId2": *onDisk}
		}
		MergeLearned(m, withLearning["mock-217"], binds["mock-217"])
		return m.Spec.Variables["appId2"]
	}
	noOf, otherOf := own, own
	noOf.Of, otherOf.Of = "", other
	for _, c := range []struct {
		name         string
		onDisk, mine *models.MockVariable
		use          models.MockState
		want         string
	}{
		{"the owning call's learning beside a later use", nil, &own, later(at), of},
		{"on the recording already, a later use learned after", &own, nil, later(at), of},
		{"on the recording and learned again, beside a later use", &own, &own, later(at), of},
		{"the owning call names none", nil, &noOf, later(at), ""},
		{"learned again naming none, beside a later use", &own, &noOf, later(at), ""},
		{"learned again naming another", &own, &otherOf, later(at), ""},
		{"on the recording with none, a later use learned after", &noOf, nil, later(at), ""},
		{"a later use naming the bind at another word", &own, nil, later("body.docs.msgs.0.appId#3"), ""},
	} {
		if got := merged(c.onDisk, c.mine, c.use); got.Of != c.want {
			t.Errorf("%s: Of %q, want %q (%+v)", c.name, got.Of, c.want, got)
		}
	}
}
