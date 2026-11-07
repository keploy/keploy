package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// placeRelease stands in for the release enterprise registers
// (models.RegisterAnswerRelease): a moment inside a window is due once that
// window opened, one in the gap after a test once that test ended, one
// before every window at once, and every one at the set's end.
func placeRelease(p models.RunPlace, t time.Time) bool {
	if p.SetEnd || p.Windows.Len() == 0 {
		return true
	}
	i := p.Windows.ReleaseIndex(t)
	if i < 0 {
		return true
	}
	anchor := models.RunOpen(i)
	if !p.Windows.Contains(t) {
		anchor = models.RunEnd(i)
	}
	return anchor <= p.Reached
}

// holdSeen is a manager as the HTTP parser's store, which says when the
// parser first waits on the run's place: it holds an answer it matched.
type holdSeen struct {
	*proxyPkg.MockManager
	held chan struct{}
	once sync.Once
}

func (h *holdSeen) IdleUntilRunPlaceChanges(changed <-chan struct{}, mark func() (release func())) (func(), bool) {
	release, ok := h.MockManager.IdleUntilRunPlaceChanges(changed, mark)
	if ok {
		h.once.Do(func() { close(h.held) })
	}
	return release, ok
}

// heldCall sends GET path through the real HTTP parser, waits until the
// parser holds the answer of the recording it matched, and returns, once the
// application has read the whole answer, the recording that answered it
// ("" for none). A row moves the run's place only after the hold: a parser
// that read the call after a move would match it in a later test, or find
// its answer due at once.
func heldCall(t *testing.T, mm *proxyPkg.MockManager, path string) <-chan string {
	t.Helper()
	db, out := sendCall(mm, path)
	select {
	case <-db.held:
	case a := <-out:
		t.Fatalf("GET %s: the application read %q before the parser held an answer", path, a)
	case <-time.After(10 * time.Second):
		t.Fatalf("GET %s: the parser held no answer", path)
	}
	return out
}

// sendCall sends GET path through the real HTTP parser and returns its store,
// and, once the application has read the whole answer, the recording that
// answered it ("" for none).
func sendCall(mm *proxyPkg.MockManager, path string) (*holdSeen, <-chan string) {
	db := &holdSeen{MockManager: mm, held: make(chan struct{})}
	out := make(chan string, 1)
	app, agentEnd := net.Pipe()
	go func() {
		_ = httpparser.New(zap.NewNop()).MockOutgoing(context.Background(), agentEnd, &models.ConditionalDstCfg{}, db, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	go func() {
		defer func() { _ = app.Close() }()
		got := ""
		if _, err := app.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: api\r\n\r\n")); err == nil {
			if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					got = string(body)
				}
			}
		}
		out <- got
	}()
	return db, out
}

// notYet fails the test when the application has its answer.
func notYet(t *testing.T, got <-chan string, what string) {
	t.Helper()
	select {
	case a := <-got:
		t.Fatalf("%s: the application read %q", what, a)
	case <-time.After(200 * time.Millisecond):
	}
}

