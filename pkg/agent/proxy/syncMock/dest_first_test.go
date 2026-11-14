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

// appOf names the app a mock goes to by its "app" metadata, "" when it has
// none: a node's resolver that cannot tell it.
func appOf(mk *models.Mock) string { return mk.Spec.Metadata["app"] }

// appStartupAt is a start-up mock (IsStartup) no test owns, requested at at
// on conn, that goes to app's recording ("" for one the node cannot tell).
func appStartupAt(conn, app string, at time.Time) *models.Mock {
	mk := startupAt(conn, at)
	if app != "" {
		if mk.Spec.Metadata == nil {
			mk.Spec.Metadata = map[string]string{}
		}
		mk.Spec.Metadata["app"] = app
	}
	return mk
}

// nodeManagers is a node's default manager, whose mocks go to the apps'
// recordings (SetDestinationOf), to a client that deletes the test cases it
// revokes, with its logs, and apps A's and B's own managers.
func nodeManagers(t *testing.T) (node, appA, appB *SyncMockManager, logs *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	node, appA, appB = New(zap.New(core)), New(nil), New(nil)
	node.SetRevokeCapable(true)
	node.SetDestinationOf(appOf, nil)
	for _, m := range []*SyncMockManager{node, appA, appB} {
		m.SetOutputChannel(make(chan *models.Mock, 1))
	}
	return node, appA, appB, logs
}

// sendTo has the stream send app's test case named name over [start, end],
// through app's own manager and the node's, and fails the row if it was
// left out.
func sendTo(t *testing.T, node, own *SyncMockManager, app, name string, start, end time.Time) {
	t.Helper()
	if !SendTestCaseOf([]*SyncMockManager{own, node}, app, name, start, end, nil) {
		t.Fatalf("setup: %s's test case %s was left out", app, name)
	}
}

// A node's default manager takes the boot-time mocks of every app whose
// connections it cannot place yet, and notes every app's test cases. Each
// app's recording has its own first test case, which its replay serves its
// start-up tier from: app B's boot read, requested after app A's first test
// case and before B's own, is one any of B's tests may need. Its loss leaves
// out B's test cases from its request, with the shared loss's ERROR, though
// A's first test case came before it.
func TestAStartUpMockLostBeforeItsOwnAppsFirstTestCaseIsSharedThoughAnotherAppsCameFirst(t *testing.T) {
	node, appA, appB, logs := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0, t0.Add(time.Second))
	boot := appStartupAt("b-conn", "B", t0.Add(10*time.Second))
	if !node.servedToAnyTest(boot, "") {
		t.Fatal("B's boot read, before B's first test case, counts as a test's: A's first test case was read as B's")
	}
	node.dropOut(boot, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if n := leftOutLines(logs); n != 1 {
		t.Fatalf("%d shared-loss ERRORs for B's lost boot read, want 1", n)
	}
	if SendTestCaseOf([]*SyncMockManager{appB, node}, "B", "b-1", t0.Add(20*time.Second), t0.Add(21*time.Second), nil) {
		t.Fatal("B's first test case, recorded after B's lost boot read, was kept")
	}
}

