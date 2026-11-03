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
	if mk != nil && h.othersOwn(h.movedTo(), mk) {
		return true
	}
	if mk == nil || h.owns == nil && !h.hideBg || !h.background(mk.Name) {
		return false
	}
	return h.HidesBackground()
}

// hides reports whether h hides mk: the background's, by name (hidesName),
// or, by the view that served it, another test's own (othersOwn).
func (h *heldScope) hides(mk *models.Mock) bool {
	return mk != nil && (h.hidesName(mk.Name) || h.othersOwn(h.movedTo(), mk))
}

// hidesName reports whether h hides the recording named name: the call is
// one of the open test's own calls (HidesBackground said so) and name is one
// of the background's recordings it does not own (background); or the call
// was decided in a view that did not own name, and the view in force that
// replaced it does (othersOwn's, read by name alone, MockSpent's).
func (h *heldScope) hidesName(name string) bool {
	if h.hideBg && h.background(name) {
		return true
	}
	moved := h.movedTo()
	return moved.owns(name) && !h.heldIn.owns(name)
}

// movedTo is the view in force when h's call reads it but was not decided
// in it: the call was marked from a round snapshot (snapped: record's full
// round, whose learn step decided it in the view that snapshot read,
// heldIn), and a test's begin or end has since installed another view. nil
// otherwise: while that view is in force, and for every other call, which
// reads the view its mark kept (readIn) or is decided in the view in force
// (one marked where the learn tracker was ahead of the store). Asked after a
// read, nil says the read was of the call's view: views are installed new
// and never again, so one still in force after a read was for all of it.
func (h *heldScope) movedTo() *exactView {
	if !h.snapped {
		return nil
	}
	if v := h.exactView(); v != h.heldIn {
		return v
	}
	return nil
}

// othersOwn reports whether mk, read or consumed by h's call once its view
// moved (moved, movedTo: nil for none), is a test's own recording that the
// view the call was decided in did not own: a copy another view served
// (models.TestModeInfo.ServedIn: the next test's, or a pool view's ahead,
// the next test's calls in flight), however late in the move the read that
// found it was, or, a mock that carries no view, one the view in force
// owns. Such a call reads, walks and spends the shared scopes (the pool,
// the worker scope, the start-up) in the view in force, but is never
// answered from another test's own recordings: no read of h holds one, and
// no consume or spend door of h settles one (refuses). A recording the
// call's view owned too (a between-tests call's in-flight recording of the
// test that then began) is the call's own still.
func (h *heldScope) othersOwn(moved *exactView, mk *models.Mock) bool {
	if moved == nil || h.heldIn.owns(mk.Name) {
		return false
	}
	if mk.TestModeInfo.ServedIn != 0 {
		// A view's copy: the view that served it filed it in its index
		// (exactView.add), so one heldIn served is one heldIn owns, and
		// this copy another view served.
		return true
	}
	return moved.owns(mk.Name)
}

// owns reports whether the recording named name is one of v's own: the
// open test's, or on a pool view the next test's in flight (ahead). None
// for a nil v.
func (v *exactView) owns(name string) bool {
	if v == nil {
		return false
	}
	_, own := v.index[name]
	return own
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
// is answered by. A skipped test's read keploy mock replay moved to the
// worker scope (isSkippedRead) is never hidden: the open test that holds its
// own recording of the read now makes the skipped test's too, after its own,
// and the read answers it once (ServeExactWorkerSkipped).
func (h *heldScope) background(name string) bool {
	v := h.exactView()
	if v == nil || v.test == "" || !v.bg.has(name) && !v.isWorker(name) || v.isSkippedRead(name) {
		return false
	}
	_, own := v.index[name]
	return !own
}

// unhidden is mocks less what h hides (hides, the view in force read once,
// after the read); mocks itself when it hides nothing.
func (h *heldScope) unhidden(mocks []*models.Mock) []*models.Mock {
	moved := h.movedTo()
	if !h.hideBg && moved == nil {
		return mocks
	}
	out := mocks
	for i, mk := range mocks {
		hide := mk != nil && (h.hideBg && h.background(mk.Name) || h.othersOwn(moved, mk))
		if !hide {
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
// view's h reads in (readView), as the manager's is the view in force's, and
// every recording h hides (Spent's, by name). Its reads drop what that view
// spent, never what another view did.
func (h *heldScope) MockSpent(name string) bool {
	if h.hidesName(name) {
		return true
	}
	v := h.readView()
	return v != nil && v.spentNow(name)
}

// The consume doors refuse what h hides, the call's decision asked first for
// a background recording (refuses); any other consume is the held round's
// (roundScope), and one it refuses is settled (settled).

func (h *heldScope) DeleteFilteredMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.settled(h.roundScope.DeleteFilteredMock(mock))
}

func (h *heldScope) DeleteUnFilteredMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.settled(h.roundScope.DeleteUnFilteredMock(mock))
}

func (h *heldScope) DeleteStartupMock(mock models.Mock) bool {
	return !h.refuses(&mock) && h.settled(h.roundScope.DeleteStartupMock(mock))
}

func (h *heldScope) UpdateUnFilteredMock(old, new *models.Mock) bool {
	return !h.refuses(old) && h.settled(h.roundScope.UpdateUnFilteredMock(old, new))
}

func (h *heldScope) MarkMockAsUsed(mock models.Mock) bool {
	return !h.refuses(&mock) && h.settled(h.roundScope.MarkMockAsUsed(mock))
}

// settled is ok, a consume door's answer, after h took a refusal into its
// reads: once a consume the view in force settles is refused while h reads in
// another view (readIn: a test's begin or end installed the view in force
// after the call's read), h reads, walks and spends in the view in force
// from then on (heldScope.readView). A parser
// takes a refused consume for a recording another call took, and reads
// again: read in the view the call was read in, the recording the view in
// force refused was offered again, and refused again, without end (the
// Generic parser's matcher spun on a test's own exchange once the test ended
// under the call). Read in the view in force, the call reads what any call
// made now reads: that recording is not offered, and the call is answered
// by another or misses, as a call read after the begin or end is.
func (h *heldScope) settled(ok bool) bool {
	if !ok && h.elsewhere() {
		h.g.readIn = nil
	}
	return ok
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
