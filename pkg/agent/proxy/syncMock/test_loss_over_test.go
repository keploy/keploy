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

// asyncAt is a test's mock as the async capture sends it: no owner, and
// tagged IsStartup, as AddMock tags every mock while no request was signalled
// (the async path never signals one), requested at at.
func asyncAt(conn string, at time.Time) *models.Mock {
	return startupAt(conn, at)
}

// sentFirstAt has m's stream send a test case whose window is [start,
// start]: the recording's first test case is then known.
func sentFirstAt(m *SyncMockManager, start time.Time) {
	m.SendingTestCase(fmt.Sprintf("first-%d", start.UnixNano()), start, start)
}

// A mock no test owns is one any test may need only if it is a session's or a
// connection's, one with no request time, or one of the start-up window's
// (IsStartup) requested before the recording's first test case (the earliest
// window start among those its stream sent and did not revoke), or that may
// have been: none was sent yet. IsStartup alone, which the async capture's
// mocks all carry, does not make one. The earliest test case counts,
// whatever the order they are sent in, and a new recording forgets the last
// one's.
func TestOnlyAMockBeforeTheFirstTestCaseIsOneOfTheStartUpWindows(t *testing.T) {
	m := New(nil)
	t0 := time.Now().Add(-time.Minute)
	if !m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
		t.Fatal("a mock no test owns, sent before any test case was, is not one any test may need")
	}
	sentFirstAt(m, t0.Add(2*time.Second))
	if !m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
		t.Fatal("a mock requested before the first test case sent is not one any test may need")
	}
	sentFirstAt(m, t0)
	sentFirstAt(m, t0.Add(3*time.Second))
	if m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
		t.Fatal("a mock requested after the earliest test case sent is read as one any test may need")
	}
	if m.servedToAnyTest(asyncAt("c1", t0), "") {
		t.Fatal("a mock requested as the first test case began is read as one any test may need")
	}
	if !m.servedToAnyTest(asyncAt("c1", t0.Add(-time.Nanosecond)), "") {
		t.Fatal("a mock requested before the first test case is not one any test may need")
	}
	noTime := asyncAt("c1", t0)
	noTime.Spec.ReqTimestampMock = time.Time{}
	if !m.servedToAnyTest(noTime, "") {
		t.Fatal("a start-up mock no test owns with no request time is not read as one any test may need")
	}
	notStartup := addMockTo("c1", false)
	notStartup.Spec.ReqTimestampMock = t0.Add(-time.Second)
	if m.servedToAnyTest(notStartup, "") {
		t.Fatal("a mock AddMock did not tag as the start-up window's is read as one any test may need")
	}
	notStartup.Spec.ReqTimestampMock = time.Time{}
	if !m.servedToAnyTest(notStartup, "") {
		t.Fatal("a mock no test owns with no request time is not read as one any test may need")
	}
	if !m.servedToAnyTest(sharedAt(t, "c1", t0.Add(time.Second)), "") {
		t.Fatal("a session's mock after the first test case is not one any test may need")
	}
	if m.servedToAnyTest(asyncAt("c1", t0.Add(-time.Second)), "test-1") {
		t.Fatal("a mock a test owns is read as one any test may need")
	}
	if got := m.queueBound(asyncAt("c1", t0.Add(time.Second)), "", m.revokeEpoch.Load()); got != outQueueMaxBytes-outQueueSharedRoom {
		t.Fatalf("the bound of a test's mock no test owns is %d, want the room left for shared mocks kept (%d)", got, outQueueMaxBytes-outQueueSharedRoom)
	}
	m.SetOutputChannel(make(chan *models.Mock, 1))
	if !m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
		t.Fatal("a new recording reads the last one's first test case as its own")
	}
}

