package replay

import (
	"reflect"
	"slices"
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
	narrowed := p.shareConcurrent(mappings)
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
	if cands := s.p.concurrentServing("a"); !reflect.DeepEqual(cands, []string{"c1", "c2"}) {
		t.Fatalf("a's candidates %v; want [c1 c2]", cands)
	}
	if cands := s.p.concurrentServing("b"); !reflect.DeepEqual(cands, []string{"c1", "c2", "s1"}) {
		t.Fatalf("b's candidates %v; want [c1 c2 s1]", cands)
	}
	for _, n := range s.p.pool {
		if n == "s1" {
			t.Fatal("a shared recording is in the pool")
		}
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
	if !ok || got.State != models.SharesComplete || !reflect.DeepEqual(got.Pins, map[string]string{"ca": "a", "cb": "b"}) || len(got.Moves) != 0 {
		t.Fatalf("shares %+v", got)
	}
	if got, _ := s.p.setShares(models.TestSetStatusInternalErr); got.State != models.SharesIncomplete {
		t.Fatalf("a set not judged keeps its shares: %+v", got)
	}
}

// A candidate no test made that a kept test lists charges every test of its
// group that ran, once each, whether or not its group held a test the
// recording did not keep; one the mapping lists as shared (attributed to a
// test the recording did not keep) charges no one, and stays shared.
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
		if got.Pins["ca"] != "a" || got.Moves["left"] != (models.ShareMove{To: "a", Marker: models.MockEntryUnresolvedUnproven}) {
			t.Fatalf("shares %+v; want ca pinned and left staying with a, which is charged, marked unproven", got)
		}
		for _, test := range []string{"a", "b"} {
			if u := s.resultOf(test).FailureInfo.UnconsumedMocks[0]; u.WhyNot != "" {
				t.Fatalf("%s: a recording the round shared and none made says it was not shared (%q)", test, u.WhyNot)
			}
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
	t.Run("a kept test lists it beside a test not kept", func(t *testing.T) {
		// Before it was shared, left was required of a, which lists it: a
		// round that does not make it fails a. Sharing it must not leave it
		// required of no test because a test of its group was not kept.
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		left := shMock("left", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}, Unkept: 1}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(left, "g1")}, "b": {}}, nil, nil, ca, left)
		s.run("a", used("ca"))
		s.run("b")
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want a and b, the kept tests of its group that ran", charged)
		}
		for _, test := range []string{"a", "b"} {
			if r := s.resultOf(test); r.Status != models.TestStatusFailed || !reflect.DeepEqual(reasonsOf(r), []string{"unconsumed:left"}) {
				t.Fatalf("%s: %s %v", test, r.Status, reasonsOf(r))
			} else if u := r.FailureInfo.UnconsumedMocks[0]; u.Unkept != 1 {
				t.Fatalf("%s: the leftover says %d tests not kept were open; want 1, which may have made it", test, u.Unkept)
			}
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.State != models.SharesComplete || got.Pins["ca"] != "a" || got.Moves["left"].To != "a" || got.Pins["left"] != "" {
			t.Fatalf("shares %+v; want ca pinned and left staying with a, which is charged", got)
		}
	})
	t.Run("a shared one", func(t *testing.T) {
		// s1 was attributed to a test the recording did not keep: no kept
		// test lists it, and none is charged when no test makes it.
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		s1 := shMock("s1", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}, Unkept: 1}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1")}}, []models.MockEntry{shEntry(s1, "g1")}, nil, ca, s1)
		s.run("a", used("ca"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v; the test it was attributed to was not kept, and may have made it", charged)
		}
		got, _ := s.p.setShares(models.TestSetStatusPassed)
		if got.State != models.SharesComplete || got.Pins["ca"] != "a" || got.Pins["s1"] != "" || len(got.Moves) != 0 {
			t.Fatalf("shares %+v; want ca pinned, s1 left shared", got)
		}
	})
	t.Run("a shared one a kept test made", func(t *testing.T) {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
		s1 := shMock("s1", 200, "INSERT", "bind.$1=z", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}, Unkept: 1}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1")}}, []models.MockEntry{shEntry(s1, "g1")}, nil, ca, s1)
		s.run("a", used("ca"), used("s1"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
		if got, _ := s.p.setShares(models.TestSetStatusPassed); got.Pins["s1"] != "a" {
			t.Fatalf("shares %+v; want s1 pinned to a, which made it", got)
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
		if got.Moves["c"] != (models.ShareMove{To: "b", Marker: models.MockEntryUnresolvedUnproven}) || got.Pins["c"] != "" {
			t.Fatalf("order %v: shares %+v; want c moved to b, marked unproven, not pinned", order, got)
		}
	}
}

// A candidate a call was answered with only once the learn step learned on it
// (noise, a variable, a bind) is never pinned, however many others drifted
// alike: another test open while it was recorded may have made it. Its
// consumer is charged, concurrent_ambiguous naming it and the places, and it
// stays with that test, marked unproven. What was learned on the test's own
// recording is no reason.
func TestSettleSharesNeverPinsACandidateTheLearnStepLearnedOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	mk := func() *shareSet {
		ca := shMock("ca", 100, "INSERT", "bind.$1=a;bind.$2=t1", "ok")
		cb := shMock("cb", 200, "INSERT", "bind.$1=b;bind.$2=t2", "ok")
		own := shMock("own", 50, "INSERT", "bind.$1=o;bind.$2=t0", "ok")
		return newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(cb, "g1")}, "b": {}, "o": {shEntry(own, "")}}, nil, nil, ca, cb, own)
	}
	check := func(t *testing.T, s *shareSet, test, mock string, places ...string) {
		t.Helper()
		cs := s.resultOf(test).FailureInfo.UnmatchedCalls
		if len(cs) != 1 || cs[0].Reason != models.MissConcurrentAmbiguous || cs[0].ClosestMock != mock {
			t.Fatalf("%s: charged %+v; want concurrent_ambiguous on %s", test, cs, mock)
		}
		n := cs[0].Concurrent
		if n.Case != models.ConcurrentCaseNotOwn || !reflect.DeepEqual(n.Class, []string{mock}) || !reflect.DeepEqual(n.Places, places) {
			t.Fatalf("%s: the charge says %+v", test, n)
		}
		if !strings.Contains(cs[0].NextSteps, "keploy does not learn what changed against a recording another test may own") {
			t.Fatalf("%s: next steps %q", test, cs[0].NextSteps)
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Pins[mock] != "" || got.Moves[mock] != (models.ShareMove{To: test, Marker: models.MockEntryUnresolvedUnproven}) {
			t.Fatalf("shares %+v; want %s with %s, charged, marked unproven", got, mock, test)
		}
	}
	t.Run("noise", func(t *testing.T) {
		s := mk()
		s.run("a", learned("ca", "bind.$2"))
		s.run("b", used("cb"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a"}) {
			t.Fatalf("charged %v; want a", charged)
		}
		check(t, s, "a", "ca", "bind.$2")
	})
	t.Run("two drifted alike, and a test's own too", func(t *testing.T) {
		s := mk()
		s.run("a", learned("ca", "bind.$2"))
		s.run("b", learned("cb", "bind.$2"))
		s.run("o", learned("own", "bind.$2"))
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b"}) {
			t.Fatalf("charged %v; want a and b (o's recording is its own)", charged)
		}
		check(t, s, "a", "ca", "bind.$2")
		check(t, s, "b", "cb", "bind.$2")
	})
	t.Run("a variable", func(t *testing.T) {
		s := mk()
		st := used("ca")
		st.Variables = models.MockVariables{"id": {Bind: "bind.$1", Echo: true}}
		s.run("a", st)
		s.run("b", used("cb"))
		s.settle(t)
		check(t, s, "a", "ca", "bind.$1")
	})
	t.Run("a value bound at an earlier recording", func(t *testing.T) {
		s := mk()
		st := used("ca")
		st.Binds = map[string]models.MockVariables{"own": {"tok": {Bind: "bind.$2"}}}
		s.run("a", st)
		s.run("b", used("cb"))
		s.settle(t)
		check(t, s, "a", "ca", "bind.$2")
	})
	t.Run("nothing learned pins", func(t *testing.T) {
		s := mk()
		s.run("a", used("ca"))
		s.run("b", used("cb"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
	})
}

// A candidate answered for different tests across attempts, or not on its
// test's last attempt, charges every candidate test of it, and stays with a
// charged test.
func TestSettleSharesARetryDisagreementChargesEveryCandidateTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	mk := func() *shareSet {
		c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
		return newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b", "c"}}},
			map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}, "c": {}}, nil, nil, c)
	}
	check := func(t *testing.T, s *shareSet) {
		t.Helper()
		if charged := s.settle(t); !reflect.DeepEqual(charged, []string{"a", "b", "c"}) {
			t.Fatalf("charged %v; want every candidate test that ran", charged)
		}
		for _, test := range []string{"a", "b", "c"} {
			cs := s.resultOf(test).FailureInfo.UnmatchedCalls
			if len(cs) != 1 || cs[0].Reason != models.MissConcurrentAmbiguous || cs[0].Concurrent.Case != models.ConcurrentCaseRetry {
				t.Fatalf("%s: charged %+v; want the retry's charge", test, cs)
			}
		}
		got, _ := s.p.setShares(models.TestSetStatusFailed)
		if got.Pins["c"] != "" || got.Moves["c"] != (models.ShareMove{To: "a", Marker: models.MockEntryUnresolvedUnproven}) {
			t.Fatalf("shares %+v; want c unpinned, staying with a, which is charged, marked unproven", got)
		}
	}
	t.Run("the retry did not make it", func(t *testing.T) {
		s := mk()
		s.run("a", used("c"))
		s.run("a")
		s.run("b")
		s.run("c")
		check(t, s)
	})
	t.Run("two tests answered with it", func(t *testing.T) {
		// The claims make this one staging's impossible; a staging begun
		// again mid-set (a new claims epoch) does not.
		s := mk()
		s.run("a", used("c"))
		s.run("b", used("c"))
		s.run("c")
		check(t, s)
	})
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
	s.run("a", used("ca"))
	cb2 := learned("cb", "bind.$1", "bind.$2")
	cb2.Variables = models.MockVariables{"id": {Bind: "bind.$1", Echo: true}}
	cb2.Binds = map[string]models.MockVariables{"ca": {"tok": {Bind: "bind.$2"}}}
	s.run("b", cb2)
	s.settle(t)
	// ca is pinned: whatever its state holds is written (here, a stand-in).
	got := s.p.stripUnresolved(map[string]models.MockState{"ca": learned("ca", "bind.$2"), "cb": cb2})
	if len(got["ca"].ReqBodyNoise) != 1 || got["cb"].ReqBodyNoise != nil || got["cb"].Variables != nil || got["cb"].Binds != nil {
		t.Fatalf("kept %+v; want ca's (pinned) and nothing of cb's (learned on, so not pinned)", got)
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
			got := r.planShares("test-set-0", mappings)
			narrowed := len(got["a"]) == 0
			if narrowed != tc.wants || r.exact.share.sharing != tc.wants {
				t.Fatalf("narrowed=%v sharing=%v; want %v", narrowed, r.exact.share.sharing, tc.wants)
			}
			if unserved := tc.learn && tc.share && !tc.serves; r.exact.share.unserved != unserved {
				t.Fatalf("unserved=%v; want %v: only the full round with an agent that does not serve them", r.exact.share.unserved, unserved)
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
// test recorded beside others say so: whom with, whom the recording closest
// to the call is listed under, and that only the full round shares them. A
// test that lists a recording the full round could not give to one test is
// charged.
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
		{TestCaseID: "b", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "PostgreSQL", Kind: models.MissInTest, ClosestMock: "c"}}}},
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
	if got := results[1].FailureInfo.UnconsumedMocks[0]; got.Concurrent != "b" || got.WhyNot != models.ConcurrentNotSharedStrict {
		t.Fatalf("a's unconsumed %+v; want b, not shared by this replay", got)
	}
	if results[2].FailureInfo.UnmatchedCalls[0].Concurrent != nil {
		t.Fatal("a test recorded alone is said to have run beside others")
	}
	// A parser's note on a call its learn step refused among shared
	// recordings names them and the case, not whom with: the replay says
	// that, on a copy, and keeps the rest.
	parsed := &models.ConcurrentNote{Class: []string{"c"}, Case: models.ConcurrentCaseNotOwn, Places: []string{"bind.$1"}}
	refused := []models.TestResult{{TestCaseID: "b", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
		{Protocol: "PostgreSQL", Kind: models.MissInTest, ClosestMock: "c", Reason: models.MissConcurrentAmbiguous, Concurrent: parsed}}}}}
	p.annotateConcurrent(refused, nil)
	if n := refused[0].FailureInfo.UnmatchedCalls[0].Concurrent; !reflect.DeepEqual(n.With, []string{"a"}) || n.Case != models.ConcurrentCaseNotOwn || !reflect.DeepEqual(n.Class, []string{"c"}) || len(parsed.With) != 0 {
		t.Fatalf("the parser's note became %+v (its own %+v); want with a, its case and class kept, the parser's left as it was", n, parsed)
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

// An unconsumed recording of a test recorded beside others says why this
// replay did not share it, true in every shape a replay takes: a strict
// replay (keploy test in CI, keploy test --judge's rounds) serves each
// recording to the test time listed it under; keploy record's full round
// shares none of a kind that registered no keys, none it does not key of one
// that did, and none whose recording states what keploy does not share, and
// with an agent that does not serve shared recordings it shares none; a
// recording the full round left with a charged test says so (unproven). Only
// a recording the full round shared and none of its tests made says nothing
// here (settleShares says that). A miss whose closest recording is one of
// these says the same, attributed to the test that lists it.
func TestAnnotateConcurrentSaysTrulyWhyARecordingWasNotShared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}
	annotate := func(p *exactPlan, mock string) (models.UnconsumedMock, *models.ConcurrentNote) {
		t.Helper()
		results := []models.TestResult{
			{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnconsumedMocks: []models.UnconsumedMock{{Name: mock}}}},
			{TestCaseID: "b", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "PostgreSQL", Kind: models.MissInTest, ClosestMock: mock}}}},
		}
		if charged := p.annotateConcurrent(results, nil); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
		return results[0].FailureInfo.UnconsumedMocks[0], results[1].FailureInfo.UnmatchedCalls[0].Concurrent
	}
	want := func(t *testing.T, u models.UnconsumedMock, n *models.ConcurrentNote, whyNot, stated string) {
		t.Helper()
		if u.Concurrent != "b" || u.WhyNot != whyNot || u.Stated != stated {
			t.Fatalf("a's unconsumed %+v; want with b, why_not %q, stated %q", u, whyNot, stated)
		}
		if n == nil || n.WhyNot != whyNot || n.Stated != stated || n.AttributedTo != "a" {
			t.Fatalf("b's miss says %+v; want why_not %q, stated %q, attributed to a", n, whyNot, stated)
		}
	}
	strict := func(mocks []*models.Mock, entries ...models.MockEntry) *exactPlan {
		mappings := map[string][]models.MockEntry{"a": entries, "b": {}}
		p := newExactPlan(mappings, nil, mocks)
		p.share = newSharePlan(concurrencyOf(groups, mappings, nil), nil, mocks)
		return p
	}
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	t.Run("a strict replay: keploy test, and keploy test --judge", func(t *testing.T) {
		u, n := annotate(strict([]*models.Mock{c}, shEntry(c, "g1")), "c")
		want(t, u, n, models.ConcurrentNotSharedStrict, "")
	})
	t.Run("a strict replay of a recording the full round left unproven", func(t *testing.T) {
		e := shEntry(c, "g1")
		e.Unresolved = models.MockEntryUnresolvedUnproven
		u, n := annotate(strict([]*models.Mock{c}, e), "c")
		want(t, u, n, models.ConcurrentNotSharedUnproven, "")
	})
	t.Run("the full round, a kind that registered no keys", func(t *testing.T) {
		h := &models.Mock{Name: "h", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
		s := newShareSet(t, groups, map[string][]models.MockEntry{"a": {{Name: "h", Kind: string(models.HTTP), Group: "g1"}}, "b": {}}, nil, nil, h)
		u, n := annotate(s.p, "h")
		want(t, u, n, models.ConcurrentNotSharedKind, "")
		if n.Kind != string(models.HTTP) {
			t.Fatalf("b's miss names kind %q", n.Kind)
		}
	})
	t.Run("the full round, a recording its kind does not key", func(t *testing.T) {
		unkeyed := shMock("u", 100, "", "bind.$1=v", "ok")
		s := newShareSet(t, groups, map[string][]models.MockEntry{"a": {shEntry(unkeyed, "g1")}, "b": {}}, nil, nil, unkeyed)
		u, n := annotate(s.p, "u")
		want(t, u, n, models.ConcurrentNotSharedCall, "")
		if n.Kind != string(shareKind) {
			t.Fatalf("b's miss names kind %q", n.Kind)
		}
	})
	t.Run("the full round, a recording that states noise", func(t *testing.T) {
		noisy := shMock("n", 100, "INSERT", "bind.$1=v", "ok")
		noisy.Spec.Metadata["share"] = "no"
		s := newShareSet(t, groups, map[string][]models.MockEntry{"a": {shEntry(noisy, "g1")}, "b": {}}, nil, nil, noisy)
		u, n := annotate(s.p, "n")
		want(t, u, n, models.ConcurrentNotSharedStates, "noise")
	})
	t.Run("the full round, an agent that does not serve shared recordings", func(t *testing.T) {
		mappings := map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}
		r := &Replayer{logger: zap.NewNop(), config: &config.Config{Test: config.Test{Learn: true, ShareConcurrent: true}}, instrumentation: servesConcurrent(false)}
		r.exact = newExactPlan(mappings, nil, []*models.Mock{c})
		r.exact.share = newSharePlan(concurrencyOf(groups, mappings, nil), nil, []*models.Mock{c})
		r.planShares("test-set-0", mappings)
		u, n := annotate(r.exact, "c")
		want(t, u, n, models.ConcurrentNotSharedAgent, "")
	})
	// The recording's own reason holds in every replay, so it comes before
	// the agent's: a same-version agent would not share these either.
	unserved := func(m *models.Mock, entry models.MockEntry) *exactPlan {
		mappings := map[string][]models.MockEntry{"a": {entry}, "b": {}}
		r := &Replayer{logger: zap.NewNop(), config: &config.Config{Test: config.Test{Learn: true, ShareConcurrent: true}}, instrumentation: servesConcurrent(false)}
		r.exact = newExactPlan(mappings, nil, []*models.Mock{m})
		r.exact.share = newSharePlan(concurrencyOf(groups, mappings, nil), nil, []*models.Mock{m})
		r.planShares("test-set-0", mappings)
		return r.exact
	}
	t.Run("the full round, an agent that does not serve shared recordings, of a kind that registered no keys", func(t *testing.T) {
		h := &models.Mock{Name: "h", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
		u, n := annotate(unserved(h, models.MockEntry{Name: "h", Kind: string(models.HTTP), Group: "g1"}), "h")
		want(t, u, n, models.ConcurrentNotSharedKind, "")
		if n.Kind != string(models.HTTP) {
			t.Fatalf("b's miss names kind %q", n.Kind)
		}
	})
	t.Run("the full round, an agent that does not serve shared recordings, of a recording that states noise", func(t *testing.T) {
		noisy := shMock("n", 100, "INSERT", "bind.$1=v", "ok")
		noisy.Spec.Metadata["share"] = "no"
		u, n := annotate(unserved(noisy, shEntry(noisy, "g1")), "n")
		want(t, u, n, models.ConcurrentNotSharedStates, "noise")
	})
	t.Run("a strict replay, the closest recording kept by no test", func(t *testing.T) {
		// keploy test, and keploy test --judge: the recording closest to
		// a's call is one only Mapping.Shared lists (made while a test the
		// recording did not keep was open). It is in no kept test's
		// mapping, so it is not "listed under this test by time alone".
		s := shMock("s", 100, "INSERT", "bind.$1=v", "ok")
		mappings := map[string][]models.MockEntry{"a": {}, "b": {}}
		p := newExactPlan(mappings, nil, []*models.Mock{s})
		p.share = newSharePlan(concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}, Unkept: 1}}, mappings, []models.MockEntry{shEntry(s, "g1")}), nil, []*models.Mock{s})
		results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "ShareTest", Kind: models.MissInTest, ClosestMock: "s"}}}}}
		if charged := p.annotateConcurrent(results, nil); len(charged) != 0 {
			t.Fatalf("charged %v", charged)
		}
		n := results[0].FailureInfo.UnmatchedCalls[0].Concurrent
		if n == nil || !slices.Equal(n.With, []string{"b"}) || n.Unkept != 1 || n.WhyNot != models.ConcurrentNotSharedUnkept || n.AttributedTo != "" || len(n.MayBe) != 0 {
			t.Fatalf("a's miss says %+v; want with b and 1 test the recording did not keep, its closest kept by no test, attributed to none", n)
		}
	})
}

