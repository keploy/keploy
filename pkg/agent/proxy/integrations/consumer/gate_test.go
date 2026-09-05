package consumer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	"go.keploy.io/server/v3/pkg/models"
)

// testBudget is the wall-clock failure backstop for the fake-clock
// handshakes. No test SPENDS it on a passing run; it only bounds a hang.
const testBudget = 5 * time.Second

// recordingDeliverer captures what the gate handed to the application.
type recordingDeliverer struct {
	mu    sync.Mutex
	got   []*models.Mock
	fail  error
	calls int
}

func (d *recordingDeliverer) Deliver(_ context.Context, m *models.Mock) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.fail != nil {
		return d.fail
	}
	d.got = append(d.got, m)
	return nil
}

func (d *recordingDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.got)
}

func arm(testID string, expect int) models.ConsumerArm {
	return models.ConsumerArm{
		TestID:    testID,
		TestSetID: "test-set-0",
		Protocol:  consumerfake.Protocol,
		Trigger:   consumerfake.View("fetch", "orders", "k", `{"a":1}`),
		Completion: models.ConsumerCompletion{
			ExpectEffects: expect,
			GraceMs:       250,
			TimeoutMs:     5000,
		},
	}
}

func triggerMock() *models.Mock {
	return consumerfake.Mock(consumerfake.MockOptions{
		Name:  "mock-trigger",
		Role:  models.RoleTrigger,
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "k", `{"a":1}`)},
	})
}

// newGate wires a gate with a fake clock and a recording deliverer.
func newGate(t *testing.T) (*consumer.Gate, *consumerfake.Clock, *recordingDeliverer) {
	t.Helper()
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	d := &recordingDeliverer{}
	g.RegisterDeliverer(consumerfake.Protocol, d)
	return g, clk, d
}

// THE GATE IS DEFAULT-CLOSED (design §0.1, rule 1).
//
// This is the single most load-bearing property in the package. The whole mock
// pool is resident while the application boots — the replay path sends an
// empty mock-filter mapping before starting the app, and an empty mapping
// falls through to a load-everything path — and a consumer polls immediately.
// If delivery were permitted by pool residency, the worker would drain every
// trigger in the set before test-1 existed.
func TestDeliverIsRefusedInBootAndInDraining(t *testing.T) {
	g, clk, d := newGate(t)

	if got := g.Phase(); got != consumer.PhaseBoot {
		t.Fatalf("a fresh gate must be in boot, got %q", got)
	}
	err := g.Deliver(context.Background(), consumerfake.Protocol, triggerMock())
	if !errors.Is(err, consumer.ErrNotArmed) {
		t.Fatalf("boot: want ErrNotArmed, got %v", err)
	}
	if d.calls != 0 {
		t.Fatalf("boot: the deliverer was called %d times; nothing may reach the application before test-1 arms", d.calls)
	}

	// Armed: delivery is permitted, exactly once.
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if g.Phase() != consumer.PhaseArmed {
		t.Fatalf("after Arm the gate must be armed, got %q", g.Phase())
	}
	if d.count() != 1 {
		t.Fatalf("Arm must hand the trigger to the deliverer exactly once, got %d", d.count())
	}

	// Draining: refused again.
	//
	// The accept is not decoration. This test arms a window that expects zero
	// effects, and the completion rule refuses to close such a window on the
	// count until something proves the application actually ran — either it
	// produced, or the parser confirmed it took the trigger. Without this the
	// window would (correctly) time out as trigger_not_delivered.
	g.MarkTriggerAccepted("test-1")
	res := completeSync(t, g, clk, "test-1")
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason: got %q", res.EndReason)
	}
	if g.Phase() != consumer.PhaseDraining {
		t.Fatalf("after Complete the gate must be draining, got %q", g.Phase())
	}
	err = g.Deliver(context.Background(), consumerfake.Protocol, triggerMock())
	if !errors.Is(err, consumer.ErrNotArmed) {
		t.Fatalf("draining: want ErrNotArmed, got %v", err)
	}
	if d.count() != 1 {
		t.Fatalf("draining: the deliverer was called again (%d total); a poll between tests must get a synthesized empty response, never a trigger", d.count())
	}
}

// THE PULL-PROTOCOL HALF OF DEFAULT-CLOSED, AND NOTHING ELSE ENFORCES IT.
//
// A push protocol is held closed by Deliver, which the test above pins. A PULL
// protocol never goes through Deliver for the polls it must NOT answer: the
// parser holds a stashed poll response and the contract (see the Deliverer doc
// comment) is that it consults ArmedTest first and drops the stash when the
// gate is not armed. ArmedTest is therefore the entire default-closed check on
// that path — a Kafka worker polls continuously from the instant it joins the
// group, and the whole mock pool is resident while the application boots, so an
// ArmedTest that answered "yes" outside an armed window would let the worker
// drain every trigger in the set before test-1 exists.
//
// It is pinned on its own because the property is invisible to every other
// test in this file: deleting the phase check leaves ArmedTest returning a
// stale-or-zero arm with ok=true, the rest of the suite green, and rule 1
// broken for the one protocol shape this contract was designed around.
func TestArmedTestIsClosedOutsideAnArmedWindow(t *testing.T) {
	g, clk, _ := newGate(t)

	if a, ok := g.ArmedTest(); ok {
		t.Fatalf("boot: ArmedTest must be closed, got ok=true arm=%+v; a pull parser reads this before answering a poll from its stash", a)
	}

	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	a, ok := g.ArmedTest()
	if !ok || a.TestID != "test-1" {
		t.Fatalf("armed: ArmedTest = (%+v, %v), want test-1 and true", a, ok)
	}

	g.MarkTriggerAccepted("test-1")
	if res := completeSync(t, g, clk, "test-1"); res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("precondition: end reason %q", res.EndReason)
	}
	if g.Phase() != consumer.PhaseDraining {
		t.Fatalf("precondition: phase %q", g.Phase())
	}
	if a, ok := g.ArmedTest(); ok {
		t.Fatalf("draining: ArmedTest must be closed, got ok=true arm=%+v; a poll between two tests must be answered empty, never with the completed test's trigger", a)
	}

	g.Reset()
	if g.Phase() != consumer.PhaseBoot {
		t.Fatalf("precondition: Reset must return the gate to boot, got %q", g.Phase())
	}
	if a, ok := g.ArmedTest(); ok {
		t.Fatalf("after Reset: ArmedTest must be closed, got ok=true arm=%+v", a)
	}
}

