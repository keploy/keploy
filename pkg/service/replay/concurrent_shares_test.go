package replay

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// shareKind is a kind whose fake keys read a recording's metadata: stmt is its
// statement, req its sent values ("place=value;..."), ans its answer, and
// share=no makes it not shareable.
const shareKind models.Kind = "ShareTest"

var shareT0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func registerShareKeys(t *testing.T) {
	t.Helper()
	positions := func(m *models.Mock) map[string]string {
		out := map[string]string{}
		for _, kv := range strings.Split(m.Spec.Metadata["req"], ";") {
			if i := strings.IndexByte(kv, '='); i > 0 {
				out[kv[:i]] = kv[i+1:]
			}
		}
		return out
	}
	restore := models.RegisterConcurrentKeys(shareKind, models.ConcurrentKeys{
		Statement: func(m *models.Mock) (string, bool) { return m.Spec.Metadata["stmt"], m.Spec.Metadata["stmt"] != "" },
		Request: func(m *models.Mock, excused []string) (string, bool) {
			ex := map[string]bool{}
			for _, k := range excused {
				ex[k] = true
			}
			pos := positions(m)
			var parts []string
			for _, k := range keysSorted(pos) {
				if ex[k] {
					parts = append(parts, k+"=*")
				} else {
					parts = append(parts, k+"="+pos[k])
				}
			}
			return m.Spec.Metadata["stmt"] + "|" + strings.Join(parts, ";"), true
		},
		Answer: func(m *models.Mock, echoes []string) (string, bool) {
			a := m.Spec.Metadata["ans"]
			pos := positions(m)
			for _, k := range echoes {
				if v := pos[k]; v != "" {
					a = strings.ReplaceAll(a, v, "<"+k+">")
				}
			}
			return a, true
		},
		Positions: positions,
		Shareable: func(m *models.Mock) (bool, string) {
			if m.Spec.Metadata["share"] == "no" {
				return false, "noise"
			}
			return true, ""
		},
	})
	t.Cleanup(restore)
}

// shMock is a recording of statement stmt sending req, answered ans, at ms.
func shMock(name string, ms int, stmt, req, ans string) *models.Mock {
	return &models.Mock{Name: name, Kind: shareKind, Spec: models.MockSpec{
		Metadata:         map[string]string{"stmt": stmt, "req": req, "ans": ans},
		ReqTimestampMock: shareT0.Add(time.Duration(ms) * time.Millisecond),
		ResTimestampMock: shareT0.Add(time.Duration(ms+1) * time.Millisecond),
	}}
}

func shEntry(m *models.Mock, group string) models.MockEntry {
	return models.MockEntry{Name: m.Name, Kind: string(m.Kind), ReqTimestampMock: models.FormatMockTimestamp(m.Spec.ReqTimestampMock), Group: group}
}

// shareSet is a set to settle: its plan, and each test's result.
type shareSet struct {
	p        *exactPlan
	mappings map[string][]models.MockEntry
	results  []models.TestResult
	cons     map[string][]string
}

func newShareSet(t *testing.T, groups []models.ConcurrencyGroup, mappings map[string][]models.MockEntry, shared []models.MockEntry, worker []models.MockEntry, mocks ...*models.Mock) *shareSet {
	t.Helper()
	conc := concurrencyOf(groups, mappings, shared)
	p := newExactPlan(mappings, worker, mocks)
	p.share = newSharePlan(conc, worker, mocks)
	var cases []*models.TestCase
	for _, test := range keysSorted(mappings) {
		cases = append(cases, &models.TestCase{Name: test, Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: "http://app/companies", Body: `{"name":"` + test + `"}`}})
	}
	narrowed := p.shareConcurrent(mappings, cases)
	return &shareSet{p: p, mappings: narrowed, cons: map[string][]string{}}
}

// run runs test, which consumes consumed.
func (s *shareSet) run(test string, consumed ...models.MockState) {
	s.p.begin(test)
	s.p.noteShares(test, consumed, nil)
	noteConsumers(s.cons, test, consumed)
	s.results = append(s.results, models.TestResult{TestCaseID: test, Status: models.TestStatusPassed})
}

