package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A call's mark says why its set was not served by proven routing
// (integrations.CallRound.Unrouted), as the view its first read found it,
// through the store's mark, a scoped view's and a round snapshot alike:
// the reason the replay staged the set with (StageExactUnrouted: the record
// lacks the mark, or this replay does not enable it), "" when the replay
// said none or the set is routed, and models.UnroutedUnstaged only when the
// agent said it (keploy mock replay's session, again after a session reset).
// A view nobody said a reason for reads "", keploy test's set boundary
// included: a session reset forgets the reason and never reads as keploy mock
// replay's. A staging view reads its set's reason too, and a call marked in
// one set keeps that set's reason once the next set is staged.
func TestACallsMarkSaysWhyItsSetIsNotRouted(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stage := func(rules, why string, says bool) func(mm *MockManager) {
		return func(mm *MockManager) {
			mm.StageExactBackground(rules)
			if says {
				mm.StageExactUnrouted(why)
			}
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.ServeExactBackground(rules, nil, nil)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
	}
	// keploy mock replay's session, as its agent serves it
	// (servePoolExactly): told, before its first view.
	mockReplay := func(mm *MockManager) {
		mm.StageExactUnrouted(models.UnroutedUnstaged)
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}
	marks := map[string]func(mm *MockManager) integrations.CallRound{
		"the store's mark": func(mm *MockManager) integrations.CallRound { return mm.MarkCallRound() },
		"a scoped view's mark": func(mm *MockManager) integrations.CallRound {
			return integrations.MarkCallRound(&scopedMockDb{MockMemDb: mm})
		},
		"a round snapshot": func(mm *MockManager) integrations.CallRound {
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{})
			if !ok {
				t.Fatal("no snapshot")
			}
			return snap.CallRound()
		},
	}
	for _, tc := range []struct {
		name          string
		before, under func(mm *MockManager)
		want          string
	}{
		{"keploy mock replay: its agent says it", mockReplay, nil, models.UnroutedUnstaged},
		{"a view nobody said a reason for", func(mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, nil) }, nil, ""},
		{"a set without the mark", stage("", models.UnroutedUnmarked, true), nil, models.UnroutedUnmarked},
		{"a marked set this replay does not route", stage("", models.UnroutedNotEnabled, true), nil, models.UnroutedNotEnabled},
		{"a replay that says no reason", stage("", "", false), nil, ""},
		{"a routed set", stage(models.RecordedWithS12Routing, models.UnroutedUnmarked, true), nil, ""},
		{"a set without the mark, read once a marked one is staged", stage("", models.UnroutedUnmarked, true), stage("", models.UnroutedNotEnabled, true), models.UnroutedUnmarked},
		{"a set without the mark's staging view", func(mm *MockManager) {
			mm.StageExactBackground("")
			mm.StageExactUnrouted(models.UnroutedUnmarked)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}, nil, models.UnroutedUnmarked},
		{"keploy test's set boundary: a session reset before the next set's staging", func(mm *MockManager) {
			stage("", models.UnroutedUnmarked, true)(mm)
			mm.ResetForReplaySession()
		}, nil, ""},
		{"keploy mock replay's next session, after a session reset", func(mm *MockManager) {
			mockReplay(mm)
			mm.ResetForReplaySession()
			mockReplay(mm)
		}, nil, models.UnroutedUnstaged},
		// keploy test's next set: its staging said its reason, and its first
		// view is not installed yet. The view a call reads there is the one
		// the reset installed, which nobody said a reason for: never the
		// reason the next set's views will read.
		{"keploy test's next set, said but before its first view", func(mm *MockManager) {
			stage("", models.UnroutedUnmarked, true)(mm)
			mm.ResetForReplaySession()
			mm.StageExactBackground("")
			mm.StageExactUnrouted(models.UnroutedNotEnabled)
		}, nil, ""},
	} {
		for how, mark := range marks {
			t.Run(how+": "+tc.name, func(t *testing.T) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				tc.before(mm)
				call := mark(mm)
				if !call.Read() {
					t.Fatal("precondition: the call was not marked")
				}
				if tc.under != nil {
					tc.under(mm)
				}
				if got := call.Unrouted(); got != tc.want {
					t.Fatalf("the call's set reads as not routed because %q; want %q", got, tc.want)
				}
			})
		}
	}
	if got := (integrations.CallRound{}).Unrouted(); got != "" {
		t.Fatalf("the zero CallRound says %q; want none", got)
	}
}