// A STALE ACCEPT MUST NOT ARM THE EVIDENCE OF THE NEXT WINDOW.
//
// MarkTriggerAccepted is the ONLY evidence a zero-effect window can have that
// the application ever received its message — the consume-and-write-to-a-
// database shape produces nothing to count. A parser that reports the accept
// late, or that names the window it was working on rather than the one now
// armed, would flip `accepted` for a test whose trigger nothing took, and that
// window would then close count_reached with the worker never having seen the
// message: design §5's false-pass row 2, reported green.
//
// The test-id guard is what stops it, and it is one `if` that no other test in
// this file executes with a mismatched id.
func TestAStaleAcceptFromAnotherWindowIsNotEvidence(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-2", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()

	// The previous window's accept arrives now — a parser goroutine that was
	// still finishing test-1 when test-2 armed.
	g.MarkTriggerAccepted("test-1")

	done := completeAsync(g, "test-2")
	if clk.WaitBlockedAt(armedAt.Add(250*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("Complete is waiting on the grace deadline, so the stale accept was taken as evidence that test-2's own trigger was delivered")
	}
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(5 * time.Second)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
		t.Fatalf("end reason = %q, want trigger_not_delivered: an accept naming another test is not evidence about this one", res.EndReason)
	}
	if res.TriggerAccepted {
		t.Fatal("TriggerAccepted must be false; a window's accept belongs to that window")
	}
}

// A REFUSAL OR A DISCARD NAMING ANOTHER TEST MUST NOT END THIS TEST'S WINDOW.
//
// TestAStaleAcceptFromAnotherWindowIsNotEvidence pins exactly this property for
// the ACCEPT signal; nothing pinned it for refuse/discard, and an automated
// &&->|| sweep left four identity guards alive with the package green:
//
//	MarkTriggerDiscarded  `g.phase == PhaseArmed && g.arm.TestID == testID`
//	Complete's pre-check  `refusedTest == testID && refusal != ""`
//	Complete's loop       `case refusal != "" && refusedTest == testID:`
//	abortArm              `g.phase == PhaseArmed && g.arm.TestID == testID`
//
// Each `||` makes another test's refusal end THIS test's window, which is the
// mis-attribution Gate.abortArm's own comment says to avoid: one test's failure
// reported against another's spec, with a remedy pointing at the wrong worker.
func TestARefusalNamingAnotherTestDoesNotEndThisTestsWindow(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()

	// A parser goroutine still finishing test-2 reports both signals against
	// it while test-1 owns the window.
	g.MarkTriggerDiscarded("test-2", "the client rejected the stashed fetch response")
	g.Refuse("test-2", models.CategoryConsumerUnsupportedSpec, "an un-modelled wire version")

	// test-1's own window still closes on its own terms.
	g.MarkTriggerAccepted("test-1")
	done := completeAsync(g, "test-1")
	if !clk.WaitBlockedAt(armedAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("Complete is not waiting on this test's own grace deadline; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(250 * time.Millisecond)

	res := awaitResult(t, done)
	if res.Refused() {
		t.Fatalf("test-1 was refused %q (%s); a refusal naming test-2 belongs to test-2's window",
			res.Refusal, res.RefusalDetail)
	}
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason = %q, want count_reached: test-1's window must close for its own reason", res.EndReason)
	}
	if !res.TriggerAccepted {
		t.Fatal("test-1's own accept must survive another test's discard")
	}

	// AND THE SIGNAL IS NOT SWALLOWED. The guards narrow attribution; they do
	// not discard. Asked about test-2, the gate still answers with the
	// refusal that was raised against test-2 — the first one, since a later
	// refusal is usually the consequence of the earlier one.
	refused := g.Complete(context.Background(), "test-2")
	if refused.Refusal != models.CategoryConsumerTriggerDiscarded {
		t.Fatalf("test-2 must carry the refusal raised against it, got %+v", refused)
	}

	// AND A THIRD TEST, NEITHER ARMED NOR REFUSED, GETS THE CALLER-ERROR
	// ANSWER — not the refusal standing against test-2. This is Complete's
	// pre-check guard (`refusedTest == testID && refusal != ""`): as `||` it
	// hands every unarmed Complete whatever refusal happens to be resident,
	// so a caller bug is reported as another test's application failure.
	stray := g.Complete(context.Background(), "test-3")
	if stray.Refusal != models.CategoryConsumerUnsupportedAgent {
		t.Fatalf("Complete for a test that was never armed and never refused must be a caller error, got %+v", stray)
	}
	if stray.EndReason != models.ConsumerEndReasonInternalError {
		t.Fatalf("end reason = %q, want internal_error", stray.EndReason)
	}
}

// A trigger for a protocol other than the armed one is refused too: the
// default-closed check is on the armed test's protocol, not merely on "some
// test is armed".
func TestDeliverIsRefusedForAnotherProtocol(t *testing.T) {
	g, _, _ := newGate(t)
	other := &recordingDeliverer{}
	g.RegisterDeliverer("other", other)

	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if err := g.Deliver(context.Background(), "other", triggerMock()); !errors.Is(err, consumer.ErrNotArmed) {
		t.Fatalf("want ErrNotArmed for an unarmed protocol, got %v", err)
	}
	if other.calls != 0 {
		t.Fatalf("the other protocol's deliverer was called %d times", other.calls)
	}
}

// SINGLE-FLIGHT. Two open windows would make every effect ambiguous.
func TestArmIsSingleFlight(t *testing.T) {
	g, _, d := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm test-1: %v", err)
	}
	err := g.Arm(context.Background(), arm("test-2", 1), triggerMock())
	if !errors.Is(err, consumer.ErrAlreadyArmed) {
		t.Fatalf("want ErrAlreadyArmed, got %v", err)
	}
	if d.count() != 1 {
		t.Fatalf("the second Arm delivered anyway (%d deliveries)", d.count())
	}
	if got, _ := g.ArmedTest(); got.TestID != "test-1" {
		t.Fatalf("the second Arm stole the window: armed test is %q", got.TestID)
	}
}

