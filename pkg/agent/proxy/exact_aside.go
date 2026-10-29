package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// SERVED, UNCHECKED: A SET-ASIDE TEST'S RECORDINGS, TO THE CALLS AFTER IT.
//
// A test the replay sets aside (the proven gate skips it) makes none of its
// calls, but what its calls did can still be needed: a cache it filled at
// record is empty at replay, and the next test that reads the cache makes the
// set-aside test's call in its own window, where no recording of its own
// answers it. Under exact serving such a call is served the set-aside test's
// recording (founder D1: serve, don't require; S12: a data-plane call is
// answered by the recording it is, wherever the replay's timing puts it): by
// exact match only, unchecked and never required of the test it lands in, at
// most once in the staging, and named on that test's evidence ("<mock>
// served from <test> (set aside)", models.ScopeEvidence.ServedAside). A
// recording that places or binds variables is not served aside: its values
// flow from or into its own test's other calls, which were never made.
//
// The set-aside recordings are in none of the view's reads: a parser asks
// for them (integrations.SetAsideReader) only for a call that no recording
// it reads answered, matches the call exactly against them alone, and
// consumes the one it matched through the store's door. That includes a call
// one more than the open test recorded: a set-aside test's per-process
// first-use refresh (a read, the same read again, a write) lands in the
// window of the next test that uses it, beside that test's own copy of the
// read. Each set-aside recording answers once in the staging, so the calls of
// one shape across the set never exceed what the set recorded; past every
// exact copy left the call stays an extra call (keploy #4745's F20: never a
// recording served again, nor one the call is not). A parser that does not
// ask serves none, as before.

// asideRecording is one recording of a set-aside test: the test, and the
// recording as the replay staged it.
type asideRecording struct {
	from string
	mock *models.Mock
}

// ServeExactAside adds the recordings of test from, which the replay set
// aside (skipped), to those served to the tests after it, cumulative over the
// session: each test view installed after it shows them after its own, as
// the package comment says. It installs no view itself: the next
// ServeExactTest does. A session reset forgets them.
func (m *MockManager) ServeExactAside(from string, mocks []*models.Mock) {
	if from == "" || len(mocks) == 0 {
		return
	}
	var next []asideRecording
	if p := m.exact.setAside.Load(); p != nil {
		next = append(next, (*p)...)
	}
	for _, mk := range mocks {
		if mk != nil && mk.Name != "" {
			next = append(next, asideRecording{from: from, mock: mk})
		}
	}
	m.exact.setAside.Store(&next)
}

// setAsideRecordings is what ServeExactAside added so far; read-only.
func (m *MockManager) setAsideRecordings() []asideRecording {
	if p := m.exact.setAside.Load(); p != nil {
		return *p
	}
	return nil
}

// addAside files the set-aside recordings into v after every recording it
// already holds (exactView.aside), each copy marked with its test
// (models.TestModeInfo.SetAsideFrom). One v already holds (the open test's
// own, or a shared one) stays as it is, and so does none that places or
// binds variables.
func (v *exactView) addAside(recs []asideRecording) {
	for _, r := range recs {
		if _, dup := v.index[r.mock.Name]; dup {
			continue
		}
		if len(variablesOf(r.mock.Spec)) > 0 || len(bindsOf(r.mock.Spec)) > 0 {
			continue
		}
		n := len(v.owned)
		v.add([]*models.Mock{r.mock}, false)
		if len(v.owned) == n {
			continue
		}
		if v.aside == nil {
			v.aside = make([]string, n, n+1)
		}
		for len(v.aside) < n {
			v.aside = append(v.aside, "")
		}
		v.aside = append(v.aside, r.from)
		v.owned[n].TestModeInfo.SetAsideFrom = r.from
	}
}

// SetAsideMocks implements integrations.SetAsideReader: the open test's
// set-aside recordings of kind (every kind for "") that have not answered,
// and that no other test answered a call with in the staging, in the order
// ServeExactAside gave them; none while no test is open. The manager's
// copies; read-only.
func (m *MockManager) SetAsideMocks(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v == nil || v.test == "" || len(v.aside) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*models.Mock
	for i, mk := range v.owned {
		if v.asideOf(i) != "" && !v.consumed[i] && (kind == "" || mk.Kind == kind) && !v.asideTaken(i) {
			out = append(out, mk)
		}
	}
	return out
}

var _ integrations.SetAsideReader = (*MockManager)(nil)

// ExactTestServedAside is, under exact serving, the latest test opened (as
// ExactTestOutcome names it) and the calls of its window a set-aside test's
// recording answered, in the order they did, each as
// models.ServedAsideLine names it. Read once the test stops being served, it
// is final. ok is false when exact serving is off.
func (m *MockManager) ExactTestServedAside() (test string, served []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, false
	}
	v := m.exact.last.Load()
	if v == nil {
		return "", nil, true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, i := range v.order {
		if from := v.asideOf(i); from != "" {
			served = append(served, models.ServedAsideLine(v.owned[i].Name, from))
		}
	}
	return v.test, served, true
}