// A call's mark names, of keploy mock replay's mappings, the tests whose lists
// hold each recording (integrations.CallRound.ListedFor), as the view its
// first read found was installed with: the listing the agent handed
// (SetExactListing) before the view, through the store's mark, a scoped
// view's and a round snapshot alike. A view installed before it reads none,
// a session reset forgets it until the next session's table, and an empty
// table is a listing that holds no recording, never none.
func TestACallsMarkNamesTheTestsItsRecordingIsListedFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	listing := map[string][]string{"mock-2": {"o::TestA"}, "mock-5": {"o::TestA", "o::TestC"}}
	next := map[string][]string{"mock-2": {"o::TestB"}}
	mockReplay := func(listed map[string][]string) func(mm *MockManager) {
		return func(mm *MockManager) {
			mm.SetExactListing(listed)
			mm.StageExactUnrouted(models.UnroutedUnstaged)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
	}
	marks := map[string]func(mm *MockManager) integrations.CallRound{
		"the store's mark": func(mm *MockManager) integrations.CallRound { return mm.MarkCallRound() },
		"a scoped view's mark": func(mm *MockManager) integrations.CallRound {
			return integrations.MarkCallRound(&scopedMockDb{MockMemDb: mm})
		},
		"a round snapshot": func(mm *MockManager) integrations.CallRound {
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{})
			if !ok {
				t.Fatal("no snapshot")
			}
			return snap.CallRound()
		},
	}
	for _, tc := range []struct {
		name   string
		before func(mm *MockManager)
		want   map[string][]string
	}{
		{"keploy mock replay's session", mockReplay(listing), listing},
		{"an empty table", mockReplay(nil), map[string][]string{}},
		{"a view installed before the listing", func(mm *MockManager) {
			mm.StageExactUnrouted(models.UnroutedUnstaged)
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.SetExactListing(listing)
		}, nil},
		{"after a session reset, before the next table", func(mm *MockManager) {
			mockReplay(listing)(mm)
			mm.ResetForReplaySession()
			mm.StageExactUnrouted(models.UnroutedUnstaged)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}, nil},
		{"the next session's own", func(mm *MockManager) {
			mockReplay(listing)(mm)
			mm.ResetForReplaySession()
			mockReplay(next)(mm)
		}, next},
	} {
		for how, mark := range marks {
			t.Run(how+": "+tc.name, func(t *testing.T) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				tc.before(mm)
				call := mark(mm)
				if !call.Read() {
					t.Fatal("precondition: the call was not marked")
				}
				for _, name := range []string{"mock-2", "mock-5", "mock-9"} {
					got, known := call.ListedFor(name)
					if known != (tc.want != nil) || !slices.Equal(got, tc.want[name]) {
						t.Fatalf("%s reads listed for %v (known %v); want %v (known %v)", name, got, known, tc.want[name], tc.want != nil)
					}
				}
			})
		}
	}
	if _, known := (integrations.CallRound{}).ListedFor("mock-2"); known {
		t.Fatal("the zero CallRound knows a listing")
	}
}