// One trigger per window. A pull parser that asks twice must not be served
// twice — that would be a duplicate delivery the recording never had.
func TestTheTriggerIsDeliveredOnlyOncePerWindow(t *testing.T) {
	g, _, d := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	err := g.Deliver(context.Background(), consumerfake.Protocol, triggerMock())
	if !errors.Is(err, consumer.ErrAlreadyDelivered) {
		t.Fatalf("want ErrAlreadyDelivered, got %v", err)
	}
	if d.count() != 1 {
		t.Fatalf("delivered %d times", d.count())
	}
}

// A protocol with no registered deliverer is REFUSED, not degraded. There is
// no weak-verdict path that can still print PASSED.
func TestArmWithNoDelivererIsRefused(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	err := g.Arm(context.Background(), arm("test-1", 1), triggerMock())
	if !errors.Is(err, consumer.ErrNoDeliverer) {
		t.Fatalf("want ErrNoDeliverer, got %v", err)
	}
	if g.Phase() != consumer.PhaseBoot {
		t.Fatalf("a failed Arm must leave the gate closed, got %q", g.Phase())
	}
}

// ARM ADOPTS, IT NEVER CLEARS (rule 2).
//
// The mock pool is swapped ~20 lines before the test is armed, and a
// prefetching client can be answered in that window. An effect produced there
// belongs to the test being armed. Clearing the buffer at Arm would either
// lose it — the test then times out, a false red — or leave it to be counted
// against the following test, a false red there instead.
func TestArmAdoptsAnEffectObservedBetweenThePoolSwapAndTheArm(t *testing.T) {
	g, clk, _ := newGate(t)

	// test-1 runs and completes normally.
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm test-1: %v", err)
	}
	first := consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)
	g.ObserveEffect(first)
	res1 := completeSync(t, g, clk, "test-1")
	if res1.ObservedEffects != 1 || len(res1.Effects) != 1 || res1.Effects[0].Key != "o-1" {
		t.Fatalf("test-1 must own exactly its own effect, got %+v", res1)
	}

	// THE RACE WINDOW: the pool has been swapped for test-2 and a
	// prefetching client has already been answered, so the worker produces
	// test-2's effect BEFORE test-2 is armed. The gate is draining here.
	if g.Phase() != consumer.PhaseDraining {
		t.Fatalf("precondition: gate should be draining, got %q", g.Phase())
	}
	early := consumerfake.View("produce", "order-events", "o-2", `{"status":"CONFIRMED"}`)
	g.ObserveEffect(early)

	// Now test-2 arms. It must ADOPT the early effect.
	if err := g.Arm(context.Background(), arm("test-2", 1), triggerMock()); err != nil {
		t.Fatalf("Arm test-2: %v", err)
	}
	res2 := completeSync(t, g, clk, "test-2")
	if res2.ObservedEffects != 1 {
		t.Fatalf("test-2 must adopt the effect observed before it armed; observed=%d (0 means Arm cleared the buffer, which times out a healthy worker)", res2.ObservedEffects)
	}
	if len(res2.Effects) != 1 || res2.Effects[0].Key != "o-2" {
		t.Fatalf("test-2 owns the wrong effect: %+v", res2.Effects)
	}
	if res2.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("test-2 end reason: got %q, want count_reached", res2.EndReason)
	}
}

// An adopted effect must not SHORTEN the drain. Its observation timestamp can
// be arbitrarily old; anchoring the grace on it would close the window before
// the worker had any chance to react to the trigger it was just handed, and
// report count_reached for work the worker never did.
func TestAnAdoptedEffectDoesNotShortenTheGrace(t *testing.T) {
	g, clk, _ := newGate(t)

	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	// Time passes with nothing armed — far more than one grace window.
	clk.Advance(2 * time.Second)

	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")

	// The count is already satisfied by the adopted effect, but the drain
	// is anchored at the ARM, so nothing may complete yet.
	if !clk.WaitBlockedAt(armedAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("the drain is not anchored at the arm; deadlines: %v (want %v)", clk.Deadlines(), armedAt.Add(250*time.Millisecond))
	}
	clk.Advance(249 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the window closed after 249ms of a 250ms grace: %+v", res)
	default:
	}

	clk.Advance(2 * time.Millisecond)
	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonCountReached || res.ObservedEffects != 1 {
		t.Fatalf("got %+v", res)
	}
}