// The async capture's mock of a test, lost at the bound, leaves out only the
// test cases whose window overlaps its call, and says no ERROR: those before
// and after it are sent. It is bounded as a test's mock is, so the room kept
// for the mocks any test may need stays theirs.
func TestATestsMockNoTestOwnsLostAtTheBoundLeavesOutOnlyTheTestsOverIt(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	queueAt(t, outQueueMaxBytes-outQueueSharedRoom)
	at := t0.Add(10 * time.Second)
	mk := asyncAt("c1", at)
	mk.Spec.ResTimestampMock = at.Add(time.Second)
	if got := sendNow(m, mk, ""); got != MockDroppedOutputFull {
		t.Fatalf("a test's mock no test owns past the room kept: %q, want %q", got, MockDroppedOutputFull)
	}
	if leftOutLines(logs) != 0 {
		t.Fatal("a test's mock lost said the recording leaves out the test cases after it")
	}
	if leftOutAfter(m, at.Add(5*time.Second)) || !m.SendingTestCase("after", at.Add(3*time.Second), at.Add(4*time.Second)) {
		t.Fatal("a test case after the lost test's mock was left out")
	}
	if !m.SendingTestCase("before", at.Add(-2*time.Second), at.Add(-time.Second)) {
		t.Fatal("a test case before the lost test's mock was left out")
	}
	for name, w := range map[string][2]time.Time{
		"over its request": {at.Add(-time.Second), at},
		"over its answer":  {at.Add(time.Second), at.Add(2 * time.Second)},
		"inside its call":  {at.Add(100 * time.Millisecond), at.Add(200 * time.Millisecond)},
		"around its call":  {at.Add(-time.Second), at.Add(2 * time.Second)},
	} {
		if m.SendingTestCase(name, w[0], w[1]) {
			t.Fatalf("a test case whose window is %s was sent without the lost mock", name)
		}
	}
}

// One whose answer is stamped before its request spans its request alone:
// it revokes a test case sent before it that starts at its request, and
// leaves out one checked after it over its request, but not one before it.
func TestATestsMockNoTestOwnsAnsweredBeforeItsRequestSpansItsRequest(t *testing.T) {
	m := New(nil)
	m.SetRevokeCapable(true)
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	queueAt(t, outQueueMaxBytes)
	at := t0.Add(10 * time.Second)
	if !m.SendingTestCase("sent-from-its-request", at, at.Add(time.Second)) {
		t.Fatal("a test case was left out with no mock lost")
	}
	mk := asyncAt("c1", at)
	mk.Spec.ResTimestampMock = at.Add(-time.Second)
	sendNow(m, mk, "")
	if got := revokesQueued(m); len(got) != 1 || got[0] != "sent-from-its-request" {
		t.Fatalf("revoked %v, want the test case sent from its request on", got)
	}
	if m.SendingTestCase("over its request", at, at.Add(time.Second)) {
		t.Fatal("a test case over the lost mock's request was sent")
	}
	if !m.SendingTestCase("before its request", at.Add(-time.Second), at.Add(-time.Millisecond)) {
		t.Fatal("a test case before the lost mock's request was left out")
	}
}

