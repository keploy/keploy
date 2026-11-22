package record

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test case one of whose mocks is lost is never kept: deleted as soon as the
// loss is known, while the recording goes on, and never saved when the loss is
// known first. Applied only at the recording's end, every auto-replay round in
// between replayed the case, and the upload cycle sent it, as a failure that
// was the recording's own loss.

// liveRecording is a recording, started, fed through its fake agent streams.
type liveRecording struct {
	t      *testing.T
	f      *fakeInstr
	db     *recTestDB
	mdb    *recMockDB
	tele   *recTelemetry
	cancel context.CancelFunc
	done   chan error
}

func startLiveRecording(t *testing.T) *liveRecording {
	t.Helper()
	return startLiveRecordingWith(t, &recTestDB{})
}

func startLiveRecordingWith(t *testing.T, db *recTestDB) *liveRecording {
	t.Helper()
	return startLiveRecordingOn(t, db, db)
}

// startLiveRecordingOn starts a recording saving to store, whose saved test
// cases db records.
func startLiveRecordingOn(t *testing.T, store TestDB, db *recTestDB) *liveRecording {
	t.Helper()
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	lr := &liveRecording{t: t, f: f, db: db, mdb: &recMockDB{unencodable: map[string]bool{}}, tele: &recTelemetry{}, done: make(chan error, 1)}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          store,
		mockDB:          lr.mdb,
		mappingDb:       &recMappingDB{},
		telemetry:       lr.tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}
	var ctx context.Context
	ctx, lr.cancel = context.WithCancel(context.Background())
	go func() { lr.done <- r.Start(ctx) }()
	return lr
}

func (lr *liveRecording) mock(m *models.Mock) {
	lr.t.Helper()
	select {
	case lr.f.outgoing <- m:
	case <-time.After(10 * time.Second):
		lr.t.Fatalf("the recorder never took mock %q", m.Name)
	}
}

func (lr *liveRecording) testCase(name string) {
	lr.t.Helper()
	lr.sendTestCase(&models.TestCase{Name: name, Kind: models.HTTP})
}

func (lr *liveRecording) sendTestCase(tc *models.TestCase) {
	lr.t.Helper()
	select {
	case lr.f.incoming <- tc:
	case <-time.After(10 * time.Second):
		lr.t.Fatalf("the recorder never took test case %q", tc.Name)
	}
}

// insertedCount is how many times name was saved.
func (lr *liveRecording) insertedCount(name string) int {
	lr.db.mu.Lock()
	defer lr.db.mu.Unlock()
	n := 0
	for _, d := range lr.db.inserted {
		if d == name {
			n++
		}
	}
	return n
}

func (lr *liveRecording) mapping(m models.TestMockMapping) {
	lr.t.Helper()
	select {
	case lr.f.mappings <- m:
	case <-time.After(10 * time.Second):
		lr.t.Fatalf("the recorder never took the mapping of %q", m.TestName)
	}
}

func liveRevokeFrame(names string) *models.Mock {
	return &models.Mock{Version: models.GetVersion(), Name: "revoke-frame", Kind: models.RevokedTests,
		Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": names}}}
}

func savedMock(name string) *models.Mock {
	ts := time.Unix(1700000000, 0)
	return &models.Mock{Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts}}
}

