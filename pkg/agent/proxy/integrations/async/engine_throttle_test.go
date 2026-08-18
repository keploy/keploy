package async

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestDecideHeldUpToThrottle(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})
	// Type must end in "Poll" (IsPoll) — the throttle hold is gated to poll
	// lanes only; BaseType() still strips the suffix to "fake", resolving to
	// the parser newTestEngine registered under that key.
	lane := models.AsyncLane{Name: "L", Type: "fakePoll", ThrottleMs: 80}
	start := time.Now()
	rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
	elapsed := time.Since(start)
	if rec == nil || rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("want V0, got %v", rec)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("unchanged poll should be paced by throttle (~80ms), returned in %v", elapsed)
	}
}

// TestDecideNonPollLaneSkipsThrottle proves the throttle hold (Fix: gate to
// poll lanes only) does not apply to a non-poll async lane: Type "fake" does
// not end in "Poll" (IsPoll false), so Decide must serve its current epoch
// immediately even with a large ThrottleMs — the hold is a poll-only pacing
// knob, not a blanket delay on every async Decide.
func TestDecideNonPollLaneSkipsThrottle(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 5000} // non-poll: large throttle must be ignored
	start := time.Now()
	rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
	elapsed := time.Since(start)
	if rec == nil || rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("want V0, got %v", rec)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("non-poll lane must not be held by throttle, took %v (ThrottleMs=5000)", elapsed)
	}
}

func TestDecideWakesEarlyOnAdvance(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0"), asyncMock("L", 2, 1, "V1")})
	lane := models.AsyncLane{Name: "L", Type: "fakePoll", ThrottleMs: 5000} // poll lane; long, must be cut short
	e.AdvanceWindow()                                                       // windowSeen, completed=0
	got := make(chan string, 1)
	start := time.Now()
	go func() {
		rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
		got <- rec.Spec.HTTPResp.Body
	}()
	time.Sleep(20 * time.Millisecond)
	e.AdvanceWindow() // completed=1 -> V1 effective, must wake the poll early
	select {
	case body := <-got:
		if body != "V1" {
			t.Fatalf("want V1 after advance, got %q", body)
		}
		if time.Since(start) > 1*time.Second {
			t.Fatalf("did not wake early on advance (took %v of 5s throttle)", time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Decide did not wake early on AdvanceWindow")
	}
}

func TestDecideReturnsOnCtxCancel(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})
	lane := models.AsyncLane{Name: "L", Type: "fakePoll", ThrottleMs: 5000} // poll lane, so it actually holds
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, _, _ = e.Decide(ctx, lane, &models.Mock{})
	if time.Since(start) > 1*time.Second {
		t.Fatalf("ctx cancel should end the hold promptly, took %v", time.Since(start))
	}
}