func written(t *testing.T, got <-chan string, want, what string) {
	t.Helper()
	select {
	case a := <-got:
		if a != want {
			t.Fatalf("%s: the application read %q, want %q", what, a, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the answer was not written", what)
	}
}

// keploy mock replay with the proven switch off: the scope table's recorded
// windows give the run's place the set's windows, and each test's begin
// moves it. A call made in Test1 whose answer was recorded in Test2's window
// is matched in Test1 and written at Test2's begin, not before. One whose
// answer was recorded after Test1's last recording, before Test2 began, is
// Test1's (models.RunWindows): a test's last answer lies there, and the test
// cannot end without it. A test with no mapping moves the place too.
func TestMockReplayWritesAnAnswerAtItsRecordedPlaceWithTheSwitchOff(t *testing.T) {
	t.Cleanup(models.RegisterAnswerRelease(models.HTTP, placeRelease))
	slow := stageMock("mock-slow", "/slow", stageT0)
	answeredAt(slow, stageT0.Add(5*time.Second))
	gap := stageMock("mock-gap", "/gap", stageT0.Add(500*time.Millisecond))
	answeredAt(gap, stageT0.Add(3*time.Second))
	late := stageMock("mock-late", "/late", stageT0.Add(5*time.Second))
	answeredAt(late, stageT0.Add(9*time.Second))
	a, p := newStageReplay(t,
		[]*models.Mock{slow, gap, late, stageMock("mock-b", "/b", stageT0.Add(4*time.Second))},
		nil,
		map[string][]string{"Test1": {"mock-slow", "mock-gap"}, "Test2": {"mock-b", "mock-late"}})
	if err := a.SetRunPlaceWindows(context.Background(), []models.TestWindow{
		{TestCase: "Test1", Start: stageT0, End: stageT0.Add(time.Second)},
		{TestCase: "Test2", Start: stageT0.Add(4 * time.Second), End: stageT0.Add(6 * time.Second)},
		{TestCase: "Test3", Start: stageT0.Add(8 * time.Second), End: stageT0.Add(10 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}

	scopeBegin(t, a, "Test1")
	slowGot := heldCall(t, p.mm, "/slow")
	notYet(t, slowGot, "Test2's answer during Test1")
	_, gapGot := sendCall(p.mm, "/gap")
	written(t, gapGot, "mock-gap", "a test's own last answer, during that test")
	scopeEnd(t, a, "Test1")
	notYet(t, slowGot, "Test2's answer after Test1's end")
	// A begin keploy refuses (a worker pid with no marks) opens nothing.
	if err := a.BeginScope(context.Background(), "Test2", 4242); err == nil {
		t.Fatal("a begin with a worker pid and no marks was not refused")
	}
	notYet(t, slowGot, "Test2's answer at a refused begin")
	scopeBegin(t, a, "Test2")
	written(t, slowGot, "mock-slow", "at Test2's begin")

	lateGot := heldCall(t, p.mm, "/late")
	notYet(t, lateGot, "Test3's answer during Test2")
	scopeEnd(t, a, "Test2")
	notYet(t, lateGot, "Test3's answer after Test2's end")
	scopeBegin(t, a, "Test3")
	written(t, lateGot, "mock-late", "at the begin of Test3, which has no mapping")
	scopeEnd(t, a, "Test3")
}

// An answer recorded in the window of the set's last test, which the run
// set aside (it never began), is written at the read of the set's end
// (TestGetMockErrors_ASetEndReadFilesAfterTheSettle: before that read
// settles), not at the end of the last test that ran, nor at a test's read.
func TestTheSetsEndReadWritesAnAnswerOfATestTheRunNeverReached(t *testing.T) {
	t.Cleanup(models.RegisterAnswerRelease(models.HTTP, placeRelease))
	late := stageMock("mock-late", "/late", stageT0.Add(500*time.Millisecond))
	answeredAt(late, stageT0.Add(9*time.Second))
	a, p := newStageReplay(t, []*models.Mock{late, stageMock("mock-b", "/b", stageT0.Add(4*time.Second))}, nil,
		map[string][]string{"Test1": {"mock-late"}, "Test2": {"mock-b"}})
	if err := a.SetRunPlaceWindows(context.Background(), []models.TestWindow{
		{TestCase: "Test1", Start: stageT0, End: stageT0.Add(time.Second)},
		{TestCase: "Test2", Start: stageT0.Add(4 * time.Second), End: stageT0.Add(6 * time.Second)},
		{TestCase: "Test3", Start: stageT0.Add(8 * time.Second), End: stageT0.Add(10 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	scopeBegin(t, a, "Test1")
	got := heldCall(t, p.mm, "/late")
	scopeEnd(t, a, "Test1")
	scopeBegin(t, a, "Test2")
	scopeEnd(t, a, "Test2")
	notYet(t, got, "Test3's answer at the end of Test2, the last test that ran")
	if _, err := a.GetMockErrors(context.Background()); err != nil {
		t.Fatal(err)
	}
	notYet(t, got, "Test3's answer at a test's read")
	if _, err := a.GetMockErrors(models.WithSetEndRead(context.Background())); err != nil {
		t.Fatal(err)
	}
	written(t, got, "mock-late", "at the set's end read")
}