// THE GRACE DRAIN IS MANDATORY (rule 6). Without it an N+1 over-production
// arriving inside the drain is attributed to the NEXT test and this one
// passes, which would make the over-produce regression uncatchable.
func TestTheGraceDrainCatchesAnExtraEffect(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	done := completeAsync(g, "test-1")

	firstAt := clk.Now()
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	if !clk.WaitBlockedAt(firstAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("the drain did not start on the first effect; deadlines: %v", clk.Deadlines())
	}
	// The count is satisfied; 20ms into the drain the worker produces
	// again.
	clk.Advance(20 * time.Millisecond)
	extraAt := clk.Now()
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1-dup", `{}`))

	// The drain restarts from the new observation, so the window is still
	// open 249ms later.
	if !clk.WaitBlockedAt(extraAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("the drain did not restart on the extra effect; deadlines: %v (want %v)", clk.Deadlines(), extraAt.Add(250*time.Millisecond))
	}
	clk.Advance(249 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the window closed before the extra's own drain elapsed: %+v", res)
	default:
	}
	clk.Advance(2 * time.Millisecond)

	res := awaitResult(t, done)
	if res.ObservedEffects != 2 {
		t.Fatalf("the extra was not counted: observed=%d expected=%d — an over-production that lands inside the drain must be visible to the judge", res.ObservedEffects, res.ExpectEffects)
	}
	if len(res.Effects) != 2 {
		t.Fatalf("want both views, got %d", len(res.Effects))
	}
}

// The awkward completion case from §9: the ONLY effect arrives one
// millisecond after the grace would have elapsed. It must be counted, and the
// drain must then run from IT — not be treated as too late.
func TestTheOnlyEffectArrivingAtGracePlusOneMillisecondStillCompletes(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")
	// With the count unmet the only thing to wait for is the backstop.
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}

	clk.Advance(251 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the window closed with zero of one expected effects: %+v", res)
	default:
	}
	effectAt := clk.Now()
	g.ObserveEffect(consumerfake.View("write", "orders", "o-1", `{}`))

	// THE ASSERTION THAT MATTERS: the drain must now be anchored on the
	// effect, at effect+250ms — not on the arm, whose grace expired a
	// millisecond ago and would close the window instantly.
	if !clk.WaitBlockedAt(effectAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("the drain was not re-anchored on the effect; deadlines: %v (want %v)", clk.Deadlines(), effectAt.Add(250*time.Millisecond))
	}
	clk.Advance(249 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the window closed before the effect's own drain elapsed: %+v", res)
	default:
	}
	clk.Advance(2 * time.Millisecond)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q, want count_reached", res.EndReason)
	}
	if res.ObservedEffects != 1 {
		t.Fatalf("observed=%d, want 1", res.ObservedEffects)
	}
}

// THE PUSH-PROTOCOL SKETCH (design §4 P1), which is a REQUIRED review artifact
// before this SPI tags: the pull/push split is the one genuine one-way door in
// the interface.
//
// A Pulsar client sends ONE flow-control frame at subscribe carrying its whole
// receiver queue and then says nothing until it has consumed about half of it.
// Under the pull-shaped SPI this design rejected — "TakeTrigger(protocol)",
// the parser asking whether a trigger is waiting — the parser would have
// exactly one prompt for the entire run: test-1 would consume it and tests
// 2..N would never be handed anything, each closing on the backstop with
// trigger_not_delivered. Arm + Deliver makes the GATE decide when a message
// goes out and the parser decide how, so a single FLOW covers every test.
//
// This drives three tests through consumerfake.PushDeliverer, which is that
// sketch written as compiling code.
func TestOneFlowFrameCoversEveryTestUnderAPushProtocol(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	push := consumerfake.NewPushDeliverer(g, true)
	g.RegisterDeliverer(consumerfake.Protocol, push)

	// The client's ONE flow frame, at subscribe.
	push.GrantFlow(1000)

	for i := 1; i <= 3; i++ {
		id := "test-" + itoaGate(i)
		if err := g.Arm(context.Background(), arm(id, 0), triggerMock()); err != nil {
			t.Fatalf("%s: Arm: %v", id, err)
		}
		// Zero expected effects — the consume-and-write shape — so the only
		// thing that can close the window is the parser confirming the
		// client TOOK the message. That is what MarkTriggerAccepted is for.
		done := completeAsync(g, id)
		clk.Advance(300 * time.Millisecond)
		res := awaitResult(t, done)
		if res.EndReason != models.ConsumerEndReasonCountReached {
			t.Fatalf("%s: end reason %q, want count_reached", id, res.EndReason)
		}
		if !res.TriggerAccepted {
			t.Fatalf("%s: the push was never acknowledged", id)
		}
	}

	if got := len(push.Pushed()); got != 3 {
		t.Fatalf("%d messages pushed, want 3: under a pull-shaped SPI only the first test would ever be handed one", got)
	}
	if got := push.Permits(); got != 997 {
		t.Fatalf("permits left = %d, want 997: each test spends one credit of the single FLOW frame", got)
	}
}

// A push with no credits left is REFUSED, not written anyway. A client whose
// receiver queue is full drops what it is pushed, and the test would then
// report "the worker stopped producing" for a message keploy threw away.
//
// It lands on TRIGGER_NOT_DELIVERED rather than TRIGGER_DISCARDED, and the
// distinction is Gate.abortArm's: nothing reached the application, so blaming
// a fetch position or a session id would send the reader to the wrong place.
// The parser's own words still reach the report.
func TestAPushWithNoFlowPermitsIsRefusedRatherThanDropped(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	push := consumerfake.NewPushDeliverer(g, true)
	g.RegisterDeliverer(consumerfake.Protocol, push)

	err := g.Arm(context.Background(), arm("test-1", 1), triggerMock())
	if err == nil {
		t.Fatal("a push with no flow-control permits must fail the arm, not silently write bytes the client will drop")
	}
	if g.Phase() == consumer.PhaseArmed {
		t.Fatal("a failed delivery must not leave the gate armed")
	}
	if len(push.Pushed()) != 0 {
		t.Fatal("nothing may be written when the client has no room for it")
	}
	res := g.Complete(context.Background(), "test-1")
	if res.Refusal != models.CategoryConsumerTriggerNotDelivered {
		t.Fatalf("refusal %q, want CONSUMER_TRIGGER_NOT_DELIVERED: nothing reached the application", res.Refusal)
	}
	if !strings.Contains(res.RefusalDetail, "flow-control permits") {
		t.Fatalf("the parser's own words must reach the report, got %q", res.RefusalDetail)
	}
}

