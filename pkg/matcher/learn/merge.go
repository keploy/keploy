package learn

// The merge of what the learn step learned onto a recording: keploy record
// writes it onto the recordings it persists (mockdb), and the agent puts it
// on the recordings it serves a test set's retry cycles (KeptLearning), so
// those cycles are served the recordings as the next keploy test reads them.

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// MergeReqBodyNoise returns a fresh map combining the existing on-disk
// request-body noise with newly-detected noise carried on the MockState.
// Existing entries win on key collision (noise is monotonic), and every slice
// is copied so the result shares no backing storage with its inputs.
func MergeReqBodyNoise(existing, detected map[string][]string) map[string][]string {
	out := make(map[string][]string, len(existing)+len(detected))
	for k, v := range existing {
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	for k, v := range detected {
		if _, ok := out[k]; ok {
			continue
		}
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	return out
}

// MergeLearned writes what a consumed mock's state carries of what the
// learn step learned onto the mock read from disk: its learned noise, its
// variables, binds, the variables other mocks of its test use that are
// bound at it, and its clock reads (MockSpec.Clocks). It returns what it
// added, sorted, each as "<path> (noise)", "<place> (variable <name>:
// bind|use)", "<place> (echo <name>)" or "<path> (<offset>)" ("clock",
// "clock + 30m"). Noise and variables are monotonic (what is on disk
// stays), with two exceptions: a place that is a variable's is never noise,
// so learned noise there is dropped (at the leaf the replay reads the place
// at, VariableLeafPath: a word's field; the learn step may have learned a
// value as noise at the call that first sent it, before a later call showed
// it flowed on); and a place that is a clock read is never noise, so noise
// there, learned or on disk (an older keploy learned a clock read as
// noise), is dropped too: the place stays compared. A clock read learned at
// a place that holds one meets it (models.ClockOffset.Meet), so it holds
// only the offsets both do, and is added when that changed it; one that
// shares no offset with it is Empty and refuses every value there. A clock
// read at a variable's place stays: the replay checks both. A value can be
// the application's clock and flow on to a later call (an insert's
// createdAt that a read returns), so dropping either check lets a wrong
// value pass. The Mongo replay checks both; the HTTP replay refuses such a
// place (it cannot hold one value to both yet), which fails closed.
func MergeLearned(mock *models.Mock, st models.MockState, binds models.MockVariables) []string {
	var added []string
	if !provenswitch.On() {
		// Without the switch nothing learns variables, and noise merges as
		// it always has.
		for k := range st.ReqBodyNoise {
			if _, ok := mock.Spec.ReqBodyNoise[k]; !ok {
				added = append(added, k+" (noise)")
			}
		}
		if len(st.ReqBodyNoise) > 0 {
			mock.Spec.ReqBodyNoise = MergeReqBodyNoise(mock.Spec.ReqBodyNoise, st.ReqBodyNoise)
		}
		sort.Strings(added)
		return added
	}
	if len(st.Variables) > 0 || len(binds) > 0 {
		vars := mock.Spec.Variables.Clone()
		if vars == nil {
			vars = models.MockVariables{}
		}
		names := make([]string, 0, len(st.Variables)+len(binds))
		for n := range st.Variables {
			names = append(names, n)
		}
		for n := range binds {
			if _, ok := st.Variables[n]; !ok {
				names = append(names, n)
			}
		}
		for _, n := range names {
			v, ok := st.Variables[n]
			if b, isBind := binds[n]; isBind {
				if ok {
					v = MergeVariable(v, b)
				} else {
					v = b
				}
			}
			cur, had := vars[n]
			if had && cur.Recorded != v.Recorded {
				// Another variable of the same name: not this one.
				continue
			}
			next := v
			if had {
				next = MergeVariable(cur, v)
			}
			added = append(added, variableAdded(n, cur, next)...)
			vars[n] = next
		}
		mock.Spec.Variables = vars
	}
	varPlaces := map[string]bool{}
	for _, v := range mock.Spec.Variables {
		for _, place := range append([]string{v.Bind}, v.Use...) {
			if p, ok := VariableLeafPath(mock, place); ok {
				varPlaces[p] = true
			}
		}
	}
	if len(st.ReqBodyNoise) > 0 {
		noise := make(map[string][]string, len(st.ReqBodyNoise))
		for k, v := range st.ReqBodyNoise {
			_, clock := st.Clocks[k]
			if _, onDisk := mock.Spec.Clocks[k]; !varPlaces[k] && !clock && !onDisk {
				noise[k] = v
			}
		}
		for k := range noise {
			if _, ok := mock.Spec.ReqBodyNoise[k]; !ok {
				added = append(added, k+" (noise)")
			}
		}
		if len(noise) > 0 {
			mock.Spec.ReqBodyNoise = MergeReqBodyNoise(mock.Spec.ReqBodyNoise, noise)
		}
	}
	if len(st.Clocks) > 0 {
		clocks := models.MergeClocks(mock.Spec.Clocks, nil)
		if clocks == nil {
			clocks = make(map[string]models.ClockOffset, len(st.Clocks))
		}
		for path, o := range st.Clocks {
			cur, had := clocks[path]
			next := o
			if had {
				next = cur.Meet(o)
			}
			if !had || next != cur {
				added = append(added, path+" ("+next.String()+")")
			}
			clocks[path] = next
		}
		mock.Spec.Clocks = clocks
	}
	for k := range mock.Spec.ReqBodyNoise {
		_, clock := mock.Spec.Clocks[k]
		if varPlaces[k] || clock {
			delete(mock.Spec.ReqBodyNoise, k)
		}
	}
	sort.Strings(added)
	return added
}

// MergeVariable is two declarations of one variable in one call as one: a
// bind place from either, every use and answer place of both, and an echo
// only when both say so (a call another uses the value of is not its echo).
// A clock read goes with its bind, and holds for one both bind only when
// both say so (models.MergeVariables).
func MergeVariable(a, b models.MockVariable) models.MockVariable {
	if a.Recorded == "" {
		a.Recorded, a.Echo = b.Recorded, b.Echo
	} else {
		a.Echo = a.Echo && b.Echo
	}
	if a.Bind == "" {
		a.Bind, a.Clock = b.Bind, append([]models.ClockRead(nil), b.Clock...)
	} else if b.Bind != "" && len(b.Clock) == 0 {
		a.Clock = nil
	}
	a.Use = unionSorted(a.Use, b.Use)
	a.Answer = unionSorted(a.Answer, b.Answer)
	return a
}

func unionSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string(nil), a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// variableAdded says what next declares of variable name that was not
// declared before (was).
func variableAdded(name string, was, next models.MockVariable) []string {
	var out []string
	if next.Bind != "" && next.Bind != was.Bind {
		if next.Echo {
			out = append(out, next.Bind+" (echo "+name+")")
		} else {
			out = append(out, next.Bind+" (variable "+name+": bind)")
		}
	}
	have := map[string]bool{}
	for _, u := range was.Use {
		have[u] = true
	}
	for _, u := range next.Use {
		if !have[u] {
			out = append(out, u+" (variable "+name+": use)")
		}
	}
	return out
}

// Taken is a test's own variable the run's variable took the place of
// (RunTakesPlaces): Own, of Test, was bound or used at Place of Mock, where
// the run's Run is bound, and is read as Run.
type Taken struct {
	Test, Own, Run, Mock, Place string
}

// RunTakesPlaces gives each request place of the set's recordings one
// variable. A later test's call that sends a value an earlier test's call
// sent binds the run's variable (models.RunVariable) at that earlier call's
// place (Tracker.RunName). When the earlier test's own variable is bound or
// used at that place too, the two are one value, and the run's takes the
// place: every state of that test reads the run's name for its own. A
// replay so binds it once, where the earlier test's call sends it, and keeps
// it for the set: the earlier test's later calls read it as the later
// tests' do. testOf names the test each mock's state was learned in, ""
// for none (the start-up's, which carries no test's variable). states is
// left as it is: the result is a copy, with each state of such a test
// renamed. taken is what was renamed, sorted, for the log (no value).
func RunTakesPlaces(states map[string]models.MockState, testOf func(mock string) string) (map[string]models.MockState, []Taken) {
	type place struct{ mock, at string }
	run := map[place]string{}
	for _, st := range states {
		for target, vs := range st.Binds {
			for name, v := range vs {
				if models.RunVariable(name) && v.Bind != "" {
					run[place{target, v.Bind}] = name
				}
			}
		}
	}
	if len(run) == 0 {
		return states, nil
	}
	// By test and own name: what takes it. Two run's names at one test's
	// value (no ranking gives that) take it by the lesser, whatever order
	// the states are read in.
	takes := map[string]map[string]Taken{}
	take := func(state, mock, own, at string) {
		r, ok := run[place{mock, at}]
		test := testOf(state)
		if !ok || test == "" || models.RunVariable(own) {
			return
		}
		if cur, ok := takes[test][own]; ok && cur.Run <= r {
			return
		}
		if takes[test] == nil {
			takes[test] = map[string]Taken{}
		}
		takes[test][own] = Taken{Test: test, Own: own, Run: r, Mock: mock, Place: at}
	}
	for name, st := range states {
		for target, vs := range st.Binds {
			for own, v := range vs {
				take(name, target, own, v.Bind)
			}
		}
		for own, v := range st.Variables {
			for _, at := range append([]string{v.Bind}, v.Use...) {
				take(name, name, own, at)
			}
		}
	}
	if len(takes) == 0 {
		return states, nil
	}
	renamed := func(vs models.MockVariables, to map[string]Taken) models.MockVariables {
		if vs == nil {
			return nil
		}
		out := make(models.MockVariables, len(vs))
		for name, v := range vs {
			if t, ok := to[name]; ok {
				name = t.Run
			}
			if was, ok := out[name]; ok {
				v = MergeVariable(was, v)
			}
			out[name] = v
		}
		return out
	}
	out := make(map[string]models.MockState, len(states))
	for name, st := range states {
		if to := takes[testOf(name)]; to != nil {
			st.Variables = renamed(st.Variables, to)
			if st.Binds != nil {
				binds := make(map[string]models.MockVariables, len(st.Binds))
				for target, vs := range st.Binds {
					binds[target] = renamed(vs, to)
				}
				st.Binds = binds
			}
		}
		out[name] = st
	}
	var taken []Taken
	for _, to := range takes {
		for _, t := range to {
			taken = append(taken, t)
		}
	}
	sort.Slice(taken, func(i, j int) bool {
		if taken[i].Test != taken[j].Test {
			return taken[i].Test < taken[j].Test
		}
		return taken[i].Own < taken[j].Own
	})
	return out, taken
}

// FoldLearned is, of states (by mock name), the states that carry learning
// (noise, variables or clock reads) and, by target mock, the variables other mocks' states
// bind at it (MockState.Binds), folded (MergeVariable). A target no state of
// its own carries learning for is in the first with a bare state, so the
// merge reaches it (MergeLearned).
func FoldLearned(states map[string]models.MockState) (map[string]models.MockState, map[string]models.MockVariables) {
	withLearning := make(map[string]models.MockState)
	binds := map[string]models.MockVariables{}
	for name, st := range states {
		if len(st.ReqBodyNoise) > 0 || len(st.Variables) > 0 || len(st.Clocks) > 0 {
			withLearning[name] = st
		}
		for target, vs := range st.Binds {
			if binds[target] == nil {
				binds[target] = models.MockVariables{}
			}
			for vn, v := range vs {
				binds[target][vn] = MergeVariable(binds[target][vn], v)
			}
		}
	}
	for target := range binds {
		if _, ok := withLearning[target]; !ok {
			withLearning[target] = models.MockState{Name: target}
		}
	}
	return withLearning, binds
}

// KeptLearning is what keploy record keeps of a test set's learning, by mock
// name, as the recordings carry it once it is persisted: a test set's retry
// cycles run after its learn step ended, and are served the recordings so
// (models.MockFilterParams.Learned). A nil KeptLearning holds nothing.
type KeptLearning struct {
	states map[string]models.MockState
	binds  map[string]models.MockVariables
}

// NewKeptLearning is the learning states carry (FoldLearned); nil when none
// carries any.
func NewKeptLearning(states []models.MockState) *KeptLearning {
	byName := make(map[string]models.MockState, len(states))
	for _, st := range states {
		if st.Name != "" {
			byName[st.Name] = st
		}
	}
	withLearning, binds := FoldLearned(byName)
	if len(withLearning) == 0 {
		return nil
	}
	return &KeptLearning{states: withLearning, binds: binds}
}

// Has says whether l holds learning for the mock named name.
func (l *KeptLearning) Has(name string) bool {
	if l == nil {
		return false
	}
	_, ok := l.states[name]
	return ok
}

// Onto is m with what l holds of it merged (MergeLearned), on a deep copy: m
// itself, untouched, when l holds nothing of it.
func (l *KeptLearning) Onto(m *models.Mock) *models.Mock {
	if m == nil || !l.Has(m.Name) {
		return m
	}
	c := m.DeepCopy()
	l.Into(c)
	return c
}

// Into merges what l holds of m onto m itself (MergeLearned), for a mock no
// one else holds: one just decoded.
func (l *KeptLearning) Into(m *models.Mock) {
	if m == nil || !l.Has(m.Name) {
		return
	}
	MergeLearned(m, l.states[m.Name], l.binds[m.Name])
}