// waitMockSaved waits for the recorder to save the mock name: every frame the
// agent sent before it has been taken by the mock loop.
func (lr *liveRecording) waitMockSaved(name string) {
	lr.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lr.mdb.mu.Lock()
		saved := append([]string(nil), lr.mdb.inserted...)
		lr.mdb.mu.Unlock()
		for _, m := range saved {
			if m == name {
				return
			}
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("the recorder never saved mock %q (saved: %v)", name, saved)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitDeleted waits, with the recording still going, for name to be deleted.
func (lr *liveRecording) waitDeleted(name string) {
	lr.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lr.db.mu.Lock()
		deleted := append([]string(nil), lr.db.deleted...)
		lr.db.mu.Unlock()
		for _, d := range deleted {
			if d == name {
				return
			}
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("test case %q, revoked, was not deleted while the recording went on (deleted so far: %v): a replay or an upload meanwhile still finds it", name, deleted)
		}
		time.Sleep(time.Millisecond)
	}
}

// stop ends the recording, with or without its mock stream (already ended).
func (lr *liveRecording) stop(outgoingOpen bool) {
	lr.t.Helper()
	lr.cancel()
	if outgoingOpen {
		close(lr.f.outgoing)
	}
	close(lr.f.incoming)
	close(lr.f.mappings)
	select {
	case err := <-lr.done:
		require.NoError(lr.t, err)
	case <-time.After(90 * time.Second):
		lr.t.Fatal("Start did not return")
	}
}

func (lr *liveRecording) deletedCount(name string) int {
	lr.db.mu.Lock()
	defer lr.db.mu.Unlock()
	n := 0
	for _, d := range lr.db.deleted {
		if d == name {
			n++
		}
	}
	return n
}

func TestARevokedTestCaseIsDeletedWhileTheRecordingGoesOn(t *testing.T) {
	lr := startLiveRecording(t)
	lr.mock(savedMock("mock-1"))
	lr.testCase("test-1")
	waitForPersistedTestCase(t, lr.db, "test-1")
	lr.mock(liveRevokeFrame("test-1"))
	lr.waitDeleted("test-1")
	lr.stop(true)
	require.Equal(t, 1, lr.deletedCount("test-1"), "a revoked test case is deleted once")
}

func TestATestCaseRevokedBeforeItArrivesIsNotSaved(t *testing.T) {
	lr := startLiveRecording(t)
	lr.mock(savedMock("mock-1"))
	lr.mock(liveRevokeFrame("test-2"))
	// The mock loop has taken the frame once it saved the next mock.
	lr.mock(savedMock("mock-2"))
	lr.waitMockSaved("mock-2")
	lr.testCase("test-2")
	lr.testCase("test-3")
	// One loop saves the test cases in order: test-2 has had its turn.
	waitForPersistedTestCase(t, lr.db, "test-3")
	lr.db.mu.Lock()
	inserted := append([]string(nil), lr.db.inserted...)
	lr.db.mu.Unlock()
	require.NotContains(t, inserted, "test-2", "a test case revoked before it arrived was saved")
	lr.stop(true)
	require.Zero(t, lr.deletedCount("test-3"), "a test case nobody revoked was deleted")
}

func TestATestWhoseMockTheAgentNeverSentBeforeItEndedTheStreamIsRevoked(t *testing.T) {
	lr := startLiveRecording(t)
	lr.mock(savedMock("mock-1"))
	lr.testCase("test-1")
	waitForPersistedTestCase(t, lr.db, "test-1")
	// The mapping names a mock the agent has not sent yet: it is held.
	lr.mapping(models.TestMockMapping{TestName: "test-1", MockIDs: []string{"mock-1", "never-sent"}})
	// The agent ends the mock stream without its end marker (it was killed,
	// or crashed): the mock it had not sent is lost, not merely unmatched.
	close(lr.f.outgoing)
	lr.waitDeleted("test-1")
	lr.stop(false)
	require.Equal(t, 1, lr.deletedCount("test-1"), "a revoked test case is deleted once")
	lr.tele.mu.Lock()
	suite := lr.tele.suite
	lr.tele.mu.Unlock()
	require.NotContains(t, suite, "tests-short-pool", "a revoked test is counted as recorded with a short pool")
}

// A revoke that comes while its test case is being saved deletes it once it
// is saved, while the recording goes on.
func TestATestCaseRevokedWhileItIsSavedIsDeletedOnceSaved(t *testing.T) {
	var lr *liveRecording
	db := &recTestDB{}
	revoked := make(chan struct{})
	db.beforeInsert = func(tc *models.TestCase) {
		if tc.Name != "test-1" {
			return
		}
		// The revoke is taken, and applied to what is saved so far, before
		// this insert goes on.
		go func() {
			lr.mock(liveRevokeFrame("test-1"))
			lr.mock(savedMock("mock-2"))
			close(revoked)
		}()
		<-revoked
		time.Sleep(50 * time.Millisecond)
	}
	lr = startLiveRecordingWith(t, db)
	lr.mock(savedMock("mock-1"))
	lr.testCase("test-1")
	lr.waitDeleted("test-1")
	lr.stop(true)
	require.Equal(t, 1, lr.deletedCount("test-1"), "a revoked test case is deleted once")
}

// A test case named by its request's Keploy-Test-Name header keeps that name
// on every capture of the request (pkg/agent/hooks/conn/util.go). After the
// agent drops a mock of a test under a name it streams no capture under that
// name, so a capture under it that reaches the recorder after the agent's
// revoke was streamed before the drop, and may own the dropped mock: it is not
// saved, and the saved capture is deleted. Applied at the end, the revoke
// deleted the name whole; applied as it comes, it leaves the same.
func TestACaptureUnderANameTheAgentRevokedIsNotSaved(t *testing.T) {
	lr := startLiveRecording(t)
	lr.mock(savedMock("mock-1"))
	lr.testCase("login")
	waitForPersistedTestCase(t, lr.db, "login")
	lr.mock(liveRevokeFrame("login"))
	lr.waitDeleted("login")
	lr.mock(savedMock("mock-3"))
	lr.testCase("login")
	lr.testCase("test-3")
	waitForPersistedTestCase(t, lr.db, "test-3")
	saved := lr.insertedCount("login")
	lr.stop(true)
	require.Equal(t, 1, saved, "a capture that came under a name the agent revoked was saved")
	require.Equal(t, 1, lr.deletedCount("login"))
}

// An agent's revoke that comes before its capture keeps out every capture
// under the name that comes after it: the agent streams none after its drop,
// so each was streamed before it, and any may own the dropped mock.
func TestAnAgentsRevokeThatComesFirstKeepsOutEveryCaptureUnderTheName(t *testing.T) {
	lr := startLiveRecording(t)
	lr.mock(savedMock("mock-1"))
	lr.mock(liveRevokeFrame("login"))
	lr.mock(savedMock("mock-2"))
	lr.waitMockSaved("mock-2")
	lr.testCase("login")
	lr.testCase("login")
	lr.testCase("test-3")
	waitForPersistedTestCase(t, lr.db, "test-3")
	saved := lr.insertedCount("login")
	lr.stop(true)
	require.Zero(t, saved, "a capture under a name the agent revoked first was saved")
	require.Zero(t, lr.deletedCount("login"), "a capture never saved was deleted")
}

// A test case the agent sent with no name, revoked by its request time before
// it arrives, is not saved; one of another request time is.
func TestATestCaseWithNoNameRevokedByItsRequestTimeBeforeItArrivesIsNotSaved(t *testing.T) {
	lr := startLiveRecordingWith(t, &recTestDB{autoName: "post-customers"})
	at := time.Now().Add(-time.Minute)
	lr.mock(savedMock("mock-1"))
	lr.mock(liveRevokeFrame(models.RevokeNameByRequest(at)))
	lr.mock(savedMock("mock-2"))
	lr.waitMockSaved("mock-2")
	for _, req := range []time.Time{at, at.Add(time.Millisecond)} {
		lr.sendTestCase(&models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: req}, HTTPResp: models.HTTPResp{Timestamp: req.Add(time.Millisecond)}})
	}
	waitForPersistedTestCase(t, lr.db, "post-customers-1")
	lr.db.mu.Lock()
	inserted := append([]string(nil), lr.db.inserted...)
	lr.db.mu.Unlock()
	lr.stop(true)
	require.Equal(t, []string{"post-customers-1"}, inserted, "the test case revoked by its request time before it arrived was saved")
	require.Zero(t, lr.deletedCount("post-customers-1"), "a test case nobody revoked was deleted")
}