func itoaGate(i int) string {
	return string(rune('0' + i))
}

// THE BACKSTOP MUST NOT CUT A DRAIN THAT IS ALREADY RUNNING.
//
// The two deadlines are anchored differently: the timeout on the ARM, the
// grace on max(arm, last observation). So an effect that lands later than
// Timeout-Grace after the arm has its drain truncated — the loop reaches the
// timeout case with the grace deadline still in the future, the window closes
// with end_reason=timeout, and the judge turns that into
// CONSUMER_COMPLETION_TIMEOUT and a FAILED test even though the expected count
// was reached. With the design's own numbers (5000ms timeout, grace clamped to
// at most 2000ms) any effect after ~3s was affected: an avoidable red on a
// slow-but-correct worker.
//
// The single effect here arrives at Timeout-Grace+1ms.
func TestTheOnlyEffectArrivingJustInsideTheBackstopStillDrains(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}

	// timeout(5000) - grace(250) + 1ms.
	clk.Advance(4751 * time.Millisecond)
	effectAt := clk.Now()
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))

	// The drain is anchored on the effect, at effect+250ms — which is 1ms
	// PAST the arm-anchored timeout. The backstop has to yield to it.
	wantDrain := effectAt.Add(250 * time.Millisecond)
	if !clk.WaitBlockedAt(wantDrain, testBudget) {
		t.Fatalf("the backstop pre-empted the drain; deadlines: %v (want %v)", clk.Deadlines(), wantDrain)
	}
	clk.Advance(249 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the window closed before the effect's own drain elapsed: %+v", res)
	default:
	}
	clk.Advance(2 * time.Millisecond)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q, want count_reached: a worker that produced everything the recording says it produces, only slowly, must not be failed by the backstop", res.EndReason)
	}
	if res.ObservedEffects != 1 {
		t.Fatalf("observed=%d, want 1", res.ObservedEffects)
	}
}

// AND THE EXTENSION IS BOUNDED. Every observation re-anchors the grace, so an
// unbounded yield would let a worker producing continuously hold the window
// open for ever. The backstop may slip by at most ONE grace window:
// armedAt + timeout + grace, and no further.
func TestTheBackstopExtensionIsBoundedByOneGraceWindow(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	g.MarkTriggerAccepted("test-1")
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}

	// An effect at 4900ms: its drain ends at 5150ms, past the 5000ms timeout
	// and inside the 5250ms hard bound, so the backstop yields to it.
	clk.Advance(4900 * time.Millisecond)
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	if !clk.WaitBlockedAt(armedAt.Add(5150*time.Millisecond), testBudget) {
		t.Fatalf("the backstop did not yield to the drain; deadlines: %v", clk.Deadlines())
	}

	// A second effect at 5100ms would push the drain to 5350ms — past the
	// hard bound. The backstop must clamp at 5250ms instead of following it.
	clk.Advance(200 * time.Millisecond)
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	if !clk.WaitBlockedAt(armedAt.Add(5250*time.Millisecond), testBudget) {
		t.Fatalf("the backstop followed the drain past its bound, so a continuously producing worker could hold the window open for ever; deadlines: %v", clk.Deadlines())
	}

	clk.Advance(150 * time.Millisecond)
	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonTimeout {
		t.Fatalf("end reason %q, want timeout: a window closed by the bounded backstop must say so", res.EndReason)
	}
}

// THE TIMEOUT BACKSTOP NAMES ITS REASON (rule 7). "The app never took the
// message" and "the app took it and produced nothing" need opposite remedies,
// and a bare "0 effects observed" cannot tell them apart.
func TestTimeoutBackstopReportsTriggerNotDeliveredWhenNothingWasAccepted(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(5 * time.Second)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
		t.Fatalf("end reason %q, want trigger_not_delivered", res.EndReason)
	}
	if res.TriggerAccepted {
		t.Fatal("TriggerAccepted must be false when the client never took the trigger")
	}
	if res.ObservedEffects != 0 {
		t.Fatalf("observed=%d", res.ObservedEffects)
	}
}

func TestTimeoutBackstopReportsTimeoutWhenTheTriggerWasAccepted(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 2), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	g.MarkTriggerAccepted("test-1")
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))

	armedAt := clk.Now()
	done := completeAsync(g, "test-1")
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(5 * time.Second)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonTimeout {
		t.Fatalf("end reason %q, want timeout", res.EndReason)
	}
	if !res.TriggerAccepted {
		t.Fatal("TriggerAccepted must survive into the result")
	}
	if res.ObservedEffects != 1 || res.ExpectEffects != 2 {
		t.Fatalf("observed=%d expected=%d", res.ObservedEffects, res.ExpectEffects)
	}
}

