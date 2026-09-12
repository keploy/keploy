package http_test

// THE CONSUMER SEAM, END TO END, ACROSS THE PROCESS BOUNDARY.
//
// replay.ConsumerInstrumentation is reached by type assertion, and what
// `keploy test` actually holds is an *http.AgentClient — cli/provider hands the
// replayer that client, never the in-process agent service. So the seam is only
// closed if THREE things line up: the client implements the interface, the
// agent exposes the routes, and the two agree on the wire shape. Any one of
// them missing makes every consumer test in every deployment fail
// CONSUMER_UNSUPPORTED_AGENT, which is a different and much worse thing than
// "inert because OSS ships no consumer protocol parser".
//
// The mutation this exists to kill: delete any of the three routes, or any of
// the three client methods, and nothing else in the tree notices.

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	khttp "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.uber.org/zap"
)

// The client must satisfy the interface RunTestSet type-asserts for. Without
// this the seam compiles and is never taken.
var _ replay.ConsumerInstrumentation = (*khttp.AgentClient)(nil)

// fakeConsumerAgent is an agentsvc.Service that records what the routes handed it
// and answers with a fixed result.
type fakeConsumerAgent struct {
	agentsvc.Service
	armed    []models.ConsumerArm
	awaited  []string
	resetFor []string
	result   models.ConsumerResult
	armErr   error
	resetErr error
	// trailing is the over-production count the agent reports back. It has
	// to reach the replayer as DATA: as an error it could not be told apart
	// from an unreachable agent.
	trailing int
}

func (f *fakeConsumerAgent) ArmConsumerTrigger(_ context.Context, arm models.ConsumerArm) error {
	f.armed = append(f.armed, arm)
	return f.armErr
}

func (f *fakeConsumerAgent) AwaitConsumerEffects(_ context.Context, testID string) (*models.ConsumerResult, error) {
	f.awaited = append(f.awaited, testID)
	res := f.result
	res.TestID = testID
	return &res, nil
}

func (f *fakeConsumerAgent) ResetConsumerGate(_ context.Context, testSetID string) (int, error) {
	f.resetFor = append(f.resetFor, testSetID)
	return f.trailing, f.resetErr
}

// plainAgent implements agentsvc.Service and nothing else — the older-agent case.
type plainAgent struct{ agentsvc.Service }

func newSeam(t *testing.T, svc agentsvc.Service) (*khttp.AgentClient, func()) {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	return khttp.New(zap.NewNop(), nil, cfg), srv.Close
}

func TestTheConsumerSeamIsClosedOverHTTP(t *testing.T) {
	fake := &fakeConsumerAgent{
		trailing: 3,
		result: models.ConsumerResult{
			TriggerAccepted: true,
			ExpectEffects:   2,
			ObservedEffects: 2,
			EndReason:       models.ConsumerEndReasonCountReached,
			Effects: []models.EffectView{{
				Protocol: "fake", Op: "produce", Target: "order-events", Key: "o-1",
				Body: `{"status":"CONFIRMED"}`, BodyType: models.JSON,
				Decoded: models.DecodedConfident, Records: 1,
			}},
		},
	}
	client, stop := newSeam(t, fake)
	defer stop()

	arm := models.ConsumerArm{
		TestID:    "test-7",
		TestSetID: "test-set-0",
		Protocol:  "fake",
		Trigger: models.EffectView{
			Protocol: "fake", Op: "fetch", Target: "orders", Key: "o-1",
			Coords: map[string]string{"partition": "0", "offset": "1840"},
		},
		Completion: models.ConsumerCompletion{ExpectEffects: 2, GraceMs: 250, TimeoutMs: 5000},
	}
	if err := client.ArmConsumerTrigger(context.Background(), arm); err != nil {
		t.Fatalf("ArmConsumerTrigger: %v", err)
	}
	if len(fake.armed) != 1 {
		t.Fatalf("the agent was armed %d times, want 1", len(fake.armed))
	}
	got := fake.armed[0]
	// The whole arm has to survive the round trip: the completion rule is
	// what the gate's window is opened with, and the judge cross-checks the
	// expected count it comes back with against the test's own spec.
	if got.TestID != arm.TestID || got.TestSetID != arm.TestSetID || got.Protocol != arm.Protocol {
		t.Fatalf("arm identity did not survive the round trip: %+v", got)
	}
	if got.Completion != arm.Completion {
		t.Fatalf("completion rule = %+v, want %+v", got.Completion, arm.Completion)
	}
	if got.Trigger.Target != "orders" || got.Trigger.Coords["offset"] != "1840" {
		t.Fatalf("the recorded trigger view did not survive the round trip: %+v", got.Trigger)
	}

	res, err := client.AwaitConsumerEffects(context.Background(), "test-7")
	if err != nil {
		t.Fatalf("AwaitConsumerEffects: %v", err)
	}
	if res.TestID != "test-7" || !res.TriggerAccepted || res.ObservedEffects != 2 ||
		res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("the delivery window did not survive the round trip: %+v", res)
	}
	if len(res.Effects) != 1 || res.Effects[0].Body != `{"status":"CONFIRMED"}` {
		t.Fatalf("the observed effect payloads must survive: without them the judge has nothing to diff: %+v", res.Effects)
	}

	trailing, err := client.ResetConsumerGate(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("ResetConsumerGate: %v", err)
	}
	// The trailing count has to survive the HTTP boundary as DATA. Carried as
	// an error it was indistinguishable from an unreachable agent, and the
	// replayer blamed the worker for keploy's own transport failure.
	if trailing != 3 {
		t.Fatalf("trailing effect count = %d, want 3: it is the only evidence of an over-production after the last test of a set", trailing)
	}
	if len(fake.resetFor) != 1 || fake.resetFor[0] != "test-set-0" {
		t.Fatalf("reset reached the agent as %v", fake.resetFor)
	}
}