// failingDeleteDB saves test cases and cannot delete them, and counts the
// deletes it was asked for.
type failingDeleteDB struct {
	recTestDB
	calls atomic.Int64
}

func (d *failingDeleteDB) DeleteTests(context.Context, string, []string) error {
	d.calls.Add(1)
	return errors.New("the store could not delete it")
}

// A revoke whose delete failed is not tried again for every test case saved
// after it: each try asks the store again (k8s-proxy's waits for the case's
// writers), and warns once per name. The end tries it once more.
func TestAFailedLiveDeleteIsNotRetriedOnEverySave(t *testing.T) {
	db := &failingDeleteDB{}
	lr := startLiveRecordingOn(t, db, &db.recTestDB)
	lr.mock(savedMock("mock-1"))
	lr.testCase("test-1")
	waitForPersistedTestCase(t, lr.db, "test-1")
	lr.mock(liveRevokeFrame("test-1"))
	deadline := time.Now().Add(10 * time.Second)
	for db.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the revoke's delete was never asked of the store")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	before := db.calls.Load()
	for i := 2; i <= 101; i++ {
		lr.testCase(fmt.Sprintf("test-%d", i))
	}
	waitForPersistedTestCase(t, lr.db, "test-101")
	time.Sleep(100 * time.Millisecond)
	during := db.calls.Load() - before
	lr.stop(true)
	t.Logf("deletes asked of the store: %d before the 100 saves, %d during them, %d in all", before, during, db.calls.Load())
	require.Zero(t, during, "one revoke whose delete failed was asked of the store again while 100 more test cases were saved")
	// deleteRevoked tries a batch, then each name: two calls for the one
	// name live, two at the end.
	require.LessOrEqual(t, db.calls.Load(), int64(4), "the failed delete was retried more than once at the end")
}