// keploy test's set boundary: the next set's MockOutgoing resets the session
// (ResetForReplaySession) before its staging, while the previous set's
// recordings are still staged. A call an application kept alive across sets
// (compose, --keep-app-alive) makes in that gap is refused, and the not-owned
// finder still names a previous set's recording for it. Its mark never reads
// as keploy mock replay's (models.UnroutedUnstaged, whose miss tells the
// mapping edit): nobody said a reason, so it reads "", and the miss names
// both sides.
func TestKeployTestsSetBoundaryNeverReadsAsKeployMockReplay(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := httpCall("own", 0, "/orders")
	other := httpCall("other", time.Second, "/delete")
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	// Set 1's staging, as the agent does it for keploy test.
	mm.StageExactBackground("")
	mm.StageExactUnrouted(models.UnroutedUnmarked)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.SetMocksWithWindow(nil, []*models.Mock{own, other}, models.BaseTime, time.Now())
	mm.ServeExactBackground("", nil, nil)
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := mm.MarkCallRound().Unrouted(); got != models.UnroutedUnmarked {
		t.Fatalf("precondition: a call in set 1 reads %q; want %q", got, models.UnroutedUnmarked)
	}
	mm.ResetForReplaySession()
	call := mm.MarkCallRound()
	name, _ := mm.FindStagedNotOwned(models.HTTP, call, func(m *models.Mock) bool { return m.Name == "other" })
	if !call.Read() || name != "other" {
		t.Fatalf("precondition: the gap's call (read %v) names %q; want the previous set's recording other", call.Read(), name)
	}
	if got := call.Unrouted(); got != "" {
		t.Fatalf("keploy test's set boundary reads as not routed because %q; want none said", got)
	}
}

// Through the learn tracker (record's full round) a call's mark and the
// round's snapshot read why its set is not routed too. With the tracker ahead
// of the store's view in the staged set (TrackerAhead: of its pool view, or
// of test-1's view as test-2 begins), the call is the staged set's, and reads
// the reason (and the listing) its view was installed with. Once the tracker began a test set
// the store has not staged (SetUnstaged), the call is that next set's, whose
// staging has said no reason yet: it reads none, never the set before's.
func TestALearnRoundsCallReadsWhyItsOwnSetIsNotRouted(t *testing.T) {
	test1 := func(p *Proxy, mm *MockManager) {
		mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
		p.ContinueTestErrorCapture()
		p.EndTestErrorCapture()
	}
	for _, tc := range []struct {
		name  string
		move  func(p *Proxy, mm *MockManager)
		ahead bool
		want  string
	}{
		{"the tracker ahead of the pool view", func(p *Proxy, _ *MockManager) { p.ContinueTestErrorCapture() }, true, models.UnroutedUnmarked},
		{"the tracker ahead of test-1's view", func(p *Proxy, mm *MockManager) {
			test1(p, mm)
			p.ContinueTestErrorCapture()
		}, true, models.UnroutedUnmarked},
		{"the tracker in a test set the store has not staged", func(p *Proxy, mm *MockManager) {
			test1(p, mm)
			p.learnBoundary(func(t *learn.Tracker) { t.NewSet() })
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mm := heldOrdersWith(t, false, zap.NewNop(), nil)
			// The set's staging says it carries no mark, before its views,
			// and the listing its views read.
			mm.SetExactListing(map[string][]string{"mock-own1": {"test-1"}})
			mm.StageExactUnrouted(models.UnroutedUnmarked)
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			tc.move(p, mm)
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
			if !ok || snap.TrackerAhead() != tc.ahead || snap.SetUnstaged() == tc.ahead {
				t.Fatalf("precondition: the snapshot reads %q, tracker ahead %v, set unstaged %v", snap.Boundary, snap.TrackerAhead(), snap.SetUnstaged())
			}
			if got := snap.CallRound().Unrouted(); got != tc.want {
				t.Fatalf("the snapshot's call reads as not routed because %q; want %q", got, tc.want)
			}
			if got := mm.MarkCallRound().Unrouted(); got != tc.want {
				t.Fatalf("the store's mark reads as not routed because %q; want %q", got, tc.want)
			}
			// The listing goes with the reason: the staged set's, and none
			// for the next set's call.
			for how, call := range map[string]integrations.CallRound{"the snapshot's call": snap.CallRound(), "the store's mark": mm.MarkCallRound()} {
				if got, known := call.ListedFor("mock-own1"); known != tc.ahead || (tc.ahead && !slices.Equal(got, []string{"test-1"})) {
					t.Fatalf("%s reads mock-own1 listed for %v (known %v); want test-1 only while the call is the staged set's", how, got, known)
				}
			}
		})
	}
}
