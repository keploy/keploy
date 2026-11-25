package http

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ciWord is the repository's word of ciKeyPut's request, recorded or as F
// sent it: a UnixNano, or with hex a random word.
func ciWord(hex, live bool) string {
	switch {
	case hex && live:
		return "e5f6a7b8c9"
	case hex:
		return "a1b2c3d4e5"
	case live:
		return strconv.FormatInt(learnAt.Add(10*time.Minute+100*time.Millisecond).UnixNano(), 10)
	}
	return strconv.FormatInt(ciRecBegin().Add(100*time.Millisecond).UnixNano(), 10)
}

// answersAt reports whether a variable m learned is answered at place at.
func answersAt(m models.Mock, at string) bool {
	for _, v := range m.Spec.Learned() {
		if containsString(v.Answer, at) {
			return true
		}
	}
	return false
}

// A variable bound at a word of a client test's request is answered where
// the call's answer holds that word whole, with the word F's request sent.
// An answer that holds the whole name, the longer value the word sits in,
// is answered as recorded there: keploy answers a variable only at a place
// that holds its value whole, and the value is the word, never the name.
// For a UnixNano word and a random one, under both time settings.
func TestLearnMatchAnswersAClientTestsWordWhereTheAnswerHoldsItWhole(t *testing.T) {
	for _, hex := range []bool{false, true} {
		for _, timeForm := range []bool{true, false} {
			served, miss, db := ciKeyPut(t, ciKeyCase{hex: hex, respWord: true}, timeForm)
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("the answer holds the word whole (random word %v, time default %v): served %v, miss %+v; want the PUT answered", hex, timeForm, served != nil, miss)
			}
			if want := `{"data":{"ref":"` + ciWord(hex, true) + `","version":1}}`; served.Spec.HTTPResp.Body != want || !answersAt(db.consumed[0], "body.data.ref") {
				t.Errorf("random word %v, time default %v: answer %s, variables %+v; want %s, the word answered at body.data.ref", hex, timeForm, served.Spec.HTTPResp.Body, db.consumed[0].Spec.Learned(), want)
			}

			served, miss, db = ciKeyPut(t, ciKeyCase{hex: hex, respName: true}, timeForm)
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("the answer holds the whole name (random word %v, time default %v): served %v, miss %+v; want the PUT answered", hex, timeForm, served != nil, miss)
			}
			if want := `{"data":{"name":"CI identity: github octo-org/repo-` + ciWord(hex, false) + `","version":1}}`; served.Spec.HTTPResp.Body != want || answersAt(db.consumed[0], "body.data.name") {
				t.Errorf("random word %v, time default %v: answer %s, variables %+v; want %s, the name as recorded and answered by no variable", hex, timeForm, served.Spec.HTTPResp.Body, db.consumed[0].Spec.Learned(), want)
			}
		}
	}
}

// A client test's request that holds the record's name whole binds it
// there, whole, before a field the request sent earlier in its order that
// holds the one word the name changed by: the whole place ranks first
// (clientWholeRank), the word's after it. For a UnixNano word and a random
// one, under both time settings: the field drifted with the name, so the
// search finds it there before it reads any echo of an instant.
func TestLearnMatchBindsAClientTestsWholeValueBeforeAFieldThatHoldsItsWord(t *testing.T) {
	for _, hex := range []bool{false, true} {
		for _, timeForm := range []bool{true, false} {
			served, miss, db := ciKeyPut(t, ciKeyCase{hex: hex, label: true}, timeForm)
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("the request holds the name whole (random word %v, time default %v): served %v, miss %+v; want the PUT answered", hex, timeForm, served != nil, miss)
			}
			m := db.consumed[0]
			if !bindsAt(m, "body.variables.label") || bindsAt(m, "body.variables.input.conditions[0].value#3") {
				t.Errorf("random word %v, time default %v: binds %+v; want the name bound whole at the test case's body.variables.label, none at the repository's word", hex, timeForm, m.Spec.LearnedBinds())
			}
			var uses []string
			for _, v := range m.Spec.Learned() {
				uses = append(uses, v.Use...)
			}
			if !containsString(uses, "body.data.name") || containsString(uses, "body.data.name#6") {
				t.Errorf("random word %v, time default %v: uses %v; want the name used whole at body.data.name", hex, timeForm, uses)
			}
		}
	}
}

