package replay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Hooks.simulateConsumer is the replay side's arm-and-await, and until this
// file existed nothing had ever run it: ConsumerInstrumentation had an
// interface declaration, two type assertions and NO implementation anywhere,
// so replacing simulateConsumer's whole body with a constant left the suite
// green. Worse, it was unproven that a consumer.Gate could even implement the
// interface it was designed for — the Gate's Arm/Complete signatures and the
// interface's ArmConsumerTrigger/AwaitConsumerEffects had never been composed.
// Publishing that on a tagged module would have made slice 6, in another
// repository, the first thing to find out.
//
// gateAgent below is that composition. It is backed by a REAL consumer.Gate
// and a real Deliverer, so these tests exercise the actual state machine
// rather than a mock of it.

// ---------------------------------------------------------------------------
// A minimal Instrumentation. Every method is a stub except the three consumer
// ones: SimulateRequest's consumer arm touches nothing else.
// ---------------------------------------------------------------------------

type stubInstrumentation struct{}

func (stubInstrumentation) Setup(context.Context, string, models.SetupOptions) error { return nil }
func (stubInstrumentation) MockOutgoing(context.Context, models.OutgoingOptions) error {
	return nil
}
func (stubInstrumentation) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, nil
}
func (stubInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	return models.AppError{}
}
func (stubInstrumentation) GetErrorChannel() <-chan error { return nil }
func (stubInstrumentation) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}
func (stubInstrumentation) BeforeSimulate(context.Context, *time.Time, string, string) error {
	return nil
}
func (stubInstrumentation) AfterSimulate(context.Context, string, string) error { return nil }
func (stubInstrumentation) BeforeTestRun(context.Context, string) error         { return nil }
func (stubInstrumentation) BeforeTestSetCompose(context.Context, string, string, bool) error {
	return nil
}
func (stubInstrumentation) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	return nil
}
func (stubInstrumentation) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return nil
}
func (stubInstrumentation) UpdateMockParams(context.Context, models.MockFilterParams) error {
	return nil
}
func (stubInstrumentation) GetRecentAppLogs(context.Context) string              { return "" }
func (stubInstrumentation) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (stubInstrumentation) NotifyGracefulShutdown(context.Context) error         { return nil }
func (stubInstrumentation) ComposeDownOnSetupFailure(context.Context) error      { return nil }

// ---------------------------------------------------------------------------
// gateAgent: a real *consumer.Gate wearing the ConsumerInstrumentation
// interface. THIS COMPOSITION IS THE POINT — if the two shapes ever stop
// fitting, this file stops compiling, here, rather than in another repository
// after the tag.
// ---------------------------------------------------------------------------

type gateAgent struct {
	stubInstrumentation
	gate *consumer.Gate
	// trigger is the mock the agent would have pulled out of the armed test's
	// mock pool. Nil models "the pool had nothing for this test".
	trigger *models.Mock
	// armErr / awaitErr force the two failure paths.
	armErr, awaitErr error
	// nilResult forces the "agent reported no window" path.
	nilResult bool
	// resetErr forces the "the reset CALL failed" path — an unreachable
	// agent, a 501 from a build that predates the route. It must never be
	// reported as the worker over-producing.
	resetErr error
	resets   atomic.Int32
}

var _ ConsumerInstrumentation = (*gateAgent)(nil)

func (a *gateAgent) ArmConsumerTrigger(ctx context.Context, arm models.ConsumerArm) error {
	if a.armErr != nil {
		return a.armErr
	}
	return a.gate.Arm(ctx, arm, a.trigger)
}

func (a *gateAgent) AwaitConsumerEffects(ctx context.Context, testID string) (*models.ConsumerResult, error) {
	if a.awaitErr != nil {
		return nil, a.awaitErr
	}
	if a.nilResult {
		return nil, nil
	}
	res := a.gate.Complete(ctx, testID)
	return &res, nil
}

