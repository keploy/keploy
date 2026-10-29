package replay

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// noAnswerHold is what the mock prune kept for one test whose request got no
// answer (pkg.IsAppNoAnswer): the test, and how many of the set's mocks it
// holds.
type noAnswerHold struct {
	test string
	kept int
}

// holdNoAnswerMocks adds to keep, the prune's keep-set, the mocks each test in
// unanswered would be served on its next replay, so the prune can run around
// the tests that got no answer instead of keeping the whole set for them:
//
//   - its mapping entry's names (mapped), when it has one;
//   - every mock of the set's mock file that replay places in its recorded
//     window (testWindowOf): one whose place (models.PlaceOf) is in it, or that
//     has no recorded time, which the window filter hands every test. That is
//     the per-test pool replay reads for a test with no mapping entry
//     (GetFilteredMocks), and the session-pool mocks recorded in its window
//     too: replay serves those to every test, and a recording whose recorder
//     stated no lifetime has its HTTP, Postgres and MySQL calls there;
//   - what it consumed before the error: its own entry in consumed, the
//     mapping this run builds, and the mocks consumers names it for.
//
// A test that got no answer consumed nothing for want of an answer, not for
// want of need: these are the mocks the prune would otherwise delete although
// it still needs them (#4662). The set's mock file is read once, whatever the
// number of tests. When that read fails, or a test has no recorded window to
// hold, keep is left as it was and the error says why: the caller then holds
// the whole set, as before.
func (r *Replayer) holdNoAnswerMocks(ctx context.Context, testSetID string, unanswered []string, testCases []*models.TestCase, mapped map[string][]models.MockEntry, consumed *models.Mapping, consumers map[string][]string, keep map[string]models.MockState) ([]noAnswerHold, error) {
	byName := make(map[string]*models.TestCase, len(testCases))
	for _, tc := range testCases {
		if tc != nil {
			byName[tc.Name] = tc
		}
	}
	type window struct {
		test       string
		start, end time.Time
	}
	windows := make([]window, 0, len(unanswered))
	seen := make(map[string]struct{}, len(unanswered))
	for _, name := range unanswered {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		start, end := testWindowOf(byName[name])
		if start.IsZero() || end.IsZero() {
			return nil, fmt.Errorf("test %q has no recorded window to hold its mocks by", name)
		}
		windows = append(windows, window{test: name, start: start, end: end})
	}
	if len(windows) == 0 {
		return nil, nil
	}

	mocks, err := r.readSetMocks(ctx, testSetID)
	if err != nil {
		return nil, fmt.Errorf("read the set's mocks to hold the unanswered tests' own: %w", err)
	}

	consumedBy := map[string][]string{}
	for mock, tests := range consumers {
		for _, test := range tests {
			consumedBy[test] = append(consumedBy[test], mock)
		}
	}
	var consumedEntries map[string][]models.MockEntry
	if consumed != nil {
		consumedEntries = make(map[string][]models.MockEntry, len(consumed.TestCases))
		for _, tc := range consumed.TestCases {
			consumedEntries[tc.ID] = append(consumedEntries[tc.ID], tc.Mocks...)
		}
	}

	held := make([]noAnswerHold, 0, len(windows))
	add := map[string]models.MockState{}
	for _, w := range windows {
		names := map[string]struct{}{}
		hold := func(name string, kind models.Kind) {
			if name == "" {
				return
			}
			names[name] = struct{}{}
			if _, kept := keep[name]; kept {
				return
			}
			if _, added := add[name]; !added {
				add[name] = models.MockState{Name: name, Kind: kind}
			}
		}
		for _, e := range mapped[w.test] {
			hold(e.Name, models.Kind(e.Kind))
		}
		for _, e := range consumedEntries[w.test] {
			hold(e.Name, models.Kind(e.Kind))
		}
		for _, name := range consumedBy[w.test] {
			hold(name, "")
		}
		for _, m := range mocks {
			if placedInWindow(m, w.start, w.end) {
				hold(m.Name, m.Kind)
			}
		}
		held = append(held, noAnswerHold{test: w.test, kept: len(names)})
	}
	for name, st := range add {
		keep[name] = st
	}
	return held, nil
}

// placedInWindow reports whether replay's window filter places m in the window
// [start, end]: its place (models.PlaceOf) is in it, or it has no recorded
// time, which the filter hands every window (pkg.FilterTcsMocks).
func placedInWindow(m *models.Mock, start, end time.Time) bool {
	if m == nil {
		return false
	}
	if m.Spec.ReqTimestampMock.IsZero() || m.Spec.ResTimestampMock.IsZero() {
		return true
	}
	place := models.PlaceOf(m)
	return !place.Before(start) && !place.After(end)
}

// readSetMocks reads every mock of the set's mock file, both pools, before the
// mapping prune and the window filter: in one read when the store can (the
// index read leaves each payload in the file; only names and times are read
// here), else in one read per pool, the per-test one with no window, so that
// it filters nothing out.
func (r *Replayer) readSetMocks(ctx context.Context, testSetID string) ([]*models.Mock, error) {
	if r.mockDB == nil {
		return nil, fmt.Errorf("no mock store")
	}
	if reader, ok := r.mockDB.(pkg.TestSetMockIndexReader); ok {
		set, err := reader.IndexTestSetMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			return nil, err
		}
		return append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...), nil
	}
	if reader, ok := r.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := reader.GetTestSetMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			return nil, err
		}
		return append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...), nil
	}
	perTest, err := r.mockDB.GetFilteredMocks(ctx, testSetID, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		return nil, err
	}
	session, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		return nil, err
	}
	return append(append([]*models.Mock(nil), perTest...), session...), nil
}
