package replay

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch a replay never creates mappings.yaml from a run in
// which a test left a test mock of its recorded window unconsumed (design v14
// row S3). Without a mappings.yaml each test is served the mocks of its
// recorded window, and the file a run creates maps each passing test to what
// it consumed: a mock it did not consume (its call answered by another test's
// mock, or not made) would be out of its mapping for good, and no later replay
// could see the test leave it unconsumed. keploy record writes the mappings of
// a recording made under the switch; this keeps a replay from writing worse
// ones for a recording made without it.
//
// A test's recorded window gives it the test mocks models.DeriveWindowMappings
// gives it, as record would: data-plane mocks no async lane serves, less the
// kinds the per-test assertion leaves out (DNS, connection failures).

// windowUse is what a run's tests own of their recorded windows, and which of
// them ran. Nil with the switch off: it then reads nothing and holds nothing
// back.
type windowUse struct {
	// known says the set's mocks were read: owned is their derivation.
	known bool
	// owned is each test's test mocks by its recorded window.
	owned map[string][]string
	// seen names the tests whose consumption the run read.
	seen map[string]bool
}

func newWindowUse() *windowUse {
	if !provenswitch.On() {
		return nil
	}
	return &windowUse{seen: map[string]bool{}}
}

// load derives each test's test mocks from the set's recorded windows (every
// test case of the set, selected or not) and its mocks (both pools).
func (w *windowUse) load(testCases []*models.TestCase, pools ...[]*models.Mock) {
	if w == nil {
		return
	}
	windows := make([]models.TestWindow, 0, len(testCases))
	for _, tc := range testCases {
		windows = append(windows, models.RecordedWindow(tc))
	}
	var mocks []models.WindowedMock
	for _, pool := range pools {
		for _, m := range pool {
			if m == nil || models.ExcludedFromDependencyAssertion(m.Kind) {
				continue
			}
			mocks = append(mocks, models.NewWindowedMock(m))
		}
	}
	derived := models.DeriveWindowMappings(windows, mocks)
	w.owned = make(map[string][]string, len(derived.Tests))
	for _, tc := range derived.Tests {
		for _, e := range tc.Mocks {
			w.owned[tc.ID] = append(w.owned[tc.ID], e.Name)
		}
	}
	w.known = true
}

// ran marks a test whose consumption the run read.
func (w *windowUse) ran(test string) {
	if w == nil {
		return
	}
	w.seen[test] = true
}

// holdCreate says why the run must not create mappings.yaml, or "" when every
// test it ran consumed every test mock of its recorded window. A test's
// consumption is what the run attributes to it (actual:
// carryOverAttribution.mapConsumed), so a carried-over mock its window
// recorded counts as its own wherever it was consumed. unread names the tests
// whose consumption could not be read. left names, in testCases order, the
// tests that ran and left a test mock of their window unconsumed; unseen those
// that ran, own one, and whose consumption could not be read.
//
// A test the run did not run (ignored, or not selected) gets no entry in a
// file the run creates, as a test that did not pass gets none (#4773): what it
// recorded is not cut down, so it holds nothing. A run that did not score
// every test it loaded is held before this (allTestsGotAVerdict).
func (w *windowUse) holdCreate(testCases []*models.TestCase, unread []string, actual *models.Mapping) (reason string, left, unseen []string) {
	if w == nil {
		return "", nil, nil
	}
	if !w.known {
		return "this run did not load the set's mocks, so it cannot show that every test consumed the mocks recorded in its window (KEPLOY_PROVEN=1)", nil, nil
	}
	isUnread := make(map[string]bool, len(unread))
	for _, id := range unread {
		isUnread[id] = true
	}
	consumed := map[string]map[string]bool{}
	if actual != nil {
		for _, tc := range actual.TestCases {
			if consumed[tc.ID] == nil {
				consumed[tc.ID] = map[string]bool{}
			}
			for _, e := range tc.Mocks {
				consumed[tc.ID][e.Name] = true
			}
		}
	}
	for _, tc := range testCases {
		if tc == nil || len(w.owned[tc.Name]) == 0 {
			continue
		}
		if !w.seen[tc.Name] {
			continue
		}
		if isUnread[tc.Name] {
			unseen = appendOnce(unseen, tc.Name)
			continue
		}
		for _, name := range w.owned[tc.Name] {
			if !consumed[tc.Name][name] {
				left = appendOnce(left, tc.Name)
				break
			}
		}
	}
	if len(left) == 0 && len(unseen) == 0 {
		return "", nil, nil
	}
	return "a test left a mock recorded in its window unconsumed, or what it consumed could not be read, so a mappings.yaml made from this run could leave a call out of the test's mapping for good (KEPLOY_PROVEN=1); keploy record writes the mappings of a recording made under the switch", left, unseen
}