func (s *shareSet) settle(t *testing.T) []string {
	t.Helper()
	return s.p.settleShares(s.results, s.cons, true, map[string]mockDisplayInfo{})
}

func used(name string) models.MockState { return models.MockState{Name: name, Kind: shareKind} }

func learned(name string, places ...string) models.MockState {
	st := used(name)
	st.ReqBodyNoise = map[string][]string{}
	for _, p := range places {
		st.ReqBodyNoise[p] = []string{}
	}
	return st
}

// resultOf is test's last result.
func (s *shareSet) resultOf(test string) models.TestResult {
	var out models.TestResult
	for _, r := range s.results {
		if r.TestCaseID == test {
			out = r
		}
	}
	return out
}

func reasonsOf(r models.TestResult) []string {
	var out []string
	for _, c := range r.FailureInfo.UnmatchedCalls {
		out = append(out, c.Reason+":"+c.ClosestMock)
	}
	for _, u := range r.FailureInfo.UnconsumedMocks {
		out = append(out, "unconsumed:"+u.Name)
	}
	sort.Strings(out)
	return out
}

// A grouped, shareable recording is a candidate of every test of its group
// and required of none; the test's own (definite) recordings are what it must
// make. A recording that is not shareable, or of a kind that keys none, stays
// the test that lists it. A shared recording (Mapping.Shared) is in no pool.
func TestShareConcurrentSplitsDefiniteFromCandidates(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	own := shMock("own-a", 100, "INSERT", "bind.$1=x", "ok")
	c1 := shMock("c1", 200, "INSERT", "bind.$1=a", "ok")
	c2 := shMock("c2", 300, "INSERT", "bind.$1=b", "ok")
	stated := shMock("stated", 350, "INSERT", "bind.$1=s", "ok")
	stated.Spec.Metadata["share"] = "no"
	http := &models.Mock{Name: "http-call", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0.Add(400 * time.Millisecond)}}
	sharedOne := shMock("s1", 500, "INSERT", "bind.$1=c", "ok")
	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}, {ID: "g2", Tests: []string{"b"}, Unkept: 1}}
	mappings := map[string][]models.MockEntry{
		"a": {shEntry(own, ""), shEntry(c1, "g1"), shEntry(c2, "g1"), shEntry(stated, "g1"), {Name: "http-call", Kind: "Http", Group: "g1"}},
		"b": {},
	}
	s := newShareSet(t, groups, mappings, []models.MockEntry{shEntry(sharedOne, "g2")}, nil, own, c1, c2, stated, http, sharedOne)
	names := func(es []models.MockEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Name)
		}
		return out
	}
	if got := names(s.mappings["a"]); !reflect.DeepEqual(got, []string{"own-a", "stated", "http-call"}) {
		t.Fatalf("a is required %v; want its definite own-a, stated (not shareable) and http-call (a kind that keys none)", got)
	}
	if got := s.mappings["b"]; len(got) != 0 {
		t.Fatalf("b is required %v; want nothing", names(got))
	}
	if !reflect.DeepEqual(s.p.owned["a"], []string{"own-a", "stated", "http-call"}) {
		t.Fatalf("a owns %v", s.p.owned["a"])
	}
	cands, _, inputs := s.p.concurrentServing("a")
	if !reflect.DeepEqual(cands, []string{"c1", "c2"}) {
		t.Fatalf("a's candidates %v; want [c1 c2]", cands)
	}
	if cands, _, _ := s.p.concurrentServing("b"); !reflect.DeepEqual(cands, []string{"c1", "c2", "s1"}) {
		t.Fatalf("b's candidates %v; want [c1 c2 s1]", cands)
	}
	if !reflect.DeepEqual(inputs, []string{"", "companies", "a"}[1:]) {
		t.Fatalf("a's inputs %v; want its path segment and its body's name", inputs)
	}
	for _, n := range s.p.pool {
		if n == "s1" {
			t.Fatal("a shared recording is in the pool")
		}
	}
}