// Given up on with the consumer stalled, the async capture's mock of a test
// revokes by name the test cases the stream sent before it whose window
// overlaps its call, and only those; one given up on an older recording's
// channel revokes none of the current one's.
func TestATestsMockNoTestOwnsGivenUpRevokesTheSentTestCasesOverIt(t *testing.T) {
	t.Run("the current recording", func(t *testing.T) {
		m := New(nil)
		m.SetRevokeCapable(true)
		_, clk := fullLane(t, m)
		t0 := time.Now().Add(-time.Minute)
		sentFirstAt(m, t0)
		at := t0.Add(10 * time.Second)
		for name, w := range map[string][2]time.Time{
			"ended-before":  {at.Add(-2 * time.Second), at.Add(-time.Second)},
			"over":          {at.Add(-time.Second), at.Add(time.Second)},
			"no-start":      {{}, at.Add(time.Millisecond)},
			"started-after": {at.Add(2 * time.Second), at.Add(3 * time.Second)},
		} {
			if !m.SendingTestCase(name, w[0], w[1]) {
				t.Fatalf("%s was left out with no mock lost", name)
			}
		}
		mk := asyncAt("c1", at)
		mk.Spec.ResTimestampMock = at.Add(time.Second)
		lost := make(chan bool, 1)
		m.send(mk, "", func(d bool) { lost <- !d })
		clk.Advance(time.Hour) // the consumer took nothing for the bound
		if !<-lost {
			t.Fatal("a mock no consumer took was delivered")
		}
		got := map[string]bool{}
		for _, n := range revokesQueued(m) {
			got[n] = true
		}
		if len(got) != 2 || !got["over"] || !got["no-start"] {
			t.Fatalf("revoked %v, want over and no-start", revokesQueued(m))
		}
		m.sharedLossMu.Lock()
		named := len(m.sentTCs.of("").tcs)
		m.sharedLossMu.Unlock()
		if named != 3 {
			t.Fatalf("%d test cases still named after the loss, want 3: a revoked one is taken out", named)
		}
		if m.SendingTestCase("checked-after", at, at.Add(time.Millisecond)) {
			t.Fatal("a test case over the given-up mock's call, checked after it, was sent")
		}
	})
	t.Run("an older recording's channel", func(t *testing.T) {
		m := New(nil)
		m.SetRevokeCapable(true)
		_, clk := fullLane(t, m)
		t0 := time.Now().Add(-time.Minute)
		sentFirstAt(m, t0)
		at := t0.Add(10 * time.Second)
		lost := make(chan bool, 1)
		m.send(asyncAt("c1", at), "", func(d bool) { lost <- !d })
		m.SetOutputChannel(make(chan *models.Mock, 1))
		if !m.SendingTestCase("new", at.Add(-time.Second), at.Add(time.Second)) {
			t.Fatal("a test case of the new recording was left out with no mock of its lost")
		}
		clk.Advance(time.Hour) // the old consumer took nothing for the bound
		if !<-lost {
			t.Fatal("a mock no consumer took was delivered")
		}
		if got := revokesQueued(m); len(got) != 0 {
			t.Fatalf("the old recording's loss revoked %v of the new one's", got)
		}
		if !m.SendingTestCase("new-2", at.Add(-time.Second), at.Add(time.Second)) {
			t.Fatal("the old recording's loss left out a test case of the new one")
		}
	})
}

// A test's mock no test owns, lost once the stream sent more test cases than
// it names, says at WARN, for the first such loss and the sampled ones, that
// those it no longer names may stay recorded without it. One whose request
// came after them all says nothing.
func TestATestsMockNoTestOwnsLostSaysTheSentTestCasesItCanNoLongerName(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	queueAt(t, outQueueMaxBytes)
	at := t0.Add(10 * time.Second)
	for i := 0; i <= maxSentTCs; i++ {
		m.SendingTestCase(fmt.Sprintf("tc-%d", i), at.Add(-time.Second), at.Add(time.Second))
	}
	warns := func() int {
		n := 0
		for _, e := range logs.All() {
			if _, ok := e.ContextMap()["unnamed_sent_through"]; ok && e.Level == zapcore.WarnLevel {
				n++
			}
		}
		return n
	}
	sendNow(m, asyncAt("c1", at.Add(2*time.Second)), "")
	if got := warns(); got != 0 {
		t.Fatalf("a loss after every unnamed test case's window said %d WARNs, want none", got)
	}
	for i := 0; i < 2; i++ {
		sendNow(m, asyncAt("c1", at), "")
	}
	if got := warns(); got != 1 {
		t.Fatalf("%d WARNs say sent test cases may stay without the mock, want 1 (the first such loss)", got)
	}
	var said map[string]interface{}
	for _, e := range logs.All() {
		if f := e.ContextMap(); f["unnamed_sent_through"] != nil {
			said = f
		}
	}
	if said["unnamed_sent_through"] != at.Add(time.Second).Format(time.RFC3339Nano) || said["sent_named_max"] != int64(maxSentTCs) || said["such_losses_so_far"] != uint64(1) {
		t.Fatalf("the WARN says %v", said)
	}
	if leftOutLines(logs) != 0 {
		t.Fatal("a test's mock lost said the recording leaves out the test cases after it")
	}
	m.SetOutputChannel(make(chan *models.Mock, 1))
	m.sharedLossMu.Lock()
	n := m.perTestUnnamed
	m.sharedLossMu.Unlock()
	if n != 0 {
		t.Fatalf("a new recording starts with %d such losses counted, want 0", n)
	}
}

