package record

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A recording made without --sync maps its tests from their windows under the
// proven switch (design v14 row S3). These tests drive Recorder.Start with the
// agent's streams faked, so what they assert is what the recorder hands the
// mappings store at the end of a recording.

var win0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func winAt(ms int) time.Time { return win0.Add(time.Duration(ms) * time.Millisecond) }

// winTest is an HTTP test case whose request came at from and whose response
// went at to.
func winTest(name string, from, to int) *models.TestCase {
	return &models.TestCase{
		Name: name, Kind: models.HTTP,
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://localhost:8080/" + name, Timestamp: winAt(from)},
		HTTPResp: models.HTTPResp{StatusCode: 200, Timestamp: winAt(to)},
	}
}

// winMock is a mock of kind, tagged tag (none when empty), whose request went
// at at.
func winMock(name string, kind models.Kind, tag string, at int) *models.Mock {
	m := &models.Mock{
		Version: models.GetVersion(), Name: name, Kind: kind,
		Spec: models.MockSpec{ReqTimestampMock: winAt(at), ResTimestampMock: winAt(at + 1)},
	}
	if tag != "" {
		m.Spec.Metadata = map[string]string{"type": tag}
	}
	return m
}

// winRecording is one recording against a fake agent.
type winRecording struct {
	f       *fakeInstr
	r       *Recorder
	testDB  *recTestDB
	mockDB  *recMockDB
	mapping *recMappingDB
	done    chan error
	cancel  context.CancelFunc
}

func startWinRecording(t *testing.T, cfg *config.Config, mappingDB MappingDb) *winRecording {
	t.Helper()
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	w := &winRecording{f: f, testDB: &recTestDB{}, mockDB: &recMockDB{unencodable: map[string]bool{}}, done: make(chan error, 1)}
	if mappingDB == nil {
		w.mapping = &recMappingDB{}
		mappingDB = w.mapping
	}
	w.r = &Recorder{
		logger:          zap.NewNop(),
		testDB:          w.testDB,
		mockDB:          w.mockDB,
		mappingDb:       mappingDB,
		telemetry:       &recTelemetry{},
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	go func() { w.done <- w.r.Start(ctx) }()
	return w
}

func (w *winRecording) mock(t *testing.T, m *models.Mock) {
	t.Helper()
	select {
	case w.f.outgoing <- m:
	case <-time.After(10 * time.Second):
		t.Fatalf("the recorder stopped taking mocks at %s", m.Name)
	}
}

func (w *winRecording) test(t *testing.T, tc *models.TestCase) {
	t.Helper()
	select {
	case w.f.incoming <- tc:
	case <-time.After(10 * time.Second):
		t.Fatalf("the recorder stopped taking test cases at %s", tc.Name)
	}
}

func (w *winRecording) streamed(t *testing.T, m models.TestMockMapping) {
	t.Helper()
	select {
	case w.f.mappings <- m:
	case <-time.After(10 * time.Second):
		t.Fatalf("the recorder stopped taking mappings at %s", m.TestName)
	}
}

// stop ends the recording once every test case and mock sent is persisted,
// as a Ctrl+C does, and waits for Start to return.
func (w *winRecording) stop(t *testing.T, tests, mocks int) {
	t.Helper()
	require.Eventually(t, func() bool {
		w.testDB.mu.Lock()
		nt := len(w.testDB.inserted)
		w.testDB.mu.Unlock()
		w.mockDB.mu.Lock()
		nm := len(w.mockDB.inserted)
		w.mockDB.mu.Unlock()
		return nt == tests && nm == mocks
	}, 10*time.Second, 10*time.Millisecond, "the recorder did not persist what it was sent")
	w.cancel()
	close(w.f.outgoing)
	close(w.f.incoming)
	close(w.f.mappings)
	select {
	case err := <-w.done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}
}

// batches is every batch the recorder handed the mappings store, by test and
// mock name.
func (w *winRecording) batches() []map[string][]string {
	w.mapping.mu.Lock()
	defer w.mapping.mu.Unlock()
	out := make([]map[string][]string, 0, len(w.mapping.batches))
	for _, b := range w.mapping.batches {
		names := make(map[string][]string, len(b))
		for test, entries := range b {
			names[test] = []string{}
			for _, e := range entries {
				names[test] = append(names[test], e.Name)
			}
		}
		out = append(out, names)
	}
	return out
}

// keepExitCode isolates the process's pending exit code for one test.
func keepExitCode(t *testing.T) {
	prev := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = prev })
}

