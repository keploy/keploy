package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A CALL ONE MORE THAN THE OPEN TEST'S OWN RECORDINGS OF IT
// (integrations.BackgroundHider).
//
// A view shows a test a background after the test's own recordings
// (exactView.shows): under the background rules the pool's and the reached
// worker scope's data-plane recordings, under any rules the reached worker
// scope's (ServeExactWorker), each answering once in the staging. A parser
// gives a call's held store its decision whether the call is one of the open
// test's own recordings, answered or not (HideBackgroundWhen). The store asks
// it at the first consume or spend door a background recording that is not
// the open test's own reaches (refuses): the background is about to answer
// the call. A call the test's own recordings answer never asks it. When the
// call is one of the test's own, that door refuses, and from then on none of
// the store's reads (but KindBackground's, which no call is answered from)
// holds a background recording that is not the open test's own, and its
// consume and spend doors refuse one (hides). The call, matched
// again, finds the test's own recordings not yet answered, if any, and
// otherwise misses as one call more than the test recorded
// (models.MissExtraCall), whatever the background holds of it: a duplicate
// of a recorded call never passes answered by a call the application made
// between tests or from a worker, made after the test's own call or at once
// with it. The background's recording is not spent, and still answers the
// call it is the recording of.

// HideBackgroundWhen implements integrations.BackgroundHider: h, in place (it
// is one call's, as ServingFor's is), with owns kept for the first door a
// background recording reaches (refuses) or HidesBackground, whichever asks
// first.
func (h *heldScope) HideBackgroundWhen(owns func() bool) integrations.MockMemDb {
	h.owns = owns
	return h
}

// HidesBackground implements integrations.BackgroundHider: owns's answer,
// asked now if no door asked it. owns is dropped before it is asked, so a
// read it makes of h reads the background as before the decision.
func (h *heldScope) HidesBackground() bool {
	if owns := h.owns; owns != nil {
		h.owns = nil
		h.hideBg = owns()
	}
	return h.hideBg
}

var _ integrations.BackgroundHider = (*heldScope)(nil)

// RefusesAnswer implements integrations.BackgroundHider: refuses.
func (h *heldScope) RefusesAnswer(mk *models.Mock) bool {
	return h.refuses(mk)
}

// refuses is a consume or spend door's check of mk: whether h hides it, the
// call's decision asked first when mk is a background recording the open
// test does not own (background), about to answer the call.
func (h *heldScope) refuses(mk *models.Mock) bool {
	if mk == nil || h.owns == nil && !h.hideBg || !h.background(mk.Name) {
		return false
	}
	return h.HidesBackground()
}

// hides reports whether h hides mk (hidesName).
func (h *heldScope) hides(mk *models.Mock) bool {
	return mk != nil && h.hidesName(mk.Name)
}

// hidesName reports whether h hides the recording named name: the call is
// one of the open test's own calls (HidesBackground said so) and name is one
// of the background's recordings it does not own (background).
func (h *heldScope) hidesName(name string) bool {
	return h.hideBg && h.background(name)
}

// background reports whether the recording named name is one of the
// background's recordings the open test does not own, while a test is open
// in the view in force: under the background rules the pool's or the worker
// scope's, under any rules the reached worker scope's (isWorker). A
// background recording a test owns (one judging moved into it) is that
// test's own, and answers its call. It reads the name alone, so every read
// and door of h, and a parser's index that asks MockSpent by name, hide the
// same recordings: a protocol-phase or async recording of the background is
// hidden too, which no call that repeats a test's own data-plane recording
// is answered by.
func (h *heldScope) background(name string) bool {
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

// The consume doors refuse what h hides, the call's decision asked first for
// a background recording (refuses); any other consume is the held round's
// (roundScope).

func (h *heldScope) DeleteFilteredMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.roundScope.DeleteFilteredMock(mock)
}

func (h *heldScope) DeleteUnFilteredMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.roundScope.DeleteUnFilteredMock(mock)
}

func (h *heldScope) DeleteStartupMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.roundScope.DeleteStartupMock(mock)
}

func (h *heldScope) UpdateUnFilteredMock(old, new *models.Mock) bool {
	return !h.refuses(old) && h.roundScope.UpdateUnFilteredMock(old, new)
}

func (h *heldScope) MarkMockAsUsed(mock models.Mock) bool {
	return !h.refuses(&mock) && h.roundScope.MarkMockAsUsed(mock)
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