func (a *gateAgent) ResetConsumerGate(_ context.Context, _ string) (int, error) {
	a.resets.Add(1)
	trailing := a.gate.Reset()
	if a.resetErr != nil {
		return 0, a.resetErr
	}
	return trailing, nil
}

// workerDeliverer stands in for the application: when keploy hands it the
// trigger, it produces `views` back through the gate, exactly as a protocol
// parser would on seeing the worker's outgoing frame.
type workerDeliverer struct {
	gate  *consumer.Gate
	views []models.EffectView
	got   atomic.Int32
}

func (d *workerDeliverer) Deliver(_ context.Context, _ *models.Mock) error {
	d.got.Add(1)
	d.gate.MarkTriggerAccepted(d.gateArmedTest())
	for _, v := range d.views {
		d.gate.ObserveEffect(v)
	}
	return nil
}

func (d *workerDeliverer) gateArmedTest() string {
	if arm, ok := d.gate.ArmedTest(); ok {
		return arm.TestID
	}
	return ""
}

func consumerTestCase(name string, effects ...models.EffectView) *models.TestCase {
	records := 0
	for _, e := range effects {
		records += e.RecordCount()
	}
	return &models.TestCase{
		Kind: models.CONSUMER,
		Name: name,
		ConsumerSpec: &models.ConsumerSpec{
			Protocol: consumerfake.Protocol,
			Trigger:  consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`),
			Effects:  effects,
			Completion: models.ConsumerCompletion{
				ExpectEffects: records,
				GraceMs:       models.ConsumerGraceMinMs,
				TimeoutMs:     models.ConsumerDefaultTimeoutMs,
			},
		},
	}
}

func newConsumerHooks(inst Instrumentation) *Hooks {
	return &Hooks{logger: zap.NewNop(), cfg: &config.Config{}, instrumentation: inst}
}

// driveClock advances the fake clock until stopped, so a blocking
// Gate.Complete makes progress without a real sleep. Deterministic: the clock
// only ever moves forward, so every deadline is crossed in order.
func driveClock(clk *consumerfake.Clock) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
				clk.Advance(25 * time.Millisecond)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	return func() { close(done); <-stopped }
}

// THE HAPPY PATH, through the real gate: SimulateRequest arms the window, the
// deliverer hands the trigger to the "worker", the worker produces, the
// completion rule closes the window, and the judge passes it.
func TestSimulateConsumerArmsAwaitsAndPasses(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	gate := consumer.NewGate(zap.NewNop(), clk)
	produced := consumerfake.View("produce", "order-events", "o-1", `{"orderId":"o-1","status":"CONFIRMED"}`)

	d := &workerDeliverer{gate: gate, views: []models.EffectView{produced}}
	gate.RegisterDeliverer(consumerfake.Protocol, d)

	agent := &gateAgent{gate: gate, trigger: consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger,
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	})}
	h := newConsumerHooks(agent)
	tc := consumerTestCase("test-1", produced)

	stop := driveClock(clk)
	resp, err := h.SimulateRequest(context.Background(), tc, "set-0")
	stop()
	if err != nil {
		t.Fatalf("SimulateRequest: %v", err)
	}
	res, ok := resp.(*models.ConsumerResult)
	if !ok {
		t.Fatalf("SimulateRequest must return *models.ConsumerResult for a CONSUMER test, got %T", resp)
	}
	if d.got.Load() != 1 {
		t.Fatalf("the trigger reached the application %d times, want 1", d.got.Load())
	}
	if res.TestID != "test-1" {
		t.Fatalf("result is for %q", res.TestID)
	}
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q (%s)", res.EndReason, res.RefusalDetail)
	}
	if !res.TriggerAccepted {
		t.Fatal("the positive delivery check must be reported")
	}
	if res.ObservedEffects != 1 || res.ExpectEffects != 1 {
		t.Fatalf("observed=%d expected=%d", res.ObservedEffects, res.ExpectEffects)
	}

	// And the whole loop: the judge grades what the gate handed back.
	r := &Replayer{logger: zap.NewNop()}
	pass, _ := r.CompareEffects(tc, res, "set-0", true, depChecked)
	if !pass {
		t.Fatal("a worker that reproduced its recording must pass end to end")
	}
}

// A worker that stopped producing must reach the judge as a real, named
// failure — not as an error, which would route the test through
// CreateFailedTestResult with no verdict on it.
func TestSimulateConsumerReportsAWorkerThatStoppedProducing(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	gate := consumer.NewGate(zap.NewNop(), clk)
	produced := consumerfake.View("produce", "order-events", "o-1", `{"a":1}`)

	// The deliverer accepts the trigger and produces NOTHING.
	d := &workerDeliverer{gate: gate}
	gate.RegisterDeliverer(consumerfake.Protocol, d)
	agent := &gateAgent{gate: gate, trigger: consumerfake.Mock(consumerfake.MockOptions{Name: "trigger-1", Role: models.RoleTrigger})}
	h := newConsumerHooks(agent)
	tc := consumerTestCase("test-1", produced)

	stop := driveClock(clk)
	resp, err := h.SimulateRequest(context.Background(), tc, "set-0")
	stop()
	if err != nil {
		t.Fatalf("SimulateRequest: %v", err)
	}
	res := resp.(*models.ConsumerResult)
	if res.EndReason != models.ConsumerEndReasonTimeout {
		t.Fatalf("end reason %q, want timeout: the count was never reached", res.EndReason)
	}

	r := &Replayer{logger: zap.NewNop()}
	pass, result := r.CompareEffects(tc, res, "set-0", true, depChecked)
	if pass {
		t.Fatal("THE FLAGSHIP REGRESSION must never pass")
	}
	if len(result.FailureInfo.Category) == 0 {
		t.Fatal("a failure with no category is one an agent loop cannot act on")
	}
}

// Every refusal path returns a *models.ConsumerResult carrying a named
// Refusal, never an error. An error would route the test through
// CreateFailedTestResult, which builds a result with no consumer verdict and
// no failure category — red with nothing naming why.
func TestSimulateConsumerRefusalsAreResultsNotErrors(t *testing.T) {
	newGate := func() *consumer.Gate {
		clk := consumerfake.NewClock(time.Time{})
		g := consumer.NewGate(zap.NewNop(), clk)
		g.RegisterDeliverer(consumerfake.Protocol, &workerDeliverer{gate: g})
		return g
	}

	t.Run("no spec", func(t *testing.T) {
		h := newConsumerHooks(&gateAgent{gate: newGate()})
		resp, err := h.SimulateRequest(context.Background(), &models.TestCase{Kind: models.CONSUMER, Name: "test-1"}, "set-0")
		if err != nil {
			t.Fatalf("must not be an error: %v", err)
		}
		res := resp.(*models.ConsumerResult)
		if res.Refusal != models.CategoryConsumerUnsupportedSpec {
			t.Fatalf("refusal %q", res.Refusal)
		}
	})

	t.Run("agent without consumer instrumentation", func(t *testing.T) {
		// THE ASSERTION FAILING IS A REFUSAL, NEVER A DEGRADATION. An agent
		// that cannot arm a window never delivers the message, so the worker
		// produces nothing and the test would otherwise report "the worker
		// stopped producing" — blaming the application for a missing
		// capability in keploy.
		h := newConsumerHooks(stubInstrumentation{})
		resp, err := h.SimulateRequest(context.Background(), consumerTestCase("test-1"), "set-0")
		if err != nil {
			t.Fatalf("must not be an error: %v", err)
		}
		res := resp.(*models.ConsumerResult)
		if res.Refusal != models.CategoryConsumerUnsupportedAgent {
			t.Fatalf("refusal %q", res.Refusal)
		}
	})

	t.Run("arm failed", func(t *testing.T) {
		h := newConsumerHooks(&gateAgent{gate: newGate(), armErr: errors.New("no trigger mock in the pool")})
		resp, _ := h.SimulateRequest(context.Background(), consumerTestCase("test-1"), "set-0")
		res := resp.(*models.ConsumerResult)
		if res.Refusal != models.CategoryConsumerTriggerNotDelivered {
			t.Fatalf("refusal %q", res.Refusal)
		}
		if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
			t.Fatalf("end reason %q must agree with the category", res.EndReason)
		}
	})

	t.Run("await failed", func(t *testing.T) {
		h := newConsumerHooks(&gateAgent{gate: newGate(), awaitErr: errors.New("agent stream closed"), trigger: consumerfake.Mock(consumerfake.MockOptions{Name: "t"})})
		resp, _ := h.SimulateRequest(context.Background(), consumerTestCase("test-1"), "set-0")
		res := resp.(*models.ConsumerResult)
		if res.Refusal != models.CategoryConsumerUnsupportedAgent {
			t.Fatalf("refusal %q", res.Refusal)
		}
	})

	t.Run("agent reported no window", func(t *testing.T) {
		h := newConsumerHooks(&gateAgent{gate: newGate(), nilResult: true, trigger: consumerfake.Mock(consumerfake.MockOptions{Name: "t"})})
		resp, _ := h.SimulateRequest(context.Background(), consumerTestCase("test-1"), "set-0")
		res := resp.(*models.ConsumerResult)
		if res.Refusal != models.CategoryConsumerUnsupportedAgent {
			t.Fatalf("refusal %q", res.Refusal)
		}
	})

	// Every one of those must reach the judge as a FAILED test with the
	// category the gate named — never a pass.
	t.Run("none of them can produce a pass", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop()}
		for _, cat := range []models.FailureCategory{
			models.CategoryConsumerUnsupportedSpec,
			models.CategoryConsumerUnsupportedAgent,
			models.CategoryConsumerTriggerNotDelivered,
		} {
			tc := consumerTestCase("test-1", consumerfake.View("produce", "t", "k", `{"a":1}`))
			pass, result := r.CompareEffects(tc, &models.ConsumerResult{
				TestID: "test-1", Refusal: cat, EndReason: models.ConsumerEndReasonInternalError,
			}, "set-0", true, depChecked)
			if pass {
				t.Fatalf("%s produced a pass", cat)
			}
			if len(result.FailureInfo.Category) != 1 || result.FailureInfo.Category[0] != cat {
				t.Fatalf("%s: categories %v", cat, result.FailureInfo.Category)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// The two gate boundaries: the reset that OPENS a test set and the drain that
// CLOSES it. Both live on *Replayer, where the test cases are in scope.
// ---------------------------------------------------------------------------

// newGateAgent returns a *Replayer wired to a real consumer.Gate through the
// ConsumerInstrumentation seam, plus the agent so a test can count calls.
func newGateReplayer(t *testing.T) (*Replayer, *gateAgent, *consumer.Gate) {
	t.Helper()
	clk := consumerfake.NewClock(time.Time{})
	gate := consumer.NewGate(zap.NewNop(), clk)
	gate.RegisterDeliverer(consumerfake.Protocol, &workerDeliverer{gate: gate})
	agent := &gateAgent{gate: gate, trigger: consumerfake.Mock(consumerfake.MockOptions{Name: "t"})}
	return &Replayer{logger: zap.NewNop(), instrumentation: agent}, agent, gate
}

func consumerSetOf(names ...string) []*models.TestCase {
	out := make([]*models.TestCase, 0, len(names))
	for _, n := range names {
		out = append(out, consumerTestCase(n))
	}
	return out
}

// The start-of-set reset must return the gate to boot: --keep-app-alive reuses
// one application process across sets, so a gate left armed — or an effect
// adopted across the boundary — leaks one set's state into the next set's
// first test.
//
// AND IT MUST NOT RUN FOR A SET WITH NO CONSUMER TEST IN IT. This used to live
// in Hooks.BeforeTestSetReplay, which has no test cases in scope; the shipping
// instrumentation implements ConsumerInstrumentation unconditionally, so every
// `keploy test` run — a pure HTTP suite included — made an extra agent round
// trip per test set. The HTTP row below is that pin, and it deliberately uses
// an agent that DOES implement the interface, because that is the shipping
// shape: an agent that does not would pass the row for the wrong reason.
func TestResetConsumerGateAtTheStartOfASet(t *testing.T) {
	t.Run("a consumer set opens with a default-closed gate", func(t *testing.T) {
		r, agent, gate := newGateReplayer(t)

		// Leave the gate armed, as an interrupted set would.
		if err := gate.Arm(context.Background(), models.ConsumerArm{
			TestID: "test-1", Protocol: consumerfake.Protocol,
			Completion: models.ConsumerCompletion{ExpectEffects: 1},
		}, agent.trigger); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		if gate.Phase() != consumer.PhaseArmed {
			t.Fatal("precondition: the gate should be armed")
		}

		r.resetConsumerGate(context.Background(), "set-1", consumerSetOf("test-1"))
		if agent.resets.Load() != 1 {
			t.Fatalf("ResetConsumerGate called %d times, want 1", agent.resets.Load())
		}
		if gate.Phase() != consumer.PhaseBoot {
			t.Fatalf("phase %q after a test-set boundary, want boot", gate.Phase())
		}
	})

	t.Run("an HTTP-only set makes no consumer call at all", func(t *testing.T) {
		r, agent, _ := newGateReplayer(t)
		r.resetConsumerGate(context.Background(), "set-1", []*models.TestCase{{Kind: models.HTTP}})
		if agent.resets.Load() != 0 {
			t.Fatalf("an HTTP-only test set made %d consumer call(s); this path must be untouched by the consumer slice", agent.resets.Load())
		}
	})

	t.Run("an agent without the capability is silent", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), instrumentation: stubInstrumentation{}}
		r.resetConsumerGate(context.Background(), "set-1", consumerSetOf("test-1"))
	})
}

// BeforeTestSetReplay must NOT touch the gate any more. Left there it fired
// for every test set of every run, consumer or not.
func TestBeforeTestSetReplayMakesNoConsumerCall(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	gate := consumer.NewGate(zap.NewNop(), clk)
	agent := &gateAgent{gate: gate}
	h := newConsumerHooks(agent)

	if err := h.BeforeTestSetReplay(context.Background(), "set-1"); err != nil {
		t.Fatalf("BeforeTestSetReplay: %v", err)
	}
	if agent.resets.Load() != 0 {
		t.Fatalf("the hook made %d consumer call(s); the reset belongs in RunTestSet, where containsConsumerTest can short-circuit it", agent.resets.Load())
	}
	if err := newConsumerHooks(stubInstrumentation{}).BeforeTestSetReplay(context.Background(), "set-1"); err != nil {
		t.Fatalf("an HTTP-only run must be unaffected: %v", err)
	}
}

// drainConsumerGate is the ONLY place an over-production after the last test
// of the last set can ever be seen: within a set a late effect fails the next
// test as an extra, and the last test has no next test.
//
// Both adjacent links were pinned before this test existed and the MIDDLE one
// — "a trailing count turns into TestSetStatusFailed" — was not, so replacing
// this function's body with `return false` left the whole package green while
// silently reopening the N+1 regression on exactly the suffix of a run that
// nothing else watches.
func TestDrainConsumerGate(t *testing.T) {
	consumerSet := consumerSetOf("test-1")
	httpSet := []*models.TestCase{{Kind: models.HTTP}}

	t.Run("a trailing effect fails the set", func(t *testing.T) {
		r, agent, gate := newGateReplayer(t)
		// The worker produced after the last test's window closed.
		gate.ObserveEffect(consumerfake.View("produce", "order-events", "o-9", `{"a":1}`))

		if !r.drainConsumerGate(context.Background(), "set-1", consumerSet) {
			t.Fatal("an effect that belongs to no test must fail the test set; it is the N+1 emission the mandatory grace drain exists to catch")
		}
		if agent.resets.Load() != 1 {
			t.Fatalf("ResetConsumerGate called %d times, want 1", agent.resets.Load())
		}
	})

	t.Run("a clean set is not failed", func(t *testing.T) {
		r, agent, _ := newGateReplayer(t)
		if r.drainConsumerGate(context.Background(), "set-1", consumerSet) {
			t.Fatal("a set whose gate had nothing left over must not be failed")
		}
		if agent.resets.Load() != 1 {
			t.Fatalf("the drain must still happen on a clean set, calls=%d", agent.resets.Load())
		}
	})

	t.Run("a failed reset CALL is not the worker over-producing", func(t *testing.T) {
		// A 501 from an agent that predates the route, a 500, a dropped
		// connection. Reporting these as an over-production sends whoever
		// reads the report to debug their worker's emissions for a keploy
		// failure — the exact confusion Gate.abortArm's comment names.
		r, agent, _ := newGateReplayer(t)
		agent.resetErr = errors.New("reset consumer gate returned status 501")
		if r.drainConsumerGate(context.Background(), "set-1", consumerSet) {
			t.Fatal("an infrastructure failure must not be reported as an application regression")
		}
	})

	t.Run("an HTTP-only set makes no consumer call at all", func(t *testing.T) {
		r, agent, _ := newGateReplayer(t)
		if r.drainConsumerGate(context.Background(), "set-1", httpSet) {
			t.Fatal("an HTTP-only set has no gate to drain")
		}
		if agent.resets.Load() != 0 {
			t.Fatalf("an HTTP-only test set made %d consumer call(s)", agent.resets.Load())
		}
	})

	t.Run("an agent without the capability is silent", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), instrumentation: stubInstrumentation{}}
		if r.drainConsumerGate(context.Background(), "set-1", consumerSet) {
			t.Fatal("an agent that cannot reset a gate has not proved the worker over-produced")
		}
	})
}

// THE ROUND TRIP, ACROSS THE SEAM THE TWO HALVES OF THIS SLICE DISAGREED ON.
//
// The recorder deliberately mints a unit that produced nothing but made calls
// of another protocol family — consume-and-write-to-a-database, one of the two
// most common consumer shapes. The judge used to refuse exactly that shape as
// vacuous, so such a test recorded cleanly, persisted, and then failed 100% of
// the time at replay with a message telling the user to re-record or delete a
// test keploy had just told them it recorded successfully.
//
// This starts from a RECORDER-MINTED test case rather than a hand-written one,
// which is the only way the two halves can be checked against each other.
func TestARecorderMintedConsumeAndWriteTestPasses(t *testing.T) {
	t.Cleanup(consumerfake.Register())

	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 64))
	ctx := syncMock.NewContext(context.Background(), mgr)

	clk := consumerfake.NewClock(time.Time{})
	minted := make(chan *models.TestCase, 4)
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.NewNop(), Clock: clk, TestCases: minted,
	})
	// The production side-effect ingest (Proxy.installConsumerEgressObserver).
	// The write below arrives the way a real one does — through the postgres
	// parser, straight to the syncMock manager, with the recorder never told —
	// so this test also proves the choke point is what makes the shape
	// recordable at all.
	mgr.SetEgressObserver(rec.OnEgress)

	base := clk.Now()
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	// The database write: a mock of a DIFFERENT protocol family, no role tag,
	// emitted by a parser that has never heard of the consumer contract.
	mgr.AddMock(consumerfake.Mock(consumerfake.MockOptions{
		Name: "postgres-insert", Kind: consumerfake.SideEffectKind,
		ReqAt: base.Add(10 * time.Millisecond), ResAt: base.Add(12 * time.Millisecond),
	}))
	stats := rec.Close(ctx)
	if err := stats.Err(); err != nil {
		t.Fatalf("the recording must reconcile: %v", err)
	}

	var tc *models.TestCase
	select {
	case tc = <-minted:
	default:
		t.Fatal("a consume-and-write unit must be minted, not refused")
	}

	// Exactly the shape the recorder writes: no effects, no expected count.
	if len(tc.ConsumerSpec.Effects) != 0 || tc.ConsumerSpec.Completion.ExpectEffects != 0 {
		t.Fatalf("unexpected minted shape: %+v", tc.ConsumerSpec)
	}

	// A clean replay of it: the worker did its write, which the gate cannot
	// see, so the window closed with nothing observed.
	res := &models.ConsumerResult{
		TestID: tc.Name, TriggerAccepted: true,
		ExpectEffects: 0, ObservedEffects: 0,
		EndReason: models.ConsumerEndReasonCountReached,
	}
	v := judge(tc, res)
	if !v.Pass {
		t.Fatalf("a recorder-minted consume-and-write test must be judgeable and pass: categories=%v summary=%s",
			categoryStrings(v.Categories), v.Summary)
	}

	// AND THE SAME TEST WITH NOTHING TO CHECK IT AGAINST IS REFUSED. The pass
	// above rests ENTIRELY on the sync path's presence rows having run for
	// this test; spec.SideEffects is a record-time count and nothing at replay
	// turns it into an assertion. Judged without that, this exact minted test
	// used to return Pass with zero rows and zero categories — the flagship
	// "the worker stopped writing" regression reported as verified_green.
	refused := compareEffects(tc, res, consumerDepAssertion{})
	if refused.Pass {
		t.Fatal("the same test, with its only claim unverifiable, must be FAILED by name")
	}
	if len(refused.Categories) != 1 || refused.Categories[0] != models.CategoryConsumerMappingsRequired {
		t.Fatalf("categories %v, want CONSUMER_MAPPINGS_REQUIRED", refused.Categories)
	}
}

// And the genuinely vacuous unit — nothing produced, nothing written — is
// still refused by BOTH halves: the recorder never mints one, and the judge
// refuses one that reached the file by hand.
func TestAUnitWhereNothingHappenedIsRefusedByBothHalves(t *testing.T) {
	t.Cleanup(consumerfake.Register())

	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 64))
	ctx := syncMock.NewContext(context.Background(), mgr)

	clk := consumerfake.NewClock(time.Time{})
	minted := make(chan *models.TestCase, 4)
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.NewNop(), Clock: clk, TestCases: minted,
	})
	base := clk.Now()
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	stats := rec.Close(ctx)

	select {
	case tc := <-minted:
		t.Fatalf("a unit that can only ever pass must not be minted, got %q", tc.Name)
	default:
	}
	if stats.UnitsRefused != 1 || stats.Refusals[0].Category != models.CategoryConsumerNoObservableEffect {
		t.Fatalf("want one no-observable-effect refusal, got %+v", stats.Refusals)
	}

	// The judge refuses the same shape if it reaches a file by hand.
	handWritten := consumerTestCase("test-1")
	v := judge(handWritten, &models.ConsumerResult{
		TestID: "test-1", EndReason: models.ConsumerEndReasonCountReached,
	})
	if v.Pass {
		t.Fatal("a spec that asserts nothing must be refused, not passed")
	}
	if len(v.Categories) != 1 || v.Categories[0] != models.CategoryConsumerNoObservableEffect {
		t.Fatalf("categories %v", v.Categories)
	}
}