// A candidate holding, at a place that tells its statement's recordings apart,
// a value another test of its group sent that this test did not, is never
// paired by drift with a call of this test.
func TestNotLearnableRefusesAnotherTestsInput(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	ca := shMock("ca", 100, "INSERT", "bind.$1=a;bind.$2=t1", "ok")
	cb := shMock("cb", 200, "INSERT", "bind.$1=b;bind.$2=t1", "ok")
	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}
	s := newShareSet(t, groups, map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(cb, "g1")}, "b": {}}, nil, nil, ca, cb)
	if _, nl, _ := s.p.concurrentServing("a"); !reflect.DeepEqual(nl, []string{"cb"}) {
		t.Fatalf("a may not pair with %v; want [cb], which holds b's name", nl)
	}
	if _, nl, _ := s.p.concurrentServing("b"); !reflect.DeepEqual(nl, []string{"ca"}) {
		t.Fatalf("b may not pair with %v; want [ca]", nl)
	}
}

// Each candidate a test consumed is pinned to it; nobody is charged.
func TestSettleSharesPinsEachCallToTheTestThatMadeIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	ca := shMock("ca", 100, "INSERT", "bind.$1=a", "id-a")
	cb := shMock("cb", 200, "INSERT", "bind.$1=b", "id-b")
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
		map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(cb, "g1")}, "b": {}}, nil, nil, ca, cb)
	s.run("a", used("ca"))
	s.run("b", used("cb"))
	if charged := s.settle(t); len(charged) != 0 {
		t.Fatalf("charged %v", charged)
	}
	got, ok := s.p.setShares(models.TestSetStatusPassed)
	if !ok || got.State != models.SharesComplete || !reflect.DeepEqual(got.Pins, map[string]string{"ca": "a", "cb": "b"}) || len(got.Moves) != 0 || len(got.ToShared) != 0 {
		t.Fatalf("shares %+v", got)
	}
	if got, _ := s.p.setShares(models.TestSetStatusInternalErr); got.State != models.SharesIncomplete {
		t.Fatalf("a set not judged keeps its shares: %+v", got)
	}
}

// A candidate no test made, with every test of its group kept, charges every
// one of them that ran, once each; with a test the recording did not keep, no
// one is charged and it is shared.
func TestSettleSharesLeftovers(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	t.Run("every test kept", func(t *testing.T) {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		left := shMock("left", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b", "c"}}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(left, "g1")}, "b": {}, "c": {}}, nil, nil, ca, left)
		s.run("a", used("ca"))
		s.run("b")
		// c did not run.
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want a and b, the tests of its group that ran", charged)
		}
		for _, test := range []string{"a", "b"} {
			r := s.resultOf(test)
			if r.Status != models.TestStatusFailed || !reflect.DeepEqual(reasonsOf(r), []string{"unconsumed:left"}) {
				t.Fatalf("%s: %s %v", test, r.Status, reasonsOf(r))
			}
			if u := r.FailureInfo.UnconsumedMocks[0]; u.Concurrent == "" || strings.Contains(u.Concurrent, test) {
				t.Fatalf("%s: the leftover names %q", test, u.Concurrent)
			}
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Pins["ca"] != "a" || got.Moves["left"].To != "a" {
			t.Fatalf("shares %+v; want ca pinned and left staying with a, which is charged", got)
		}
	})
	t.Run("the test whose call came closest is named", func(t *testing.T) {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		left := shMock("left", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(left, "g1")}, "b": {}}, nil, nil, ca, left)
		s.run("a", used("ca"))
		s.run("b")
		// b's call drifted from left, and the learn step refused it.
		s.results[1].Status = models.TestStatusFailed
		s.results[1].FailureInfo.UnmatchedCalls = []models.UnmatchedCall{{Protocol: "PostgreSQL", ClosestMock: "left", Kind: models.MissInTest, Reason: models.MissRequestChanged}}
		s.settle(t)
		for _, test := range []string{"a", "b"} {
			us := s.resultOf(test).FailureInfo.UnconsumedMocks
			if len(us) != 1 || us[0].Closest != "b" {
				t.Fatalf("%s: unconsumed %+v; want left naming b, whose call came closest", test, us)
			}
		}
	})
	t.Run("a test not kept", func(t *testing.T) {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		left := shMock("left", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}, Unkept: 1}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(left, "g1")}}, nil, nil, ca, left)
		s.run("a", used("ca"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v; a test the recording did not keep may have made it", charged)
		}
		got, _ := s.p.setShares(models.TestSetStatusPassed)
		if !reflect.DeepEqual(got.ToShared, []string{"left"}) || got.Pins["ca"] != "a" {
			t.Fatalf("shares %+v; want left shared", got)
		}
	})
}