// A test whose mock the agent never sent before it ended the mock stream is
// revoked, as one whose mock the recorder could not save is: kept because the
// store could not delete it, it is said as a revoke that failed, not counted
// as a test recorded with a short mock pool.
func TestALostStreamsTestTheStoreCannotDeleteIsNotCountedAsAShortPool(t *testing.T) {
	db := &failingDeleteDB{}
	lr := startLiveRecordingOn(t, db, &db.recTestDB)
	lr.mock(savedMock("mock-1"))
	lr.testCase("test-1")
	waitForPersistedTestCase(t, lr.db, "test-1")
	lr.mapping(models.TestMockMapping{TestName: "test-1", MockIDs: []string{"mock-1", "never-sent"}})
	close(lr.f.outgoing)
	deadline := time.Now().Add(10 * time.Second)
	for db.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the lost stream's test was never revoked")
		}
		time.Sleep(time.Millisecond)
	}
	lr.stop(false)
	lr.tele.mu.Lock()
	suite := lr.tele.suite
	lr.tele.mu.Unlock()
	require.NotContains(t, suite, "tests-short-pool", "a test revoked for a mock the agent never sent is counted as recorded with a short pool")
}

// deleteGate is a store whose deletes wait until let go, and that notes the
// order of its saves and deletes.
type deleteGate struct {
	mu      sync.Mutex
	events  []string
	started chan struct{}
	release chan struct{}
}

func (d *deleteGate) DeleteTests(_ context.Context, _ string, names []string) error {
	d.mu.Lock()
	d.events = append(d.events, "delete "+strings.Join(names, ","))
	d.mu.Unlock()
	close(d.started)
	<-d.release
	d.mu.Lock()
	d.events = append(d.events, "deleted")
	d.mu.Unlock()
	return nil
}

func (d *deleteGate) note(e string) {
	d.mu.Lock()
	d.events = append(d.events, e)
	d.mu.Unlock()
}

// A save under a name waits out a revoke's delete under it, so the delete
// takes the capture it is about and never the later one the store puts under
// the name meanwhile.
func TestASaveUnderANameWaitsOutTheDeleteUnderIt(t *testing.T) {
	td := &deleteGate{started: make(chan struct{}), release: make(chan struct{})}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	save := func(tc *models.TestCase) {
		c := lr.arrive(tc)
		require.NotNil(t, c)
		lr.saving(c, tc.Name)
		td.note("save " + tc.Name)
		lr.saved(c, tc.Name, tc, nil)
	}
	save(winTest("login", 1000, 1100))
	lr.revokeLost("login", []time.Time{winAt(1050)}) // a mock of the first capture lost
	select {
	case <-td.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke's delete never began")
	}
	second := make(chan struct{})
	go func() {
		defer close(second)
		save(winTest("login", 2000, 2100))
	}()
	select {
	case <-second:
		t.Fatal("a save under a name went on while a delete under it ran")
	case <-time.After(200 * time.Millisecond):
	}
	close(td.release)
	<-second
	deleted, _, left := lr.end()
	td.mu.Lock()
	events := append([]string(nil), td.events...)
	td.mu.Unlock()
	require.Equal(t, []string{"save login", "delete login", "deleted", "save login"}, events)
	require.Contains(t, deleted, "login")
	require.Empty(t, left, "the later capture is left to the end's delete")
}