// Each app's first test case is its own: a start-up mock of A's requested
// after A's first test case is A's test's, while B has sent none (B's first
// is not A's), and once B has sent one, a start-up mock of B's requested
// after it is B's test's too. Neither loss leaves out test cases as shared.
func TestAStartUpMockAfterItsOwnAppsFirstTestCaseIsATestsWhileAnotherAppHasSentNone(t *testing.T) {
	node, appA, appB, logs := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0, t0.Add(time.Second))
	a := appStartupAt("a-conn", "A", t0.Add(5*time.Second))
	if node.servedToAnyTest(a, "") {
		t.Fatal("A's start-up mock after A's first test case counts as one any test may need while B has sent none")
	}
	node.dropOut(a, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	sendTo(t, node, appB, "B", "b-1", t0.Add(10*time.Second), t0.Add(11*time.Second))
	b := appStartupAt("b-conn", "B", t0.Add(15*time.Second))
	if node.servedToAnyTest(b, "") {
		t.Fatal("B's start-up mock after B's first test case counts as one any test may need")
	}
	node.dropOut(b, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if n := leftOutLines(logs); n != 0 {
		t.Fatalf("%d shared-loss ERRORs for start-up mocks lost after their own app's first test case, want 0", n)
	}
	sendTo(t, node, appB, "B", "b-2", t0.Add(20*time.Second), t0.Add(21*time.Second))
}

// A start-up mock whose app the node cannot tell may be any app's, a new
// one's too, whose first test case is still to come: it counts as one any
// test may need, after every app's first test case, and after one sent with
// no app named.
func TestAStartUpMockOfAnAppTheNodeCannotTellIsOneAnyTestMayNeed(t *testing.T) {
	node, appA, appB, _ := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0, t0.Add(time.Second))
	sendTo(t, node, appB, "B", "b-1", t0.Add(2*time.Second), t0.Add(3*time.Second))
	if !node.SendingTestCase("no-app", t0.Add(4*time.Second), t0.Add(5*time.Second)) {
		t.Fatal("setup: a test case sent with no app named was left out")
	}
	if !node.servedToAnyTest(appStartupAt("x-conn", "", t0.Add(10*time.Second)), "") {
		t.Fatal("a start-up mock of an app the node cannot tell counts as a test's")
	}
}

// A manager whose mocks go to one recording (no SetDestinationOf), as an
// app's own does, notes a test case sent to a named app as that recording's:
// a start-up mock requested after it is a test's.
func TestAManagerOfOneRecordingNotesATestCaseSentToANamedAppAsItsOwn(t *testing.T) {
	own := New(nil)
	own.SetOutputChannel(make(chan *models.Mock, 1))
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, nil, own, "B", "b-1", t0, t0.Add(time.Second))
	if own.servedToAnyTest(appStartupAt("b-conn", "B", t0.Add(5*time.Second)), "") {
		t.Fatal("a start-up mock after the recording's first test case, sent to a named app, counts as one any test may need")
	}
	if f := own.firstSentOf(""); f == nil || !f.Equal(t0) {
		t.Fatalf("the recording's first test case is %v, want %v", f, t0)
	}
}

// pastWindow has m signal its first request and resolve
// models.StartupMockTestCaseWindow kept test cases from from on, each an
// empty window: the start-up window of the recording its own requests go
// to is past.
func pastWindow(m *SyncMockManager, from time.Time) {
	m.SetFirstRequestSignaled()
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		at := from.Add(time.Duration(i) * time.Millisecond)
		m.ResolveRange(at, at, fmt.Sprintf("test-%d", i), true, false)
	}
}

// appMockAt is a test's mock no test owns, not tagged, requested at at on
// conn, that goes to app's recording ("" for one the node cannot tell).
func appMockAt(conn, app string, at time.Time) *models.Mock {
	mk := addMockTo(conn, false)
	mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	if app != "" {
		mk.Spec.Metadata = map[string]string{"app": app}
	}
	return mk
}

// oneRecording is a manager whose own requests and mocks all go to one
// recording: one with no SetDestinationOf, and a node's manager that names
// that recording for both, as a DaemonSet's does when one app records.
var oneRecording = []struct {
	name string
	set  func(*SyncMockManager)
}{
	{"one recording's", func(*SyncMockManager) {}},
	{"the node's, of one recording", func(m *SyncMockManager) {
		m.SetDestinationOf(func(*models.Mock) string { return "app" }, func() string { return "app" })
	}},
}

// A node's manager whose own requests and mocks all go to one recording tags
// a mock as a manager of one recording does: past the start-up window, a
// static-dedup duplicate's mock is not a start-up one, so the duplicate's
// resolve prunes it, and does not hand it on with no owner.
func TestANodeManagerOfOneRecordingPrunesADuplicatesMockPastTheWindow(t *testing.T) {
	for _, tc := range oneRecording {
		t.Run(tc.name, func(t *testing.T) {
			out := make(chan *models.Mock, 4)
			m := New(nil)
			m.SetOutputChannel(out)
			tc.set(m)
			now := time.Now()
			pastWindow(m, now.Add(-time.Second))
			mk := startupHTTPMock(now.Add(-20 * time.Millisecond))
			m.AddMock(mk)
			if mk.TestModeInfo.IsStartup {
				t.Fatal("a mock taken past the recording's start-up window is tagged start-up")
			}
			m.ResolveRange(now.Add(-50*time.Millisecond), now, "test-dup", false, false)
			select {
			case got := <-out:
				t.Fatalf("a static-dedup duplicate's mock past the start-up window was handed on (start-up %v), not pruned", got.TestModeInfo.IsStartup)
			default:
			}
			m.mu.Lock()
			held := len(m.buffer)
			m.mu.Unlock()
			if held != 0 {
				t.Fatalf("the duplicate's mock is still buffered (%d), not pruned", held)
			}
		})
	}
}