// The recording the row is about: no --sync, the switch on. At the end every
// test case is mapped to the data-plane mocks recorded inside its window, in
// recorded order: a handshake, a DNS lookup and an async lane's delivery
// inside a window are not the test's, start-up traffic is nobody's, and a test
// that made no call has an entry with none. One write, after the drain.
func TestStart_ProvenRecordWithoutSyncMapsEachTestFromItsWindow(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keepExitCode(t)
	w := startWinRecording(t, &config.Config{}, nil)

	async := winMock("lane-delivery", models.HTTP, "", 1060)
	async.Spec.Async = &models.AsyncMeta{Lane: "events", Seq: 1}
	for _, m := range []*models.Mock{
		winMock("boot-query", models.Postgres, "mocks", 500),
		winMock("handshake", models.Postgres, "config", 1010),
		winMock("lookup", models.DNS, "", 1020),
		winMock("rates-1", models.HTTP, "", 1030),
		winMock("insert-1", models.Postgres, "mocks", 1040),
		async,
		winMock("rates-2", models.HTTP, "", 2050),
	} {
		w.mock(t, m)
	}
	// Test cases reach the recorder after the mocks of their window.
	w.test(t, winTest("test-1", 1000, 1100))
	w.test(t, winTest("test-2", 2000, 2100))
	w.test(t, winTest("test-3", 3000, 3100))
	w.stop(t, 3, 7)

	want := []map[string][]string{{
		"test-1": {"rates-1", "insert-1"},
		"test-2": {"rates-2"},
		"test-3": {},
	}}
	if got := w.batches(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappings written: %v; want %v", got, want)
	}
	w.mapping.mu.Lock()
	entry := w.mapping.batches[0]["test-2"][0]
	w.mapping.mu.Unlock()
	if entry.Kind != string(models.HTTP) || entry.ReqTimestampMock != models.FormatMockTimestamp(winAt(2050)) ||
		entry.ResTimestampMock != models.FormatMockTimestamp(winAt(2051)) {
		t.Fatalf("test-2's entry is %+v; want the mock's kind and recorded times", entry)
	}
	if utils.ErrCode != 0 {
		t.Fatalf("a recording that saved its mappings set exit code %d", utils.ErrCode)
	}
}

// Every other recording writes exactly what it wrote before the row: nothing
// with the switch off or --disable-mapping, and with --sync only the mappings
// the agent streams.
func TestStart_OnlyAProvenRecordWithoutSyncMapsFromWindows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proven  bool
		sync    bool
		disable bool
	}{
		{name: "switch off"},
		{name: "--sync under the switch", proven: true, sync: true},
		{name: "--disable-mapping under the switch", proven: true, disable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			keepExitCode(t)
			cfg := &config.Config{}
			cfg.Record.Synchronous = tc.sync
			cfg.DisableMapping = tc.disable
			w := startWinRecording(t, cfg, nil)
			w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
			w.test(t, winTest("test-1", 1000, 1100))
			var want []map[string][]string
			if tc.sync {
				// The agent maps a --sync recording itself.
				w.streamed(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"rates-1"}})
				want = []map[string][]string{{"test-1": {"rates-1"}}}
				require.Eventually(t, func() bool { return len(w.batches()) == 1 }, 10*time.Second, 10*time.Millisecond)
			}
			w.stop(t, 1, 1)
			if got := w.batches(); len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
				t.Fatalf("mappings written: %v; want %v", got, want)
			}
		})
	}
}

