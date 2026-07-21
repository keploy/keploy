package async

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Mocks exactly as the recorder emits them for a config-watch lane: epoch-0 at
// boot (NOT_MODIFIED) and a change received after test-2 (AnchorPos=2).
func TestBootAnsweredThenChangeAtAnchor(t *testing.T) {
	lane := models.AsyncLane{Name: "cfg", Type: "fake", ThrottleMs: 10}
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")}, "cfg")
	e.Load([]*models.Mock{
		asyncMock("cfg", 1, 0, "V0"), // initial, boot
		asyncMock("cfg", 2, 2, "V1"), // change received after test-2
	})

	// Boot: no test has run (completed=0). The poll MUST be answered (this is the
	// deadlock that used to hang the app), with the initial value.
	ctx := context.Background()
	if rec, _, _ := e.Decide(ctx, lane, &models.Mock{}); rec == nil || rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("boot poll must be answered with V0, got %v", rec)
	}

	// test-1 and test-2 windows still serve V0 (change not received yet).
	e.AdvanceWindow() // windowSeen, completed=0 (test-1 running)
	e.AdvanceWindow() // completed=1 (test-2 running)
	if rec, _, _ := e.Decide(ctx, lane, &models.Mock{}); rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("test-2 must still see V0, got %q", rec.Spec.HTTPResp.Body)
	}

	// After test-2 completes, V1 is effective.
	e.AdvanceWindow() // completed=2 (test-3 running)
	if rec, _, _ := e.Decide(ctx, lane, &models.Mock{}); rec.Spec.HTTPResp.Body != "V1" {
		t.Fatalf("test-3 must see V1, got %q", rec.Spec.HTTPResp.Body)
	}
}

// A lane with only NOT_MODIFIED (single epoch-0) — the no-change case — always
// answers immediately and never blocks, at any completed.
func TestStableConfigNeverBlocks(t *testing.T) {
	lane := models.AsyncLane{Name: "cfg", Type: "fake", ThrottleMs: 10}
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")}, "cfg")
	e.Load([]*models.Mock{asyncMock("cfg", 1, 0, "NOT_MODIFIED")})
	for i := 0; i < 5; i++ {
		start := time.Now()
		rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
		if rec == nil || rec.Spec.HTTPResp.Body != "NOT_MODIFIED" {
			t.Fatalf("poll %d: want NOT_MODIFIED, got %v", i, rec)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("poll %d took %v; must be throttle-bounded, never an open-ended park", i, time.Since(start))
		}
	}
}
