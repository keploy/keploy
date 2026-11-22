package manager

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A test case that loses two mocks is one test case lost: the session counts
// its name once (SessionDrops.TestCases), as the set holds it once.
func TestATestCaseLosingTwoMocksIsCountedOnce(t *testing.T) {
	m := New(zap.NewNop())
	mark := m.StartDropSession()
	for range 2 {
		m.dropOut(addMockTo("c", false), "tc-1", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
	}
	if got := m.DropsSince(mark); got.TestCases != 1 || got.Capacity != 2 {
		t.Fatalf("the session reports %d test cases and %d mocks dropped, want 1 test case for its 2 mocks", got.TestCases, got.Capacity)
	}
}

// An answer the client never read that the manager drops for its output is
// not reported added, and is named: its parser may have said it kept it.
func TestAnUnreadAnswerDroppedForTheOutputIsNamedAndNotAdded(t *testing.T) {
	m := New(zap.NewNop())
	m.SetOutputChannel(make(chan *models.Mock, 1))
	m.CloseOutChan()
	mark := m.StartDropSession()
	unread := addMockTo("unread-out", false)
	unread.MarkUndelivered()
	if got := m.AddMock(unread); got != MockDroppedOutputClosed {
		t.Fatalf("an unread answer after the close: %q, want %q", got, MockDroppedOutputClosed)
	}
	got := m.DropsSince(mark)
	if got.Added != 0 || got.Capacity != 1 {
		t.Fatalf("the session reports %d added and %d dropped, want it dropped and not added", got.Added, got.Capacity)
	}
	if len(got.Named) != 1 || !strings.Contains(got.Named[0], "unread-out") || !strings.Contains(got.Named[0], string(MockDroppedOutputClosed)) {
		t.Fatalf("the session names %q, want the unread answer with why", got.Named)
	}
}

// A drop's line says when its mock was asked as the summary names it, a time
// in RFC 3339 with nanoseconds, not a zap time the agent's encoder bannered;
// a DNS mock, which has no connection, is named by its question.
func TestADropSaysWhenAndWhichQuestion(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	mark := m.StartDropSession()
	at := time.Date(2026, 11, 7, 9, 42, 28, 490000001, time.UTC)
	dnsMock := &models.Mock{Kind: models.DNS}
	dnsMock.Spec.Metadata = map[string]string{"qtype": "AAAA"}
	dnsMock.Spec.DNSReq = &models.DNSReq{Name: "gone.keploy.test."}
	dnsMock.Spec.ReqTimestampMock = at
	m.dropOut(dnsMock, "", 0, string(MockDroppedOutputClosed))
	f := logs.FilterLevelExact(zapcore.WarnLevel).All()[0].ContextMap()
	if f["mock_req_ts"] != at.Format(time.RFC3339Nano) || f["dns_question"] != "gone.keploy.test. AAAA" {
		t.Fatalf("the drop said mock_req_ts %v and dns_question %v, want %q and the question", f["mock_req_ts"], f["dns_question"], at.Format(time.RFC3339Nano))
	}
	named := m.DropsSince(mark).Named
	if want := "DNS mock of question gone.keploy.test. AAAA, asked at " + at.Format(time.RFC3339Nano); len(named) != 1 || !strings.HasPrefix(named[0], want) {
		t.Fatalf("the session names %q, want it to start %q", named, want)
	}
	plain := addMockTo("conn-7", false)
	m.dropOut(plain, "", 0, string(MockDroppedOutputClosed))
	if f := logs.All()[logs.Len()-1].ContextMap(); f["dns_question"] != nil {
		t.Fatalf("a mock with no question was said with dns_question %v", f["dns_question"])
	}
	if named := m.DropsSince(mark).Named; len(named) != 2 || !strings.HasPrefix(named[1], "Kafka mock of connection conn-7, asked at ") {
		t.Fatalf("the session names %q, want the second by its connection", named)
	}
}

// SendConfigMock says what it did with a mock, as AddMock does: a nil manager
// has no output and drops it.
func TestSendConfigMockSaysWhatItDid(t *testing.T) {
	var none *SyncMockManager
	if got := none.SendConfigMock(addMockTo("n", true)); got != MockDroppedOutputClosed {
		t.Fatalf("a nil manager: %q, want %q", got, MockDroppedOutputClosed)
	}
	m := New(zap.NewNop())
	ch := make(chan *models.Mock, 1)
	m.SetOutputChannel(ch)
	if got := m.SendConfigMock(addMockTo("s", true)); got != MockTaken {
		t.Fatalf("a mock the output took: %q, want it taken", got)
	}
	<-ch
	m.CloseOutChan()
	if got := m.SendConfigMock(addMockTo("s", true)); got != MockDroppedOutputClosed {
		t.Fatalf("a mock sent to a closed output: %q, want %q", got, MockDroppedOutputClosed)
	}
}
