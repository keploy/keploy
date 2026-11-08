package manager

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A recording whose first request opened a window (as an ingress opens one
// before the request reaches the app) but became no test case: keploy's
// filters left it out after the exchange, or its sampling did, or the test
// case was left out. A replay places a start-up read the app made in that
// window (the async capture's: no owner, IsStartup) before the first test
// case the recording holds, in the start-up tier every test reads. Lost at
// the bound, it is one any test may need: the recording leaves out the test
// cases after its request and says so at ERROR, and the first test case
// sent after it is not sent without it.
func TestAStartUpReadInAWindowThatBecameNoTestCaseIsOneAnyTestMayNeed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is what happened before the read was lost: a window that
		// became no test case, and maybe a test case sent after it.
		before func(m *SyncMockManager, t0 time.Time)
		// revoked is the test case sent before the loss that it revokes.
		revoked string
	}{
		{name: "a filtered first request", before: func(m *SyncMockManager, t0 time.Time) {
			m.OpenWindow(t0, nil).Close()
		}},
		{name: "a sampled-out first exchange, then a test case sent", before: func(m *SyncMockManager, t0 time.Time) {
			m.OpenWindow(t0, nil).Close()
			w := m.OpenWindow(t0.Add(time.Second), nil)
			m.SendingTestCase("second-request", t0.Add(time.Second), t0.Add(2*time.Second))
			w.Close()
		}, revoked: "second-request"},
		{name: "a left-out first test case", before: func(m *SyncMockManager, t0 time.Time) {
			// A test's mock no start-up tag marks, lost over the first test
			// case's window, leaves it out: it is not noted as sent.
			m.OpenWindow(t0, nil).Close()
			lost := addMockTo("c2", false)
			lost.Spec.ReqTimestampMock, lost.Spec.ResTimestampMock = t0.Add(time.Millisecond), t0.Add(2*time.Millisecond)
			m.leaveOutOver(lost, m.revokeEpoch.Load())
			if m.SendingTestCase("first-request", t0, t0.Add(5*time.Millisecond)) {
				t.Fatal("setup: a test case over a lost call was sent")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			m := New(zap.New(core))
			m.SetRevokeCapable(true)
			fullLane(t, m)
			t0 := time.Now().Add(-time.Minute)
			m.StartRecordSession()
			tc.before(m, t0)
			queueAt(t, outQueueMaxBytes-outQueueSharedRoom)
			cfg := asyncAt("c1", t0.Add(10*time.Millisecond))
			cfg.Spec.ResTimestampMock = t0.Add(20 * time.Millisecond)
			if !m.servedToAnyTest(cfg, "") {
				t.Fatal("a start-up read before the first test case sent is read as one test's")
			}
			if got := sendNow(m, cfg, ""); got != MockTaken {
				t.Fatalf("a start-up read before the first test case, queued past the room kept: %q, want it queued in that room", got)
			}
			queueAt(t, outQueueSharedRoom)
			again := asyncAt("c1", t0.Add(10*time.Millisecond))
			again.Spec.ResTimestampMock = t0.Add(20 * time.Millisecond)
			if got := sendNow(m, again, ""); got != MockDroppedOutputFull {
				t.Fatalf("a start-up read at the bound: %q, want %q", got, MockDroppedOutputFull)
			}
			if leftOutLines(logs) != 1 {
				t.Fatalf("%d ERRORs say the recording leaves out the test cases after the lost read, want 1", leftOutLines(logs))
			}
			if m.SendingTestCase("first-test", t0.Add(5*time.Second), t0.Add(6*time.Second)) {
				t.Fatal("the first test case after the lost start-up read was sent without it")
			}
			if tc.revoked != "" && !m.WasMockDroppedForTC(tc.revoked) {
				t.Fatalf("%s, sent before the loss over the read's request, was not revoked", tc.revoked)
			}
		})
	}
}