// A test case the finalize deletes (the agent revoked it: a mock it owned was
// dropped) gets no entry, and the mocks recorded inside its window stay its
// own: they are not handed to the test whose window overlapped it.
func TestStart_ProvenWindowMappingsLeaveARevokedTestOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keepExitCode(t)
	w := startWinRecording(t, &config.Config{}, nil)
	w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
	w.mock(t, winMock("rates-2", models.HTTP, "", 2060))
	w.mock(t, winMock("rates-3", models.HTTP, "", 2200))
	w.mock(t, &models.Mock{Kind: models.RevokedTests, Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": "test-2"}}})
	w.test(t, winTest("test-1", 1000, 1100))
	w.test(t, winTest("test-2", 2000, 2100))
	w.test(t, winTest("test-3", 2050, 2300))
	w.stop(t, 3, 3)

	w.testDB.mu.Lock()
	deleted := append([]string(nil), w.testDB.deleted...)
	w.testDB.mu.Unlock()
	require.Equal(t, []string{"test-2"}, deleted, "precondition: the finalize deletes the revoked test")
	want := []map[string][]string{{"test-1": {"rates-1"}, "test-3": {"rates-3"}}}
	if got := w.batches(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappings written: %v; want %v", got, want)
	}
}

// failingMappingDB refuses every write.
type failingMappingDB struct{ recMappingDB }

func (*failingMappingDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return errors.New("read-only file system")
}

// A mappings.yaml the recording could not write fails the recording, and says
// so: a test set without one replays with no test owning its calls.
func TestStart_ProvenWindowMappingsWriteFailureFailsTheRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keepExitCode(t)
	core, logs := observer.New(zap.ErrorLevel)
	w := startWinRecording(t, &config.Config{}, &failingMappingDB{})
	w.r.logger = zap.New(core)
	w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
	w.test(t, winTest("test-1", 1000, 1100))
	w.stop(t, 1, 1)

	if utils.ErrCode != utils.ExitKeployError {
		t.Fatalf("exit code %d after the mappings write failed; want %d", utils.ErrCode, utils.ExitKeployError)
	}
	if n := logs.FilterMessage("failed to save the test-mock mappings derived from the test windows").Len(); n != 1 {
		t.Fatalf("want one error naming the failed mappings write, got %d: %+v", n, logs.All())
	}
}

// When the record drain did not join every persist goroutine, a test case or
// mock may still be on its way to disk: no mapping is derived from what
// arrived so far, and the recording fails.
func TestWindowMappingsWriteNothingAfterADrainThatTimedOut(t *testing.T) {
	keepExitCode(t)
	core, logs := observer.New(zap.ErrorLevel)
	db := &recMappingDB{}
	w := &windowMappings{}
	w.addTest(winTest("test-1", 1000, 1100))
	w.addMock(winMock("rates-1", models.HTTP, "", 1030))
	w.write(context.Background(), zap.New(core), db, "test-set-0", nil, false, 0)

	if len(db.batches) != 0 {
		t.Fatalf("wrote %v after a drain that timed out", db.batches)
	}
	if utils.ErrCode != utils.ExitKeployError {
		t.Fatalf("exit code %d; want %d", utils.ErrCode, utils.ExitKeployError)
	}
	if logs.FilterMessageSnippet("mappings.yaml was not written").Len() != 1 {
		t.Fatalf("want one error saying mappings.yaml was not written, got %+v", logs.All())
	}

	// The control: drained, the same collector writes.
	utils.ErrCode = 0
	w.write(context.Background(), zap.NewNop(), db, "test-set-0", nil, true, 0)
	if len(db.batches) != 1 || utils.ErrCode != 0 {
		t.Fatalf("a drained recording wrote %v (exit code %d); want one batch", db.batches, utils.ErrCode)
	}
}

// Tests whose windows were open together while a call was recorded are named:
// which of them owns the call was decided by time alone.
func TestWindowMappingsNameConcurrentTests(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	db := &recMappingDB{}
	w := &windowMappings{}
	w.addTest(winTest("slow", 1000, 2000))
	w.addTest(winTest("fast", 1200, 1300))
	w.addTest(winTest("alone", 3000, 3100))
	w.addMock(winMock("call", models.HTTP, "", 1250))
	w.addMock(winMock("other", models.HTTP, "", 3050))
	w.write(context.Background(), zap.New(core), db, "test-set-0", nil, true, 0)

	warned := logs.FilterMessageSnippet("recorded while another test's request was still open").All()
	if len(warned) != 1 {
		t.Fatalf("want one warning naming the concurrent tests, got %+v", logs.All())
	}
	if got := warned[0].ContextMap()["tests"]; !reflect.DeepEqual(got, []interface{}{"slow", "fast"}) {
		t.Fatalf("the warning names %v; want [slow fast]", got)
	}
}