// The same manager reaps at a duplicate's prune horizon
// (DeleteMocksStrictlyBefore) as a manager of one recording does: a mock
// past the start-up window requested before it is pruned, not handed on.
func TestANodeManagerOfOneRecordingReapsAtThePruneHorizon(t *testing.T) {
	for _, tc := range oneRecording {
		t.Run(tc.name, func(t *testing.T) {
			out := make(chan *models.Mock, 4)
			m := New(nil)
			m.SetOutputChannel(out)
			tc.set(m)
			now := time.Now()
			pastWindow(m, now.Add(-time.Second))
			m.AddMock(startupHTTPMock(now.Add(-20 * time.Millisecond)))
			m.DeleteMocksStrictlyBefore(now)
			select {
			case got := <-out:
				t.Fatalf("a mock behind a duplicate's prune horizon was handed on (start-up %v), not pruned", got.TestModeInfo.IsStartup)
			default:
			}
			m.mu.Lock()
			held := len(m.buffer)
			m.mu.Unlock()
			if held != 0 {
				t.Fatalf("the mock behind the prune horizon is still buffered (%d)", held)
			}
		})
	}
}

// Until the recording's first request is signalled, a manager of one
// recording tags every mock it takes a start-up one, however many test cases
// it has resolved: a capture that resolves kept windows and never signals it
// leaves every mock in the start-up tier. Once it is signalled, past the
// window, a mock is not tagged. A node's manager of one recording tags them
// the same.
func TestAMockIsStartUpUntilTheFirstRequestIsSignalledThoughTheWindowIsPast(t *testing.T) {
	for _, tc := range oneRecording {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil)
			m.SetOutputChannel(make(chan *models.Mock, 4))
			tc.set(m)
			now := time.Now()
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				at := now.Add(-time.Second + time.Duration(i)*time.Millisecond)
				m.ResolveRange(at, at, fmt.Sprintf("test-%d", i), true, false)
			}
			mk := appMockAt("c", "", now)
			m.AddMock(mk)
			if !mk.TestModeInfo.IsStartup {
				t.Fatalf("%d test cases resolved and no first request signalled: the mock is not tagged start-up", models.StartupMockTestCaseWindow)
			}
			m.SetFirstRequestSignaled()
			mk = appMockAt("c", "", now)
			m.AddMock(mk)
			if mk.TestModeInfo.IsStartup {
				t.Fatal("past the window with the first request signalled, the mock is tagged start-up")
			}
		})
	}
}

// A node's manager counts the start-up window of the recording its own
// requests go to (app A's): past it, A's mock is not a start-up one, while
// app B's, whose requests it does not resolve, is, and still counts as one
// any test may need before B's first test case; so is a mock whose app it
// cannot tell.
func TestANodeManagerTagsAMockByItsOwnRecordingsCount(t *testing.T) {
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 4))
	m.SetDestinationOf(appOf, func() string { return "A" })
	now := time.Now()
	pastWindow(m, now.Add(-time.Second))
	for _, tc := range []struct {
		app  string
		want bool
	}{{"A", false}, {"B", true}, {"", true}} {
		mk := appMockAt("c-"+tc.app, tc.app, now)
		m.AddMock(mk)
		if mk.TestModeInfo.IsStartup != tc.want {
			t.Fatalf("app %q's mock past A's start-up window is tagged start-up %v, want %v", tc.app, mk.TestModeInfo.IsStartup, tc.want)
		}
		if served := m.servedToAnyTest(mk, ""); served != tc.want {
			t.Fatalf("app %q's mock counts as one any test may need: %v, want %v", tc.app, served, tc.want)
		}
	}
}

