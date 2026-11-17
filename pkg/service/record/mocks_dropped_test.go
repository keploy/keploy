package record

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// endSummaryInstr is an instrumentation whose agent's end marker said sum
// (ok: the agent ended the stream with one).
type endSummaryInstr struct {
	Instrumentation
	sum models.StreamEndSummary
	ok  bool
}

func (e endSummaryInstr) IncomingEndSummary() (models.StreamEndSummary, bool) { return e.sum, e.ok }

// A recording whose agent dropped mocks after capturing them keeps what the
// agent's summary said of them in its test-set's config, with or without the
// proven switch, and says so at WARN: a drop for memory pressure and one for
// the mock output alike. The rest of the config is kept, and no secret is
// written into it. Nothing is written when the agent dropped none, when it
// sent no summary, when the instrumentation cannot read one, or when the
// config store cannot read a config for update.
func TestARecordingWhoseAgentDroppedMocksSaysSoInItsConfig(t *testing.T) {
	const marked = "the test-set's recording does not hold every mock the agent captured: the agent dropped some after capturing them, so a replay of it may find no mock for a call that needs one; its config.yaml names them (recordMocksDropped)"
	recorded := func() *models.TestSet {
		return &models.TestSet{PostScript: "make check", Metadata: map[string]interface{}{"join": "ui"}}
	}
	pressure := models.MockDropSummary{ByPressure: 1, Named: []string{"Http2 mock of connection 7, asked at 2026-11-16T07:29:25Z: its request was made while the memory guard had paused recording"}}
	output := models.MockDropSummary{Capacity: 2}
	for _, switchOn := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+switchOn, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", switchOn)
			for _, d := range []models.MockDropSummary{pressure, output} {
				core, logs := observer.New(zapcore.WarnLevel)
				store := &updatingConf{&updatingStore{confStore{conf: recorded()}}}
				r := &Recorder{logger: zap.New(core), testSetConf: store,
					instrumentation: endSummaryInstr{sum: models.StreamEndSummary{TestCasesSent: 4, MocksDropped: d}, ok: true}}
				r.markMocksDropped(context.Background(), "test-set-0")
				got := store.got()
				if got == nil || got.RecordMocksDropped == nil || !reflect.DeepEqual(*got.RecordMocksDropped, d) {
					t.Fatalf("the config written is %+v; want the agent's drops %+v in it", got, d)
				}
				if got.PostScript != "make check" || got.Metadata["join"] != "ui" {
					t.Errorf("the config's other fields were not kept: %+v", got)
				}
				if got.Secret != nil {
					t.Errorf("a secret was written into the config: %v", got.Secret)
				}
				if said := logs.FilterMessage(marked).All(); len(said) != 1 || said[0].ContextMap()["testSet"] != "test-set-0" {
					t.Errorf("the mark was said %v, want once, naming the test-set", logs.All())
				}
			}
			for name, instr := range map[string]Instrumentation{
				"no drop":    endSummaryInstr{sum: models.StreamEndSummary{TestCasesSent: 4, TestCasesSuppressed: 2}, ok: true},
				"no summary": endSummaryInstr{sum: models.StreamEndSummary{MocksDropped: pressure}},
				"no reader":  struct{ Instrumentation }{},
			} {
				core, logs := observer.New(zapcore.WarnLevel)
				store := &updatingConf{&updatingStore{confStore{conf: recorded()}}}
				(&Recorder{logger: zap.New(core), testSetConf: store, instrumentation: instr}).markMocksDropped(context.Background(), "test-set-0")
				if store.got() != nil || logs.Len() != 0 {
					t.Fatalf("%s: the config was written (%+v) or a line said (%v)", name, store.got(), logs.All())
				}
			}
			plain := &plainConf{&confStore{conf: recorded()}}
			(&Recorder{logger: zap.NewNop(), testSetConf: plain,
				instrumentation: endSummaryInstr{sum: models.StreamEndSummary{MocksDropped: pressure}, ok: true}}).markMocksDropped(context.Background(), "test-set-0")
			if plain.got() != nil {
				t.Fatalf("a store with no read for update was written: %+v", plain.got())
			}
		})
	}
}

