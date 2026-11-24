package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func bgRuleIDs(rules []models.BackgroundMongoRule) []string {
	var ids []string
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestBackgroundMongoRulesFollowStagingAndRound(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	reads := func(what string, call integrations.CallRound, want ...string) {
		t.Helper()
		if got := bgRuleIDs(mm.BackgroundMongoRulesIn(call)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: manager reads %v; want %v", what, got, want)
		}
		if got := bgRuleIDs(integrations.BackgroundMongoRulesAt(view, call)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: scoped view reads %v; want %v", what, got, want)
		}
	}

	if got := mm.BackgroundMongoRules(); got != nil {
		t.Fatalf("before seed: got %v; want nil", got)
	}

	// Set A, with defensive clone verification on nested slice values.
	inA := []models.BackgroundMongoRule{{
		ID:          "ui-join-backfill-index-ready",
		DB:          "api-server",
		Command:     "listIndexes",
		Collections: []string{"uiTestSetJoins"},
		Where:       []models.BackgroundMongoWhere{{Field: "body.cmd.filter.status.$in", Equals: []any{nil, ""}}},
	}}
	mm.SeedBackgroundMongoRules(inA)
	inA[0].Collections[0] = "mutated"
	inA[0].Where[0].Equals.([]any)[1] = "mutated"

	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	a := mark()
	reads("set A call", a, "ui-join-backfill-index-ready")
	gotA := integrations.BackgroundMongoRulesOf(view)
	if len(gotA) != 1 || gotA[0].Collections[0] != "uiTestSetJoins" || gotA[0].Where[0].Equals.([]any)[1] != "" {
		t.Fatalf("caller mutation leaked into stored rules: %+v", gotA)
	}

	// Set B parked at boundary: A stays active until B is staged.
	mm.ResetForReplaySession()
	mm.SeedBackgroundMongoRules([]models.BackgroundMongoRule{{
		ID:          "e2e-pack-sweep-unread",
		DB:          "api-server",
		Command:     "find",
		Collections: []string{"e2eRecs"},
	}})
	reads("set A while B parked", a, "ui-join-backfill-index-ready")
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	b := mark()
	reads("set A call decided after B staged", a, "ui-join-backfill-index-ready")
	reads("set B call", b, "e2e-pack-sweep-unread")
	reads("zero CallRound", integrations.CallRound{}, "e2e-pack-sweep-unread")

	// Set C seeds none: B kept for B's call, A (two sets back) reads none.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	c := mark()
	reads("set C call with no rules", c)
	reads("set B call after C staged", b, "e2e-pack-sweep-unread")
	reads("set A call two sets back", a)
}

func bgHTTPRuleIDs(rules []models.BackgroundHTTPRule) []string {
	var ids []string
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestBackgroundHTTPRulesFollowScopedViewStagingAndAbortedBoundary(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	reads := func(what string, call integrations.CallRound, want ...string) {
		t.Helper()
		if got := bgHTTPRuleIDs(mm.BackgroundHTTPRulesIn(call)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: manager reads %v; want %v", what, got, want)
		}
		if got := bgHTTPRuleIDs(integrations.BackgroundHTTPRulesAt(view, call)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: scoped view reads %v; want %v", what, got, want)
		}
	}

	if got := integrations.BackgroundHTTPRulesOf(view); got != nil {
		t.Fatalf("before seed: got %v; want nil", got)
	}

	// Set A
	inA := []models.BackgroundHTTPRule{{
		ID:     "openbao-liveness-check",
		Method: "GET",
		Host:   "localhost:8200",
		Path:   "/v1/sys/health",
	}}
	mm.SeedBackgroundHTTPRules(inA)
	inA[0].Path = "/mutated"
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	a := mark()
	reads("set A call", a, "openbao-liveness-check")
	if got := integrations.BackgroundHTTPRulesOf(view); len(got) != 1 || got[0].Path != "/v1/sys/health" {
		t.Fatalf("caller mutation leaked into stored HTTP rules: %+v", got)
	}

	// Set B parked at boundary, then ABORTED by a second ResetForReplaySession
	// before SetMocksWithWindow: parked rules must NOT leak into Set C!
	mm.ResetForReplaySession()
	mm.SeedBackgroundHTTPRules([]models.BackgroundHTTPRule{{
		ID:     "aborted-set-b-rule",
		Method: "GET",
		Host:   "localhost:8200",
		Path:   "/v1/aborted",
	}})
	reads("set A while B parked", a, "openbao-liveness-check")
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	c := mark()
	reads("set C after aborted B boundary must have nil HTTP rules", c)
	reads("set A call decided after C staged", a, "openbao-liveness-check")
}