// Recordings keploy cannot tell apart (equal requests once every place learned
// in their component is excused) that got different answers make an
// ambiguous class: every test that consumed one, and every candidate test of
// one none consumed, is charged; equal answers pin as usual.
func TestSettleSharesAmbiguousClasses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	t.Run("twins with different answers", func(t *testing.T) {
		t1 := shMock("t1", 100, "INSERT", "bind.$1=same", "201")
		t2 := shMock("t2", 200, "INSERT", "bind.$1=same", "23505")
		other := shMock("other", 300, "INSERT", "bind.$1=c", "201")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b", "c"}}},
			map[string][]models.MockEntry{"a": {shEntry(t1, "g1"), shEntry(t2, "g1"), shEntry(other, "g1")}, "b": {}, "c": {}}, nil, nil, t1, t2, other)
		s.run("a", used("t1"))
		s.run("b", used("t2"))
		s.run("c", used("other"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want the twins' tests", charged)
		}
		if got := reasonsOf(s.resultOf("a")); !reflect.DeepEqual(got, []string{models.MissConcurrentAmbiguous + ":t1"}) {
			t.Fatalf("a: %v", got)
		}
		c := s.resultOf("b").FailureInfo.UnmatchedCalls[0]
		if c.Concurrent == nil || !reflect.DeepEqual(c.Concurrent.Class, []string{"t1", "t2"}) || c.Concurrent.Case != models.ConcurrentCaseClass || !c.CountsAgainstTest() {
			t.Fatalf("b's charge %+v", c)
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		want := map[string]models.ShareMove{"t1": {To: "a", Marker: models.MockEntryUnresolvedAmbiguous}, "t2": {To: "b", Marker: models.MockEntryUnresolvedAmbiguous}}
		if !reflect.DeepEqual(got.Moves, want) || got.Pins["other"] != "c" {
			t.Fatalf("shares %+v", got)
		}
	})
	t.Run("an unconsumed twin charges its candidate tests and its owner keeps it", func(t *testing.T) {
		t1 := shMock("t1", 100, "INSERT", "bind.$1=same", "201")
		t2 := shMock("t2", 200, "INSERT", "bind.$1=same", "23505")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(t1, "g1"), shEntry(t2, "g1")}, "b": {}}, nil, nil, t1, t2)
		s.run("a", used("t1"))
		s.run("b")
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want both: a consumed t1, and b is a candidate test of t2, which none consumed", charged)
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Moves["t2"].To != "a" || got.Moves["t2"].Marker != models.MockEntryUnresolvedAmbiguous {
			t.Fatalf("t2 goes %+v; want it to stay with a, charged, marked", got.Moves["t2"])
		}
	})
	t.Run("equal answers pin", func(t *testing.T) {
		t1 := shMock("t1", 100, "SELECT", "bind.$1=same", "row")
		t2 := shMock("t2", 200, "SELECT", "bind.$1=same", "row")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(t1, "g1"), shEntry(t2, "g1")}, "b": {}}, nil, nil, t1, t2)
		s.run("a", used("t1"))
		s.run("b", used("t2"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v for recordings that answer alike", charged)
		}
	})
	t.Run("a chain links what no one view shows together", func(t *testing.T) {
		// a sees x (own) and y; c sees y and z. x and z are never in one view,
		// but y links them: x and z, equal and answered differently, are
		// one ambiguous class.
		x := shMock("x", 100, "INSERT", "bind.$1=same", "201")
		y := shMock("y", 200, "INSERT", "bind.$1=y", "201")
		z := shMock("z", 300, "INSERT", "bind.$1=same", "23505")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}, {ID: "g2", Tests: []string{"b", "c"}}},
			map[string][]models.MockEntry{"a": {shEntry(x, ""), shEntry(y, "g1")}, "b": {shEntry(z, "g2")}, "c": {}}, nil, nil, x, y, z)
		s.run("a", used("x"))
		s.run("b", used("y"))
		s.run("c", used("z"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "c"}) {
			t.Fatalf("charged %v; want a and c, whose recordings x and z keploy cannot tell apart", charged)
		}
	})
	t.Run("a worker-scope recording is in the class", func(t *testing.T) {
		w := shMock("w", 50, "INSERT", "bind.$1=same", "23505")
		c := shMock("c", 100, "INSERT", "bind.$1=same", "201")
		worker := []models.MockEntry{{Name: "w", Kind: string(shareKind)}}
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}, nil, worker, w, c)
		s.run("a", used("c"))
		s.run("b", used("w"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want both", charged)
		}
	})
	t.Run("modulo what was learned", func(t *testing.T) {
		// $2 drifted (a clock) and was learned on one: excused, the two
		// recordings are equal, and their answers differ.
		c1 := shMock("c1", 100, "INSERT", "bind.$1=same;bind.$2=t1", "201")
		c2 := shMock("c2", 200, "INSERT", "bind.$1=same;bind.$2=t2", "23505")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(c1, "g1"), shEntry(c2, "g1")}, "b": {}}, nil, nil, c1, c2)
		s.run("a", learned("c1", "bind.$2"))
		s.run("b", used("c2"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want both", charged)
		}
	})
}