// A discarded trigger ends the window at once with a named refusal, instead of
// waiting out the timeout and reporting a missing effect. Bytes on the wire
// are not delivery.
func TestADiscardedTriggerRefusesImmediatelyByName(t *testing.T) {
	g, _, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	done := completeAsync(g, "test-1")
	g.MarkTriggerDiscarded("test-1", "the client re-fetched the offset it was just served")

	res := awaitResult(t, done)
	if res.Refusal != models.CategoryConsumerTriggerDiscarded {
		t.Fatalf("refusal %q, want %q", res.Refusal, models.CategoryConsumerTriggerDiscarded)
	}
	if res.EndReason != models.ConsumerEndReasonTriggerDiscarded {
		t.Fatalf("end reason %q", res.EndReason)
	}
	if !res.Refused() {
		t.Fatal("Refused() must be true")
	}
}

// A record count, not a request count. Batching makes requests-per-record
// nondeterministic between runs, so a request-count rule flakes by
// construction.
func TestCompletionCountsRecordsNotViews(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 3), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	batched := consumerfake.View("produce", "order-events", "o-1", `{}`)
	batched.Records = 3
	g.ObserveEffect(batched)

	res := completeSync(t, g, clk, "test-1")
	if res.ObservedEffects != 3 {
		t.Fatalf("one view carrying three records must count as three, got %d", res.ObservedEffects)
	}
}

// Complete for a test that was never armed is a LOUD refusal, never a verdict.
func TestCompleteForAnUnarmedTestRefusesLoudly(t *testing.T) {
	g, _, _ := newGate(t)
	res := g.Complete(context.Background(), "test-9")
	if !res.Refused() {
		t.Fatalf("want a refusal, got %+v", res)
	}
	if res.EndReason != models.ConsumerEndReasonInternalError {
		t.Fatalf("end reason %q", res.EndReason)
	}
}

// P9: a consumer's fetch position and producer sequence do not rewind between
// attempts, so --retryPassing / --must-pass over one application process is
// not a repeat of the first run. Refused by name.
func TestRearmingACompletedTestIsRefused(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	completeSync(t, g, clk, "test-1")

	err := g.Arm(context.Background(), arm("test-1", 0), triggerMock())
	if err == nil {
		t.Fatal("re-arming a completed test must be refused")
	}
	if !contains(err.Error(), string(models.CategoryConsumerRepeatPassUnsupported)) {
		t.Fatalf("the refusal must name its category, got %q", err)
	}
}

// Reset is the test-set boundary: --keep-app-alive reuses one process across
// sets, so a gate left armed or an adopted effect carried across would leak
// one set's state into the next set's first test.
func TestResetReturnsTheGateToBootAndDropsNothingSilently(t *testing.T) {
	g, _, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	g.Reset()

	if g.Phase() != consumer.PhaseBoot {
		t.Fatalf("phase after Reset is %q, want boot", g.Phase())
	}
	if err := g.Deliver(context.Background(), consumerfake.Protocol, triggerMock()); !errors.Is(err, consumer.ErrNotArmed) {
		t.Fatalf("after Reset delivery must be refused again, got %v", err)
	}
	// Re-arming test-1 is allowed after a reset: a new test set is a new
	// application lifecycle.
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm after Reset: %v", err)
	}
	// And the previous set's effect must not be adopted into it.
	res := g.Complete(cancelledCtx(), "test-1")
	if res.ObservedEffects != 0 {
		t.Fatalf("an effect from the previous test set leaked into this one: observed=%d", res.ObservedEffects)
	}
}

// CANCELLING A RUN IS NOT AN AGENT THAT LACKS CONSUMER SUPPORT.
//
// This path used to report CONSUMER_UNSUPPORTED_AGENT, which is a different
// and much more alarming claim: an operator reading it goes looking for a
// missing capability instead of noticing that someone stopped the run.
// CONSUMER_RUN_CANCELLED exists for exactly this call site and was, for a
// while, documented as its fix without the call site having been changed.
func TestCancellingTheRunIsReportedAsACancelledRun(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan models.ConsumerResult, 1)
	go func() { out <- g.Complete(ctx, "test-1") }()

	// Wait until Complete is parked on its backstop, so the cancellation
	// lands inside the wait rather than before it.
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting; deadlines: %v", clk.Deadlines())
	}
	cancel()

	res := awaitResult(t, out)
	if res.Refusal != models.CategoryConsumerRunCancelled {
		t.Fatalf("refusal = %q, want %s — a cancelled run must not be reported as a missing agent capability",
			res.Refusal, models.CategoryConsumerRunCancelled)
	}
	if res.EndReason != models.ConsumerEndReasonInternalError {
		t.Fatalf("end reason = %q", res.EndReason)
	}
	if !contains(res.RefusalDetail, "cancelled") {
		t.Fatalf("the detail must say the run was cancelled, got %q", res.RefusalDetail)
	}
}