// A test case revoked after it was sent, for a cause of its own (a mock it
// owns dropped, its window given up), is not the recording's first: a
// start-up read between its start and the next test case's is then one any
// test may need. One revoked that it no longer names (it let go of it at the
// cap) may have been the first: then every start-up mock is one any test may
// need until the recording ends. A test case taken out by a loss, whose
// mock it was, moves the first on as well.
func TestARevokedTestCaseIsNotTheRecordingsFirst(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	read := asyncAt("c1", t0.Add(1500*time.Millisecond))
	sendTwo := func(m *SyncMockManager) {
		m.SendingTestCase("a", t0, t0.Add(time.Second))
		m.SendingTestCase("b", t0.Add(2*time.Second), t0.Add(3*time.Second))
		if m.servedToAnyTest(read, "") {
			t.Fatal("setup: a start-up read after the first test case is read as one any test may need")
		}
	}
	t.Run("its own mock dropped", func(t *testing.T) {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		sendTwo(m)
		m.countDrop(asyncAt("c1", t0.Add(time.Millisecond)), "a", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if !m.servedToAnyTest(read, "") {
			t.Fatal("a start-up read before the first test case still sent is read as a test's")
		}
	})
	t.Run("its window given up", func(t *testing.T) {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		sendTwo(m)
		m.RevokeTestCase("never-sent")
		if m.servedToAnyTest(read, "") {
			t.Fatal("a name never sent, revoked, moved the first test case on")
		}
		m.RevokeTestCase("a")
		if !m.servedToAnyTest(read, "") {
			t.Fatal("a start-up read before the first test case still sent is read as a test's")
		}
	})
	t.Run("an older recording's drop", func(t *testing.T) {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		old := m.revokeEpoch.Load()
		m.SetOutputChannel(make(chan *models.Mock, 1))
		sendTwo(m)
		m.countDrop(asyncAt("c1", t0.Add(time.Millisecond)), "a", old, string(MockDroppedOutputFull))
		if m.servedToAnyTest(read, "") {
			t.Fatal("an older recording's drop for a name moved this recording's first test case")
		}
	})
	t.Run("a loss over it", func(t *testing.T) {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		sendTwo(m)
		lost := addMockTo("c2", false)
		lost.Spec.ReqTimestampMock, lost.Spec.ResTimestampMock = t0.Add(100*time.Millisecond), t0.Add(200*time.Millisecond)
		m.leaveOutOver(lost, m.revokeEpoch.Load())
		if !m.servedToAnyTest(read, "") {
			t.Fatal("a start-up read before the first test case still sent is read as a test's")
		}
	})
	t.Run("one let go at the cap", func(t *testing.T) {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		sendTwo(m)
		for i := 0; i < maxSentTCs; i++ {
			at := t0.Add(time.Hour + time.Duration(i)*time.Millisecond)
			m.SendingTestCase(fmt.Sprintf("later-%d", i), at, at)
		}
		if m.servedToAnyTest(read, "") {
			t.Fatal("letting go of the first test case named moved the first on")
		}
		// A loss over every test case still named leaves the first among
		// those let go.
		over := addMockTo("c2", false)
		over.Spec.ReqTimestampMock, over.Spec.ResTimestampMock = t0.Add(time.Hour), t0.Add(time.Hour+10*time.Second)
		m.leaveOutOver(over, m.revokeEpoch.Load())
		if m.servedToAnyTest(read, "") {
			t.Fatal("with every test case named revoked, the first let go is not the first")
		}
		m.RevokeTestCase("a")
		if !m.servedToAnyTest(read, "") {
			t.Fatal("a test case let go and revoked since may have been the first, and a start-up read after it is read as a test's")
		}
		m.SetOutputChannel(make(chan *models.Mock, 1))
		m.SendingTestCase("next", t0, t0)
		if m.servedToAnyTest(read, "") {
			t.Fatal("a new recording does not know its first test case")
		}
	})
}

// A name lost in one recording leaves out no test case of the next that
// carries it, nor does a drop a replaced channel's lane makes after the
// switch. The same name lost again in the new recording is left out there.
func TestANameLostInOneRecordingIsNotLostInTheNext(t *testing.T) {
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	old := m.revokeEpoch.Load()
	m.RevokeTestCase("test-1")
	if !m.WasMockDroppedForTC("test-1") {
		t.Fatal("setup: a revoked name is not lost")
	}
	m.SetOutputChannel(make(chan *models.Mock, 1))
	if m.WasMockDroppedForTC("test-1") {
		t.Fatal("a name lost in the last recording leaves out the next one's test case of that name")
	}
	m.countDrop(asyncAt("c1", time.Now()), "test-2", old, string(MockDroppedOutputFull))
	if m.WasMockDroppedForTC("test-2") {
		t.Fatal("a replaced lane's drop leaves out the new recording's test case of that name")
	}
	m.RevokeTestCase("test-1")
	if !m.WasMockDroppedForTC("test-1") {
		t.Fatal("a name lost again in the new recording is not lost there")
	}
	m.countDrop(asyncAt("c1", time.Now()), "test-1", old, string(MockDroppedOutputFull))
	if !m.WasMockDroppedForTC("test-1") {
		t.Fatal("an older lane's drop of a name took it from the new recording's losses")
	}
	// Past the bound the oldest loss is let go: test-1's first, not the
	// later one.
	for i := 0; i < maxDroppedTCNames-2; i++ {
		m.RevokeTestCase(fmt.Sprintf("other-%d", i))
	}
	if !m.WasMockDroppedForTC("test-1") {
		t.Fatal("letting go of a name's first loss let go of its later one")
	}
}

// The boundaries of a loss are fail-closed to the nanosecond: a test case
// whose window ends at the lost mock's request is left out (checked after)
// or revoked (sent before), one let go that ended at it is said, and a
// second loss with the same request moves nothing and says no second ERROR.
func TestALossReachesATestCaseThatEndsAtItsRequest(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	m, said := sentThenLost(t, at, at, at.Add(-time.Nanosecond))
	if got := revokesQueued(m); len(got) != 1 || got[0] != "tc-0" {
		t.Fatalf("revoked %v, want [tc-0], whose window ends at the lost mock's request", got)
	}
	if said == nil || said["revoked_sent"] != int64(1) {
		t.Fatalf("the ERROR says %v, want revoked_sent 1", said)
	}
	if m.SendingTestCase("ends-at-it", at.Add(-time.Second), at) {
		t.Fatal("a test case checked after the loss whose window ends at its request was sent")
	}
	if !m.SendingTestCase("ends-before-it", at.Add(-time.Second), at.Add(-time.Nanosecond)) {
		t.Fatal("a test case checked after the loss that ended before its request was left out")
	}

	core, logs := observer.New(zapcore.DebugLevel)
	m2 := New(zap.New(core))
	m2.SetOutputChannel(make(chan *models.Mock, 1))
	for i := 0; i < maxSentTCs; i++ {
		m2.SendingTestCase(fmt.Sprintf("tc-%d", i), at.Add(-time.Hour), at.Add(time.Duration(i-maxSentTCs/2+1)*time.Nanosecond))
	}
	m2.SendingTestCase("one-more", at.Add(time.Hour), at.Add(time.Hour))
	for range 2 {
		m2.dropOut(sharedAt(t, "c1", at), "", m2.revokeEpoch.Load(), string(MockDroppedOutputFull))
	}
	if n := leftOutLines(logs); n != 1 {
		t.Fatalf("%d ERRORs for two losses with the same request, want 1", n)
	}
	for _, e := range logs.All() {
		if f := e.ContextMap(); f["left_out_from"] != nil {
			if f["unnamed_sent_through"] != at.Format(time.RFC3339Nano) {
				t.Fatalf("the ERROR says unnamed_sent_through %v, want the lost mock's request, at which one let go ended", f["unnamed_sent_through"])
			}
		}
	}
}

// A loss finds the test cases its call reaches by a search from the longest
// window noted: one sent long before, whose window still reaches the call,
// is revoked, and the short ones that ended before it are kept.
func TestALossFindsALongTestCaseSentLongBefore(t *testing.T) {
	m := New(nil)
	m.SetRevokeCapable(true)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	at := time.Now().Add(-time.Minute)
	m.SendingTestCase("long", at.Add(-time.Hour), at.Add(time.Millisecond))
	for i := 0; i < 100; i++ {
		s := at.Add(-time.Duration(100-i) * time.Second)
		m.SendingTestCase(fmt.Sprintf("short-%d", i), s, s.Add(time.Millisecond))
	}
	lost := addMockTo("c1", false)
	lost.Spec.ReqTimestampMock, lost.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	m.leaveOutOver(lost, m.revokeEpoch.Load())
	if got := revokesQueued(m); len(got) != 1 || got[0] != "long" {
		t.Fatalf("revoked %v, want only the long test case over the call", got)
	}
	m.sharedLossMu.Lock()
	named := len(m.sentTCs.tcs)
	m.sharedLossMu.Unlock()
	if named != 100 {
		t.Fatalf("%d test cases still named, want the 100 short ones", named)
	}
}

// Past its cap the sent list lets go of the half that started first, all at
// once, and keeps the earliest start and the latest end among them, which
// need not be the last let go's: the recording's first test case is still
// known, and a loss before that end says it cannot name them. A revoke by
// name of one it let go makes the first unknown (fail closed).
func TestTheSentListLetsGoTheHalfThatStartedFirst(t *testing.T) {
	var l sentList
	t0 := time.Now().Add(-time.Hour)
	half := maxSentTCs / 2
	for i := 0; i < maxSentTCs; i++ {
		start := t0.Add(time.Duration(maxSentTCs-i) * time.Millisecond) // sent latest-start first
		end := start.Add(time.Millisecond)
		if i == maxSentTCs-100 {
			end = start.Add(time.Hour) // the latest end: neither the first nor the last let go
		}
		l.add(fmt.Sprintf("tc-%d", i), start, end)
	}
	l.add("one-more", t0.Add(time.Hour), t0.Add(time.Hour))
	if len(l.tcs) != half+1 || len(l.byName) != half+1 {
		t.Fatalf("%d named (%d by name), want %d", len(l.tcs), len(l.byName), half+1)
	}
	if _, ok := l.byName[fmt.Sprintf("tc-%d", maxSentTCs-1)]; ok {
		t.Fatal("the test case that started first is still named")
	}
	if _, ok := l.byName["tc-0"]; !ok {
		t.Fatal("the test case that started last of those sent first was let go")
	}
	first, ok := l.first()
	if want := t0.Add(time.Millisecond); !ok || !first.Equal(want) {
		t.Fatalf("first %v (%v), want %v, the earliest start let go", first, ok, want)
	}
	if got, want := l.unnamedReaching(t0.Add(time.Minute)), t0.Add(100*time.Millisecond+time.Hour); !got.Equal(want) {
		t.Fatalf("unnamed reaching %v, want %v, the latest end let go", got, want)
	}
	if got := l.unnamedReaching(t0.Add(2 * time.Hour)); !got.IsZero() {
		t.Fatalf("unnamed reaching %v past every end let go, want none", got)
	}
	l.forget("tc-0")
	if first, ok := l.first(); !ok || !first.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("revoking one still named moved the first to %v (%v)", first, ok)
	}
	l.forget(fmt.Sprintf("tc-%d", maxSentTCs-1))
	if _, ok := l.first(); ok {
		t.Fatal("a revoke of one let go left the first known")
	}
}

