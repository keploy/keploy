package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A CALL ONE MORE THAN THE OPEN TEST'S OWN RECORDINGS OF IT
// (integrations.HideBackground).
//
// A view shows a test a background after the test's own recordings
// (exactView.shows): under the background rules the pool's and the reached
// worker scope's data-plane recordings, under any rules the reached worker
// scope's (ServeExactWorker), each answering once in the staging. A parser
// that finds a call is one of the open test's own recordings, answered or
// not, reads the call's held store with the background hidden: none of its
// reads holds a background recording that is not the open test's own, and
// its consume and spend doors refuse one. The call then finds the test's own
// recordings not yet answered, if any, and otherwise misses as one call more
// than the test recorded (models.MissExtraCall), whatever the background
// holds of it: a duplicate of a recorded call never passes answered by a
// call the application made between tests or from a worker, made after the
// test's own call or at once with it. The background's recording is not
// spent, and still answers the call it is the recording of.

// HideBackground implements integrations.BackgroundHider: h, in place (it is
// one call's, as ServingFor's is), with the background hidden from its reads
// and doors from now on (hides).
func (h *heldScope) HideBackground() integrations.MockMemDb {
	h.hideBg = true
	return h
}

var _ integrations.BackgroundHider = (*heldScope)(nil)

// hides reports whether h hides mk (hidesName).
func (h *heldScope) hides(mk *models.Mock) bool {
	return mk != nil && h.hidesName(mk.Name)
}

// hidesName reports whether h hides the recording named name: h's call is
// one more than the open test's own recordings of it (HideBackground), a
// test is open in the view in force, and name is one of the background's
// recordings the open test does not own: under the background rules the
// pool's or the worker scope's, under any rules the reached worker scope's
// (isWorker). A background recording a test
// owns (one judging moved into it) is that test's own, and answers its call.
// It reads the name alone, so every read and door of h, and a parser's index
// that asks MockSpent by name, hide the same recordings: a protocol-phase or
// async recording of the background is hidden too, which no call that
// repeats a test's own data-plane recording is answered by.
func (h *heldScope) hidesName(name string) bool {
	if !h.hideBg {
		return false
	}
	v := h.exactView()
	if v == nil || v.test == "" || !v.bg.has(name) && !v.isWorker(name) {
		return false
	}
	_, own := v.index[name]
	return !own
}

// unhidden is mocks less what h hides; mocks itself when it hides nothing.
func (h *heldScope) unhidden(mocks []*models.Mock) []*models.Mock {
	if !h.hideBg {
		return mocks
	}
	out := mocks
	for i, mk := range mocks {
		if !h.hides(mk) {
			if len(out) != len(mocks) {
				out = append(out, mk)
			}
			continue
		}
		if len(out) == len(mocks) {
			out = append(make([]*models.Mock, 0, len(mocks)), mocks[:i]...)
		}
	}
	return out
}

// Spent implements integrations.SpentReader: the manager's, and every
// recording h hides, so a parser's index of the staging, built from a read
// that holds the background, never offers one.
func (h *heldScope) Spent(mk *models.Mock) bool {
	return h.hides(mk) || h.MockManager.Spent(mk)
}

// MockSpent implements integrations.StagingGenerations' spent read: the
// manager's, and every recording h hides (Spent's, by name).
func (h *heldScope) MockSpent(name string) bool {
	return h.hidesName(name) || h.MockManager.MockSpent(name)
}

// The consume doors refuse what h hides; any other consume is the held
// round's (roundScope).

func (h *heldScope) DeleteFilteredMock(mock models.Mock) bool {
	return !h.hides(&mock) && h.roundScope.DeleteFilteredMock(mock)
}

func (h *heldScope) DeleteUnFilteredMock(mock models.Mock) bool {
	return !h.hides(&mock) && h.roundScope.DeleteUnFilteredMock(mock)
}

func (h *heldScope) DeleteStartupMock(mock models.Mock) bool {
	return !h.hides(&mock) && h.roundScope.DeleteStartupMock(mock)
}

func (h *heldScope) UpdateUnFilteredMock(old, new *models.Mock) bool {
	return !h.hides(old) && h.roundScope.UpdateUnFilteredMock(old, new)
}

func (h *heldScope) MarkMockAsUsed(mock models.Mock) bool {
	return !h.hides(&mock) && h.roundScope.MarkMockAsUsed(mock)
}

// OnConsume is a test seam: f, when not nil, runs on a consuming call's
// goroutine as the store is about to settle the consume of the recording
// named name, holding no lock of the store's; nil removes it. A test runs
// another call to its end between a call's match and its consume with it, so
// two calls interleave the same way every run, never by timing. Production
// code never sets it.
func (m *MockManager) OnConsume(f func(name string)) {
	if f == nil {
		m.exact.round.onConsume.Store(nil)
		return
	}
	m.exact.round.onConsume.Store(&f)
}