// COMPLETING A WINDOW ON THE COUNT NEEDS POSITIVE EVIDENCE THE APPLICATION RAN.
//
// A test that expects zero effects satisfies "observed >= expected" at t=0.
// That is not a corner: it is exactly what the recorder mints for a
// consume-and-write-to-a-database worker, one of the two most common consumer
// shapes. Without this rule an application that crashed at boot, joined the
// wrong group or never subscribed closes its window with end_reason
// count_reached and every downstream assertion vacuously satisfied — design
// §5's false-pass row 2, "0 of 0 reads green".
func TestAZeroEffectWindowDoesNotCloseOnCountWithoutEvidence(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")

	// The grace alone must not be enough: nothing proves the worker ever
	// received the message.
	if clk.WaitBlockedAt(armedAt.Add(250*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("Complete is waiting on the grace deadline; with no evidence the application ran it must wait out the timeout backstop instead")
	}
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("Complete is not waiting on the timeout backstop; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(5 * time.Second)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
		t.Fatalf("end reason = %q, want trigger_not_delivered", res.EndReason)
	}
	if res.TriggerAccepted {
		t.Fatal("TriggerAccepted must be false")
	}
}

// The positive check is what lets the same window close cleanly. A parser that
// confirms the client took the message is the ONLY evidence available for a
// worker that produces nothing.
func TestAZeroEffectWindowClosesOnCountOnceTheTriggerIsAccepted(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	g.MarkTriggerAccepted("test-1")
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")

	if !clk.WaitBlockedAt(armedAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("Complete is not waiting on the grace; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(251 * time.Millisecond)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason = %q, want count_reached", res.EndReason)
	}
	if !res.TriggerAccepted {
		t.Fatal("TriggerAccepted must survive into the result")
	}
}

// Reset is the only place an over-production after the LAST test of a set can
// be seen: within a set such an effect fails the next test as an extra, and
// the last test has no next test. The count it returns is what a caller turns
// into a failed set.
func TestResetReportsTheEffectsLeftOverAfterTheLastTest(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	g.MarkTriggerAccepted("test-1")
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	if res := completeSync(t, g, clk, "test-1"); res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q", res.EndReason)
	}

	// The N+1 message, arriving just outside the last window of the set.
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-2", `{}`))

	if got := g.Reset(); got != 1 {
		t.Fatalf("Reset reported %d trailing effect records, want 1 — an over-production after the last test of the last set is otherwise never reported anywhere", got)
	}
	// And a clean boundary reports nothing, so the caller cannot be made to
	// fail a healthy set.
	if got := g.Reset(); got != 0 {
		t.Fatalf("a second Reset reported %d trailing records, want 0", got)
	}
}

// A deliverer that fails hands the permit back and names the failure, rather
// than leaving the window open on a trigger nothing received.
func TestADelivererFailureIsNamedAndDoesNotLeaveTheGateArmed(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	boom := errors.New("socket closed")
	g.RegisterDeliverer(consumerfake.Protocol, &recordingDeliverer{fail: boom})

	err := g.Arm(context.Background(), arm("test-1", 1), triggerMock())
	if !errors.Is(err, boom) {
		t.Fatalf("Arm must surface the deliverer's error, got %v", err)
	}
	if g.Phase() == consumer.PhaseArmed {
		t.Fatal("a failed delivery must not leave the gate armed")
	}
	res := g.Complete(context.Background(), "test-1")
	// NOT_DELIVERED, not DISCARDED. The deliverer failed, so nothing was ever
	// written to the application; "discarded" claims keploy wrote the bytes
	// and the client threw them away, which sends the reader to debug a fetch
	// position instead of the failing parser. The category and the end reason
	// must agree — they used to contradict each other inside one call.
	if res.Refusal != models.CategoryConsumerTriggerNotDelivered {
		t.Fatalf("refusal %q", res.Refusal)
	}
	if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
		t.Fatalf("end reason %q", res.EndReason)
	}
}

// The other abortArm path: armed with no trigger mock at all. Nothing reached
// the application here either, so it must name the same failure — and it must
// not leave the gate armed for the next test to walk into.
func TestArmingWithNoTriggerMockIsNamedNotDelivered(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)
	d := &recordingDeliverer{}
	g.RegisterDeliverer(consumerfake.Protocol, d)

	if err := g.Arm(context.Background(), arm("test-1", 1), nil); err == nil {
		t.Fatal("arming with a nil trigger must fail")
	}
	if g.Phase() == consumer.PhaseArmed {
		t.Fatal("a nil trigger must not leave the gate armed")
	}
	if got := d.count(); got != 0 {
		t.Fatalf("nothing may be delivered for a nil trigger, got %d deliveries", got)
	}
	res := g.Complete(context.Background(), "test-1")
	if res.Refusal != models.CategoryConsumerTriggerNotDelivered {
		t.Fatalf("refusal %q", res.Refusal)
	}
	if res.EndReason != models.ConsumerEndReasonTriggerNotDelivered {
		t.Fatalf("end reason %q", res.EndReason)
	}
	if res.TriggerAccepted {
		t.Fatal("a trigger that was never written cannot be accepted")
	}
}

// A PRESENCE VIEW IS RECORDED BUT NOT COUNTED, and the two ends of this rule
// have to agree or a healthy test goes red.
//
// The recorder writes ExpectEffects from PRODUCED RECORDS ONLY — it
// deliberately excludes mapped writes, because nothing on the replay side
// calls ObserveEffect for a database write. If the gate counted presence views
// on the observed side, a worker with one produce and one mapped write would
// report 2 observed against 1 expected and the judge's count assertion would
// fire EFFECT_UNEXPECTED for a worker that did exactly what it recorded.
func TestAPresenceViewIsReportedButNotCounted(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	done := completeAsync(g, "test-1")

	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	g.ObserveEffect(models.EffectView{
		Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence,
	})

	lastAt := clk.Now()
	if !clk.WaitBlockedAt(lastAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("deadlines: %v", clk.Deadlines())
	}
	clk.Advance(251 * time.Millisecond)

	res := awaitResult(t, done)
	if res.ObservedEffects != 1 {
		t.Fatalf("observed=%d, want 1: the write is not something ExpectEffects counts, so counting it here reddens a healthy test", res.ObservedEffects)
	}
	if len(res.Effects) != 2 {
		t.Fatalf("both views must still be REPORTED (the judge filters them, the gate never drops them), got %d", len(res.Effects))
	}
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q", res.EndReason)
	}
}