// droppedInstr is a recording's instrumentation whose agent's end marker
// said it dropped d. As the agent's does, the marker comes only at the end of
// the incoming stream, once the recording's drain closes it: the stream stays
// open until then, and the summary is there only once the stream has ended.
// A mark taken before the drain finds none.
type droppedInstr struct {
	*blockingInstr
	d     models.MockDropSummary
	ended atomic.Bool
}

func (i *droppedInstr) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		for tc := range i.incoming {
			select {
			case out <- tc:
			case <-ctx.Done():
			}
		}
		<-ctx.Done() // the drain closed the stream: the agent's end marker comes now
		i.ended.Store(true)
	}()
	return out, nil
}

func (i *droppedInstr) IncomingEndSummary() (models.StreamEndSummary, bool) {
	if !i.ended.Load() {
		return models.StreamEndSummary{}, false
	}
	return models.StreamEndSummary{TestCasesSent: 1, MocksDropped: i.d}, true
}

// writtenConfs is a test-set config store that keeps each config written.
type writtenConfs struct {
	mu      sync.Mutex
	written map[string]*models.TestSet
}

func (w *writtenConfs) Read(context.Context, string) (*models.TestSet, error) { return nil, nil }
func (w *writtenConfs) ReadForUpdate(_ context.Context, id string) (*models.TestSet, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c := w.written[id]; c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}
func (w *writtenConfs) Write(_ context.Context, id string, c *models.TestSet) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written[id] = c
	return nil
}

// A recording's stop marks its test-set with what its agent's end marker said
// it dropped, once the streams have drained, with or without the proven
// switch. The marker is there only after the drain (droppedInstr), so a mark
// taken before it would find nothing and leave the test-set unmarked.
func TestARecordingsStopMarksItsTestSetWithTheAgentsDrops(t *testing.T) {
	d := models.MockDropSummary{ByPressure: 1, Named: []string{"Http2 mock of connection 7"}}
	for _, switchOn := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+switchOn, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", switchOn)
			keepExitCode(t)
			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			confs := &writtenConfs{written: map[string]*models.TestSet{}}
			w := &winRecording{f: f, testDB: &recTestDB{}, mockDB: &recMockDB{unencodable: map[string]bool{}}, mapping: &recMappingDB{}, done: make(chan error, 1)}
			w.r = &Recorder{
				logger: zap.NewNop(), testDB: w.testDB, mockDB: w.mockDB, mappingDb: w.mapping,
				telemetry: &recTelemetry{}, instrumentation: &droppedInstr{blockingInstr: &blockingInstr{f}, d: d}, testSetConf: confs,
				hooks: BaseRecordHooks{}, config: &config.Config{}, frameQuiet: 50 * time.Millisecond,
			}
			ctx, cancel := context.WithCancel(context.Background())
			w.cancel = cancel
			go func() { w.done <- w.r.Start(ctx) }()
			w.mock(t, winMock("rates-1", models.HTTP, "", 1030))
			w.test(t, winTest("test-1", 1000, 1100))
			w.stop(t, 1, 1)

			confs.mu.Lock()
			defer confs.mu.Unlock()
			var marked []string
			for id, c := range confs.written {
				if c.RecordMocksDropped != nil && reflect.DeepEqual(*c.RecordMocksDropped, d) {
					marked = append(marked, id)
				}
			}
			if len(marked) != 1 {
				t.Fatalf("the test-sets marked with the agent's drops are %v (configs written: %v), want the recording's one", marked, confs.written)
			}
		})
	}
}

// failingWriteConf is a test-set config store that reads a config for update
// but fails every write.
type failingWriteConf struct{ updatingStore }