// A node's manager that cannot tell the recording its own requests go to (a
// nil namer, or one that cannot tell, as with several apps on the node)
// counts their test cases toward none it can name: past the window by its
// own count, a mock of a known app and one whose app it cannot tell both
// stay start-up ones.
func TestANodeManagerThatCannotTellItsOwnRecordingKeepsTaggingStartUp(t *testing.T) {
	for _, own := range []func() string{nil, func() string { return "" }} {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 4))
		m.SetDestinationOf(appOf, own)
		now := time.Now()
		pastWindow(m, now.Add(-time.Second))
		for _, app := range []string{"B", ""} {
			mk := appMockAt("c-"+app, app, now)
			m.AddMock(mk)
			if !mk.TestModeInfo.IsStartup {
				t.Fatalf("own namer %v: app %q's mock is not tagged start-up, though no recording's count it can name is past the window", own != nil, app)
			}
		}
	}
}

// Each recording's count is its own, and a record session clears them all:
// on a node's manager of one recording at a time, the next recording starts
// its start-up window afresh, and after a new session a mock of a recording
// from before it is a start-up one again.
func TestANodeManagersCountsArePerRecordingAndClearedBySession(t *testing.T) {
	key := "r1"
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 8))
	m.SetDestinationOf(func(*models.Mock) string { return key }, func() string { return key })
	now := time.Now()
	tagged := func(what string, want bool) {
		t.Helper()
		mk := appMockAt("c", "", now)
		m.AddMock(mk)
		if mk.TestModeInfo.IsStartup != want {
			t.Fatalf("%s: tagged start-up %v, want %v", what, mk.TestModeInfo.IsStartup, want)
		}
	}
	pastWindow(m, now.Add(-3*time.Second))
	tagged("r1 past its window", false)
	key = "r2"
	tagged("r2 before its own window passed", true)
	pastWindow(m, now.Add(-2*time.Second))
	tagged("r2 past its window", false)
	m.StartRecordSession()
	pastWindow(m, now.Add(-time.Second))
	key = "r1"
	tagged("r1 after a new session, whose requests went to r2", true)
}

// A recording's start-up window is counted in the test cases it keeps: on a
// node's manager whose first recording is past its window, a second
// recording that has resolved only static-dedup duplicates (twice the
// window's count of them) is still inside its own window, so its mock stays
// a start-up one, and dedup pruning and the stale cutoff leave it alone.
func TestDuplicatesDoNotCountTowardARecordingsStartUpWindow(t *testing.T) {
	key := "r1"
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 64))
	m.SetDestinationOf(func(*models.Mock) string { return key }, func() string { return key })
	now := time.Now()
	pastWindow(m, now.Add(-3*time.Second))
	key = "r2"
	for i := 0; i < 2*models.StartupMockTestCaseWindow; i++ {
		at := now.Add(-2*time.Second + time.Duration(i)*time.Millisecond)
		m.ResolveRange(at, at, fmt.Sprintf("dup-%d", i), false, false)
	}
	mk := appMockAt("c", "", now)
	m.AddMock(mk)
	if !mk.TestModeInfo.IsStartup {
		t.Fatal("r2 resolved only duplicates, yet its mock is not tagged start-up: duplicates counted toward its window")
	}
}

// On a node's manager, a re-read's ERROR names the first test case of the
// re-read mock's own app as it now stands, not that of the manager's other
// recordings (it has none under "").
func TestAReReadOnANodeNamesItsOwnAppsFirstNow(t *testing.T) {
	node, _, appB, logs := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appB, "B", "b-1", t0, t0.Add(time.Second))
	sendTo(t, node, appB, "B", "b-2", t0.Add(10*time.Second), t0.Add(11*time.Second))
	read := appStartupAt("b-conn", "B", t0.Add(5*time.Second))
	node.dropOut(read, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if n := len(rereadLines(logs)); n != 0 {
		t.Fatalf("setup: B's read after B's first test case was re-read %d times", n)
	}
	node.RevokeTestCase("b-1")
	got := rereadLines(logs)
	want := t0.Add(10 * time.Second).Format(time.RFC3339Nano)
	if len(got) != 1 || got[0]["first_test_case_now"] != want {
		t.Fatalf("the re-read said %v, want it once naming B's first now, %s", got, want)
	}
}

