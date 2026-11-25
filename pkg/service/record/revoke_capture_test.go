package record

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A revoke is about the capture it is about, never "the latest capture under
// its name". Test cases, mocks (with the agent's revokes) and mappings reach
// the recorder on three streams nothing orders, so a revoke can come before
// its capture, behind an earlier capture under the same name.
//   - The agent's revoke kills the name: after its drop the agent streams no
//     capture under it, so one that reaches the recorder after the revoke may
//     own the dropped mock.
//   - The recorder's revoke of a mock it could not save is about the capture
//     whose window holds the mock's request time, come or to come: an earlier
//     healthy capture is kept, and so is a later one with its mocks intact.
//     Where no capture can be told, the name is killed.

// waitTestDeleted waits, the recording still going, for name to be deleted.
func waitTestDeleted(t *testing.T, db *recTestDB, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		db.mu.Lock()
		gone := slices.Contains(db.deleted, name)
		db.mu.Unlock()
		if gone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked test case %q was not deleted while the recording went on", name)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitTestSaved waits for name to be saved n times.
func waitTestSaved(t *testing.T, db *recTestDB, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		db.mu.Lock()
		got := timesIn(db.inserted, name)
		db.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("test case %q was saved %d times, want %d", name, got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitMockStored waits for the recorder to save the mock name: the mock loop
// has taken every frame sent before it.
func waitMockStored(t *testing.T, db *recMockDB, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		db.mu.Lock()
		saved := slices.Contains(db.inserted, name)
		db.mu.Unlock()
		if saved {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the recorder never saved mock %q", name)
		}
		time.Sleep(time.Millisecond)
	}
}

func timesIn(names []string, name string) int {
	n := 0
	for _, s := range names {
		if s == name {
			n++
		}
	}
	return n
}

// sendCapture hands the recorder tc on the test-case stream, without waiting
// for it to be saved.
func sendCapture(t *testing.T, run *recRun, tc *models.TestCase) {
	t.Helper()
	select {
	case run.f.incoming <- tc:
	case <-time.After(hangGuard):
		t.Fatalf("the recorder never took test case %q", tc.Name)
	}
}

// capture is the test case of a request named name, URL which, whose window
// is [from, to] (winAt).
func capture(name, which string, from, to int) *models.TestCase {
	tc := winTest(name, from, to)
	tc.HTTPReq.URL = "http://localhost/" + which
	return tc
}

// holdingStore is the recording's test store as the YAML store behaves: a
// save under a name replaces what the name held, and a delete of a name
// deletes what it holds now. held is which capture (its request URL) each name
// holds.
type holdingStore struct {
	*recTestDB
	smu  sync.Mutex
	held map[string]string
}

func (d *holdingStore) InsertTestCase(ctx context.Context, tc *models.TestCase, set string, log bool) error {
	err := d.recTestDB.InsertTestCase(ctx, tc, set, log)
	if err == nil {
		d.smu.Lock()
		d.held[tc.Name] = tc.HTTPReq.URL
		d.smu.Unlock()
	}
	return err
}

func (d *holdingStore) DeleteTests(ctx context.Context, set string, ids []string) error {
	err := d.recTestDB.DeleteTests(ctx, set, ids)
	d.smu.Lock()
	for _, id := range ids {
		delete(d.held, id)
	}
	d.smu.Unlock()
	return err
}

func (d *holdingStore) holds(name string) string {
	d.smu.Lock()
	defer d.smu.Unlock()
	return d.held[name]
}

func withHoldingStore(d *holdingStore) func(*Recorder) {
	return func(r *Recorder) {
		d.recTestDB = r.testDB.(*recTestDB)
		d.held = map[string]string{}
		r.testDB = d
	}
}

// The recorder could not save a mock of the first capture of "login" (its
// request time lies in that capture's window), so that capture is revoked and
// deleted. The same request comes again: its mapping names its mock, which
// the recorder is still saving. That entry waits for the mock like any other:
// the later capture is kept, with the mock its mapping names.
func TestALaterCaptureUnderARevokedNameKeepsTheMockItsMappingNames(t *testing.T) {
	release := make(chan struct{})
	mdb := &recMockDB{unencodable: map[string]bool{"bad-1": true}, gate: map[string]chan struct{}{"mock-5": release}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, winMock("bad-1", models.HTTP, "", 1050))
	run.mapping(t, "login", "mock-1", "bad-1")
	waitTestDeleted(t, run.testDB, "login")
	run.frame(t, winMock("mock-5", models.HTTP, "", 2030))
	run.mapping(t, "login", "mock-5")
	run.until(t, "the later capture's entry waits for its mock", holds("login", "mock-5"))
	close(release)
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	waitTestSaved(t, run.testDB, "login", 2)
	run.stop(t)
	require.Equal(t, 1, timesIn(run.deleted(), "login"), "the later capture was deleted too")
	require.Contains(t, persistedFor(run.mapDB, "login"), "mock-5", "the later capture is kept without the mock its own mapping named: it replays without it")
}

// Two mocks of one capture are lost to the recorder, each known by its own
// mapping delta: two revokes of that one capture. The second takes nothing
// more, so the later capture of the request, with its mocks intact, is saved.
func TestTwoLossesOfOneCaptureKeepNoLaterCaptureOut(t *testing.T) {
	mdb := &recMockDB{unencodable: map[string]bool{"bad-1": true, "bad-2": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, winMock("bad-1", models.HTTP, "", 1050))
	run.mapping(t, "login", "mock-1", "bad-1")
	waitTestDeleted(t, run.testDB, "login")
	run.frame(t, winMock("bad-2", models.HTTP, "", 1080))
	run.frame(t, winMock("mock-3", models.HTTP, "", 1090))
	waitMockStored(t, mdb, "mock-3")
	run.mapping(t, "login", "bad-2") // a late-binned mock of the same capture, also lost
	// The consumer takes mappings in order: once the next one's entry is
	// held, it has applied the revoke.
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	run.frame(t, winMock("mock-4", models.HTTP, "", 2030))
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	run.test(t, "test-9")
	run.stop(t)
	run.testDB.mu.Lock()
	saved := timesIn(run.testDB.inserted, "login")
	run.testDB.mu.Unlock()
	require.Equal(t, 2, saved, "a second revoke of the one capture whose mocks were lost kept the request's later capture out")
}

// The first capture's second lost mock comes after the later capture's
// mapping: that revoke is the first capture's, and lets go of no entry the
// later capture's mapping holds.
func TestAnotherLossOfARevokedCaptureLetsNoLaterCapturesEntryGo(t *testing.T) {
	release := make(chan struct{})
	mdb := &recMockDB{unencodable: map[string]bool{"bad-1": true, "bad-2": true}, gate: map[string]chan struct{}{"mock-5": release}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, winMock("bad-1", models.HTTP, "", 1050))
	run.mapping(t, "login", "mock-1", "bad-1")
	waitTestDeleted(t, run.testDB, "login")
	run.frame(t, winMock("bad-2", models.HTTP, "", 1080))
	run.frame(t, winMock("mock-3", models.HTTP, "", 1090))
	waitMockStored(t, mdb, "mock-3")
	run.frame(t, winMock("mock-5", models.HTTP, "", 2030)) // the later capture's, held in the store's write
	run.mapping(t, "login", "mock-5")
	run.until(t, "the later capture's entry waits for its mock", holds("login", "mock-5"))
	run.mapping(t, "login", "bad-2")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	require.True(t, holds("login", "mock-5")(run.waits), "another loss of the revoked capture let the later capture's entry go")
	close(release)
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	waitTestSaved(t, run.testDB, "login", 2)
	run.stop(t)
	require.Contains(t, persistedFor(run.mapDB, "login"), "mock-5")
}

// A later capture kept under a revoked name, whose mapping names a mock the
// agent never sent, is counted as a test recorded with a short mock pool:
// the set keeps it, short.
func TestALaterCaptureKeptUnderARevokedNameIsCountedShort(t *testing.T) {
	mdb := &recMockDB{unencodable: map[string]bool{"bad-1": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, winMock("bad-1", models.HTTP, "", 1050))
	run.mapping(t, "login", "mock-1", "bad-1")
	waitTestDeleted(t, run.testDB, "login")
	run.frame(t, winMock("mock-7", models.HTTP, "", 2030))
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	waitTestSaved(t, run.testDB, "login", 2)
	run.mapping(t, "login", "mock-7", "never-sent")
	run.until(t, "the later capture's entry waits for its mock", holds("login", "never-sent"))
	suite := run.stop(t)
	require.EqualValues(t, 1, suite["tests-short-pool"], "the later capture the set keeps short is not counted: %v", suite)
	require.EqualValues(t, 1, suite["mocks-uncorrelated"], "%v", suite)
}

// lostMockAt is an HTTP mock whose request went at at, to the nanosecond.
func lostMockAt(name string, at time.Time) *models.Mock {
	return &models.Mock{Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// recorderRevokeAhead: the recorder cannot save a mock of the request's
// second capture, requested at lost. Its mapping (sent when the request is
// decided, before its test case is streamed) reaches the mapping consumer
// while the second capture, whose window is [2000, 2100], is still on the
// test stream, behind the first capture, saved with all its mocks. The mock's
// request time lies in the second capture's window: that capture is not saved
// when it comes, and the first is kept.
func recorderRevokeAhead(t *testing.T, lost time.Time) {
	t.Helper()
	store := &holdingStore{}
	mdb := &recMockDB{unencodable: map[string]bool{"bad-2": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop(), withHoldingStore(store))
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.mapping(t, "login", "mock-1")
	run.frame(t, lostMockAt("bad-2", lost))
	run.frame(t, winMock("mock-3", models.HTTP, "", 2030))
	waitMockStored(t, mdb, "mock-3")
	run.mapping(t, "login", "mock-3", "bad-2")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	run.test(t, "test-9")
	run.stop(t)
	require.Equal(t, "http://localhost/first", store.holds("login"), "deleted: %v", run.deleted())
	require.Empty(t, run.deleted(), "the healthy earlier capture was deleted for the later one's lost mock")
	run.testDB.mu.Lock()
	defer run.testDB.mu.Unlock()
	require.Equal(t, 1, timesIn(run.testDB.inserted, "login"), "the capture whose mock was lost was saved")
}

func TestARecorderRevokeAheadOfItsCaptureTakesThatCaptureAndKeepsTheEarlierOne(t *testing.T) {
	recorderRevokeAhead(t, winAt(2050))
}

// The agent bins a mock requested at its test's first or last instant into
// that test's window (its bounds are inclusive), and so does the recorder.
func TestALostMockAtItsCapturesFirstInstantTakesThatCapture(t *testing.T) {
	recorderRevokeAhead(t, winAt(2000))
}

func TestALostMockAtItsCapturesLastInstantTakesThatCapture(t *testing.T) {
	recorderRevokeAhead(t, winAt(2100))
}

// recorderRevokeAfter: "test-5" is captured and saved with a window of
// [2000, 2100]. Then the recorder cannot save a mock the agent mapped to it,
// requested at lost. It returns what the set holds under "test-5" at the end.
func recorderRevokeAfter(t *testing.T, lost time.Time) string {
	t.Helper()
	store := &holdingStore{}
	mdb := &recMockDB{unencodable: map[string]bool{"bad-2": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop(), withHoldingStore(store))
	run.frame(t, winMock("mock-1", models.HTTP, "", 2030))
	sendCapture(t, run, capture("test-5", "only", 2000, 2100))
	waitForPersistedTestCase(t, run.testDB, "test-5")
	run.frame(t, lostMockAt("bad-2", lost))
	run.frame(t, winMock("mock-3", models.HTTP, "", 2050))
	waitMockStored(t, mdb, "mock-3")
	run.mapping(t, "test-5", "mock-1", "mock-3", "bad-2")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	sendCapture(t, run, capture("test-9", "third", 3000, 3100))
	waitForPersistedTestCase(t, run.testDB, "test-9")
	run.stop(t)
	return store.holds("test-5")
}

func TestALostMockAtTheLastInstantOfASavedCaptureDeletesIt(t *testing.T) {
	require.Empty(t, recorderRevokeAfter(t, winAt(2100)), "the set keeps a capture whose mock, requested at its window's last instant, the recorder could not save")
}

// The agent decides a mock's test on Go's comparison of its own stamps, which
// uses the monotonic reading when both carry one; the recorder gets them over
// the wire, wall clock only. A backward step of the agent host's wall clock
// during a request (an NTP or VM time-sync step; gRPC ingress stamps its
// window with a monotonic reading) leaves a mock the agent rightly mapped to
// the test just outside the window's wall-clock span. Its loss then names no
// capture by its window: none under the name is kept, as when revokes were
// applied at the recording's end.
func TestALostMockOutsideEveryWindowUnderItsNameDeletesTheNameAtTheEnd(t *testing.T) {
	require.Empty(t, recorderRevokeAfter(t, winAt(2000).Add(-time.Nanosecond)), "the set keeps a capture whose mapping named a mock the recorder could not save: it replays without it")
}

// The same, with test cases and mocks that carry no times, so the capture the
// lost mock belongs to cannot be told: the name is killed, so neither capture
// is kept, as when revokes were applied at the recording's end.
func TestARecorderRevokeAheadOfACaptureItCannotTellKeepsNoCaptureOfTheName(t *testing.T) {
	store := &holdingStore{}
	mdb := &recMockDB{unencodable: map[string]bool{"bad-2": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop(), withHoldingStore(store))
	run.mock(t, "mock-1")
	sendCapture(t, run, &models.TestCase{Name: "login", Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://localhost/first"}})
	waitForPersistedTestCase(t, run.testDB, "login")
	run.mapping(t, "login", "mock-1")
	run.mock(t, "bad-2")
	run.mock(t, "mock-3")
	waitMockStored(t, mdb, "mock-3")
	run.mapping(t, "login", "mock-3", "bad-2")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	sendCapture(t, run, &models.TestCase{Name: "login", Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://localhost/second"}})
	run.test(t, "test-9")
	run.stop(t)
	require.Empty(t, store.holds("login"), "a capture of a name whose lost mock no capture could be told by is kept")
}

// The agent dropped a mock of the request's second capture after it streamed
// it, so it revokes "login", and from then on streams no capture under the
// name. The revoke reaches the mock loop while the second capture is still on
// the test stream: the name is killed, so the saved first capture is deleted
// and the second is not saved.
func TestAnAgentsRevokeAheadOfItsCaptureKeepsNoCaptureOfTheName(t *testing.T) {
	store := &holdingStore{}
	mdb := &recMockDB{unencodable: map[string]bool{}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop(), withHoldingStore(store))
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, revokeFrame("revoke-frame", "login"))
	run.frame(t, winMock("mock-2", models.HTTP, "", 2030))
	waitMockStored(t, mdb, "mock-2") // the mock loop has applied the revoke
	sendCapture(t, run, capture("login", "second", 2000, 2100))
	run.test(t, "test-9")
	run.stop(t)
	require.Empty(t, store.holds("login"), "a capture the agent streamed before it revoked its name is kept: its dropped mock may be this capture's")
	run.testDB.mu.Lock()
	defer run.testDB.mu.Unlock()
	require.Equal(t, 1, timesIn(run.testDB.inserted, "login"), "the capture that came after the agent's revoke was saved")
}

// Under the proven switch a recording without --sync maps its tests from their
// windows at its end, leaving out the test cases deleted. The agent revoked
// two names: the captures saved under them are deleted while the recording
// goes on, the one that comes under one of them later is not saved, and none
// of them has an entry; the calls in their windows go to no other test.
func TestAProvenRecordingMapsNoCaptureUnderANameTheAgentRevoked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keepExitCode(t)
	w := startWinRecording(t, &config.Config{}, nil, nil)
	w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
	w.test(t, winTest("login", 1000, 1100))
	w.mock(t, winMock("rates-3", models.HTTP, "", 1530))
	w.test(t, winTest("logout", 1500, 1600))
	waitForPersistedTestCase(t, w.testDB, "logout")
	w.mock(t, &models.Mock{Kind: models.RevokedTests, Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": "login,logout"}}})
	waitTestDeleted(t, w.testDB, "login")
	waitTestDeleted(t, w.testDB, "logout")
	w.mock(t, winMock("rates-2", models.HTTP, "", 2030))
	w.test(t, winTest("login", 2000, 2100))
	w.mock(t, winMock("rates-4", models.HTTP, "", 3030))
	w.test(t, winTest("other", 3000, 3100))
	w.stop(t, 3, 4)
	got := w.batches()
	require.Equal(t, []map[string][]string{{"other": {"rates-4"}}}, got, "mappings written")
}

// The recorder revoked a capture, told by its lost mock's time, so the name
// stays alive and a later entry under it is held. Then the agent revokes the
// name too (a mock it dropped): the name is killed, the entry its mapping
// holds for the mock the agent dropped is let go, and a later entry under the
// name is not held.
func TestAnAgentsRevokeOfACaptureTheRecorderRevokedLetsItsEntriesGo(t *testing.T) {
	mdb := &recMockDB{unencodable: map[string]bool{"bad-1": true}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, winMock("mock-1", models.HTTP, "", 1030))
	sendCapture(t, run, capture("login", "first", 1000, 1100))
	waitForPersistedTestCase(t, run.testDB, "login")
	run.frame(t, winMock("bad-1", models.HTTP, "", 1050))
	run.mapping(t, "login", "mock-1", "bad-1")
	waitTestDeleted(t, run.testDB, "login")
	run.mapping(t, "login", "dropped-by-agent")
	run.until(t, "the entry held", heldIs(1))
	run.frame(t, revokeFrame("revoke-frame", "login"))
	run.until(t, "the agent's revoke let the entry go", heldIs(0))
	run.mapping(t, "login", "dropped-by-agent-2")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	require.Equal(t, 1, run.waits.heldCount(), "a later entry under a name the agent revoked was held")
	run.stop(t)
}

// The agent revokes a name before its capture comes: the capture is not
// saved, and no entry of its mapping is held for a mock the agent dropped,
// nor is it counted as a test recorded with a short mock pool.
func TestAnAgentsRevokeBeforeItsCaptureHoldsNoEntryOfIt(t *testing.T) {
	mdb := &recMockDB{unencodable: map[string]bool{}}
	run := startRecording(t, mdb, BaseRecordHooks{}, zap.NewNop())
	run.frame(t, revokeFrame("revoke-frame", "login"))
	run.mock(t, "mock-1")
	waitMockStored(t, mdb, "mock-1") // the mock loop has applied the revoke
	run.mapping(t, "login", "mock-1", "dropped-by-agent")
	run.mapping(t, "other", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other", "never-sent"))
	require.Equal(t, 1, run.waits.heldCount(), "an entry of a capture the agent revoked before it came was held")
	run.offer(t, "login")
	run.test(t, "test-2")
	// The capture came, and was not saved: its mapping's later delta is its
	// own still.
	run.mapping(t, "login", "dropped-by-agent-2")
	run.mapping(t, "other-2", "never-sent")
	run.until(t, "the next mapping's entry held", holds("other-2", "never-sent"))
	require.Equal(t, 2, run.waits.heldCount(), "a later entry of a capture the agent revoked before it came was held")
	suite := run.stop(t)
	require.EqualValues(t, 2, suite["tests-short-pool"], "only other and other-2 ship short; the capture never saved is not counted: %v", suite)
	run.testDB.mu.Lock()
	defer run.testDB.mu.Unlock()
	require.NotContains(t, run.testDB.inserted, "login", "a capture the agent revoked before it came was saved")
}