// A recording made without --sync that the agent mapped itself (enterprise's
// static dedup resolves windows in the agent) keeps the agent's mappings and
// derives none: both could list one call under two tests.
func TestStart_ProvenWindowMappingsYieldToMappingsTheAgentStreamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keepExitCode(t)
	core, logs := observer.New(zap.InfoLevel)
	w := startWinRecording(t, &config.Config{}, nil)
	w.r.logger = zap.New(core)
	w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
	w.mock(t, winMock("rates-2", models.HTTP, "", 2030))
	w.test(t, winTest("test-1", 1000, 1100))
	w.test(t, winTest("test-2", 2000, 2100))
	w.streamed(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"rates-1"}})
	require.Eventually(t, func() bool { return len(w.batches()) == 1 }, 10*time.Second, 10*time.Millisecond)
	w.stop(t, 2, 2)

	want := []map[string][]string{{"test-1": {"rates-1"}}}
	if got := w.batches(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappings written: %v; want only the agent's %v", got, want)
	}
	if logs.FilterMessageSnippet("mapped this recording's tests to their mocks itself").Len() != 1 {
		t.Fatalf("want one line saying the agent mapped the recording, got %+v", logs.All())
	}
	if utils.ErrCode != 0 {
		t.Fatalf("exit code %d", utils.ErrCode)
	}
}

// A test case the recording failed to persist keeps its window, as a deleted
// one does: the call recorded while it and a later test were open is its own,
// not handed to the later test. Neither it nor a deleted test is named as
// concurrent.
func TestWindowMappingsALostTestKeepsItsWindow(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	db := &recMappingDB{}
	w := &windowMappings{}
	w.addLostTest(winTest("", 1000, 1500))
	w.addTest(winTest("kept", 1200, 1300))
	w.addTest(winTest("deleted", 1250, 1400))
	w.addTest(winTest("other", 3000, 3100))
	w.addMock(winMock("lost-call", models.HTTP, "", 1210))
	w.addMock(winMock("other-call", models.HTTP, "", 3050))
	w.write(context.Background(), zap.New(core), db, "test-set-0", map[string]struct{}{"deleted": {}}, true, 0)

	if len(db.batches) != 1 {
		t.Fatalf("want one batch, got %v", db.batches)
	}
	names := map[string][]string{}
	for test, entries := range db.batches[0] {
		names[test] = []string{}
		for _, e := range entries {
			names[test] = append(names[test], e.Name)
		}
	}
	if want := map[string][]string{"kept": {}, "other": {"other-call"}}; !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
	warned := logs.FilterMessageSnippet("recorded while another test's request was still open").All()
	if len(warned) != 1 || !reflect.DeepEqual(warned[0].ContextMap()["tests"], []interface{}{"kept"}) {
		t.Fatalf("want the warning to name only kept, got %+v", logs.All())
	}
}

// The collector keeps only the mocks a test may own: a recording can make
// millions of handshakes, DNS lookups and async deliveries.
func TestWindowMappingsCollectOnlyTestMocks(t *testing.T) {
	w := &windowMappings{}
	async := winMock("delivery", models.HTTP, "", 1020)
	async.Spec.Async = &models.AsyncMeta{Lane: "events"}
	w.addMock(winMock("handshake", models.Postgres, "config", 1010))
	w.addMock(winMock("lookup", models.DNS, "", 1015))
	w.addMock(async)
	w.addMock(winMock("query", models.Postgres, "mocks", 1030))
	if len(w.mocks) != 1 || w.mocks[0].Name != "query" {
		t.Fatalf("collected %+v; want only the data-plane query", w.mocks)
	}
}