// A conflict (the learn step paired a call of a with a recording b answered
// with) charges b too, in either order, and the recording goes to b.
func TestSettleSharesChargesAConflictsClaimant(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}, nil, nil, c)
		for _, test := range order {
			if test == "b" {
				s.run("b", used("c"))
				continue
			}
			s.run("a")
			s.results[len(s.results)-1].Status = models.TestStatusFailed
			s.results[len(s.results)-1].FailureInfo.UnmatchedCalls = []models.UnmatchedCall{{Protocol: "ShareTest", ClosestMock: "c", Kind: models.MissInTest,
				Reason: models.MissConcurrentConflict, Concurrent: &models.ConcurrentNote{Claimant: "b"}}}
		}
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"b"}) {
			t.Fatalf("order %v: charged %v; want b, the claimant (a has its own miss)", order, charged)
		}
		if c := s.resultOf("b").FailureInfo.UnmatchedCalls; len(c) != 1 || c[0].Concurrent.Case != models.ConcurrentCaseConflict || c[0].Concurrent.Claimant != "a" {
			t.Fatalf("order %v: b's charge %+v", order, c)
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Moves["c"].To != "b" || got.Pins["c"] != "" {
			t.Fatalf("order %v: shares %+v; want c moved to b, not pinned", order, got)
		}
	}
}

