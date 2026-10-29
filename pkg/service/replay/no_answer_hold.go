package replay

import (
	"context"
	"errors"
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
// unanswered is served on its next replay, so the prune can run around the
// tests that got no answer instead of keeping the whole set for them:
//
//   - its mapping entry's names (mapped), when it has one;
//   - every mock of the set's mock file, either pool, that the agent serves
//     in its recorded window (servedInWindow): placed in it, in flight when it
//     began, a carry-over mock it releases, or one with no recorded time. The
//     session-pool mocks recorded in it are held too: replay serves those to
//     every test, and a recording whose recorder stated no lifetime has its
//     HTTP, Postgres and MySQL calls there;
//   - what it consumed before the error: the mocks consumers names it for (a
//     streaming test's pre-stream drain records them before its body read
//     can fail). This run's mapping has no entry of its own for it: every
//     no-answer path files its row before the mapping step.
//
// Not held: a per-test mock recorded outside every window and stating no
// lifetime, which lax mode (KEPLOY_STRICT_MOCK_WINDOW=0) promotes to the
// session pool and so serves to every test. No test's window places it, so
// the prune keeps it only when a test consumed it, as for a test that failed
// on its answer.
//
// A test that got no answer consumed nothing for want of an answer, not for
// want of need: these are the mocks the prune would otherwise delete although
// it still needs them (#4662). The set's mock file is read once, whatever the
// number of tests, one mock at a time where the store can (eachSetMock): only
// each held mock's name and kind are kept. When that read fails, or a test has
// no recorded window to hold, keep is left as it was and the error says why:
// the caller then holds the whole set, as before.
//
// carry is the run's carryOverAttribution, over testCases' recorded windows:
// its schedule places the calls in flight and the carry-over mocks. agent is
// how the agent said it served the set's windows (agentMockWindow): its
// strict mock window mode and whether its load ran the carry-over lookahead
// (serveWindowOf).
func (r *Replayer) holdNoAnswerMocks(ctx context.Context, testSetID string, unanswered []string, testCases []*models.TestCase, carry *carryOverAttribution, agent models.MockWindowsSaid, mapped map[string][]models.MockEntry, consumers map[string][]string, keep map[string]models.MockState) ([]noAnswerHold, error) {
	if carry == nil {
		return nil, fmt.Errorf("no recorded windows to hold the unanswered tests' mocks by")
	}
	byName := make(map[string]*models.TestCase, len(testCases))
	for _, tc := range testCases {
		if tc != nil {
			byName[tc.Name] = tc
		}
	}
	windows := make([]models.TestWindow, 0, len(unanswered))
	seen := make(map[string]struct{}, len(unanswered))
	for _, name := range unanswered {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		// A window with no end is one the agent hands every per-test mock
		// to, which no hold can name.
		start, end := testWindowOf(byName[name])
		if start.IsZero() || end.IsZero() {
			return nil, fmt.Errorf("test %q has no recorded window to hold its mocks by", name)
		}
		windows = append(windows, models.TestWindow{TestCase: name, Start: start, End: end})
	}
	if len(windows) == 0 {
		return nil, nil
	}

	// names[i] is what windows[i]'s test holds; add, what the holds add to keep.
	names := make([]map[string]struct{}, len(windows))
	for i := range names {
		names[i] = map[string]struct{}{}
	}
	add := map[string]models.MockState{}
	hold := func(i int, name string, kind models.Kind) {
		if name == "" {
			return
		}
		names[i][name] = struct{}{}
		if _, kept := keep[name]; kept {
			return
		}
		if _, added := add[name]; !added {
			add[name] = models.MockState{Name: name, Kind: kind}
		}
	}

	serve := make([]pkg.ServeWindow, len(windows))
	for i, w := range windows {
		serve[i] = serveWindowOf(carry.sched, w, agent)
	}
	err := r.eachSetMock(ctx, testSetID, true, func(m *models.Mock, session bool) {
		for i := range serve {
			if heldFor(m, session, serve[i]) {
				hold(i, m.Name, m.Kind)
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("read the set's mocks to hold the unanswered tests' own: %w", err)
	}

	consumedBy := map[string][]string{}
	for mock, tests := range consumers {
		for _, test := range tests {
			consumedBy[test] = append(consumedBy[test], mock)
		}
	}

	held := make([]noAnswerHold, 0, len(windows))
	for i, w := range windows {
		for _, e := range mapped[w.TestCase] {
			hold(i, e.Name, models.Kind(e.Kind))
		}
		for _, name := range consumedBy[w.TestCase] {
			hold(i, name, "")
		}
		held = append(held, noAnswerHold{test: w.TestCase, kept: len(names[i])})
	}
	for name, st := range add {
		keep[name] = st
	}
	return held, nil
}

// heldFor reports whether the hold keeps m, a mock of the set's per-test pool
// (session false) or its session pool, for the unanswered test whose window w
// is. It asks the agent's own decision (pkg.ServeInWindow), and keeps:
//
//   - what the window's per-test pool gets: placed in it, in flight when it
//     began, with no recorded time, a window-bound mock recorded in it;
//   - what it releases first: a carry-over mock the carry-over tier reaches
//     from it, a server push the push hub delivers from it;
//   - of what the session pool hands every window, a mock placed in the
//     window or with no recorded time. One placed outside it is held only
//     when some test consumed it, as for a test that failed on its answer:
//     holding every such mock would be the whole-set rule again. So is a
//     per-test mock lax mode promotes to the session pool, which is outside
//     the window, and the startup tier.
func heldFor(m *models.Mock, session bool, w pkg.ServeWindow) bool {
	switch pkg.ServeInWindow(m, session, w) {
	case pkg.ServedPerTest, pkg.ServedReleased:
		return true
	case pkg.ServedSession:
		return pkg.RecordedInWindow(m, w)
	}
	return false
}

// serveWindowOf is the window the agent opens for the test whose recorded
// window w is, one of sched's, as the replayer asks it to
// (pkg.FilterPerTestAndLaxPromotedInFlightAware's arguments on a windowed
// proxy): the first recorded window starts the startup band, the gap before
// w's start says which calls were in flight, and the filter is not strict
// per call, the mock manager filtering what it is handed.
//
// The window mode is the agent's, as it said it (agent, agentMockWindow),
// with its own KEPLOY_STRICT_MOCK_WINDOW folded in (pkg.ServeWindow.Resolved),
// never this process's env, which the agent's need not share. Its load runs
// the carry-over lookahead (pkg.ServeWindow.Lookahead) when it said a load by
// the test's mapping or by the strict window, as long as its on-disk store
// holds the per-test mocks. Whether it does the replayer cannot tell (the
// agent keeps them resident when it cannot make the store), so the hold asks
// for the disk's: it holds the carry-over mocks the tier reaches first at w,
// where the resident filter would promote them to the session pool, which
// every window is served and the hold keeps only when recorded in w. Holding
// one more mock is safe; deleting one the agent serves is not.
func serveWindowOf(sched *models.WindowSchedule, w models.TestWindow, agent models.MockWindowsSaid) pkg.ServeWindow {
	sw := pkg.ServeWindow{Start: w.Start, End: w.End, StrictWindow: agent.Strict, Resolved: true, Recorded: sched, Lookahead: agent.Lookahead}
	if sched.Len() > 0 {
		sw.FirstStart = sched.Window(0).Start
	}
	if from, ok := sched.GapBefore(w.Start); ok {
		sw.InFlightFrom = from
	}
	return sw
}

// agentMockWindow is how the agent said it served the set's windows
// (pkg.MockWindowReader, models.MockWindowHeader): its strict mock window mode,
// its own KEPLOY_STRICT_MOCK_WINDOW folded in, and whether its per-test load
// ran the carry-over lookahead. Where it has not said, as an agent older than
// the header, or an instrumentation that cannot ask, the mode that holds the
// most: strict, with the lookahead. Never this process's env: sudo drops it on
// the way to a native agent, a docker agent's container is not given it, and a
// k8s-proxy agent runs in its own pod.
func (r *Replayer) agentMockWindow() models.MockWindowsSaid {
	if mr, ok := r.instrumentation.(pkg.MockWindowReader); ok {
		if said, ok := mr.AgentMockWindow(); ok {
			return said
		}
	}
	return models.MockWindowsSaid{Strict: true, Lookahead: true}
}

// eachSetMock hands visit every mock of the set's mock file before the mapping
// prune and the window filter, with its pool (session: the session pool, else
// the per-test one): TestSetMocks.AllPerTest, then AllSession. A mock in both,
// which the gob store's PostgresV2 mocks of session lifetime are, comes once
// for each.
//
// With stream, a store that can hands them over one at a time as it decodes
// them, in file order (pkg.TestSetMockWalker), so that a reader that keeps a
// few fields of each never holds the set's mocks; visit must not keep m. Else
// the set is read in one read when the store can, the index read
// (pkg.TestSetMockIndexReader, each payload left in the file) unless stream,
// whose reader places mocks by what their payloads say, then GetTestSetMocks;
// else in one read per pool, the per-test one with no window, so that it
// filters nothing out.
//
// The error says which read failed; a pool read before it was visited.
func (r *Replayer) eachSetMock(ctx context.Context, testSetID string, stream bool, visit func(m *models.Mock, session bool)) error {
	if r.mockDB == nil {
		return fmt.Errorf("no mock store")
	}
	visitPool := func(mocks []*models.Mock, session bool) {
		for _, m := range mocks {
			visit(m, session)
		}
	}
	if stream {
		if walker, ok := r.mockDB.(pkg.TestSetMockWalker); ok {
			return walker.WalkTestSetMocks(ctx, testSetID, func(m *models.Mock, perTest, session bool) {
				if perTest {
					visit(m, false)
				}
				if session {
					visit(m, true)
				}
			})
		}
	} else if reader, ok := r.mockDB.(pkg.TestSetMockIndexReader); ok {
		set, err := reader.IndexTestSetMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			return fmt.Errorf("read the per-test and session mocks: %w", err)
		}
		visitPool(set.AllPerTest, false)
		visitPool(set.AllSession, true)
		return nil
	}
	if reader, ok := r.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := reader.GetTestSetMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			return fmt.Errorf("read the per-test and session mocks: %w", err)
		}
		visitPool(set.AllPerTest, false)
		visitPool(set.AllSession, true)
		return nil
	}
	var errs []error
	if perTest, err := r.mockDB.GetFilteredMocks(ctx, testSetID, time.Time{}, time.Time{}, nil, nil); err != nil {
		errs = append(errs, fmt.Errorf("read the per-test mocks: %w", err))
	} else {
		visitPool(perTest, false)
	}
	if session, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil); err != nil {
		errs = append(errs, fmt.Errorf("read the session mocks: %w", err))
	} else {
		visitPool(session, true)
	}
	return errors.Join(errs...)
}
