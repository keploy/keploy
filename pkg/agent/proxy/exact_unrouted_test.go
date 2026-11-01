package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A call's mark says why its set was not served by proven routing
// (integrations.CallRound.Unrouted), as the view its first read found it,
// through the store's mark, a scoped view's and a round snapshot alike:
// the reason the replay staged the set with (StageExactUnrouted: the record
// lacks the mark, or this replay does not enable it), "" when the replay
// said none or the set is routed, and models.UnroutedUnstaged when exact
// serving runs with no set staged at all (keploy mock replay's, also after a
// session reset). A staging view reads its set's reason too, and a call
// marked in one set keeps that set's reason once the next set is staged.
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
	mockReplay := func(mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, nil) }
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
		{"keploy mock replay: no set staged", mockReplay, nil, models.UnroutedUnstaged},
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
		{"keploy mock replay after a session reset", func(mm *MockManager) {
			stage("", models.UnroutedUnmarked, true)(mm)
			mm.ResetForReplaySession()
			mockReplay(mm)
		}, nil, models.UnroutedUnstaged},
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
