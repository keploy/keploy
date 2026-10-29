package replay

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// SERVED, UNCHECKED: A SET-ASIDE TEST'S RECORDINGS, TO THE TEST CASES AFTER IT.
//
// A test case the replay sets aside (the proven switch skips it as
// quarantined, Replayer.skipReasonFor) makes none of its calls, but what its
// calls did can still be needed: a cache it filled at record is empty at
// replay, and the next test case that reads the cache makes the set-aside
// test case's call in its own window, where no recording of its own answers
// it. As keploy mock's agent does for a skipped begin, keploy test names the
// set-aside test cases recorded before a test case as it opens it
// (models.ExactServing.Aside), and the agent serves such a call the
// set-aside recording it is: by exact match only, unchecked, never required,
// at most once in the staging, from that test case on. The consume it reports
// says so (models.MockState.SetAsideFrom): it is no consume of the test case
// it lands in, and its result names it (FailureInfo.ServedAside). The same
// call made between two test cases gets no answer, as before.

// noteSetAside keeps, of the set's test cases in recorded order (testCases),
// the ones the replay sets aside (aside), each with its place, and every test
// case's place: a test case's opening names the set-aside ones recorded
// before it that no earlier opening named (asideBefore). A no-op for none.
func (p *exactPlan) noteSetAside(testCases []*models.TestCase, aside map[string]bool) {
	if p == nil || len(aside) == 0 {
		return
	}
	p.recIdx = make(map[string]int, len(testCases))
	p.aside = nil
	for i, tc := range testCases {
		if tc == nil {
			continue
		}
		if _, dup := p.recIdx[tc.Name]; !dup {
			p.recIdx[tc.Name] = i
		}
		if aside[tc.Name] {
			p.aside = append(p.aside, tc.Name)
		}
	}
}

// asideBefore is what test case name's opening names of the set-aside test
// cases (models.ExactServing.Aside): those recorded before it that no earlier
// opening named, in recorded order, each with the recordings of its mapping
// the agent holds; one whose mapping holds none is left out. nil for none.
func (p *exactPlan) asideBefore(name string) []models.AsideTest {
	if p == nil || len(p.aside) == 0 {
		return nil
	}
	at, ok := p.recIdx[name]
	if !ok {
		return nil
	}
	var out []models.AsideTest
	for _, s := range p.aside {
		if p.recIdx[s] >= at || p.asideSent[s] || len(p.owned[s]) == 0 {
			continue
		}
		out = append(out, models.AsideTest{Test: s, Owned: append([]string(nil), p.owned[s]...)})
	}
	return out
}

// sentAside notes that an opening named aside: the agent serves them from
// then on, for the staging, so no later opening names them again.
func (p *exactPlan) sentAside(aside []models.AsideTest) {
	if p == nil || len(aside) == 0 {
		return
	}
	if p.asideSent == nil {
		p.asideSent = make(map[string]bool, len(aside))
	}
	for _, s := range aside {
		p.asideSent[s.Test] = true
	}
}

// splitServedAside is consumed less the consumes a set-aside test case's
// recording answered (models.MockState.SetAsideFrom), and those named as
// models.ServedAsideLine names them, in the order they came: they are no
// consume of the test case whose window they landed in, and fail nothing.
// consumed itself when none did.
func splitServedAside(consumed []models.MockState) ([]models.MockState, []string) {
	n := 0
	for _, m := range consumed {
		if m.SetAsideFrom != "" {
			n++
		}
	}
	if n == 0 {
		return consumed, nil
	}
	own := make([]models.MockState, 0, len(consumed)-n)
	lines := make([]string, 0, n)
	for _, m := range consumed {
		if m.SetAsideFrom != "" {
			lines = append(lines, models.ServedAsideLine(m.Name, m.SetAsideFrom))
			continue
		}
		own = append(own, m)
	}
	return own, lines
}

// namedServedAside is what test case name's result names of the calls a
// set-aside test case's recording answered in its window (served, as
// splitServedAside named them), each said at Info: unchecked, failing
// nothing. nil with the proven switch off, which never serves one.
func (r *Replayer) namedServedAside(name string, served []string) []string {
	if !provenswitch.On() || len(served) == 0 {
		return nil
	}
	r.logger.Info("a call of this test case was answered by the recording of a test case the replay set aside: unchecked, it fails nothing",
		zap.String("testcase", name), zap.Strings("served", served),
		zap.String("next_step", "nothing to do: the set-aside test case made none of its calls, so this test case made one of them (a cache it filled at record); once that test case runs again, its own call is made in its own window"))
	return append([]string(nil), served...)
}
