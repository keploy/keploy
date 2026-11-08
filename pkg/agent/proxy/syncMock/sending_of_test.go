package manager

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// sentNames is the names m's stream noted sent (SendingTestCase).
func sentNames(m *SyncMockManager) []string {
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	var n []string
	for _, tc := range m.sentTCs.tcs {
		n = append(n, tc.name)
	}
	return n
}

// returnsWithin runs f and fails the test if it does not return within d: a
// lock it waits on forever is a failure of the row, not a hung test binary.
func returnsWithin(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// A test case whose mocks may come from more than one manager (a DaemonSet
// runtime's channel takes its own manager's and the node-default one's) is
// sent only if each of them may send it, whichever order they are named in,
// and one left out is noted sent by none: else it would stand as that
// manager's first test case.
func TestATestCaseOfManyManagersIsLeftOutWhenAnyOfThemLostAMockItMayNeed(t *testing.T) {
	own, node := New(zap.NewNop()), New(zap.NewNop())
	fullLane(t, node)
	queueAt(t, outQueueMaxBytes)
	t0 := time.Now().Add(-time.Minute)
	if !SendingTestCaseOf([]*SyncMockManager{own, node}, "before", t0.Add(-2*time.Second), t0.Add(-time.Second)) {
		t.Fatal("a test case with no loss of either manager was left out")
	}
	if got := sendNow(node, sharedAt(t, "c1", t0), ""); got != MockDroppedOutputFull {
		t.Fatalf("a session's mock at the bound: %q, want %q", got, MockDroppedOutputFull)
	}
	for _, order := range [][]*SyncMockManager{{own, node}, {node, own}} {
		if SendingTestCaseOf(order, "after", t0.Add(time.Second), t0.Add(2*time.Second)) {
			t.Fatal("a test case after a session's mock the node-default manager lost was sent")
		}
	}
	if got := fmt.Sprint(sentNames(own)); got != "[before]" {
		t.Fatalf("the runtime's own manager noted %s sent, want [before]: a test case left out was noted", got)
	}
	if got := fmt.Sprint(sentNames(node)); got != "[before]" {
		t.Fatalf("the node-default manager noted %s sent, want [before]", got)
	}
	if first := own.firstSent.Load(); first == nil || !first.Equal(t0.Add(-2*time.Second)) {
		t.Fatalf("the runtime's own first test case is %v, want the one sent", first)
	}
	// A lost test's mock no test owns, over the window, of the second named.
	own2, node2 := New(zap.NewNop()), New(zap.NewNop())
	node2.sharedLossMu.Lock()
	node2.lostOver.Record(t0, t0.Add(time.Second))
	node2.sharedLossMu.Unlock()
	if SendingTestCaseOf([]*SyncMockManager{own2, node2}, "over", t0.Add(500*time.Millisecond), t0.Add(2*time.Second)) {
		t.Fatal("a test case over a lost mock's call of the second manager was sent")
	}
	if n := sentNames(own2); len(n) != 0 || own2.firstSent.Load() != nil {
		t.Fatalf("the first manager noted %v sent (first %v) for a test case left out", n, own2.firstSent.Load())
	}
	if !SendingTestCaseOf([]*SyncMockManager{own2, node2}, "later", t0.Add(3*time.Second), t0.Add(4*time.Second)) {
		t.Fatal("a test case after the lost mock's call was left out")
	}
}

// Nil and repeated managers are skipped: a test case is noted once in each,
// and with none left every test case is sent, as a nil manager sends it.
func TestATestCaseOfManyManagersSkipsNilAndRepeatedOnes(t *testing.T) {
	m := New(zap.NewNop())
	at := time.Now().Add(-time.Minute)
	for _, mgrs := range [][]*SyncMockManager{nil, {}, {nil}, {nil, nil}} {
		if !SendingTestCaseOf(mgrs, "none", at, at) {
			t.Fatalf("a test case of managers %v was left out, want sent", mgrs)
		}
	}
	returnsWithin(t, 5*time.Second, "a test case of a manager named twice", func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("a test case of a nil and a repeated manager panicked: %v", r)
			}
		}()
		if !SendingTestCaseOf([]*SyncMockManager{nil, m, m, nil}, "one", at, at) {
			t.Error("a test case of one manager, named twice and beside nils, was left out")
		}
	})
	if got := fmt.Sprint(sentNames(m)); got != "[one]" {
		t.Fatalf("a manager named twice noted %s sent, want [one] once", got)
	}
}

// Test cases of many managers and of each alone, sent at once in either
// order, never wait on each other forever, and each is noted once in each
// manager it went through.
func TestTestCasesOfManyManagersInEitherOrderDoNotDeadlock(t *testing.T) {
	a, b := New(zap.NewNop()), New(zap.NewNop())
	at := time.Now().Add(-time.Minute)
	const n = 400
	returnsWithin(t, 20*time.Second, "test cases of two managers sent in either order", func() {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(4)
			go func(i int) {
				defer wg.Done()
				SendingTestCaseOf([]*SyncMockManager{a, b}, fmt.Sprintf("ab-%d", i), at, at)
			}(i)
			go func(i int) {
				defer wg.Done()
				SendingTestCaseOf([]*SyncMockManager{b, a}, fmt.Sprintf("ba-%d", i), at, at)
			}(i)
			go func(i int) { defer wg.Done(); a.SendingTestCase(fmt.Sprintf("a-%d", i), at, at) }(i)
			go func(i int) { defer wg.Done(); b.SendingTestCase(fmt.Sprintf("b-%d", i), at, at) }(i)
		}
		wg.Wait()
	})
	if got, want := len(sentNames(a)), 3*n; got != want {
		t.Fatalf("manager a noted %d test cases sent, want %d", got, want)
	}
	if got, want := len(sentNames(b)), 3*n; got != want {
		t.Fatalf("manager b noted %d test cases sent, want %d", got, want)
	}
}