// A capture deleted gives up its test window, also when its delete ran before
// its save placed the window: the window is lost as it is placed.
func TestAWindowPlacedAfterItsCaptureWasDeletedIsLost(t *testing.T) {
	db := &recTestDB{}
	var mu sync.Mutex
	count := 0
	windows := &windowMappings{}
	lr := startLiveRevokes(context.Background(), zap.NewNop(), db, "set", &mu, &count)
	lr.giveWindows(windows)
	tc := &models.TestCase{Name: "login", Kind: models.HTTP}
	c := lr.arrive(tc)
	lr.saving(c, "login")
	lr.revoke("login") // while it is saved: deleted once saved
	lr.saved(c, "login", tc, nil)
	waitTestDeleted(t, db, "login")
	lr.placed(c, windows.addTest(tc))
	kept := windows.addTest(&models.TestCase{Name: "logout", Kind: models.HTTP})
	lr.end()
	windows.mu.Lock()
	defer windows.mu.Unlock()
	require.True(t, strings.HasPrefix(windows.tests[0].TestCase, deletedTestPrefix), "the window of a capture deleted before it was placed is kept as %q", windows.tests[0].TestCase)
	require.Equal(t, "logout", windows.tests[kept].TestCase)
}

// What the end's own delete takes (a revoke whose delete failed while the
// recording went on) is gone as a delete while recording is: its window is
// given up, and its count is left out of the short-pool total.
func TestTheEndsDeleteOfARevokedCaptureLeavesItsWindowAndCountOut(t *testing.T) {
	td := &countingDeleter{fail: true}
	var mu sync.Mutex
	count := 0
	windows := &windowMappings{}
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	lr.giveWindows(windows)
	tc := &models.TestCase{Name: "login", Kind: models.HTTP}
	c := lr.arrive(tc)
	lr.saving(c, "login")
	lr.saved(c, "login", tc, nil)
	lr.placed(c, windows.addTest(tc))
	var counts sync.Map
	counts.Store("login", int64(2))
	lr.revoke("login")
	deadline := time.Now().Add(10 * time.Second)
	for len(td.asked()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the revoke's delete was never asked of the store")
		}
		time.Sleep(time.Millisecond)
	}
	_, _, left := lr.end()
	require.Equal(t, []string{"login"}, left)
	tests, _ := lr.shortTotals(&counts)
	require.Equal(t, 1, tests, "a capture whose delete failed is in the set, short")
	lr.endDeleted(map[string]struct{}{"login": {}})
	tests, mocks := lr.shortTotals(&counts)
	require.Zero(t, tests, "the end deleted the capture, but its short pool is counted (%d mocks)", mocks)
	windows.mu.Lock()
	defer windows.mu.Unlock()
	require.True(t, strings.HasPrefix(windows.tests[0].TestCase, deletedTestPrefix), "the end deleted the capture, but its window keeps its entry (%q)", windows.tests[0].TestCase)
}

// Two revokes of one saved capture for two of its lost mocks, the second after
// the first deleted it, take no other capture: the next capture under the
// name is saved.
func TestASecondRevokeOfADeletedCaptureTakesNoOther(t *testing.T) {
	db := &recTestDB{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), db, "set", &mu, &count)
	first := capture("login", "first", 1000, 1100)
	c := lr.arrive(first)
	lr.saving(c, "login")
	lr.saved(c, "login", first, nil)
	lr.revokeLost("login", []time.Time{winAt(1050)})
	waitTestDeleted(t, db, "login")
	lr.revokeLost("login", []time.Time{winAt(1080)}) // the same capture's second lost mock
	next := lr.arrive(capture("login", "second", 2000, 2100))
	_, notRecorded, _ := lr.end()
	require.NotNil(t, next, "a second revoke of a capture already deleted kept the next capture under its name out (not recorded: %d)", notRecorded)
}

// countingDeleter records the deletes it is asked for, each failing when
// fail is set.
type countingDeleter struct {
	mu    sync.Mutex
	calls [][]string
	fail  bool
}

func (d *countingDeleter) DeleteTests(_ context.Context, _ string, names []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, append([]string(nil), names...))
	if d.fail {
		return errors.New("the store could not delete it")
	}
	return nil
}