// A candidate both of an ambiguous class and of a conflict is marked
// ambiguous wherever it goes: the test that holds it after the pin is charged
// in every later replay (a strict keploy test --judge included), not only in
// this round. Before, the conflict's move won and left it unmarked.
func TestSettleSharesMarksAnAmbiguousConflictAmbiguous(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	t1 := shMock("t1", 100, "INSERT", "bind.$1=same", "201")
	t2 := shMock("t2", 200, "INSERT", "bind.$1=same", "23505")
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
		map[string][]models.MockEntry{"a": {shEntry(t1, "g1"), shEntry(t2, "g1")}, "b": {}}, nil, nil, t1, t2)
	s.run("a", used("t1"))
	// b's call paired with t1, which a answered with: a conflict, claimant a.
	s.run("b")
	s.results[1].Status = models.TestStatusFailed
	s.results[1].FailureInfo.UnmatchedCalls = []models.UnmatchedCall{{Protocol: "ShareTest", ClosestMock: "t1", Kind: models.MissInTest,
		Reason: models.MissConcurrentConflict, Concurrent: &models.ConcurrentNote{Claimant: "a"}}}
	s.settle(t)
	got, _ := s.p.setShares(models.TestSetStatusFailed)
	if got.Moves["t1"] != (models.ShareMove{To: "a", Marker: models.MockEntryUnresolvedAmbiguous}) {
		t.Fatalf("t1 goes %+v; want to a, marked ambiguous", got.Moves["t1"])
	}
}