func (*failingWriteConf) Write(context.Context, string, *models.TestSet) error {
	return errors.New("config.yaml: no space left on device")
}

// A mark the config store fails to write is said, naming the test-set, for
// the agent's drops and for the stop's kill alike (they share the write): a
// drop or a kill the recording could not mark never vanishes silently.
func TestAMarkTheConfigStoreFailsToWriteIsSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		dropNotMarked = "keploy could not mark the test-set's recording as missing the mocks the agent dropped; a replay that needs one fails without naming the drop"
		killNotMarked = "keploy could not mark the test-set's recording as cut by its stop's kill; a replay may fail it for a call the recording lacks"
	)
	core, logs := observer.New(zapcore.ErrorLevel)
	r := &Recorder{logger: zap.New(core), testSetConf: &failingWriteConf{updatingStore{confStore{conf: &models.TestSet{}}}},
		instrumentation: endSummaryInstr{sum: models.StreamEndSummary{MocksDropped: models.MockDropSummary{ByPressure: 1}}, ok: true}}
	r.markMocksDropped(context.Background(), "test-set-0")
	r.markStopKilled(context.Background(), "test-set-0", "keploy killed the application at the recording's end")
	for _, msg := range []string{dropNotMarked, killNotMarked} {
		if said := logs.FilterMessage(msg).All(); len(said) != 1 || said[0].ContextMap()["testSet"] != "test-set-0" {
			t.Errorf("%q was said %v; want it once, naming the test-set (all lines: %v)", msg, said, logs.All())
		}
	}
}

// staleSummaryInstr is an agent that fails the incoming stream (an error
// that is not a shutdown's, so the recording never starts) while its client
// still holds an earlier stream's end summary, one that dropped d.
type staleSummaryInstr struct {
	*refusedIncomingInstr
	d models.MockDropSummary
}

func (i *staleSummaryInstr) GetIncoming(context.Context, models.IncomingOptions) (<-chan *models.TestCase, error) {
	return nil, errors.New("the agent answered the incoming stream 500: internal error")
}

func (i *staleSummaryInstr) IncomingEndSummary() (models.StreamEndSummary, bool) {
	return models.StreamEndSummary{TestCasesSent: 1, MocksDropped: i.d}, true
}

// A recording that never started (its agent failed the incoming stream)
// marks no test-set with drops, with or without the proven switch, even
// where its client still holds an earlier stream's summary: those drops are
// another recording's, and this test-set holds no capture to miss.
func TestARecordingThatNeverStartedMarksNoTestSetWithDrops(t *testing.T) {
	d := models.MockDropSummary{ByPressure: 1, Named: []string{"Http2 mock of connection 7"}}
	for _, switchOn := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+switchOn, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", switchOn)
			keepExitCode(t)
			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			instr := &staleSummaryInstr{refusedIncomingInstr: &refusedIncomingInstr{fakeInstr: f, running: make(chan struct{})}, d: d}
			confs := &writtenConfs{written: map[string]*models.TestSet{}}
			r := &Recorder{
				logger: zap.NewNop(), testDB: &recTestDB{}, mockDB: &recMockDB{unencodable: map[string]bool{}}, mappingDb: &recMappingDB{},
				telemetry: &recTelemetry{}, instrumentation: instr, testSetConf: confs, hooks: BaseRecordHooks{}, config: &config.Config{},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx) }()
			select {
			case <-done:
			case <-instr.running:
				cancel()
				select {
				case <-done:
				case <-time.After(90 * time.Second):
					t.Fatal("Start never returned")
				}
			case <-time.After(60 * time.Second):
				t.Fatal("Start neither returned nor reached instrumentation.Run")
			}
			confs.mu.Lock()
			defer confs.mu.Unlock()
			for id, c := range confs.written {
				if c.RecordMocksDropped != nil {
					t.Errorf("test-set %s of a recording that never started is marked with drops %+v; want no mark", id, *c.RecordMocksDropped)
				}
			}
		})
	}
}