func (d *countingDeleter) asked() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]string(nil), d.calls...)
}

// waitAsked waits, the recording still going, for the store to be asked to
// delete name.
func waitAsked(t *testing.T, td *countingDeleter, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, call := range td.asked() {
			if slices.Contains(call, name) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked test case %q was not deleted while the recording went on (asked: %v)", name, td.asked())
		}
		time.Sleep(time.Millisecond)
	}
}

// A delete of a capture, queued while a later capture under its name is being
// saved, never runs: not during that save, which would race it, and not after
// it, when the name is the later capture's. The capture is revoked by its
// request time, which names it and not the later one.
func TestADeleteQueuedWhileALaterCaptureUnderItsNameIsSavedNeverRuns(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	first := time.Unix(1700000000, 0)
	tc1 := &models.TestCase{Name: "login", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: first}, HTTPResp: models.HTTPResp{Timestamp: first.Add(time.Millisecond)}}
	c1 := lr.arrive(tc1)
	lr.saving(c1, "login")
	lr.saved(c1, "login", tc1, nil)
	later := first.Add(time.Second)
	tc2 := &models.TestCase{Name: "login", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: later}, HTTPResp: models.HTTPResp{Timestamp: later.Add(time.Millisecond)}}
	c2 := lr.arrive(tc2)
	lr.saving(c2, "login")
	lr.revokeByAgent(models.RevokeNameByRequest(first))
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, td.asked(), "a delete under a name ran while a later capture under it was being saved")
	lr.saved(c2, "login", tc2, nil)
	time.Sleep(200 * time.Millisecond)
	_, _, left := lr.end()
	require.Empty(t, td.asked(), "the delete of the replaced capture took the later one saved under its name")
	require.Empty(t, left, "the end is left to delete the later capture")
}

// A mock lost of a capture the store refused takes nothing: that capture was
// not saved, and the next capture under its name, whose window does not hold
// the mock, is saved.
func TestALostMockOfACaptureTheStoreRefusedKeepsNoLaterCaptureOut(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	first := capture("login", "first", 1000, 1100)
	c1 := lr.arrive(first)
	lr.saving(c1, "login")
	lr.saved(c1, "login", first, errors.New("the store refused it"))
	lr.revokeLost("login", []time.Time{winAt(1050)})
	next := lr.arrive(capture("login", "second", 2000, 2100))
	time.Sleep(100 * time.Millisecond)
	_, _, left := lr.end()
	require.NotNil(t, next, "the capture after one the store refused was kept out for that one's lost mock")
	require.Empty(t, td.asked(), "a delete was asked for a capture never saved")
	require.Empty(t, left)
}

// Another lost mock of a capture whose delete failed asks the store for no
// delete again while the recording goes on: the capture is revoked already.
// The end tries the failed delete once more.
func TestAnotherLostMockOfACaptureWhoseDeleteFailedAsksNoDeleteAgain(t *testing.T) {
	td := &countingDeleter{fail: true}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	first := capture("login", "first", 1000, 1100)
	c1 := lr.arrive(first)
	lr.saving(c1, "login")
	lr.saved(c1, "login", first, nil)
	lr.revokeLost("login", []time.Time{winAt(1050)})
	deadline := time.Now().Add(10 * time.Second)
	for len(td.asked()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the revoke's delete was never asked of the store")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	before := len(td.asked())
	lr.revokeLost("login", []time.Time{winAt(1080)})
	time.Sleep(200 * time.Millisecond)
	during := len(td.asked()) - before
	_, _, left := lr.end()
	require.Zero(t, during, "another lost mock of the capture asked the store to delete it again: %v", td.asked())
	require.Equal(t, []string{"login"}, left, "the end tries the failed delete once more")
}

// A revoke that names a saved test case by its request time (the agent sent
// it with no name; the store named it) deletes it while the recording goes
// on, as one that names it by name does.
func TestARevokeByRequestTimeDeletesTheSavedTestCaseWhileTheRecordingGoesOn(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	at := time.Unix(1700000000, 0)
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at}, HTTPResp: models.HTTPResp{Timestamp: at.Add(time.Millisecond)}}
	c := lr.arrive(tc)
	lr.saving(c, "")
	tc.Name = "post-customers-1" // as the store names it
	lr.saved(c, "", tc, nil)
	lr.revokeByAgent(models.RevokeNameByRequest(at))
	waitAsked(t, td, "post-customers-1")
	_, _, left := lr.end()
	require.Empty(t, left)
}

