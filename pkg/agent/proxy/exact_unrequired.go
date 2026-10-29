package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// SERVED, NEVER REQUIRED: AN APPLICATION'S BACKGROUND WORK IN A TEST'S WINDOW.
//
// A timer, a start-up job that outlives the application's readiness, or a
// delayed write runs on the application's own schedule, not the test's: at
// record one of its calls may land in a test's window, and at replay in
// another test's, or in none. Keploy's judging marks such a recording on the
// test that recorded it (models.MockEntry.Background, with why). Under exact
// serving the test is served it like any of its own recordings, in its
// window, when its call is made, and is never failed for not making it: it is
// left out of the test's unconsumed recordings (ExactTestOutcome) and listed
// on its own (ExactTestUnusedBackground), so judging can say which ones a
// replay never used and take them out of the recording. A call identical to
// one that already answered in the window is answered again with it
// (AnsweredBackground): the job ran more often than at record. The marks are
// per test (models.BackgroundMarks): a recording with no mark on the open
// test's own entry is required as before, and a second copy of its call is an
// extra-call miss, whatever another test's entry of the same recording says.

// SetUnrequired installs, for the staged set, per test, the recordings of its
// own list it is served in its window but never required to make, each with
// why (judging's models.MockEntry.Background). A mark is the test's: another
// test listing the same recording without one must still make it, and its
// second copy of the call is still an extra-call miss. A replay that sends
// none installs an empty set, which requires every test mock, as does a
// session reset.
func (m *MockManager) SetUnrequired(marks models.BackgroundMarks) {
	set := marks.Clone()
	if set == nil {
		set = models.BackgroundMarks{}
	}
	m.exact.unrequired.Store(&set)
	m.beginMarkedSeries()
}

// unrequiredOf is the marks SetUnrequired last installed on test's own list;
// nil when it has none or nothing was installed.
func (m *MockManager) unrequiredOf(test string) map[string]string {
	if p := m.exact.unrequired.Load(); p != nil {
		return p.Of(test)
	}
	return nil
}

// ExactTestUnusedBackground is, under exact serving, the latest test opened
// (as ExactTestOutcome names it) and those of its own recordings marked as
// the application's background work on its own list (SetUnrequired) that have
// not answered,
// in recorded order. Read once the test stops being served, it is final. ok
// is false when exact serving is off.
func (m *MockManager) ExactTestUnusedBackground() (test string, unused []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, false
	}
	o := m.latestOutcome()
	if o == nil {
		return "", nil, true
	}
	return o.test, append([]string(nil), o.unusedBackground...), true
}

// AnsweredBackground implements integrations.AnsweredBackgroundReader: the
// open test's own recordings marked as the application's background work
// (SetUnrequired) that have answered in it, in the order they did; none while
// no test is open, when nothing is marked, and with exact serving off.
func (m *MockManager) AnsweredBackground() []*models.Mock {
	v := m.exactView()
	if v == nil || v.test == "" {
		return nil
	}
	unrequired := m.unrequiredOf(v.test)
	if len(unrequired) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*models.Mock
	for _, i := range v.order {
		if v.notOwn(i) {
			continue
		}
		if _, bg := unrequired[v.owned[i].Name]; bg {
			out = append(out, v.owned[i])
		}
	}
	return out
}

var _ integrations.AnsweredBackgroundReader = (*MockManager)(nil)