// A revoke that moves one app's first test case re-reads only a start-up
// mock of that app's lost as a test's: A's first test case taken out leaves
// B's lost read, after B's first, a test's. B's first taken out re-reads it,
// once, and the node's first test case of A's is gone.
func TestARevokeOfOneAppsFirstTestCaseReReadsOnlyThatAppsStartUpLoss(t *testing.T) {
	node, appA, appB, logs := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0, t0.Add(time.Second))
	sendTo(t, node, appB, "B", "b-1", t0.Add(2*time.Second), t0.Add(3*time.Second))
	read := appStartupAt("b-conn", "B", t0.Add(5*time.Second))
	node.dropOut(read, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if n := leftOutLines(logs); n != 0 {
		t.Fatalf("setup: B's read after B's first test case said %d shared losses", n)
	}
	node.RevokeTestCase("a-1")
	if f := node.firstSentOf("A"); f != nil {
		t.Fatalf("A's first test case is %v once its only one was revoked", f)
	}
	if n := len(rereadLines(logs)); n != 0 {
		t.Fatalf("revoking A's first test case re-read %d start-up losses; B's first did not move", n)
	}
	node.RevokeTestCase("b-1")
	got := rereadLines(logs)
	if len(got) != 1 || got[0]["left_out_from"] != read.Spec.ReqTimestampMock.Format(time.RFC3339Nano) {
		t.Fatalf("revoking B's first test case re-read %v, want B's read once", got)
	}
}

// One revoke can move two apps' first test cases past their lost start-up
// mocks: the earliest of them is re-read, so the span reaches the other's
// request too.
func TestARevokeMovingTwoAppsFirstsReReadsTheEarliestLoss(t *testing.T) {
	node, appA, appB, logs := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0.Add(10*time.Second), t0.Add(11*time.Second))
	sendTo(t, node, appB, "B", "b-1", t0.Add(14*time.Second), t0.Add(15*time.Second))
	for _, mk := range []*models.Mock{appStartupAt("a-conn", "A", t0.Add(12*time.Second)), appStartupAt("b-conn", "B", t0.Add(16*time.Second))} {
		node.dropOut(mk, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	}
	// A's call no test owns, over both first test cases, lost: it revokes
	// both.
	call := callAt("a-conn2", t0.Add(10500*time.Millisecond))
	call.Spec.ResTimestampMock = t0.Add(14500 * time.Millisecond)
	call.Spec.Metadata = map[string]string{"app": "A"}
	node.dropOut(call, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	got := rereadLines(logs)
	if len(got) != 1 || got[0]["left_out_from"] != t0.Add(12*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("the revoke of both first test cases re-read %v, want A's read, the earliest, once", got)
	}
	if SendTestCaseOf([]*SyncMockManager{appB, node}, "B", "b-2", t0.Add(14600*time.Millisecond), t0.Add(15500*time.Millisecond), nil) {
		t.Fatal("a test case between the two re-read losses was kept")
	}
}

// The per-app lists add up: a span's revokes count every app's, a loss says
// the latest end of a test case any app's list cannot name, and a revoke by
// name finds its test case in its own app's list, whatever order the apps
// are walked in.
func TestTheAppsSentListsAddUp(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		var s sentByDest
		for d := 0; d < 8; d++ {
			s.of(fmt.Sprint("app-", d)).add(fmt.Sprint("tc-", d), t0.Add(time.Duration(d)*time.Second), t0.Add(time.Duration(d)*time.Second+500*time.Millisecond))
		}
		s.of("app-3").add("", t0.Add(30*time.Second), t0.Add(40*time.Second))
		if got := s.unnamedReaching(t0.Add(35 * time.Second)); !got.Equal(t0.Add(40 * time.Second)) {
			t.Fatalf("a loss from t0+35s may reach an unnamed test case through %v, want t0+40s", got)
		}
		if n := s.revokeReaching(t0.Add(2*time.Second), time.Time{}, true, func(string) {}); n != 6 {
			t.Fatalf("a span from t0+2s revoked %d test cases of the apps' lists, want 6", n)
		}
		// A revoke by name takes the test case out of its own app's list,
		// whichever list is walked first.
		for d := 0; d < 2; d++ {
			app := fmt.Sprint("app-", d)
			s.forget(fmt.Sprint("tc-", d), true)
			if first, ok, _ := s.first(app); ok {
				t.Fatalf("%s's only test case revoked by name, its first is still %v", app, first)
			}
		}
	}
}