// The review's case, end to end through the queue: a filtered probe's
// window opens first and becomes no test case, then the app makes a
// start-up read. The read is one any test may need, so the shared room
// keeps it where a test's mock is dropped; at the full bound it is dropped,
// the first test case after it is left out, and an ERROR says so.
func TestAStartUpReadInAFilteredProbesWindowIsKeptOrSaid(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	m.StartRecordSession()
	m.OpenWindow(t0, nil).Close() // the filtered probe's window: no test case comes of it
	read := func(at time.Duration) MockDrop {
		cfg := asyncAt("c1", t0.Add(at))
		cfg.Spec.ResTimestampMock = t0.Add(at + 10*time.Millisecond)
		if !m.servedToAnyTest(cfg, "") {
			t.Fatal("a start-up read in a window that became no test case is not one any test may need")
		}
		return sendNow(m, cfg, "")
	}
	queueAt(t, outQueueMaxBytes-outQueueSharedRoom)
	if got := read(10 * time.Millisecond); got == MockDroppedOutputFull {
		t.Fatal("a start-up read was dropped where the shared room keeps it")
	}
	queueAt(t, outQueueSharedRoom)
	if got := read(20 * time.Millisecond); got != MockDroppedOutputFull {
		t.Fatalf("a start-up read at the full bound: %q, want %q", got, MockDroppedOutputFull)
	}
	if m.SendingTestCase("first-test", t0.Add(5*time.Second), t0.Add(6*time.Second)) {
		t.Fatal("the first test case after the lost start-up read was sent without it")
	}
	if n := leftOutLines(logs); n != 1 {
		t.Fatalf("%d ERRORs say what the loss left out, want 1", n)
	}
}
