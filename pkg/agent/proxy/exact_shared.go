package proxy

import (
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
type sharedIdent struct {
	worker *reachedWorker
	bg     *backgroundState
	kept   *keptLoad
}

// sharedIdentOf is v's ident now; w is the reached worker scope it was
// installed with.
func sharedIdentOf(v *exactView, w *reachedWorker) sharedIdent {
	id := sharedIdent{worker: w, bg: v.bg}
	if v.kept != nil {
		id.kept = v.kept.cur.Load()
	}
	return id
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
// view spent kept (showsStatic in place of shows). False while no test is
// open.
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

var _ integrations.SharedReads = (*MockManager)(nil)

// SharedGeneration, SharedMocks and OwnedUnanswered are the manager's while
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

var _ integrations.SharedReads = (*heldScope)(nil)

// SharedGeneration, SharedMocks and OwnedUnanswered are the store's while
// this view passes through to it (exact serving), and false otherwise: the
// view then narrows each read by its worker's names, which the shared part
// does not.
func (s *scopedMockDb) SharedGeneration(kind models.Kind) (integrations.SharedGen, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok && s.passThrough() {
		return sr.SharedGeneration(kind)
	}
	return integrations.SharedGen{}, false
}

func (s *scopedMockDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok && s.passThrough() {
		return sr.SharedMocks()
	}
	return nil, nil, false
}

func (s *scopedMockDb) OwnedUnanswered() ([]*models.Mock, bool) {
	if sr, ok := s.MockMemDb.(integrations.SharedReads); ok && s.passThrough() {
		return sr.OwnedUnanswered()
	}
	return nil, false
}

var _ integrations.SharedReads = (*scopedMockDb)(nil)
