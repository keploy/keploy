package proxy

import (
	"sort"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// sharedIdent is what a test's view shows of the trees beyond the
// protocol-phase recordings, and how it hands each out: the reached worker
// scope (its names and order), the background rules (the pool it shows in a
// test), and the kept learning each recording is handed out with
// (exactView.learned). Two test views with the same ident show the same of
// the same trees (exactView.showsStatic). A staging changes the trees, and
// the copies learned hands out are per staging epoch, but a staging moves the
// stored generation of every kind it touches (setMocksWithWindowLocked), the
// SharedGen's other half: the ident leaves the epoch out.
//
// own is what the view leaves out of its shared part for being its own
// (ownShared): two views that own different recordings of the pool or the
// worker scope show different shared parts.
type sharedIdent struct {
	worker *reachedWorker
	bg     *backgroundState
	kept   *keptLoad
	own    string
}

// sharedIdentOf is v's ident now; w is the reached worker scope it was
// installed with.
func sharedIdentOf(v *exactView, w *reachedWorker) sharedIdent {
	id := sharedIdent{worker: w, bg: v.bg, own: v.ownShared()}
	if v.kept != nil {
		id.kept = v.kept.cur.Load()
	}
	return id
}

// ownShared is, sorted and each ended by a NUL, the names of v's own
// recordings that its shared part would show were they not its own
// (showsStaticRest): one of the worker scope or the pool. "" when there are
// none, as for every test of the agent's plans, where a test owns what its
// mapping lists and the pool and the worker scope are what no mapping lists.
func (v *exactView) ownShared() string {
	var names []string
	for _, mk := range v.owned {
		if !mk.IsProtocolPhase() && v.sharesRest(mk) {
			names = append(names, mk.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(0)
	}
	return b.String()
}

// sharedViews numbers the views' shared parts (SharedGen's View): a view whose
// ident is the last view's takes its number, any other a new one. A pool view
// between two tests has their ident, so it leaves the number to the next
// test; only a test's view reports it (SharedGeneration). Held under the
// round lock.
type sharedViews struct {
	seq    uint64
	last   sharedIdent
	lastID uint64
}

// of sets v's shared number and ident (exactView.shared), w being the reached
// worker scope v is installed with.
func (s *sharedViews) of(v *exactView, w *reachedWorker) {
	id := sharedIdentOf(v, w)
	if s.lastID == 0 || s.last != id {
		s.seq++
		s.last, s.lastID = id, s.seq
	}
	v.shared = s.lastID
}

// SharedGeneration is the generation of the shared part of the open test's
// reads (integrations.SharedReads): the view's number and kind's stored
// generation (every kind's for ""), which moves with the staging generation
// except at an exact view's install. False while no test is open. Kept
// learning set while a view is in force is read from the next view's install
// on, as for StagingGeneration (SetKeptLearning).
func (m *MockManager) SharedGeneration(kind models.Kind) (integrations.SharedGen, bool) {
	v := m.exactView()
	if v == nil || v.test == "" {
		return integrations.SharedGen{}, false
	}
	var stored uint64
	if kind == "" {
		stored = atomic.LoadUint64(&m.stored)
	} else {
		m.revMu.RLock()
		ptr := m.storedByKind[kind]
		m.revMu.RUnlock()
		if ptr != nil {
			stored = atomic.LoadUint64(ptr)
		}
	}
	return integrations.SharedGen{View: v.shared, Store: stored}, true
}

// SharedMocks is the shared part of the open test's per-test read
// (GetPerTestMocksInWindow) and session read (GetSessionMocks): each read
// less the open test's unanswered recordings that lead it, with what the
// view spent kept (showsStatic in place of shows). It holds none of the open
// test's own recordings, answered or not: the view never shows its own from
// the trees (showsStaticRest). False while no test is open.
func (m *MockManager) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	v := m.exactView()
	if v == nil || v.test == "" {
		return nil, nil, false
	}
	m.swapMu.RLock()
	m.treesMu.RLock()
	filtered, startup, unfiltered := m.filtered, m.startup, m.unfiltered
	m.treesMu.RUnlock()
	m.swapMu.RUnlock()
	// A test's view reads no teardown (afterTests): its per-test read is its
	// own then what it shows of the per-test tier (exactPerTest).
	perTest = v.keepWith(v.shownOf(filtered, ""), v.showsStatic)
	// GetSessionMocks: GetStartupMocks, what the view shows of the startup
	// tier then the session tier, then GetSessionScopedMocks, what it shows
	// of the session tier then the startup tier, each mock once: the second
	// adds nothing the first did not read. Without a startup tree the first
	// reads nothing, and the session tier alone is the second.
	session = v.reusableWith("", nil, v.showsStatic, v.shownOf(startup, ""), v.shownOf(unfiltered, ""))
	return perTest, session, true
}

// OwnedUnanswered is the open test's unanswered recordings, which lead its
// per-test and session reads (integrations.SharedReads). False while no test
// is open.
func (m *MockManager) OwnedUnanswered() ([]*models.Mock, bool) {
	v := m.exactView()
	if v == nil || v.test == "" {
		return nil, false
	}
	return v.unanswered(""), true
}

// OwnedRecordings is the open test's own recordings, answered or not, in
// OwnedUnanswered's order, and the number of the view that shows them
// (exactView.gen): the list is the view's own and never changes while the
// view lasts, and each test's open installs a view with a new number. The
// caller reads it and never changes it. False while no test is open.
func (m *MockManager) OwnedRecordings() (uint64, []*models.Mock, bool) {
	v := m.exactView()
	if v == nil || v.test == "" {
		return 0, nil, false
	}
	return v.gen, v.owned, true
}

// OwnedUnansweredAt is the positions of at, in OwnedRecordings of the view
// numbered view, whose recordings have not answered, in at's order: what
// OwnedUnanswered reads of them now. False when that view is not the open
// test's now.
func (m *MockManager) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	v := m.exactView()
	if v == nil || v.test == "" || v.gen != view {
		return nil, false
	}
	return v.unansweredAt(at), true
}

var _ integrations.SharedReads = (*MockManager)(nil)

// SharedGeneration, SharedMocks and the owned reads are the manager's while
// the staging is still h's call's after they were read, and report false
// once it moved, when h reads nothing (heldScope.held).
func (h *heldScope) SharedGeneration(kind models.Kind) (integrations.SharedGen, bool) {
	g, ok := h.MockManager.SharedGeneration(kind)
	if !ok || h.moved() {
		return integrations.SharedGen{}, false
	}
	return g, true
}

func (h *heldScope) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	perTest, session, ok = h.MockManager.SharedMocks()
	if !ok || h.moved() {
		return nil, nil, false
	}
	return perTest, session, true
}

