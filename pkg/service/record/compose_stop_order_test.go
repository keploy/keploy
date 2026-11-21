package record

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stopCallComposeInstr is a docker compose stack whose app makes one call as
// keploy stops its container (StopComposeApp): the agent captures it then,
// while the stack, and the agent in it, are up. Taking the stack down (Run's
// context ending) stops the app first too, as the real stop does
// (cmdCancel), but by then the agent may have been drained: a call it
// captures afterwards is offered for a while and then lost with the stack.
type stopCallComposeInstr struct {
	*backlogComposeInstr
	stops      atomic.Int32
	drainBegan atomic.Bool
}

func (s *stopCallComposeInstr) StopComposeApp(context.Context) (string, error) {
	if s.stops.Add(1) > 1 {
		return "", nil // stopped by keploy already
	}
	select {
	case s.outgoing <- mockAt("stop-time-call", time.Now()):
		s.taken.Add(1)
	case <-time.After(2 * time.Second):
		// The drain is over: nothing reads what the agent captured now.
	}
	return "", nil
}

func (s *stopCallComposeInstr) BacklogBefore(ctx context.Context, at time.Time) (models.AgentBacklog, error) {
	s.drainBegan.Store(true)
	return s.backlogComposeInstr.BacklogBefore(ctx, at)
}

func (s *stopCallComposeInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	_, _ = s.StopComposeApp(context.WithoutCancel(ctx))
	close(s.stackDown)
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// A recording of a docker compose app stops the app's container before it
// drains the agent, in either mode: the call the app makes as it stops is
// recorded. keploy test reads the calls an app makes as it stops in every
// mode, so a recording that drained first (as default-mode compose did) left
// that call out, and every replay named it a miss after the last test.
func TestStart_AComposeRecordingKeepsTheCallTheAppMakesAsItStops(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			// The agent holds the one mock from before the stop.
			instr := &stopCallComposeInstr{backlogComposeInstr: &backlogComposeInstr{composeInstr: &composeInstr{fakeInstr: f, stackDown: make(chan struct{})}, held: 1}}
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
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx) }()
			select {
			case f.outgoing <- mockAt("before-stop", time.Now()):
				instr.taken.Add(1)
			case <-time.After(30 * time.Second):
				t.Fatal("the mock from before the stop was never taken")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("Start did not return")
			}
			if !instr.drainBegan.Load() {
				t.Fatal("the agent was never asked what it held: the row did not reach the drain")
			}
			mockDB.mu.Lock()
			defer mockDB.mu.Unlock()
			if !slices.Contains(mockDB.inserted, "stop-time-call") || !slices.Contains(mockDB.inserted, "before-stop") {
				t.Fatalf("persisted %v; want the call the app made as it stopped beside the one before the stop", mockDB.inserted)
			}
		})
	}
}