// Guards against a false red, each pinned (round-2 review): none can pass a
// test that should fail, but each without them quarantines a test that is
// fine.
func TestSettleSharesChargesOnlyWhatSharingCannotProve(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	t.Run("a class with no shared recording is today's", func(t *testing.T) {
		// A polled SELECT a's own recordings answered twice, differently:
		// equal requests, different answers, no recording shared between
		// tests in the class. A candidate of the statement elsewhere links
		// it into the component, which must not make it ambiguous.
		p1 := shMock("p1", 100, "SELECT", "bind.$1=x", "0 rows")
		p2 := shMock("p2", 200, "SELECT", "bind.$1=x", "1 row")
		cand := shMock("cand", 300, "SELECT", "bind.$1=y", "1 row")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(p1, ""), shEntry(p2, ""), shEntry(cand, "g1")}, "b": {}}, nil, nil, p1, p2, cand)
		s.run("a", used("p1"), used("p2"))
		s.run("b", used("cand"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v for a test's own recordings no test shares", charged)
		}
	})
	t.Run("answers alike but for an echoed value", func(t *testing.T) {
		// The test's own recording learned $1 as an echo: the shared ones,
		// equal once $1 is excused, answer alike once the echoed id is read
		// as its place.
		own := shMock("own", 50, "INSERT", "bind.$1=id-0;bind.$2=n", "row id-0")
		c1 := shMock("c1", 100, "INSERT", "bind.$1=id-1;bind.$2=n", "row id-1")
		c2 := shMock("c2", 200, "INSERT", "bind.$1=id-2;bind.$2=n", "row id-2")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(own, ""), shEntry(c1, "g1"), shEntry(c2, "g1")}, "b": {}}, nil, nil, own, c1, c2)
		echo := used("own")
		echo.Variables = models.MockVariables{"id": {Bind: "bind.$1", Echo: true}}
		s.run("a", echo, used("c1"))
		s.run("b", used("c2"))
		if charged := s.settle(t); len(charged) != 0 {
			t.Fatalf("charged %v for recordings that answer alike but for the id each echoes", charged)
		}
	})
	t.Run("a consumption read, then a read that failed", func(t *testing.T) {
		// What a test consumed is read in parts (a stream, then its drain):
		// one part unread leaves its shares unknown, however much was read.
		c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
		s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
			map[string][]models.MockEntry{"a": {shEntry(c, "g1")}, "b": {}}, nil, nil, c)
		s.p.begin("a")
		s.p.noteShares("a", []models.MockState{used("c")}, nil)
		s.p.noteShares("a", nil, errNotRead)
		s.results = append(s.results, models.TestResult{TestCaseID: "a", Status: models.TestStatusPassed})
		s.settle(t)
		if got, _ := s.p.setShares(models.TestSetStatusPassed); got.State != models.SharesIncomplete {
			t.Fatalf("a consumption read in part settled %+v", got)
		}
	})
}