// The second, worse consequence of counting them: with expect=1 a presence
// view arriving 10ms in would SATISFY the count, the grace would anchor on it,
// and the window would close before the produce landed 300ms later — so this
// test fails EFFECT_MISSING and the next one fails EFFECT_UNEXPECTED. That is
// exactly the truncation design §5 deleted idlePollObserved to avoid.
func TestAnEarlyPresenceViewDoesNotCloseTheWindowBeforeTheProduceLands(t *testing.T) {
	g, clk, _ := newGate(t)
	if err := g.Arm(context.Background(), arm("test-1", 1), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	armedAt := clk.Now()
	done := completeAsync(g, "test-1")

	clk.Advance(10 * time.Millisecond)
	g.ObserveEffect(models.EffectView{
		Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence,
	})

	// The window must still be waiting on the TIMEOUT, not on a grace that a
	// satisfied count would have started.
	if !clk.WaitBlockedAt(armedAt.Add(5*time.Second), testBudget) {
		t.Fatalf("the count was satisfied by a presence view; deadlines: %v (want the timeout at %v)", clk.Deadlines(), armedAt.Add(5*time.Second))
	}

	// The produce lands 300ms in, well after the grace a presence-satisfied
	// count would have run out.
	clk.Advance(290 * time.Millisecond)
	g.ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{}`))
	producedAt := clk.Now()
	if !clk.WaitBlockedAt(producedAt.Add(250*time.Millisecond), testBudget) {
		t.Fatalf("the drain did not start on the produce; deadlines: %v", clk.Deadlines())
	}
	clk.Advance(251 * time.Millisecond)

	res := awaitResult(t, done)
	if res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("end reason %q (%s)", res.EndReason, res.RefusalDetail)
	}
	if res.ObservedEffects != 1 {
		t.Fatalf("observed=%d, want 1", res.ObservedEffects)
	}
}

// --- helpers -------------------------------------------------------------

// completeAsync runs Complete on its own goroutine and returns its result
// channel.
func completeAsync(g *consumer.Gate, testID string) <-chan models.ConsumerResult {
	out := make(chan models.ConsumerResult, 1)
	go func() { out <- g.Complete(context.Background(), testID) }()
	return out
}

// completeSync drives the fake clock forward until Complete returns. It is for
// the cases whose point is NOT the timing — the timing cases advance the clock
// themselves, one deliberate step at a time.
func completeSync(t *testing.T, g *consumer.Gate, clk *consumerfake.Clock, testID string) models.ConsumerResult {
	t.Helper()
	done := completeAsync(g, testID)
	deadline := time.Now().Add(testBudget)
	for {
		select {
		case res := <-done:
			return res
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Complete(%s) never returned", testID)
		}
		// A short budget here: if Complete is not waiting on a timer it
		// is either about to return or already has, and the loop
		// re-checks. The long budget is the outer deadline.
		if clk.WaitBlocked(1, 50*time.Millisecond) {
			clk.Advance(time.Second)
		}
	}
}

// awaitResult reads a completion result with a bound. A gate that never
// completes is a product bug, and a bug must FAIL the suite rather than hang
// it: an unbounded receive here turns a broken completion rule into a CI
// timeout with no failing test name attached to it.
func awaitResult(t *testing.T, done <-chan models.ConsumerResult) models.ConsumerResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(testBudget):
		t.Fatal("the gate never closed the window")
		return models.ConsumerResult{}
	}
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// THE ADAPTER MUST SURVIVE ITS OWN UNREGISTER (see the twin test on the
// projector registry). DelivererFunc is a func type, func types are not
// comparable, and comparing two interface values whose dynamic type is not
// comparable panics at run time. A registry that compared the interface value
// would blow up for exactly the callers DelivererFunc exists to serve.
func TestUnregisteringAFuncDelivererDoesNotPanic(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)

	var delivered int
	unregister := g.RegisterDeliverer(consumerfake.Protocol, consumer.DelivererFunc(
		func(_ context.Context, _ *models.Mock) error {
			delivered++
			return nil
		}))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unregistering a DelivererFunc panicked: %v", r)
		}
	}()
	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("the registered function was called %d times, want 1", delivered)
	}
	completeSync(t, g, clk, "test-1")
	unregister()
	if err := g.Arm(context.Background(), arm("test-2", 0), triggerMock()); !errors.Is(err, consumer.ErrNoDeliverer) {
		t.Fatalf("after unregister Arm must be refused with ErrNoDeliverer, got %v", err)
	}
}

// A RECONNECT RE-REGISTERS, AND THE OLD CONNECTION'S DEFERRED UNREGISTER MUST
// NOT DELETE THE NEW ONE.
//
// A parser registers per connection and defers its unregister. When a broker
// connection is replaced, the new connection registers before the old one's
// defer runs. A registry keyed on anything but this registration's own
// identity would let the dying connection tear down the live one, and every
// subsequent Arm would be refused with "no deliverer" on a perfectly healthy
// run.
func TestAStaleUnregisterDoesNotRemoveTheReplacementDeliverer(t *testing.T) {
	clk := consumerfake.NewClock(time.Time{})
	g := consumer.NewGate(nil, clk)

	old := &recordingDeliverer{}
	unregisterOld := g.RegisterDeliverer(consumerfake.Protocol, old)
	fresh := &recordingDeliverer{}
	g.RegisterDeliverer(consumerfake.Protocol, fresh)

	unregisterOld() // the dead connection's defer finally runs

	if err := g.Arm(context.Background(), arm("test-1", 0), triggerMock()); err != nil {
		t.Fatalf("Arm after a reconnect: %v", err)
	}
	if fresh.count() != 1 {
		t.Fatalf("the live deliverer received %d triggers, want 1 — the dead connection's unregister removed it", fresh.count())
	}
	if old.count() != 0 {
		t.Fatalf("the replaced deliverer received %d triggers", old.count())
	}
}