// A learned candidate pair pins only when another learned candidate pair of
// its statement drifted at those places at least, or a pair of a test's own
// recording did; otherwise its test is charged.
func TestSettleSharesCorroboratesALearnedPair(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	mk := func() (*shareSet, []*models.Mock) {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a;bind.$2=t1", "ok")
		cb := shMock("cb", 200, "INSERT", "bind.$1=b;bind.$2=t2", "ok")
		own := shMock("own", 50, "INSERT", "bind.$1=o;bind.$2=t0", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(cb, "g1")}, "b": {}, "o": {shEntry(own, "")}}, nil, nil, ca, cb, own)
		return s, nil
	}
	t.Run("two candidate pairs drifted alike", func(t *testing.T) {
		s, _ := mk()
		s.run("a", learned("ca", "bind.$2"))
		s.run("b", learned("cb", "bind.$2"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
	})
	t.Run("one candidate pair alone", func(t *testing.T) {
		s, _ := mk()
		s.run("a", learned("ca", "bind.$2"))
		s.run("b", used("cb"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a"}) {
			t.Fatalf("charged %v; want a, whose drift nothing corroborates", charged)
		}
		if n := s.resultOf("a").FailureInfo.UnmatchedCalls[0].Concurrent; n.Case != models.ConcurrentCaseCorroboration || !reflect.DeepEqual(n.Places, []string{"bind.$2"}) {
			t.Fatalf("a's charge says %+v", n)
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Moves["ca"].To != "a" || got.Pins["ca"] != "" {
			t.Fatalf("shares %+v; want ca with a, charged", got)
		}
	})
	t.Run("corroborated by a test's own pair", func(t *testing.T) {
		s, _ := mk()
		s.run("a", learned("ca", "bind.$2"))
		s.run("b", used("cb"))
		s.run("o", learned("own", "bind.$2"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
	})
	t.Run("drifted wider than the other", func(t *testing.T) {
		s, _ := mk()
		s.run("a", learned("ca", "bind.$1", "bind.$2"))
		s.run("b", learned("cb", "bind.$2"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a"}) {
			t.Fatalf("charged %v; want a", charged)
		}
	})
}

// A candidate answered for different tests across attempts, or not on its
// test's last attempt, charges every candidate test of it.
func TestSettleSharesARetryDisagreementChargesEveryCandidateTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
		map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}, nil, nil, c)
	s.run("a", used("c"))
	s.run("a") // the retry did not make it
	s.run("b")
	if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
		t.Fatalf("charged %v", charged)
	}
}

// What cannot be settled leaves the set's shares incomplete: its run did not
// complete, or what a test served shared recordings consumed was not read.
// Nothing is charged, and mappings.yaml is to stay as recorded.
func TestSettleSharesIncomplete(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	mk := func() *shareSet {
		c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
		return newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}, nil, nil, c)
	}
	s := mk()
	s.run("a", used("c"))
	if charged := s.p.settleShares(s.results, s.cons, false, nil); charged != nil {
		t.Fatalf("charged %v", charged)
	}
	if got, _ := s.p.setShares(models.TestSetStatusPassed); got.State != models.SharesIncomplete || got.Any() {
		t.Fatalf("an incomplete run settled %+v", got)
	}
	s = mk()
	s.p.begin("a")
	s.p.noteShares("a", nil, errNotRead)
	s.results = append(s.results, models.TestResult{TestCaseID: "a", Status: models.TestStatusPassed})
	s.settle(t)
	if got, _ := s.p.setShares(models.TestSetStatusPassed); got.State != models.SharesIncomplete || !strings.Contains(got.Why, "a consumed") {
		t.Fatalf("an unread consumption settled %+v", got)
	}
	s = mk()
	s.p.begin("a") // opened, never read
	s.results = append(s.results, models.TestResult{TestCaseID: "a", Status: models.TestStatusPassed})
	s.settle(t)
	if got, _ := s.p.setShares(models.TestSetStatusPassed); got.State != models.SharesIncomplete {
		t.Fatalf("a run never read settled %+v", got)
	}
	s = mk()
	s.run("b")
	s.settle(t)
	if got, _ := s.p.setShares(models.TestSetStatusPassed); got.State != models.SharesComplete {
		t.Fatalf("got %+v", got)
	}
}

var errNotRead = errorString("the agent did not answer")

type errorString string

func (e errorString) Error() string { return string(e) }

// A shared recording no test the settlement charged may keep: when every
// candidate test of an unconsumed twin is gone, the shares are incomplete.
func TestSettleSharesNeverLeavesAnUnchargedTestACallItDidNotMake(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	t1 := shMock("t1", 100, "INSERT", "bind.$1=same", "201")
	t2 := shMock("t2", 200, "INSERT", "bind.$1=same", "23505")
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
		map[string][]models.MockEntry{"a": {shEntry(t1, "g1"), shEntry(t2, "g1")}, "b": {}}, nil, nil, t1, t2)
	// Only a ran, and the twin class is decided by a's consumption.
	s.run("a", used("t1"))
	s.settle(t)
	got, _ := s.p.setShares(models.TestSetStatusFailed)
	for name, mv := range got.Moves {
		if s.resultOf(mv.To).Status != models.TestStatusFailed {
			t.Fatalf("%s moved to %s, which was not charged", name, mv.To)
		}
	}
}

// What was learned on a shared recording the settlement did not pin is not
// written onto it: its pairing is not proven. A pinned one's is.
func TestStripUnresolvedDropsWhatWasLearnedOnAnUnpinnedRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	ca := shMock("ca", 100, "INSERT", "bind.$1=a;bind.$2=t1", "ok")
	cb := shMock("cb", 200, "INSERT", "bind.$1=b;bind.$2=t2", "ok")
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
		map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(cb, "g1")}, "b": {}}, nil, nil, ca, cb)
	s.run("a", learned("ca", "bind.$2"))
	s.run("b", learned("cb", "bind.$1", "bind.$2"))
	s.settle(t)
	got := s.p.stripUnresolved(map[string]models.MockState{"ca": learned("ca", "bind.$2"), "cb": learned("cb", "bind.$1", "bind.$2")})
	if len(got["ca"].ReqBodyNoise) != 1 || got["cb"].ReqBodyNoise != nil {
		t.Fatalf("kept %+v; want ca's (pinned) and not cb's (uncorroborated)", got)
	}
}