// A revoke by request time of a test case sent with no name, which comes while
// that test case is being saved, deletes it once the store has named and saved
// it.
func TestARevokeByRequestTimeOfACaptureBeingSavedDeletesItOnceSaved(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	at := time.Unix(1700000000, 0)
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at}, HTTPResp: models.HTTPResp{Timestamp: at.Add(time.Millisecond)}}
	c := lr.arrive(tc)
	lr.saving(c, "")
	lr.revokeByAgent(models.RevokeNameByRequest(at))
	tc.Name = "post-customers-1"
	lr.saved(c, "", tc, nil)
	waitAsked(t, td, "post-customers-1")
	_, _, left := lr.end()
	require.Empty(t, left)
}

// A mock lost of a later capture under a name, whose window holds it, while
// that capture is being saved, takes that capture, not the earlier one it
// replaces: the later one is deleted once saved, and only it.
func TestARevokeDuringALaterCapturesSaveTakesThatCapture(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	first := capture("login", "first", 1000, 1100)
	c0 := lr.arrive(first)
	lr.saving(c0, "login")
	lr.saved(c0, "login", first, nil)
	later := capture("login", "second", 2000, 2100)
	c1 := lr.arrive(later)
	lr.saving(c1, "login")
	lr.revokeLost("login", []time.Time{winAt(2050)}) // one of the later capture's mocks lost while it is saved
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, td.asked(), "the earlier capture was deleted for the later one's lost mock")
	lr.saved(c1, "login", later, nil)
	waitAsked(t, td, "login")
	_, _, left := lr.end()
	require.Equal(t, [][]string{{"login"}}, td.asked())
	require.Empty(t, left, "the end is left to delete the later capture")
}

// A mock lost under a name before any capture came under it waits for the
// capture whose window holds it. A capture that comes under the name with no
// window cannot be told from it, so it is not saved.
func TestACaptureWithNoWindowUnderANameWithALostMockWaitingIsNotSaved(t *testing.T) {
	db := &recTestDB{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), db, "set", &mu, &count)
	lr.revokeLost("login", []time.Time{winAt(1050)})
	c := lr.arrive(&models.TestCase{Name: "login", Kind: models.HTTP})
	_, notRecorded, _ := lr.end()
	require.Nil(t, c, "a capture with no window under a name whose lost mock waits for its capture was saved")
	require.Equal(t, 1, notRecorded)
}

// A lost mock whose request time is not known (the agent ended the stream
// before it sent it) cannot tell the capture it was of, though every capture
// under the name has a window: the name is killed, so the saved capture is
// deleted and the next one under the name is not saved.
func TestALostMockWithNoTimeKillsItsName(t *testing.T) {
	db := &recTestDB{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), db, "set", &mu, &count)
	first := capture("login", "first", 1000, 1100)
	c := lr.arrive(first)
	lr.saving(c, "login")
	lr.saved(c, "login", first, nil)
	lr.revokeLost("login", []time.Time{{}})
	waitTestDeleted(t, db, "login")
	next := lr.arrive(capture("login", "second", 2000, 2100))
	lr.end()
	require.Nil(t, next, "a capture under a name whose lost mock no capture could be told by was saved")
}

// A revoke that names a saved test case by the name the store gave it (the
// agent sent it with no name) deletes it.
func TestARevokeByTheStoresNameDeletesTheSavedTestCase(t *testing.T) {
	td := &countingDeleter{}
	var mu sync.Mutex
	count := 0
	lr := startLiveRevokes(context.Background(), zap.NewNop(), td, "set", &mu, &count)
	tc := capture("", "first", 1000, 1100)
	c := lr.arrive(tc)
	lr.saving(c, "")
	tc.Name = "post-customers-1" // as the store names it
	lr.saved(c, "", tc, nil)
	lr.revokeLost("post-customers-1", []time.Time{winAt(1050)})
	waitAsked(t, td, "post-customers-1")
	_, _, left := lr.end()
	require.Empty(t, left)
}