func (h *heldScope) OwnedUnanswered() ([]*models.Mock, bool) {
	owned, ok := h.MockManager.OwnedUnanswered()
	if !ok || h.moved() {
		return nil, false
	}
	return owned, true
}

func (h *heldScope) OwnedRecordings() (uint64, []*models.Mock, bool) {
	view, owned, ok := h.MockManager.OwnedRecordings()
	if !ok || h.moved() {
		return 0, nil, false
	}
	return view, owned, true
}

func (h *heldScope) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	live, ok := h.MockManager.OwnedUnansweredAt(view, at)
	if !ok || h.moved() {
		return nil, false
	}
	return live, true
}

var _ integrations.SharedReads = (*heldScope)(nil)

// SharedGeneration, SharedMocks and the owned reads are the store's. They
// report something only while a test is open, when the view passes through
// to the store (exact serving, passThrough) and narrows no read by its
// worker's names.
func (s *scopedMockDb) SharedGeneration(kind models.Kind) (integrations.SharedGen, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok {
		return sr.SharedGeneration(kind)
	}
	return integrations.SharedGen{}, false
}

func (s *scopedMockDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok {
		return sr.SharedMocks()
	}
	return nil, nil, false
}

func (s *scopedMockDb) OwnedUnanswered() ([]*models.Mock, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok {
		return sr.OwnedUnanswered()
	}
	return nil, false
}

func (s *scopedMockDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok {
		return sr.OwnedRecordings()
	}
	return 0, nil, false
}

func (s *scopedMockDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok {
		return sr.OwnedUnansweredAt(view, at)
	}
	return nil, false
}

var _ integrations.SharedReads = (*scopedMockDb)(nil)

// WhenRestaged runs release once, at the next staging, once the staging
// moved every generation, or at Close; at once once closed
// (integrations.Restaged). A parser's index
// of the staged recordings then lets them go with the trees, not at its next
// call. One slot keeps one release.
func (m *MockManager) WhenRestaged(slot any, release func()) {
	m.restagedMu.Lock()
	if m.closed.Load() {
		m.restagedMu.Unlock()
		release()
		return
	}
	if m.restaged == nil {
		m.restaged = make(map[any]func())
	}
	m.restaged[slot] = release
	m.restagedMu.Unlock()
}

// letDerivedGo runs what the parsers asked to run at a staging or Close
// (WhenRestaged), outside the lock: called after the staging's moves, so a
// parser that asks after the call samples the moved generation. A parser
// that sampled the generation before the move and asks after this call keeps
// what it built until the next staging or its next call, which finds the
// generation moved: what it keeps is then one staging's recordings, not more.
func (m *MockManager) letDerivedGo() {
	m.restagedMu.Lock()
	fns := m.restaged
	m.restaged = nil
	m.restagedMu.Unlock()
	for _, f := range fns {
		f()
	}
}