// With the switch off, or a plan that does not share, the settlement does
// nothing.
func TestSettleSharesIsANoOpWithoutSharing(t *testing.T) {
	registerShareKeys(t)
	t.Setenv(provenswitch.Env, "")
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	mappings := map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}
	p := newExactPlan(mappings, nil, []*models.Mock{c})
	p.share = newSharePlan(concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}, mappings, nil), nil, []*models.Mock{c})
	results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusPassed}}
	if charged := p.settleShares(results, nil, true, nil); charged != nil || results[0].Status != models.TestStatusPassed {
		t.Fatalf("switch off: charged %v", charged)
	}
	if _, ok := p.setShares(models.TestSetStatusPassed); ok {
		t.Fatal("a plan that does not share has shares")
	}
	if charged := p.annotateConcurrent(results, nil); charged != nil {
		t.Fatalf("switch off: annotated %v", charged)
	}
}

// Only keploy record's full round (ShareConcurrent, beside Learn) with an
// agent that serves shared recordings narrows a set's plan: the sandbox's and
// keploy mock record's rounds (Learn alone) and every strict replay keep the
// recorded ownership, and an agent that does not say it serves them is said
// so.
func TestPlanSharesOnlyInTheFullRoundWithAnAgentThatServesThem(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	mappings := map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}
	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}
	for _, tc := range []struct {
		name          string
		learn, share  bool
		serves, wants bool
	}{
		{name: "full round", learn: true, share: true, serves: true, wants: true},
		{name: "full round, older agent", learn: true, share: true, serves: false},
		{name: "sandbox or mock record (Learn only)", learn: true, serves: true},
		{name: "strict replay", serves: true},
		{name: "ShareConcurrent without Learn", share: true, serves: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			r := &Replayer{logger: zap.New(core), config: &config.Config{Test: config.Test{Learn: tc.learn, ShareConcurrent: tc.share}}, instrumentation: servesConcurrent(tc.serves)}
			r.exact = newExactPlan(mappings, nil, []*models.Mock{c})
			r.exact.share = newSharePlan(concurrencyOf(groups, mappings, nil), nil, []*models.Mock{c})
			got := r.planShares("test-set-0", mappings, nil)
			narrowed := len(got["a"]) == 0
			if narrowed != tc.wants || r.exact.share.sharing != tc.wants {
				t.Fatalf("narrowed=%v sharing=%v; want %v", narrowed, r.exact.share.sharing, tc.wants)
			}
			warned := logs.FilterMessageSnippet("does not serve a test the recordings it shares").Len() > 0
			if warned != (tc.learn && tc.share && !tc.serves) {
				t.Fatalf("warned=%v", warned)
			}
		})
	}
}

// servesConcurrent is an instrumentation that says whether its agent serves
// shared recordings.
type servesConcurrentInstr struct {
	Instrumentation
	serves bool
}

func servesConcurrent(serves bool) Instrumentation { return servesConcurrentInstr{serves: serves} }

func (s servesConcurrentInstr) ServesConcurrent() bool { return s.serves }