// A field of the client test's request that F sent as recorded, holding
// the recorded name whole, after the field that holds the name's word: the
// search for where the name came from passes over that field, which did not
// drift, and binds the word at the repository's field. For a random word
// that is the direct search (appearedIn); a UnixNano word is found as the
// echo of its instant (echoServed), which passes over it too. Under both
// time settings.
func TestLearnMatchPassesOverAClientFieldSentAsRecorded(t *testing.T) {
	for _, hex := range []bool{true, false} {
		for _, timeForm := range []bool{true, false} {
			served, miss, db := ciKeyPut(t, ciKeyCase{hex: hex, label: true, labelLive: ciWord(hex, false)}, timeForm)
			if served == nil || miss != nil || len(db.consumed) != 1 || !bindsAt(db.consumed[0], "body.variables.input.conditions[0].value#3") || bindsAt(db.consumed[0], "body.variables.label") {
				t.Fatalf("random word %v, the name sent as recorded (time default %v): served %v, miss %+v; want the PUT answered, the word bound at the repository's field", hex, timeForm, served != nil, miss)
			}
		}
	}
}

// With api-server's keploy.yml rules alone (createdAt and updatedAt
// declared clock reads, no rule at expiresAt), the key record's PUT is
// answered under the time default, the suite's setting, the name's word and
// the account's id bound. When times are strict it is refused at
// expiresAt, whose moment stands a year after the repository's stamp, which
// no rule declares: a time keploy checks by no rule is refused, never
// passed by luck.
func TestLearnMatchKeysPutUnderTheSuitesRulesAlone(t *testing.T) {
	served, miss, db := ciKeyPut(t, ciKeyCase{suiteRules: true}, true)
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("the suite's rules alone, under the time default: served %v, miss %+v; want the PUT answered", served != nil, miss)
	}
	if !bindsAt(db.consumed[0], "body.variables.input.conditions[0].value#3") || !bindsAt(db.consumed[0], "body.variables.sa") {
		t.Errorf("binds %+v; want the word and the account's id bound at the test case", db.consumed[0].Spec.LearnedBinds())
	}
	const strict = "body.data.expiresAt changed, and its moment, to the second, stands 8760h0m0s after the moment test case post-query-83 at body.variables.input.conditions[0].value"
	served, miss, db = ciKeyPut(t, ciKeyCase{suiteRules: true}, false)
	if served != nil || miss == nil || len(db.consumed) != 0 || !strings.Contains(miss.res.Why, strict) {
		t.Fatalf("the suite's rules alone, times strict: served %v, miss %+v; want it refused: %s", served != nil, miss, strict)
	}
}

// A call that does not hold the value a place of the run sent, as it did
// when recorded, is refused naming whose call that place is: an earlier
// test's, or the test set's start-up's, never "the test" running. A place of
// the test's own call is named as the test's. Under the proven switch, where
// the run's places carry their variables.
func TestLearnPlanNamesWhoseCallSentAValueTheCallDoesNotHold(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tr := learn.NewTracker()
	tr.Begin()
	test := tr.Epoch()
	m := &models.Mock{Name: "mock-8"}
	rec := learn.Leaf{Path: "body.expiresAt", Value: "1825982691", Type: learn.Number}
	live := learn.Leaf{Path: "body.expiresAt", Value: "1825985624", Type: learn.Number}
	jsonReq := learn.Shape{Fixed: map[string]string{"body": "json"}}
	base := learn.Place{Mock: "mock-3", Side: learn.Request, Path: "body.data.expiresAt", Var: "body.data.expiresAt", Live: "1825985630", Whole: true}
	for _, tc := range []struct {
		what  string
		from  func(learn.Place) learn.Place
		want  string
		wrong string
	}{
		{"an earlier test's call", func(p learn.Place) learn.Place { p.Run = true; return p },
			"body.expiresAt does not hold the value mock-3, an earlier test's call, sent at body.data.expiresAt, which it held when recorded", "the test sent"},
		{"the start-up's call", func(p learn.Place) learn.Place { p.Run, p.InStartUp = true, true; return p },
			"body.expiresAt does not hold the value mock-3, the test set's start-up's call, sent at body.data.expiresAt, which it held when recorded", "the test sent"},
		{"the test's own call", func(p learn.Place) learn.Place { p.Own = true; return p },
			"body.expiresAt does not hold the value the test sent at body.data.expiresAt of mock-3, which it held when recorded", "earlier test"},
	} {
		learned := []learn.Learned{{Kind: learn.KindVariable, Recorded: rec, Live: live, From: tc.from(base)}}
		p, why := learnPlan(tr, test, m, jsonReq, true, learned, nil, nil)
		if why != tc.want || strings.Contains(why, tc.wrong) || len(p.vars) != 0 {
			t.Errorf("%s: why %q, variables %v; want it refused: %s", tc.what, why, p.vars, tc.want)
		}
	}
}
