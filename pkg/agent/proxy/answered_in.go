package proxy

import (
	"slices"
	"sync"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// testWindows is the set of test windows the agent holds open
// (models.ScopeWindow.Seq), as it told the proxy (agent.TestWindowNoter). A
// proxy's managers share one: a worker's manager, or the one a new session
// puts in place, serves under the windows open before it was made. Its lock
// is a leaf: the agent takes it holding its scope lock, a manager holding
// consumedMu.
type testWindows struct {
	mu   sync.Mutex
	open map[uint64]struct{}
}

func newTestWindows() *testWindows {
	return &testWindows{open: make(map[uint64]struct{})}
}

func (w *testWindows) setOpen(seq uint64, open bool) {
	w.mu.Lock()
	if open {
		w.open[seq] = struct{}{}
	} else {
		delete(w.open, seq)
	}
	w.mu.Unlock()
}

// each calls fn with each window open now; it reports whether there was one.
func (w *testWindows) each(fn func(seq uint64)) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for seq := range w.open {
		fn(seq)
	}
	return len(w.open) > 0
}

// answeredInMax bounds the window ids a manager keeps between consumed reads,
// over all its mocks (answeredWins): a recording keeps one id per window it
// answered in, however many times it answered there, so it is reached only by
// many recordings each answering in many windows. An id past it is counted
// (answeredWins.lost), never kept.
const answeredInMax = 1 << 16

// answeredWins is what a manager keeps of where a mock answered since the
// last consumed read (models.MockState.AnsweredIn, AnsweredOutside,
// AnsweredInLost).
type answeredWins struct {
	ids     map[uint64]struct{}
	outside bool
	lost    int
}

// noteAnsweredInLocked records that the mock name answered now: under every
// test window open, or outside every one. Caller holds consumedMu.
func (m *MockManager) noteAnsweredInLocked(name string) {
	if m.answeredIn == nil {
		m.answeredIn = make(map[string]*answeredWins)
	}
	a := m.answeredIn[name]
	if a == nil {
		a = &answeredWins{}
		m.answeredIn[name] = a
	}
	if !m.windows.each(func(seq uint64) {
		if _, kept := a.ids[seq]; kept {
			return
		}
		if m.answeredInHeld >= answeredInMax {
			a.lost++
			return
		}
		if a.ids == nil {
			a.ids = make(map[uint64]struct{})
		}
		a.ids[seq] = struct{}{}
		m.answeredInHeld++
	}) {
		a.outside = true
	}
}

// withAnsweredInLocked is s with where it answered since the last read
// (dropAnsweredInLocked forgets it). Caller holds consumedMu. Every consumed
// state has an entry: flagMockAsUsed notes one as it lists the state, and
// both are dropped together.
func (m *MockManager) withAnsweredInLocked(s models.MockState) models.MockState {
	a := m.answeredIn[s.Name]
	if a == nil {
		return s
	}
	s.AnsweredIn = nil
	for seq := range a.ids {
		s.AnsweredIn = append(s.AnsweredIn, seq)
	}
	slices.Sort(s.AnsweredIn)
	s.AnsweredOutside, s.AnsweredInLost = a.outside, a.lost
	return s
}

// dropAnsweredInLocked forgets where every mock answered: the consumed list
// it goes with was read or dropped. Caller holds consumedMu.
func (m *MockManager) dropAnsweredInLocked() {
	m.answeredIn = nil
	m.answeredInHeld = 0
}

// OpenTestWindow says the agent holds test window seq open
// (models.ScopeWindow.Seq): every mock served from now until
// CloseTestWindow(seq) answered in it (models.MockState.AnsweredIn).
func (m *MockManager) OpenTestWindow(seq uint64) { m.windows.setOpen(seq, true) }

// CloseTestWindow says the agent no longer holds test window seq open.
func (m *MockManager) CloseTestWindow(seq uint64) { m.windows.setOpen(seq, false) }

// testWindowsShared is the windows every manager of the proxy serves under.
func (p *Proxy) testWindowsShared() *testWindows {
	p.windowsOnce.Do(func() { p.windows = newTestWindows() })
	return p.windows
}

// shareWindows makes mm, a manager the proxy just made, serve under the test
// windows the agent holds open (testWindowsShared).
func (p *Proxy) shareWindows(mm *MockManager) *MockManager {
	mm.windows = p.testWindowsShared()
	return mm
}

// OpenTestWindow implements agent.TestWindowNoter.
func (p *Proxy) OpenTestWindow(seq uint64) { p.testWindowsShared().setOpen(seq, true) }

// CloseTestWindow implements agent.TestWindowNoter.
func (p *Proxy) CloseTestWindow(seq uint64) { p.testWindowsShared().setOpen(seq, false) }

var _ agent.TestWindowNoter = (*Proxy)(nil)
