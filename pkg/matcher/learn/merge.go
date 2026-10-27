package learn

// The merge of what the learn step learned onto a recording: keploy record
// writes it onto the recordings it persists (mockdb), and the agent puts it
// on the recordings it serves a test set's retry cycles (KeptLearning), so
// those cycles are served the recordings as the next keploy test reads them.

import (
	"maps"
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
// so learned noise there is dropped (the learn step may have learned a
// value as noise at the call that first sent it, before a later call showed
// it flowed on); and a place that is a clock read is never noise, so noise
// there, learned or on disk (an older keploy learned a clock read as
// noise), is dropped too: the place stays compared. A clock read learned at
// a place that holds one meets it (models.ClockOffset.Meet), so it holds
// only the offsets both do, and is added when that changed it; one that
// shares no offset with it is Empty and refuses every value there. A clock
// read at a variable's place stays: the replay checks both.
//
// A person's entry (models.AboutState.Hand: by hand, or learned and then
// edited) wins over what was learned at its place, and blocked is each
// learned change it kept from being applied, sorted, in added's spelling
// and naming the entry that held (notApplied): the run's learn summary says
// it once, and the file never carries it. A learned check (a variable or a
// clock read) still replaces a person's noise at its place: the place is
// then compared, and their about stays, an orphan keploy mock check
// reports.
func MergeLearned(mock *models.Mock, st models.MockState, binds models.MockVariables) (added, blocked []string) {
	if !provenswitch.On() {
		// Without the switch nothing learns variables, and noise merges as
		// it always has.
		for k := range st.ReqBodyNoise {
			if _, ok := mock.Spec.ReqBodyNoise[k]; !ok {
				added = append(added, noiseAdded(k))
			}
		}
		if len(st.ReqBodyNoise) > 0 {
			mock.Spec.ReqBodyNoise = MergeReqBodyNoise(mock.Spec.ReqBodyNoise, st.ReqBodyNoise)
		}
		sort.Strings(added)
		return added, nil
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
			if s := models.VariableAboutState(n, cur); had && s.Hand() {
				// A person's variable: a learned one never changes it.
				for _, c := range variableAdded(n, cur, MergeVariable(cur, v)) {
					blocked = append(blocked, notApplied(c, s))
				}
				continue
			}
			next := v
			if had {
				next = MergeVariable(cur, v)
			}
			added = append(added, variableAdded(n, cur, next)...)
			next.About = variableAbout(n, cur, had, next, v.About)
			vars[n] = next
		}
		mock.Spec.Variables = vars
	}
	varPlaces := map[string]bool{}
	for _, v := range mock.Spec.Variables {
		for _, place := range append([]string{v.Bind}, v.Use...) {
			if p, ok := LeafPath(place); ok {
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
			if _, ok := mock.Spec.ReqBodyNoise[k]; ok {
				continue
			}
			if s := models.NoiseAboutState(&mock.Spec, k); s.Hand() {
				// A person's about at a path they took out of the noise
				// (by hand, or keploy's and edited since): the place
				// stays compared.
				blocked = append(blocked, notApplied(noiseAdded(k), s))
				delete(noise, k)
				continue
			}
			added = append(added, noiseAdded(k))
			path := k
			setAbout(&mock.Spec.NoiseAbout, k, st.NoiseAbout[k].Stamped(models.AboutKindNoise, k, false, func(run string) string { return models.NoiseSum(path, run) }))
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
			state := models.ClockAboutState(&mock.Spec, path)
			next := o
			if had {
				next = cur.Meet(o)
			}
			if had && state.Hand() || !had && state == models.AboutHand {
				// A person's clock read (or their about at a path they took
				// out of the clocks): a learned one never changes it.
				if !had || next != cur {
					blocked = append(blocked, notApplied(clockAdded(path, next), state))
				}
				continue
			}
			if !had || next != cur {
				added = append(added, clockAdded(path, next))
			}
			clocks[path] = next
			at := path
			sum := func(run string) string { return models.ClockSum(at, next, run) }
			switch {
			case !had:
				setAbout(&mock.Spec.ClockAbout, path, st.ClockAbout[path].Stamped(models.AboutKindClock, path, false, sum))
			case next != cur && state == models.AboutLearned:
				// keploy's own clock read, narrowed: its sum follows.
				a := mock.Spec.ClockAbout[path].Clone()
				a.Sum = sum(a.Run)
				mock.Spec.ClockAbout[path] = a
			}
		}
		mock.Spec.Clocks = clocks
	}
	for k := range mock.Spec.ReqBodyNoise {
		_, clock := mock.Spec.Clocks[k]
		if varPlaces[k] || clock {
			// The place is a variable's or a clock read's, so it is
			// compared: the noise goes, and keploy's own about with it. A
			// person's about stays, an orphan keploy mock check reports.
			if !models.NoiseAboutState(&mock.Spec, k).Hand() {
				delete(mock.Spec.NoiseAbout, k)
			}
			delete(mock.Spec.ReqBodyNoise, k)
		}
	}
	sort.Strings(added)
	sort.Strings(blocked)
	return added, blocked
}

// notApplied is how blocked (MergeLearned) names change, a learned change
// in added's spelling, that the person's entry at its place, in state s,
// kept from being applied.
func notApplied(change string, s models.AboutState) string {
	if s == models.AboutEdited {
		return change + "; kept the entry there, edited by hand after keploy learned it"
	}
	return change + "; kept the entry there, written by hand"
}

// BlockedMessage is the run's learn summary line over what a person's
// entries kept from being applied (BlockedFields), and BlockedNextStep what
// to do about it.
const (
	BlockedMessage  = "keploy kept the entries a person wrote or edited, and did not apply what the learn step learned at their places"
	BlockedNextStep = "to let keploy learn a place again, delete the entry there with its about block, and record again"
)

// BlockedFields is what a person's entries kept from being applied, by mock
// name (blocked, MergeLearned), each as "<mock> <what was blocked>", sorted.
func BlockedFields(blocked map[string][]string) []string {
	var out []string
	for name, bs := range blocked {
		for _, b := range bs {
			out = append(out, name+" "+b)
		}
	}
	sort.Strings(out)
	return out
}

// setAbout sets (*m)[key] to a, making the map when there is none.
func setAbout(m *map[string]models.About, key string, a models.About) {
	if *m == nil {
		*m = map[string]models.About{}
	}
	(*m)[key] = a
}

// variableAbout is the about of variable name as next declares it, where
// cur was on disk (had) and the learn step gave learned: one keploy adds is
// stamped; keploy's own (cur learned, its sum matching) keeps its about and
// its sum follows a change; one of unknown origin stays so, never stamped.
// (A person's is never merged: MergeLearned.)
func variableAbout(name string, cur models.MockVariable, had bool, next models.MockVariable, learned *models.About) *models.About {
	sum := func(run string) string { return models.VariableSum(name, next, run) }
	if !had {
		var a models.About
		if learned != nil {
			a = *learned
		}
		s := a.Stamped(models.AboutKindVariable, name, next.Echo, sum)
		return &s
	}
	if models.VariableAboutState(name, cur) != models.AboutLearned {
		return nil
	}
	a := cur.About.Clone()
	a.Sum = sum(a.Run)
	return &a
}

// MergeVariable is two declarations of one variable in one call as one: a
// bind place from either, every use and answer place of both, and an echo
// only when both say so (a call another uses the value of is not its echo).
// A clock read goes with its bind, and holds for one both bind only when
// both say so (models.MergeVariables).
func MergeVariable(a, b models.MockVariable) models.MockVariable {
	if a.Recorded == "" {
		// a declares nothing yet: b is the first declaration, and where it
		// came from goes with it.
		a.Recorded, a.Echo = b.Recorded, b.Echo
		a.About, a.Other = models.CloneAbout(b.About), maps.Clone(b.Other)
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

// noiseAdded is how added (MergeLearned) names noise learned at path.
func noiseAdded(path string) string { return path + " (noise)" }

// clockAdded is how added names the clock read o learned at path.
func clockAdded(path string, o models.ClockOffset) string { return path + " (" + o.String() + ")" }

// AddedAbouts is, by the name added (MergeLearned) gives each, the about of
// every decision on mock that carries one: each ignored field, each clock
// read, and each variable's bind and use places. The judge prints the why
// of a learned line from it, so the line says what the file says. An about
// at a path that is no longer noise or a clock read (an orphan) names no
// decision, and a decision with no about (unknown origin) is not in it;
// nil when there is none. The abouts share mock's Seen.
func AddedAbouts(mock *models.Mock) map[string]models.About {
	if mock == nil {
		return nil
	}
	out := map[string]models.About{}
	for path, a := range mock.Spec.NoiseAbout {
		if _, ok := mock.Spec.ReqBodyNoise[path]; ok {
			out[noiseAdded(path)] = a
		}
	}
	for path, a := range mock.Spec.ClockAbout {
		if o, ok := mock.Spec.Clocks[path]; ok {
			out[clockAdded(path, o)] = a
		}
	}
	for name, v := range mock.Spec.Variables {
		if v.About == nil {
			continue
		}
		for _, e := range variableAdded(name, models.MockVariable{}, v) {
			out[e] = *v.About
		}
	}
	if len(out) == 0 {
		return nil
	}
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
