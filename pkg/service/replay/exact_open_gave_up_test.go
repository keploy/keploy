package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// agentGaveUp is what keploy's agent answers a filter that opens a test case
// when the settle of the calls made before it gave up on work its proxy held
// (the agent's settle watchdog; its rows: TestKeployTestHoldsAnOpenForAPool
// CallItsRequestWokeLate, TestANoProgressSettleNamesWhatItWaitedOn).
const agentGaveUp = "keploy's agent made no progress settling the test's calls; it gave up on connection 7 from 127.0.0.1:41234, which held 1 request(s) its parser had read and not answered (no later wait waits for that work)"

// gaveUpSvc is keploy's agent's service behind its real routes: it opens
// every test case but gaveUp, whose open fails as the agent's does when the
// settle before it gave up (agentGaveUp, an error that Is
// models.ErrSettleGaveUp), or, with wordsOnly, with the same words and no
// such cause.
type gaveUpSvc struct {
	agentsvc.Service
	gaveUp    string
	wordsOnly bool
	mu        sync.Mutex
	refused   int
}

func (s *gaveUpSvc) UpdateMockParams(_ context.Context, p models.MockFilterParams) error {
	if ex := p.Exact; ex != nil && ex.Test == s.gaveUp {
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		if s.wordsOnly {
			return errors.New(agentGaveUp)
		}
		return fmt.Errorf("%w%s", models.ErrSettleGaveUp, strings.TrimPrefix(agentGaveUp, models.ErrSettleGaveUp.Error()))
	}
	return nil
}

// gaveUpOpener is gaveUpSvc for an agent that opens a test case in one step
// (the agent routes' testCaseOpener, under the proven switch), as keploy's
// own agent does: the open refuses as its filter does.
type gaveUpOpener struct{ *gaveUpSvc }

func (s gaveUpOpener) OpenTestCase(ctx context.Context, p models.MockFilterParams) error {
	return s.UpdateMockParams(ctx, p)
}

// gaveUpWire is viewInstr whose filters reach the agent's service through
// keploy's agent client and the agent's routes first, as a replay's do.
type gaveUpWire struct {
	*viewInstr
	client *httpclient.AgentClient
}

func (w *gaveUpWire) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	if err := w.client.UpdateMockParams(ctx, p); err != nil {
		return err
	}
	return w.viewInstr.UpdateMockParams(ctx, p)
}

// gaveUpRun runs a set of test-1..test-3 (streamed: the ones the streaming
// pass opens) under exact serving, whose filters reach svc, which refuses
// test-2's open, through keploy's agent client and the agent's real routes.
func gaveUpRun(t *testing.T, svc *gaveUpSvc, streamed []string, oneStep bool) (h *prRun, status models.TestSetStatus, events []string, logs *observer.ObservedLogs) {
	t.Helper()
	h, vi := exactViewRun(t, nil, streamed...)
	logs = observeLog(h)
	router := chi.NewRouter()
	var behind agentsvc.Service = svc
	if oneStep {
		behind = gaveUpOpener{svc}
	}
	routes.DefaultRoutes{}.New(router, behind, zap.NewNop())
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := httpclient.New(zap.NewNop(), nil, &config.Config{
		Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: srv.URL + "/agent"}},
	})
	h.replayer.instrumentation = &gaveUpWire{viewInstr: vi, client: client}
	status = h.run(t)
	events = vi.trace()
	svc.mu.Lock()
	refused := svc.refused
	svc.mu.Unlock()
	if refused != 1 {
		t.Fatalf("precondition: the agent refused test-2's open %d times; want once: %v", refused, events)
	}
	return h, status, events, logs
}

// gaveUpLoops are where a test case opens, the main loop and the streaming
// pass, each with an agent that opens a test case by its filter and one that
// opens it in one step (only under the proven switch: without it the agent's
// routes apply the filter either way).
var gaveUpLoops = func() []gaveUpLoop {
	var loops []gaveUpLoop
	for _, oneStep := range []bool{false, true} {
		for _, l := range []gaveUpLoop{
			{name: "the main loop"},
			// test-3 would open on the streaming pass, which the stop skips.
			{name: "the main loop, with a streamed test case after it", streamed: []string{"test-3"}},
			{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
		} {
			if l.oneStep = oneStep; oneStep {
				l.name += ", opened in one step"
			}
			loops = append(loops, l)
		}
	}
	return loops
}()

type gaveUpLoop struct {
	name     string
	streamed []string
	oneStep  bool
}

// A test case keploy's agent does not open, because the settle of the calls
// made before it gave up on work keploy's proxy held, is keploy's own
// failure, on the main loop and on the streaming pass: the set is not judged
// (INTERNAL_ERR), its report names the test case not opened, the work the
// agent waited on, and that nothing in the application is to be changed for
// it, no later test case is opened (on the streaming pass either), and the
// run goes on to the next set. The agent says the cause as data
// (models.SettleGaveUpHeader), which its client returns as an error that Is
// models.ErrSettleGaveUp. Before, the main loop ended the set FAILED as a
// run stopped early, its reason sending the reader to the application's
// logs, and still opened the streaming pass's test cases; the streaming pass
// ended it with a generic internal error.
func TestATestCaseOpenKeploysAgentGaveUpOnIsKeploysOwnFailure(t *testing.T) {
	for _, loop := range gaveUpLoops {
		t.Run(loop.name, func(t *testing.T) {
			h, status, events, logs := gaveUpRun(t, &gaveUpSvc{gaveUp: "test-2"}, loop.streamed, loop.oneStep)
			if status != models.TestSetStatusInternalErr {
				t.Errorf("the set reported %q; want %q (keploy's own failure): %v", status, models.TestSetStatusInternalErr, events)
			}
			for _, e := range events {
				if e == "test:test-3" {
					t.Errorf("test-3 was opened after the agent did not open test-2: %v", events)
				}
			}
			notBegunReported(t, h, logs, "test-2", events)
			reason := h.report.report.FailureReason
			for _, want := range []string{agentGaveUp, notBegunOwnFailure} {
				if !strings.Contains(reason, want) {
					t.Errorf("the report's reason %q does not say %q", reason, want)
				}
			}
			if strings.Contains(reason, "app_logs") {
				t.Errorf("the report's reason %q sends the reader to the application's logs for keploy's own failure", reason)
			}
		})
	}
}

// Only the agent's typed signal makes a refused test case open keploy's own
// give-up, never the message's words: an agent's refusal that reads the same
// with no such cause (an agent that predates the signal, or another
// failure) stops the set as every other refusal does, with nothing of the
// give-up's not-judged report.
func TestARefusedOpenThatOnlyReadsLikeAGiveUpKeepsItsStatus(t *testing.T) {
	for _, loop := range gaveUpLoops {
		t.Run(loop.name, func(t *testing.T) {
			h, status, events, _ := gaveUpRun(t, &gaveUpSvc{gaveUp: "test-2", wordsOnly: true}, loop.streamed, loop.oneStep)
			reason := h.report.report.FailureReason
			for _, never := range []string{notBegunOwnFailure, "did not begin test case test-2"} {
				if strings.Contains(reason, never) {
					t.Errorf("the report's reason %q says %q for a refusal the agent did not say was its give-up: %v", reason, never, events)
				}
			}
			if status == models.TestSetStatusPassed {
				t.Errorf("the set passed though the agent refused test-2's open: %v", events)
			}
		})
	}
}