// A test case with no window start is read as its end, and one whose start
// is after its end as starting at its end: either over a lost test's mock's
// call is left out.
func TestATestCaseWithNoOrABackwardWindowIsCheckedAtItsEnd(t *testing.T) {
	m := New(nil)
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	queueAt(t, outQueueMaxBytes)
	at := t0.Add(10 * time.Second)
	mk := asyncAt("c1", at)
	mk.Spec.ResTimestampMock = at.Add(time.Second)
	sendNow(m, mk, "")
	if m.SendingTestCase("no-start", time.Time{}, at.Add(500*time.Millisecond)) {
		t.Fatal("a test case with no start that ends inside the lost mock's call was sent")
	}
	if m.SendingTestCase("backward", at.Add(5*time.Second), at.Add(500*time.Millisecond)) {
		t.Fatal("a test case whose start is after its end, ending inside the lost mock's call, was sent")
	}
}

// A mock a test owns, lost, leaves out its test by name and no other: a test
// case over its call is sent. Nor does an answer the client never read leave
// out one over it.
func TestAnOwnedMockOrAnUnreadAnswerLostSpansNoCall(t *testing.T) {
	m := New(nil)
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	queueAt(t, outQueueMaxBytes)
	at := t0.Add(10 * time.Second)
	owned := asyncAt("c1", at)
	owned.Spec.ResTimestampMock = at.Add(time.Second)
	if got := sendNow(m, owned, "test-1"); got != MockDroppedOutputFull {
		t.Fatalf("an owned mock at the bound: %q, want %q", got, MockDroppedOutputFull)
	}
	unread := asyncAt("c1", at.Add(10*time.Second))
	unread.Spec.ResTimestampMock = at.Add(11 * time.Second)
	unread.Spec.Metadata = map[string]string{models.MetadataUndeliveredKey: models.MetadataUndelivered}
	if got := sendNow(m, unread, ""); got != MockDroppedOutputFull {
		t.Fatalf("an unread answer at the bound: %q, want %q", got, MockDroppedOutputFull)
	}
	if !m.WasMockDroppedForTC("test-1") {
		t.Fatal("the owner of the lost mock is not left out by name")
	}
	if !m.SendingTestCase("over-owned", at, at.Add(time.Second)) {
		t.Fatal("a test case over a lost mock another test owns was left out")
	}
	if !m.SendingTestCase("over-unread", at.Add(10*time.Second), at.Add(11*time.Second)) {
		t.Fatal("a test case over a lost answer the client never read was left out")
	}
}

// Once the spans of lost calls reach their bound, those that end before the
// stream's checked mark (CheckedBefore) are joined first: a gap between two
// later ones, where a test case still to be checked sits, is kept.
func TestTheLostCallsSpansJoinTheCheckedOnesFirst(t *testing.T) {
	m := New(nil)
	fullLane(t, m)
	t0 := time.Now().Add(-time.Hour)
	// The first test case sent ends before every loss, so none revokes it:
	// the recording's first stays known, and each loss is a test's.
	sentFirstAt(m, t0.Add(-time.Minute))
	queueAt(t, outQueueMaxBytes)
	lose := func(at time.Time) {
		mk := asyncAt("c1", at)
		mk.Spec.ResTimestampMock = at.Add(time.Millisecond)
		if got := sendNow(m, mk, ""); got != MockDroppedOutputFull {
			t.Fatalf("a test's mock at the bound: %q, want %q", got, MockDroppedOutputFull)
		}
	}
	// Checked spans far apart, then later ones close together.
	for i := 0; i < maxPressureRanges-2; i++ {
		lose(t0.Add(time.Duration(i) * time.Second))
	}
	mark := t0.Add(time.Duration(maxPressureRanges) * time.Second)
	m.CheckedBefore(mark)
	later := mark.Add(time.Second)
	for i := 0; i < 8; i++ {
		lose(later.Add(time.Duration(i) * 10 * time.Millisecond))
	}
	if !m.SendingTestCase("in-a-later-gap", later.Add(13*time.Millisecond), later.Add(17*time.Millisecond)) {
		t.Fatal("a test case in a gap between later lost calls was left out: the join took it before the checked spans")
	}
}