// A refusal is a RESULT, not a transport error: it is a named FAILED verdict
// the judge renders. Returning it as an error would route the test through
// CreateFailedTestResult with nothing naming the cause.
func TestARefusalComesBackAsAResultNotAnError(t *testing.T) {
	fake := &fakeConsumerAgent{result: models.ConsumerResult{
		EndReason:     models.ConsumerEndReasonInternalError,
		Refusal:       models.CategoryConsumerRunCancelled,
		RefusalDetail: "the run was cancelled while waiting for this test's effects",
	}}
	client, stop := newSeam(t, fake)
	defer stop()

	res, err := client.AwaitConsumerEffects(context.Background(), "test-7")
	if err != nil {
		t.Fatalf("a refusal must not surface as a transport error: %v", err)
	}
	if res.Refusal != models.CategoryConsumerRunCancelled {
		t.Fatalf("refusal = %q, want %s", res.Refusal, models.CategoryConsumerRunCancelled)
	}
}

// An agent that predates the routes must say so, loudly. It is reported as an
// error rather than swallowed the way BeginTestErrorCapture's 404 is: a
// missing error-capture window degrades to the legacy global queue and loses
// nothing, while a missing delivery window means the recorded message never
// reached the worker at all — which would report as "the worker stopped
// producing".
func TestAnAgentWithoutTheCapabilityIsNamedNotSwallowed(t *testing.T) {
	client, stop := newSeam(t, &plainAgent{})
	defer stop()

	err := client.ArmConsumerTrigger(context.Background(), models.ConsumerArm{TestID: "test-1"})
	if err == nil {
		t.Fatal("arming an agent that cannot open a delivery window must fail; swallowing it would " +
			"report the worker as broken for a capability keploy does not have")
	}
	if _, err := client.AwaitConsumerEffects(context.Background(), "test-1"); err == nil {
		t.Fatal("awaiting an agent that cannot open a delivery window must fail")
	}
	if _, err := client.ResetConsumerGate(context.Background(), "test-set-0"); err == nil {
		t.Fatal("resetting an agent that cannot open a delivery window must fail")
	}
}

// An arm the agent refuses (no recorded trigger resident, no deliverer
// registered) reaches the replayer as an error, which SimulateRequest turns
// into a refusal result carrying CONSUMER_TRIGGER_NOT_DELIVERED.
func TestAnArmFailureReachesTheCaller(t *testing.T) {
	fake := &fakeConsumerAgent{armErr: context.DeadlineExceeded}
	client, stop := newSeam(t, fake)
	defer stop()

	if err := client.ArmConsumerTrigger(context.Background(), models.ConsumerArm{TestID: "test-1"}); err == nil {
		t.Fatal("an agent that could not arm the window must not report success")
	}
}
