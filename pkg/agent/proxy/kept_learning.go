package proxy

import (
	"slices"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// What keploy record keeps of a test set's first-cycle learning, on the
// recordings the staged trees hold (the pool's, the worker scope's, the
// protocol-phase ones): the set's retry cycles are served them as the next
// keploy test reads them once the learning is persisted
// (models.MockFilterParams.Learned). The agent puts it on the recordings its
// own store hands out (a test's own, read as it opens); the trees were
// filled at the set's staging, before anything was learned, so their reads
// under exact serving put it on as they hand a recording out (keptOverlay).

// keptOverlay is the manager's kept learning (SetKeptLearning): nil while
// the set's learn step runs, and set, holding no learning when the round
// keeps none, once it has ended (SetLearnEnded). filed is what the staging's
// matchers filed over (filedBases), which it hands out as staged once the
// learn step has ended.
type keptOverlay struct {
	cur   atomic.Pointer[keptLoad]
	filed filedBases
}

// filedBases is, by name, the staged recording that the staging's first
// filing of a copy of it replaced in the trees (UpdateUnFilteredMock: a
// reusable recording a matcher updates in place, its copy carrying what the
// learn step learned or the matcher detected on it). It is the recording as
// the staging put it there, before anything was learned, and the base the
// overlay hands a filed copy out on once the learn step has ended: the
// first cycle's learning is not the round's (the round keeps what it can
// tie to a test; KeptLearning is that), so a retry cycle is never served it
// off a tree. One staging's (epoch), replaced by the next staging's first
// filing; a base read in another staging is none.
type filedBases struct {
	mu     sync.Mutex
	epoch  uint64
	byName map[string]*models.Mock
}

// note records replaced, the value a filing in staging epoch replaced, as
// its name's base, unless the staging has one for it already: a later
// filing replaces a copy filed before it.
func (f *filedBases) note(epoch uint64, replaced *models.Mock) {
	if replaced == nil || replaced.Name == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byName == nil || f.epoch != epoch {
		f.epoch, f.byName = epoch, map[string]*models.Mock{}
	}
	if _, ok := f.byName[replaced.Name]; !ok {
		f.byName[replaced.Name] = replaced
	}
}

// base is the staged recording named name that staging epoch's filings
// replaced (note); nil when none of its copies was filed in that staging.
func (f *filedBases) base(epoch uint64, name string) *models.Mock {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.epoch != epoch {
		return nil
	}
	return f.byName[name]
}

// learnsBeyond says whether caller, the copy a parser consumes a recording
// by, carries learning that base, the copy exact serving handed the parser
// out, does not: variables or binds a learn step learned on it
// (MockSpec.Learned, LearnedBinds; a recording the store hands out carries
// none), a noise path or a noise pattern base lacks (an empty pattern list
// ignores the whole field), a variable base lacks, or a clock read base
// lacks or holds with another offset (MockSpec.Clocks). No parser in the tree
// reaches three of these today, and they have no row: a new pattern on a
// noise path base holds, a whole-field noise over base's patterns (every
// noise merge keeps the keys and patterns it holds), and learned binds with
// no learned variable (LearnedBinds come only with Learned).
func learnsBeyond(base, caller *models.Mock) bool {
	if caller == nil {
		return false
	}
	if len(caller.Spec.Learned()) > 0 || len(caller.Spec.LearnedBinds()) > 0 {
		return true
	}
	var noise map[string][]string
	var vars models.MockVariables
	var clocks map[string]models.ClockOffset
	if base != nil {
		noise, vars, clocks = base.Spec.ReqBodyNoise, base.Spec.Variables, base.Spec.Clocks
	}
	for path, patterns := range caller.Spec.ReqBodyNoise {
		had, ok := noise[path]
		if !ok {
			return true
		}
		if len(had) == 0 {
			continue
		}
		if len(patterns) == 0 {
			return true
		}
		for _, p := range patterns {
			if !slices.Contains(had, p) {
				return true
			}
		}
	}
	for name := range caller.Spec.Variables {
		if _, ok := vars[name]; !ok {
			return true
		}
	}
	for path, o := range caller.Spec.Clocks {
		if had, ok := clocks[path]; !ok || had != o {
			return true
		}
	}
	return false
}

// learnedAfterEnd is MockState.LearnedAfterEnd of a consume by caller of
// the recording exact serving handed out as base: under the proven switch,
// with the set's learn step ended, caller learns beyond base.
func (m *MockManager) learnedAfterEnd(base, caller *models.Mock) bool {
	return provenswitch.On() && m.exact.learnEnded.Load() && learnsBeyond(base, caller)
}

// servedAs is the copy exact serving hands out of mk, a tree's value
// (keptOverlay.onto): the base a consume of it through a door that names
// only the caller's copy is read against.
func (m *MockManager) servedAs(mk *models.Mock) *models.Mock {
	return m.exact.kept.onto(mk, m.stagingEpoch.Load())
}

// learnedAfterEndNamed is learnedAfterEnd for a door that names only the
// caller's copy (a spend, a tree's delete door): its base is the staged
// recording of the caller's name as exact serving hands it out
// (servedAsNamed).
func (m *MockManager) learnedAfterEndNamed(caller *models.Mock) bool {
	if caller == nil || !provenswitch.On() || !m.exact.learnEnded.Load() {
		return false
	}
	return learnsBeyond(m.servedAsNamed(caller), caller)
}

// learnedAfterEndPrepared is learnedAfterEndNamed for a tier that may hold
// the round lock (MarkMockAsUsed's), which must not take swapMu to read the
// staging: it reads the outside answers the door prepared before it. ok is
// false when they are not this staging's (a staging landed, or the learn step
// ended, since the door looked), so nothing says what the consume learned.
// The tier refuses that consume and the parser reads again, as a consume
// outside every test does while a staging lands (consumeOutside).
func (m *MockManager) learnedAfterEndPrepared(caller *models.Mock) (learned, ok bool) {
	if !provenswitch.On() || !m.exact.learnEnded.Load() {
		return false, true
	}
	st := m.outsideNow()
	if st == nil {
		return false, false
	}
	return learnsBeyond(m.servedAsIn(st, caller), caller), true
}

// learnedAfterEndAt is learnedAfterEnd for a door that found found, the
// tree's value caller consumed (nil: learnedAfterEndNamed).
func (m *MockManager) learnedAfterEndAt(found, caller *models.Mock) bool {
	if found == nil {
		return m.learnedAfterEndNamed(caller)
	}
	return m.learnedAfterEnd(m.servedAs(found), caller)
}

// servedAsNamed is the copy exact serving hands out of the staged recording
// named as caller is: the staging's tree value of that name
// (outsideState.staged; a copy a matcher filed over it is handed out as
// staged, servedAs). nil when the staging holds none of that name, so every
// noise path and variable caller carries reads as learned beyond it (fail
// closed: its consumers get no verdict, never a pass through what nothing
// keeps), said in a Warn naming the recording, once a staging per name
// (warnUnstaged). A door that took the value out of its tree passes it
// instead (learnedAfterEndAt): the staging's values may be read after the
// take.
func (m *MockManager) servedAsNamed(caller *models.Mock) *models.Mock {
	m.prepareOutside()
	st := m.outsideNow()
	if st == nil {
		m.warnUnstaged(caller.Name)
		return nil
	}
	return m.servedAsIn(st, caller)
}

// outsideNow is the outside answers prepared for the current staging, nil
// when none are. It takes no lock.
func (m *MockManager) outsideNow() *outsideState {
	if st := m.exact.outside.cur.Load(); st != nil && st.epoch == m.stagingEpoch.Load() {
		return st
	}
	return nil
}

// servedAsIn is servedAsNamed read from st, the current staging's outside
// answers.
func (m *MockManager) servedAsIn(st *outsideState, caller *models.Mock) *models.Mock {
	if r := st.staged[caller.Name]; r != nil {
		return m.servedAs(r)
	}
	if _, said := st.unstaged.LoadOrStore(caller.Name, true); !said {
		m.warnUnstaged(caller.Name)
	}
	return nil
}

// warnUnstaged says that a parser consumed name, a recording the set's
// staging does not hold, after the set's learn step ended: everything its
// copy carries is read as learned then (servedAsNamed).
func (m *MockManager) warnUnstaged(name string) {
	if m.logger == nil {
		return
	}
	m.logger.Warn("a parser consumed a recording the test set's staging does not hold after the set's learn step ended, so keploy reads every noise path and variable it carries as learned then, and the tests that consumed it have no verdict",
		zap.String("mock", name))
}

// asStaged is mk, a copy a matcher filed over the staged recording base,
// with base's noise, variables and clock reads and none of what the learn
// step learned on the copy: the rest of the copy (its place in the trees) is
// mk's.
func asStaged(mk, base *models.Mock) *models.Mock {
	c := mk.ShallowCopy()
	c.Spec.ReqBodyNoise = base.Spec.ReqBodyNoise
	c.Spec.Variables = base.Spec.Variables
	c.Spec.Clocks = base.Spec.Clocks
	c.Spec.SetLearned(base.Spec.Learned(), base.Spec.LearnedBinds())
	return c
}

// keptLoad is one SetKeptLearning: the learning, and the learned copy of
// each staged recording read since, by name. A name keeps the copies of the
// last keptCopies tree values read (a recording a matcher updated is a new
// value: TreeDb.updateReplacing; the start-up and session tiers may each
// hold one), so a recording read again is the same copy while its tree
// value stays, and an updated one leaves no copy behind for good.
type keptLoad struct {
	l      *learn.KeptLearning
	mu     sync.Mutex
	copies map[string][]keptCopy
}

// keptCopies is how many tree values of one name keep their learned copy.
const keptCopies = 2

type keptCopy struct{ base, out *models.Mock }

// set replaces what o holds with l. ended says the set's learn step has
// ended: o then holds l even when it is nil (the round keeps no learning),
// so a filed copy is handed out as staged (onto). nil with the learn step
// running clears it.
func (o *keptOverlay) set(l *learn.KeptLearning, ended bool) {
	if l == nil && !ended {
		o.cur.Store(nil)
		return
	}
	o.cur.Store(newKeptLoad(l))
}

// ended says the set's learn step has ended: o holds what the round keeps
// from now on, none until SetKeptLearning says what; a load already there
// stays, with its copies.
func (o *keptOverlay) ended() {
	o.cur.CompareAndSwap(nil, newKeptLoad(nil))
}

func newKeptLoad(l *learn.KeptLearning) *keptLoad {
	return &keptLoad{l: l, copies: map[string][]keptCopy{}}
}

// onto is mk, a tree's value in staging epoch, as exact serving hands it out
// once the set's learn step has ended: a copy a matcher filed over the staged
// recording (filedBases) is that recording again (asStaged), and what o
// holds of it is merged on (learn.KeptLearning.Onto). The result is pooled
// (models.Mock.MarkPooled): matchers copy it rather than edit it, as they do
// a tree's value. It is mk itself while the learn step runs, and when mk is
// as staged and o holds nothing of it.
func (o *keptOverlay) onto(mk *models.Mock, epoch uint64) *models.Mock {
	if o == nil || mk == nil {
		return mk
	}
	k := o.cur.Load()
	if k == nil {
		return mk
	}
	base := o.filed.base(epoch, mk.Name)
	filed := base != nil && base != mk
	if !filed && !k.l.Has(mk.Name) {
		return mk
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	list := k.copies[mk.Name]
	for _, c := range list {
		if c.base == mk {
			return c.out
		}
	}
	in := mk
	if filed {
		in = asStaged(mk, base)
	}
	out := k.l.Onto(in)
	out.MarkPooled()
	list = append([]keptCopy{{base: mk, out: out}}, list...)
	if len(list) > keptCopies {
		list = list[:keptCopies]
	}
	k.copies[mk.Name] = list
	return out
}

// SetKeptLearning puts l, what keploy record keeps of the staged set's
// first-cycle learning, on every staged recording exact serving hands out
// from now on (keep, withLearned): the set's retry cycles, which the agent
// serves with its learn step ended (SetLearnEnded), are judged with what
// the first cycle learned on the recordings it shows them, the pool's and
// the worker scope's too, never failed for learning they do not take. The
// staged recordings themselves are not written: a matcher that updates one
// files its copy of the learned one, which the next staging replaces. The
// agent sets it before the filter's view goes live, and that view's install
// moves the revisions, so a parser that indexes what it read reads the
// learned copies. nil clears it, and so do the learn
// step taking up again (SetLearnEnded(false), each set's staging) and the
// next set's generation (dropPreviousSetConsumes): it is the staged set's.
func (m *MockManager) SetKeptLearning(l *learn.KeptLearning) {
	m.exact.kept.set(l, m.exact.learnEnded.Load())
}

// learned is mk as exact serving hands it out: with what the manager keeps
// of the set's first-cycle learning on it (SetKeptLearning).
func (v *exactView) learned(mk *models.Mock) *models.Mock {
	if v.kept == nil {
		return mk
	}
	var epoch uint64
	if v.epoch != nil {
		epoch = v.epoch.Load()
	}
	return v.kept.onto(mk, epoch)
}

// withLearned is in with each recording as exact serving hands it out
// (learned); in itself when none changes.
func (v *exactView) withLearned(in []*models.Mock) []*models.Mock {
	if v.kept == nil || v.kept.cur.Load() == nil {
		return in
	}
	var out []*models.Mock
	for i, mk := range in {
		l := v.learned(mk)
		if l != mk && out == nil {
			out = append(make([]*models.Mock, 0, len(in)), in[:i]...)
		}
		if out != nil {
			out = append(out, l)
		}
	}
	if out == nil {
		return in
	}
	return out
}
