package record

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// backlogComposeInstr is a compose stack whose agent says what it still holds
// from before the stop, as the real one does (/agent/record/pending): pending
// until the test has handed over every mock it held. The stop's drain then
// ends on that answer, an event, and not on the stream falling quiet, which a
// busy host's scheduling can fake.
type backlogComposeInstr struct {
	*composeInstr
	held  int
	taken atomic.Int32
}

func (b *backlogComposeInstr) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	if n := int(b.taken.Load()); n < b.held {
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(n)}, nil
	}
	return models.AgentBacklog{Verdict: models.BacklogClear, Progress: uint64(b.held)}, nil
}

// stopInSetupInstr is a compose stack whose agent is capturing (its streams
// are open) when the stop comes, before the recorder has its consumers
// running: GetOutgoing returns only once the test has stopped the recording.
type stopInSetupInstr struct {
	*backlogComposeInstr
	opening chan<- struct{}
	stopped <-chan struct{}
}

func (s *stopInSetupInstr) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	close(s.opening) // the agent is capturing: its test-case stream is open
	<-s.stopped
	return s.composeInstr.GetOutgoing(ctx, opts)
}

// A stop that comes once the agent is capturing, but before the recorder has
// started reading what it captures, still drains what the agent captured
// before it. Under docker compose the app (and the agent) are up by then. The
// recorder returned at its stop check instead, abandoning the agent's
// streams, and the stop then took the stack down with all the agent held:
// lost, said by no one. A busy host stretches that window (CI saw it as a
// flake of the compose drain test).
func TestStart_AStopBeforeTheConsumersRunStillDrainsTheAgent(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	opening, stopped := make(chan struct{}), make(chan struct{})
	const held = 10
	instr := &stopInSetupInstr{
		backlogComposeInstr: &backlogComposeInstr{composeInstr: &composeInstr{fakeInstr: f, stackDown: make(chan struct{})}, held: held},
		opening:             opening,
		stopped:             stopped,
	}
	mockDB := &recMockDB{unencodable: map[string]bool{}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the compose agent's health check
	}))
	defer agent.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	cfg.Agent.AgentURI = agent.URL
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          mockDB,
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case <-opening:
	case err := <-done:
		t.Fatalf("Start returned (%v) before it opened the agent's streams", err)
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the recorder never opened the agent's streams")
	}
	captured := time.Now()
	cancel() // the stop, while the recorder opens the agent's streams
	close(stopped)
	for i := 0; i < held; i++ {
		select {
		case f.outgoing <- mockAt(fmt.Sprintf("held-%d", i), captured):
			instr.taken.Add(1)
		case <-instr.stackDown:
			t.Fatalf("the stack (and the agent) went down with %d of the %d mocks it held from before the stop still to hand over", held-i, held)
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("mock %d of %d was never taken", i+1, held)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("Start did not return")
	}
	mockDB.mu.Lock()
	defer mockDB.mu.Unlock()
	if len(mockDB.inserted) != held {
		t.Fatalf("persisted %d of the %d mocks the agent held at the stop", len(mockDB.inserted), held)
	}
}

// captureCountingSetupInstr is stopInSetupInstr whose agent counts the calls
// it could not capture (the proven switch's capture check), answering on its
// request's context as the agent client does.
type captureCountingSetupInstr struct{ *stopInSetupInstr }

func (*captureCountingSetupInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("failed to ask the agent for its capture losses: %w", err)
	}
	return nil, nil
}

// Under the proven switch a test case that a stop in setup drains is checked
// like any other: the stop does not cut short the read of the count it is
// checked against, which failed the recording the user stopped as "not
// verified".
func TestStart_AStopBeforeTheConsumersRunLeavesNoTestCaseUnverified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	opening, stopped := make(chan struct{}), make(chan struct{})
	instr := &captureCountingSetupInstr{&stopInSetupInstr{
		backlogComposeInstr: &backlogComposeInstr{composeInstr: &composeInstr{fakeInstr: f, stackDown: make(chan struct{})}, held: 1},
		opening:             opening,
		stopped:             stopped,
	}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the compose agent's health check
	}))
	defer agent.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	cfg.Agent.AgentURI = agent.URL
	tdb := &recTestDB{}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          tdb,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	select {
	case <-opening:
	case err := <-done:
		t.Fatalf("Start returned (%v) before it opened the agent's streams", err)
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the recorder never opened the agent's streams")
	}
	captured := time.Now()
	cancel() // the stop, while the recorder opens the agent's streams
	close(stopped)
	select {
	case f.incoming <- tcAt("t-1", captured):
		instr.taken.Add(1)
	case <-instr.stackDown:
		t.Fatal("the stack went down before the test case the agent held was taken")
	case <-time.After(time.Minute):
		t.Fatal("the test case the agent held was never taken")
	}
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("Start did not return")
	}
	tdb.mu.Lock()
	defer tdb.mu.Unlock()
	if len(tdb.inserted) != 1 || utils.ErrCode != 0 {
		t.Fatalf("saved %v, exit code %d; want t-1 saved and 0: the user stopped this recording", tdb.inserted, utils.ErrCode)
	}
}