// A miss of a test recorded beside others says only what is true of its call
// (round-3 review, probes P1 and P2), in every replay shape: the test the
// recording closest to it is listed under and why this replay did not share
// it, only when that recording is one made while test's request and others'
// were open; nothing of the others' recordings when it drifted from one made
// while its request alone was open, from a recording this replay shared with
// it, or from one of another group; and, with no closest recording, only the
// recordings of its own kind, as ones it may be.
func TestAnnotateConcurrentSaysOnlyWhatIsTrueOfTheCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	g1 := models.ConcurrencyGroup{ID: "g1", Tests: []string{"a", "b"}}
	g2 := models.ConcurrencyGroup{ID: "g2", Tests: []string{"c", "d"}}
	own := shMock("own-a", 50, "SELECT", "bind.$1=x", "ok")
	grouped := shMock("mine", 100, "SELECT", "bind.$1=y", "ok")
	theirs := shMock("theirs", 150, "SELECT", "bind.$1=z", "ok")
	other := shMock("other", 200, "SELECT", "bind.$1=w", "ok")
	mg := &models.Mock{Name: "mock-0", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
	h := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
	mocks := []*models.Mock{own, grouped, theirs, other, mg, h}
	// a lists an Http recording of g1 too, c one of g2, and one of g1 no
	// kept test lists (Mapping.Shared): none is one a's call may be.
	ownH := &models.Mock{Name: "mock-a1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
	otherH := &models.Mock{Name: "mock-c1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
	sharedH := &models.Mock{Name: "mock-s1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
	mocks = append(mocks, ownH, otherH, sharedH)
	mappings := map[string][]models.MockEntry{
		"a": {shEntry(own, ""), shEntry(grouped, "g1"), {Name: "mock-a1", Kind: string(models.HTTP), Group: "g1"}},
		"b": {shEntry(theirs, "g1"), {Name: "mock-0", Kind: string(models.Mongo), Group: "g1"}, {Name: "mock-1", Kind: string(models.HTTP), Group: "g1"}},
		"c": {shEntry(other, "g2"), {Name: "mock-c1", Kind: string(models.HTTP), Group: "g2"}},
		"d": {},
	}
	shared := []models.MockEntry{{Name: "mock-s1", Kind: string(models.HTTP), Group: "g1"}}
	groups := []models.ConcurrencyGroup{g1, g2}
	strict := func() *exactPlan {
		p := newExactPlan(mappings, nil, mocks)
		p.share = newSharePlan(concurrencyOf(groups, mappings, shared), nil, mocks)
		return p
	}
	miss := func(p *exactPlan, protocol, closest string) *models.ConcurrentNote {
		t.Helper()
		results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: protocol, Kind: models.MissInTest, ClosestMock: closest}}}}}
		p.annotateConcurrent(results, nil)
		n := results[0].FailureInfo.UnmatchedCalls[0].Concurrent
		if n == nil || !reflect.DeepEqual(n.With, []string{"b"}) {
			t.Fatalf("a's miss says %+v; want with b", n)
		}
		return n
	}
	nothing := func(t *testing.T, n *models.ConcurrentNote) {
		t.Helper()
		if n.AttributedTo != "" || n.MayBe != nil || n.WhyNot != "" || n.Kind != "" || n.Stated != "" {
			t.Fatalf("a's miss says %+v; want only whom with", n)
		}
	}
	t.Run("strict, its own recording drifted (P1)", func(t *testing.T) {
		nothing(t, miss(strict(), "ShareTest", "own-a"))
	})
	t.Run("strict, the recording listed under it by time alone drifted", func(t *testing.T) {
		n := miss(strict(), "ShareTest", "mine")
		if n.AttributedTo != "" || n.MayBe != nil || n.WhyNot != models.ConcurrentNotSharedStrict {
			t.Fatalf("a's miss says %+v; want listed under a itself, not shared by a strict replay", n)
		}
	})
	t.Run("strict, the recording listed under another test of its group", func(t *testing.T) {
		n := miss(strict(), "ShareTest", "theirs")
		if n.AttributedTo != "b" || n.WhyNot != models.ConcurrentNotSharedStrict {
			t.Fatalf("a's miss says %+v; want attributed to b, not shared by a strict replay", n)
		}
	})
	t.Run("strict, a recording of a group it is not in", func(t *testing.T) {
		nothing(t, miss(strict(), "ShareTest", "other"))
	})
	t.Run("strict, a kind keploy shares none of says so, not that only the full round shares", func(t *testing.T) {
		n := miss(strict(), "HTTP", "mock-1")
		if n.AttributedTo != "b" || n.WhyNot != models.ConcurrentNotSharedKind || n.Kind != string(models.HTTP) {
			t.Fatalf("a's miss says %+v; want attributed to b, keploy sharing no Http recording", n)
		}
	})
	t.Run("the full round, a recording it shared with it drifted", func(t *testing.T) {
		s := newShareSet(t, groups, mappings, shared, nil, mocks...)
		nothing(t, miss(s.p, "ShareTest", "theirs"))
	})
	t.Run("the full round, no closest: only its own kind, as what it may be (P2)", func(t *testing.T) {
		s := newShareSet(t, groups, mappings, shared, nil, mocks...)
		n := miss(s.p, "HTTP", "")
		if n.AttributedTo != "" || !reflect.DeepEqual(n.MayBe, []string{"b"}) || n.WhyNot != models.ConcurrentNotSharedKind || n.Kind != string(models.HTTP) {
			t.Fatalf("a's HTTP miss says %+v; want it may be b's, keploy sharing no Http recording", n)
		}
	})
	t.Run("the full round, no closest: a recording of its kind it shared with it is none it may be", func(t *testing.T) {
		keys, _ := models.ConcurrentKeysOf(shareKind)
		t.Cleanup(models.RegisterConcurrentKeys(models.PostgresV3, keys))
		pg := shMock("pg-b", 300, "INSERT", "bind.$1=v", "ok")
		pg.Kind = models.PostgresV3
		m := map[string][]models.MockEntry{"a": {}, "b": {shEntry(pg, "g1")}}
		s := newShareSet(t, []models.ConcurrencyGroup{g1}, m, nil, nil, pg)
		if !s.p.share.candidate("pg-b") {
			t.Fatal("pg-b is not shared")
		}
		nothing(t, miss(s.p, "PostgreSQL", ""))
	})
	t.Run("no closest, no recording of its kind beside it", func(t *testing.T) {
		nothing(t, miss(strict(), "MySQL", ""))
	})
	t.Run("no closest, a protocol keploy names no kind for", func(t *testing.T) {
		nothing(t, miss(strict(), "unknown", ""))
	})
	t.Run("no closest, recordings of its kind not shared for different reasons", func(t *testing.T) {
		// e's recording sorts before b's: the tests are said in their order.
		h2 := &models.Mock{Name: "mock-02", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: shareT0}}
		e := models.MockEntry{Name: "mock-02", Kind: string(models.HTTP), Group: "g3", Unresolved: models.MockEntryUnresolvedUnproven}
		m := map[string][]models.MockEntry{"a": mappings["a"], "b": mappings["b"], "e": {e}}
		gs := []models.ConcurrencyGroup{g1, {ID: "g3", Tests: []string{"a", "e"}}}
		p := newExactPlan(m, nil, append(mocks, h2))
		p.share = newSharePlan(concurrencyOf(gs, m, nil), nil, append(mocks, h2))
		results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "HTTP", Kind: models.MissInTest}}}}}
		p.annotateConcurrent(results, nil)
		n := results[0].FailureInfo.UnmatchedCalls[0].Concurrent
		if n == nil || !reflect.DeepEqual(n.MayBe, []string{"b", "e"}) || n.WhyNot != "" || n.Kind != "" {
			t.Fatalf("a's HTTP miss says %+v; want it may be b's or e's, and no one reason", n)
		}
	})
}