// In a replay that does not share, a miss and an unconsumed recording of a
// test recorded beside others say so: whom with, whom the call's recordings
// were attributed to, and that only the full round shares them. A test that
// lists a recording the full round could not give to one test is charged.
func TestAnnotateConcurrentSaysWhatKeployKnows(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	twin := shMock("twin", 200, "INSERT", "bind.$1=w", "ok")
	e := shEntry(twin, "g1")
	e.Unresolved = models.MockEntryUnresolvedAmbiguous
	mappings := map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {e}, "solo": {}}
	p := newExactPlan(mappings, nil, []*models.Mock{c, twin})
	p.share = newSharePlan(concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}, mappings, nil), nil, []*models.Mock{c, twin})
	results := []models.TestResult{
		{TestCaseID: "b", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "PostgreSQL", Kind: models.MissInTest}}}},
		{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnconsumedMocks: []models.UnconsumedMock{{Name: "c"}}}},
		{TestCaseID: "solo", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "PostgreSQL"}}}},
	}
	charged := p.annotateConcurrent(results, nil)
	if !reflect.DeepEqual(charged, []string{"b"}) {
		t.Fatalf("charged %v; want b, which lists an unresolved recording", charged)
	}
	n := results[0].FailureInfo.UnmatchedCalls[0].Concurrent
	if n == nil || !reflect.DeepEqual(n.With, []string{"a"}) || n.AttributedTo != "a" || n.WhyNot != models.ConcurrentNotSharedStrict {
		t.Fatalf("b's miss says %+v", n)
	}
	if got := results[0].FailureInfo.UnmatchedCalls[1]; got.Reason != models.MissConcurrentAmbiguous || got.ClosestMock != "twin" {
		t.Fatalf("b's charge %+v", got)
	}
	if got := results[1].FailureInfo.UnconsumedMocks[0].Concurrent; got != "b" {
		t.Fatalf("a's unconsumed names %q; want b", got)
	}
	if results[2].FailureInfo.UnmatchedCalls[0].Concurrent != nil {
		t.Fatal("a test recorded alone is said to have run beside others")
	}
}

// A charge of the settlement is a data-plane miss: the evidence floor holds a
// test FAILED that a hook passed, snapshotEvidence and cloneResult carry it,
// and failTestsWithMisses fails a row that carries it.
func TestASharesChargeIsEvidenceEveryReaderHolds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	charge := models.UnmatchedCall{Protocol: "PostgresV3", ClosestMock: "t1", Kind: models.MissInTest, Reason: models.MissConcurrentAmbiguous,
		Concurrent: &models.ConcurrentNote{With: []string{"b"}, Class: []string{"t1", "t2"}}}
	for _, reason := range []string{models.MissConcurrentAmbiguous, models.MissConcurrentConflict} {
		c := charge
		c.Reason = reason
		results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{c}}}}
		if failed := failTestsWithMisses(results); !reflect.DeepEqual(failed, []string{"a"}) || results[0].Status != models.TestStatusFailed {
			t.Fatalf("%s: failTestsWithMisses %v, status %s", reason, failed, results[0].Status)
		}
		snap := snapshotEvidence(results)
		if len(snap.evidence["a"].misses) != 1 {
			t.Fatalf("%s: snapshot %+v", reason, snap.evidence["a"])
		}
		cl := cloneResult(results[0])
		cl.FailureInfo.UnmatchedCalls[0].Reason = "changed"
		if results[0].FailureInfo.UnmatchedCalls[0].Reason != reason {
			t.Fatalf("%s: the clone shares its calls", reason)
		}
		// A hook (auto-noise) passes the test and clears its calls.
		results[0].Status = models.TestStatusPassed
		results[0].FailureInfo.UnmatchedCalls = nil
		held, names := holdEvidenceFloor(results, snap)
		if held[0].Status != models.TestStatusFailed || len(held[0].FailureInfo.UnmatchedCalls) != 1 || held[0].FailureInfo.UnmatchedCalls[0].Reason != reason || !reflect.DeepEqual(names, []string{"a"}) {
			t.Fatalf("%s: the floor left %+v (held %v)", reason, held[0], names)
		}
	}
}

// What was learned on a shared recording is said under the test that made
// its call (models.SetShares.Pins), not under the test that lists it.
func TestLearnedByTestGroupsASharedRecordingByItsConsumer(t *testing.T) {
	added := map[string][]string{"m-a": {"bind.$2 (noise)"}, "m-b": {"bind.$2 (noise)"}, "own": {"bind.$1 (noise)"}}
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "a", Mocks: []models.MockEntry{{Name: "own"}, {Name: "m-a"}, {Name: "m-b"}}},
		{ID: "b"},
	}}
	got := learnedByTest(added, mapping, map[string]string{"m-a": "a", "m-b": "b"})
	want := []learnedLine{
		{test: "a", fields: []string{"own bind.$1 (noise)", "m-a bind.$2 (noise)"}},
		{test: "b", fields: []string{"m-b bind.$2 (noise)"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
