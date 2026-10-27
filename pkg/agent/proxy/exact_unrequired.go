package proxy

import "maps"

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
// replay never used and take them out of the recording. A recording with no
// mark is required as before.

// SetUnrequired installs, for the staged set, the recordings a test is served
// in its window but never required to make, each with why (judging's
// models.MockEntry.Background). A replay that sends none installs an empty
// set, which requires every test mock, as does a session reset.
func (m *MockManager) SetUnrequired(names map[string]string) {
	set := maps.Clone(names)
	if set == nil {
		set = map[string]string{}
	}
	m.exact.unrequired.Store(&set)
}

// unrequiredNames is what SetUnrequired last installed; nil when nothing was.
func (m *MockManager) unrequiredNames() map[string]string {
	if p := m.exact.unrequired.Load(); p != nil {
		return *p
	}
	return nil
}

// ExactTestUnusedBackground is, under exact serving, the latest test opened
// (as ExactTestOutcome names it) and those of its own recordings marked as
// the application's background work (SetUnrequired) that have not answered,
// in recorded order. Read once the test stops being served, it is final. ok
// is false when exact serving is off.
func (m *MockManager) ExactTestUnusedBackground() (test string, unused []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, false
	}
	v := m.exact.last.Load()
	if v == nil {
		return "", nil, true
	}
	unrequired := m.unrequiredNames()
	if len(unrequired) == 0 {
		return v.test, nil, true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, done := range v.consumed {
		if done || v.isCand(i) {
			continue
		}
		if _, bg := unrequired[v.owned[i].Name]; bg {
			unused = append(unused, v.owned[i].Name)
		}
	}
	return v.test, unused, true
}