// A test whose only other test open beside it is one the recording did not
// keep (lost, or deleted) is still recorded beside another: its miss, and a
// parser's note on its call, say so, and count it (round-3 review, probe P3).
func TestAnnotateConcurrentCountsTheTestsTheRecordingDidNotKeep(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	c := shMock("c", 100, "INSERT", "bind.$1=v", "ok")
	mappings := map[string][]models.MockEntry{"a": {shEntry(c, "g1")}}
	p := newExactPlan(mappings, nil, []*models.Mock{c})
	p.share = newSharePlan(concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}, Unkept: 1}}, mappings, nil), nil, []*models.Mock{c})
	parsed := &models.ConcurrentNote{Class: []string{"c"}, Case: models.ConcurrentCaseNotOwn, Places: []string{"bind.$1"}}
	results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{
		UnmatchedCalls: []models.UnmatchedCall{
			{Protocol: "ShareTest", Kind: models.MissInTest, ClosestMock: "c"},
			{Protocol: "ShareTest", Kind: models.MissInTest, ClosestMock: "c", Reason: models.MissConcurrentAmbiguous, Concurrent: parsed},
		},
		UnconsumedMocks: []models.UnconsumedMock{{Name: "c"}},
	}}}
	p.annotateConcurrent(results, nil)
	calls := results[0].FailureInfo.UnmatchedCalls
	if n := calls[0].Concurrent; n == nil || n.With != nil || n.Unkept != 1 || n.WhyNot != models.ConcurrentNotSharedStrict {
		t.Fatalf("a's miss says %+v; want 1 test the recording did not keep, not shared by a strict replay", n)
	}
	if n := calls[1].Concurrent; n == nil || n.Unkept != 1 || n.Case != models.ConcurrentCaseNotOwn || parsed.Unkept != 0 {
		t.Fatalf("the parser's note became %+v (its own %+v); want 1 test the recording did not keep, on a copy", n, parsed)
	}
	if u := results[0].FailureInfo.UnconsumedMocks[0]; u.Concurrent != "" || u.Unkept != 1 || u.WhyNot != models.ConcurrentNotSharedStrict {
		t.Fatalf("a's leftover says %+v; want 1 test the recording did not keep, not shared by a strict replay", u)
	}
	// A group that names a alone, and no test it did not keep, puts no test
	// beside a: a's miss and leftover are its own.
	alone := newExactPlan(mappings, nil, []*models.Mock{c})
	alone.share = newSharePlan(concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}}}, mappings, nil), nil, []*models.Mock{c})
	solo := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{
		UnmatchedCalls:  []models.UnmatchedCall{{Protocol: "ShareTest", Kind: models.MissInTest, ClosestMock: "c"}},
		UnconsumedMocks: []models.UnconsumedMock{{Name: "c"}},
	}}}
	alone.annotateConcurrent(solo, nil)
	if n, u := solo[0].FailureInfo.UnmatchedCalls[0].Concurrent, solo[0].FailureInfo.UnconsumedMocks[0]; n != nil || u != (models.UnconsumedMock{Name: "c"}) {
		t.Fatalf("a, recorded alone, says %+v and %+v", n, u)
	}
	// The full round's settlement says the same of a recording it shared
	// with a alone: the note of its not_own charge counts the test not kept.
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}, Unkept: 1}}, mappings, nil, nil, c)
	s.run("a", learned("c", "bind.$1"))
	s.settle(t)
	r := s.resultOf("a")
	if len(r.FailureInfo.UnmatchedCalls) != 1 {
		t.Fatalf("a charged %+v", r.FailureInfo.UnmatchedCalls)
	}
	if n := r.FailureInfo.UnmatchedCalls[0].Concurrent; n == nil || n.Unkept != 1 || n.With != nil || n.Case != models.ConcurrentCaseNotOwn {
		t.Fatalf("a's charge says %+v; want not_own, 1 test the recording did not keep", n)
	}
}

// A mapping entry that names no kind is read as the recording the agent holds:
// one of a kind that keys its recordings, of a call it does not key, says so
// and names that kind (not "these").
func TestAnnotateConcurrentNamesTheHeldKindOfAnEntryThatNamesNone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	unkeyed := shMock("u", 100, "", "bind.$1=v", "ok")
	e := shEntry(unkeyed, "g1")
	e.Kind = ""
	s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}}, map[string][]models.MockEntry{"a": {e}, "b": {}}, nil, nil, unkeyed)
	results := []models.TestResult{{TestCaseID: "b", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "ShareTest", Kind: models.MissInTest, ClosestMock: "u"}}}}}
	s.p.annotateConcurrent(results, nil)
	if n := results[0].FailureInfo.UnmatchedCalls[0].Concurrent; n == nil || n.WhyNot != models.ConcurrentNotSharedCall || n.Kind != string(shareKind) || n.AttributedTo != "a" {
		t.Fatalf("b's miss says %+v; want attributed to a, a %s recording keploy does not key", n, shareKind)
	}
}
