package manager

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A DaemonSet's stream may refuse a test case once asked (its upload queue
// is full): one it refused is in no recording, so no manager notes it sent.
// Noted, it would stand as the recording's first test case, and a start-up
// read lost after it, before the first test case the stream did send, would
// be read as a test's: the later test cases would be kept without it.
func TestATestCaseTheStreamRefusedIsNotTheRecordingsFirst(t *testing.T) {
	own, node := New(zap.NewNop()), New(zap.NewNop())
	fullLane(t, node)
	queueAt(t, outQueueMaxBytes)
	t0 := time.Now().Add(-time.Minute)
	mgrs := []*SyncMockManager{own, node}
	asked := 0
	if SendTestCaseOf(mgrs, "refused", t0, t0.Add(time.Second), func() bool { asked++; return false }) {
		t.Fatal("a test case its stream refused was reported sent")
	}
	if asked != 1 {
		t.Fatalf("the stream was asked %d times to send a test case no manager left out, want 1", asked)
	}
	for _, m := range mgrs {
		if got := fmt.Sprint(sentNames(m)); got != "[]" {
			t.Fatalf("a manager noted %s sent after its stream refused it", got)
		}
		if f := m.firstSent.Load(); f != nil {
			t.Fatalf("a manager's first test case is at %v after the only one asked was refused", *f)
		}
	}
	// A start-up read lost after the refused test case's start, before any
	// test case was sent, is one any test may need.
	if got := sendNow(node, startupAt("boot", t0.Add(2*time.Second)), ""); got != MockDroppedOutputFull {
		t.Fatalf("a start-up read at the bound: %q, want %q", got, MockDroppedOutputFull)
	}
	if SendTestCaseOf(mgrs, "later", t0.Add(3*time.Second), t0.Add(4*time.Second), func() bool { return true }) {
		t.Fatal("a test case after a start-up read lost before the first test case sent was sent: it is recorded without the read")
	}
}

// A test case one of its managers leaves out is never handed to its stream,
// and one the stream queued is noted sent in each manager.
func TestSendTestCaseOfAsksTheStreamOnlyForATestCaseItMaySend(t *testing.T) {
	own, node := New(zap.NewNop()), New(zap.NewNop())
	fullLane(t, node)
	queueAt(t, outQueueMaxBytes)
	t0 := time.Now().Add(-time.Minute)
	mgrs := []*SyncMockManager{own, node}
	if !SendTestCaseOf(mgrs, "queued", t0.Add(-2*time.Second), t0.Add(-time.Second), func() bool { return true }) {
		t.Fatal("a test case its stream queued, with no loss, was reported not sent")
	}
	for _, m := range mgrs {
		if got := fmt.Sprint(sentNames(m)); got != "[queued]" {
			t.Fatalf("a manager noted %s sent, want [queued]", got)
		}
	}
	if got := sendNow(node, sharedAt(t, "c1", t0), ""); got != MockDroppedOutputFull {
		t.Fatalf("a session's mock at the bound: %q, want %q", got, MockDroppedOutputFull)
	}
	if SendTestCaseOf(mgrs, "after", t0.Add(time.Second), t0.Add(2*time.Second), func() bool {
		t.Error("the stream was asked to send a test case a manager left out")
		return true
	}) {
		t.Fatal("a test case after a lost session's mock was sent")
	}
	if !SendTestCaseOf(nil, "none", t0, t0, nil) {
		t.Fatal("a test case with no manager and a nil send was not let through")
	}
}

// The stream is asked under every manager's lock, so no loss is checked
// between its answer and the note: a loss waits until the test case is noted
// sent, and then finds it to revoke.
func TestSendTestCaseOfAsksTheStreamUnderTheManagersLocks(t *testing.T) {
	own, node := New(zap.NewNop()), New(zap.NewNop())
	t0 := time.Now().Add(-time.Minute)
	SendTestCaseOf([]*SyncMockManager{own, node}, "x", t0, t0.Add(time.Second), func() bool {
		for i, m := range []*SyncMockManager{own, node} {
			if m.sharedLossMu.TryLock() {
				m.sharedLossMu.Unlock()
				t.Errorf("manager %d's lock was free while its stream was asked", i)
			}
		}
		return true
	})
}