// A start-up mock lost as a test's of its app, whose app the node can no
// longer tell by the time its loss is kept, may be any app's: it is re-read
// at once as one any test may need, with the ERROR.
func TestAStartUpLossWhoseAppTheNodeLostTrackOfIsReReadAtOnce(t *testing.T) {
	node, _, appB, logs := nodeManagers(t)
	calls := 0
	node.SetDestinationOf(func(mk *models.Mock) string {
		calls++
		if calls == 1 {
			return "B"
		}
		return ""
	}, nil)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appB, "B", "b-1", t0, t0.Add(time.Second))
	read := appStartupAt("b-conn", "B", t0.Add(5*time.Second))
	node.dropOut(read, "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if calls < 2 {
		t.Fatalf("setup: the node named the lost read's app %d times, want it named as B, then not told", calls)
	}
	got := rereadLines(logs)
	if len(got) != 1 || got[0]["left_out_from"] != read.Spec.ReqTimestampMock.Format(time.RFC3339Nano) {
		t.Fatalf("the read whose app the node lost track of re-read %v, want it once", got)
	}
}

// A recording that is over is let go (ForgetDestination): the node keeps no
// list for it, and a start-up mock that still names it finds no first test
// case, so it counts as one any test may need (fail closed), while another
// recording's first stands. Its losses re-read on a revoke go with it.
func TestAForgottenRecordingKeepsNoFirstAndItsStartUpMocksCountAsShared(t *testing.T) {
	node, appA, appB, _ := nodeManagers(t)
	t0 := time.Now().Add(-time.Hour)
	sendTo(t, node, appA, "A", "a-1", t0, t0.Add(time.Second))
	sendTo(t, node, appB, "B", "b-1", t0, t0.Add(time.Second))
	node.dropOut(appStartupAt("a-boot", "A", t0.Add(5*time.Second)), "", node.revokeEpoch.Load(), string(MockDroppedOutputFull))
	node.ForgetDestination("A")
	node.sharedLossMu.Lock()
	_, listKept := node.sentTCs.lists["A"]
	_, lossKept := node.startupLost["A"]
	node.sharedLossMu.Unlock()
	if listKept || lossKept {
		t.Fatalf("the forgotten recording's state is kept: sent list %v, start-up loss %v", listKept, lossKept)
	}
	if node.firstSentOf("A") != nil {
		t.Fatal("the forgotten recording still has a first test case")
	}
	if !node.servedToAnyTest(appStartupAt("a-conn", "A", t0.Add(5*time.Second)), "") {
		t.Fatal("a start-up mock naming a forgotten recording, after its old first, counts as a test's")
	}
	if node.firstSentOf("B") == nil || node.servedToAnyTest(appStartupAt("b-conn", "B", t0.Add(5*time.Second)), "") {
		t.Fatal("forgetting A's recording moved B's first test case")
	}
}

// A recording let go (ForgetDestination) takes its count of test cases with
// it: a mock that still names it is a start-up one again (fail closed),
// while another recording's count stands.
func TestAForgottenRecordingsMocksAreTaggedStartUpAgain(t *testing.T) {
	own := "A"
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 8))
	m.SetDestinationOf(appOf, func() string { return own })
	now := time.Now()
	pastWindow(m, now.Add(-2*time.Second))
	own = "B"
	pastWindow(m, now.Add(-time.Second))
	m.ForgetDestination("A")
	for _, tc := range []struct {
		app  string
		want bool
	}{{"A", true}, {"B", false}} {
		mk := appMockAt("c-"+tc.app, tc.app, now)
		m.AddMock(mk)
		if mk.TestModeInfo.IsStartup != tc.want {
			t.Fatalf("after A was let go, app %s's mock is tagged start-up %v, want %v", tc.app, mk.TestModeInfo.IsStartup, tc.want)
		}
	}
}
