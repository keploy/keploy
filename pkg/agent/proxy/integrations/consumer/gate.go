package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Phase is the Gate's state. The Gate is a STATE MACHINE, not a view over the
// mock pool, and that distinction is the whole of design decision §0.1.
//
// WHY ARMING A TEST'S MOCKS IS NOT ENOUGH. Narrowing the resident mock pool to
// one test's mocks looks like injection control, and it is not, for two
// verified reasons:
//
//   - The WHOLE pool is resident while the application boots. The replay path
//     sends an EMPTY mock-filter mapping before it starts the application, and
//     an empty mapping falls through to a load-everything path. An HTTP app is
//     idle at boot; a consumer joins its group and polls IMMEDIATELY, so a
//     pool-is-the-gate design would let the app drain every trigger in the set
//     before test-1 exists.
//   - Arming happens AFTER the pool swap, not with it. A prefetching client
//     can be answered in between.
//
// So the Gate is default-closed: delivery is REFUSED in boot and in draining,
// every poll outside an armed window is answered by the parser with a
// synthesized empty response, and the pool swap opens nothing. Swap/arm
// ordering stops mattering, which is the point — it removes a race instead of
// trying to win it.
type Phase string

const (
	// PhaseBoot is the initial phase and the phase after Reset: the
	// application may be running and polling, and NOTHING may be delivered
	// to it. Every consumer test set starts here.
	PhaseBoot Phase = "boot"

	// PhaseArmed means exactly one test's delivery window is open.
	PhaseArmed Phase = "armed"

	// PhaseDraining is the phase between two tests: the previous window has
	// closed and the next has not opened. Delivery is refused, and effects
	// that arrive here are still RECORDED so the next Arm can adopt them.
	PhaseDraining Phase = "draining"
)

// Deliverer is implemented by a protocol parser so the Gate can hand a
// recorded payload to the application.
//
// ARM IS AN ACTIVE PUSH, NOT A PULL. An earlier shape for this was
// "TakeTrigger(protocol) (*models.Mock, bool)" — the parser asks whether there
// is a trigger for it. That cannot express "push this now", and push protocols
// require it: a Pulsar client sends ONE flow-control frame covering its whole
// receiver queue (1000 messages by default) when it subscribes, so under a
// pull model test-1 would consume that single frame and tests 2..N would never
// be handed anything at all. Discovering that after the interface had shipped
// would be a breaking change to a published SPI, so Arm + Deliverer is the
// shape from the start.
//
// A PULL protocol (Kafka Fetch, SQS ReceiveMessage) implements Deliver by
// stashing the payload and answering the next poll from the stash. Such a
// parser MUST consult Gate.ArmedTest before answering that poll and drop the
// stash when the gate is no longer armed for it: the Gate cannot un-write
// bytes the parser already holds, so the last few metres of default-closed are
// the parser's to honour.
//
// A Deliverer MUST ALSO CALL Gate.MarkTriggerAccepted once it has positive
// evidence the client took the message — not when the bytes were written.
// That is the gate's only evidence the application ran for a test that expects
// no effects (the consume-and-write-to-a-database shape), and without it such
// a test cannot close on the count rule and times out with
// trigger_not_delivered. What counts as positive evidence is protocol
// knowledge and therefore the parser's: for Kafka it is the client NOT
// re-fetching the offset it was just served.
//
// THE TWO REVIEW ARTIFACTS THIS INTERFACE WAS REQUIRED TO SHOW BEFORE IT
// TAGS, because it is a one-way door on a published SPI:
//   - the push-shaped sketch that proves Arm+Deliver can express a push
//     protocol: consumerfake.PushDeliverer, which spends flow-control credits
//     the way a pulsar-client-go receiver queue does;
//   - the written OSS-Kafka answer: docs/reference/consumer-contract.md §9,
//     which records the question, both answers and their costs. It is NOT
//     decided there — it is a product call — and §9 says so in the heading so
//     it cannot tag unnoticed.
type Deliverer interface {
	Deliver(ctx context.Context, m *models.Mock) error
}

// DelivererFunc adapts a plain function to Deliverer.
type DelivererFunc func(ctx context.Context, m *models.Mock) error

// Deliver implements Deliverer.
func (f DelivererFunc) Deliver(ctx context.Context, m *models.Mock) error { return f(ctx, m) }

// Gate refusals. They are ordinary errors rather than panics or silent
// no-ops because every one of them means "a test cannot be judged", and the
// caller turns that into a FAILED test with a named category.
var (
	// ErrNotArmed is the default-closed refusal: delivery was attempted
	// outside an armed window. For a parser this is not an error condition
	// — it is the instruction to answer the poll with a synthesized empty
	// response.
	ErrNotArmed = errors.New("consumer gate: delivery refused, no test is armed")

	// ErrAlreadyArmed is the single-flight refusal: a second test tried to
	// arm while one was still open.
	ErrAlreadyArmed = errors.New("consumer gate: refused, another test is already armed")

	// ErrAlreadyDelivered is the single-shot refusal: the armed test's
	// trigger has already been handed to the application once.
	ErrAlreadyDelivered = errors.New("consumer gate: delivery refused, this test's trigger was already delivered")

	// ErrNoDeliverer reports that no parser registered a Deliverer for the
	// armed test's protocol. Refused, never degraded: there is no
	// weak-verdict path here that can still print PASSED.
	ErrNoDeliverer = errors.New("consumer gate: no deliverer registered for protocol")
)

// observation is one live EffectView with the bookkeeping that decides which
// test owns it.
type observation struct {
	// epoch is the monotonic observation counter at the moment the view
	// was recorded. Attribution is a comparison against this number and
	// nothing else, which is what lets the buffer be append-only.
	epoch uint64
	// at is when it was observed. The grace drain is measured from the
	// latest of these and the arm.
	at   time.Time
	view models.EffectView
}

// Gate is the delivery gate and the effect collector for one replay run.
//
// The zero value is not usable; construct it with NewGate.
type Gate struct {
	logger *zap.Logger
	clock  Clock

	// notify wakes a blocked Complete when something it waits on changes.
	// Buffered with capacity 1 and written non-blockingly, so a signal
	// raised while Complete is not selecting is still delivered on its next
	// select — there is no lost-wakeup window.
	notify chan struct{}

	mu         sync.Mutex
	phase      Phase
	deliverers map[string]*delivererReg

	// arm/armedAt/delivered/accepted describe the currently or most
	// recently armed test.
	arm       models.ConsumerArm
	armedAt   time.Time
	delivered bool
	accepted  bool

	// refusal/refusalDetail/refusedTest carry a named refusal raised
	// against a specific test by the gate itself or by a parser.
	refusal      models.FailureCategory
	endReason    models.ConsumerEndReason
	refusalDetl  string
	refusedTest  string
	completedFor map[string]bool

	// obs is the append-only observation buffer. attributedThrough is the
	// epoch of the last observation handed to a completed test.
	//
	// ARM ADOPTS, IT NEVER CLEARS. Arming carries the current epoch and
	// takes ownership of everything observed since the previous test
	// completed. It must not empty this buffer, because a live application
	// may be writing into it: a prefetching client can be answered between
	// the mock-pool swap and the arm, and clearing would either lose those
	// effects (the test then times out — a false red) or leave them to be
	// counted against the following test (a false red there instead). The
	// only place entries leave the buffer is Complete, and only entries a
	// completed test has already been given.
	obs               []observation
	epoch             uint64
	attributedThrough uint64
	lastObsAt         time.Time
}

// NewGate returns a Gate in PhaseBoot. A nil clock means the production one.
func NewGate(logger *zap.Logger, clock Clock) *Gate {
	return &Gate{
		logger:       logger,
		clock:        clockOrReal(clock),
		notify:       make(chan struct{}, 1),
		phase:        PhaseBoot,
		deliverers:   map[string]*delivererReg{},
		completedFor: map[string]bool{},
	}
}

// Phase reports the gate's current phase.
func (g *Gate) Phase() Phase {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.phase
}

// delivererReg is one deliverer registration.
//
// THE ENTRY IS A POINTER SO UNREGISTERING CANNOT PANIC — the same reason the
// projector registry uses one. Storing the Deliverer interface directly and
// having the unregister closure compare `g.deliverers[protocol] == d` panics
// at run time whenever the dynamic type is not comparable, and a func type is
// not comparable, so that version blew up for exactly the callers
// DelivererFunc exists to serve. Pointer identity is total, and it still means
// "unregister MY registration, not whatever replaced it" — which is the
// property that matters here: a reconnect legitimately re-registers the
// protocol, and the OLD connection's deferred unregister must not then delete
// the NEW connection's deliverer.
type delivererReg struct{ d Deliverer }

// RegisterDeliverer registers d as the deliverer for protocol and returns a
// function that unregisters it. Registering twice for one protocol replaces
// the previous deliverer, because a reconnect legitimately re-registers the
// same protocol on a new connection; the projector registry, whose entries are
// process-wide and set from init, panics on a duplicate instead.
func (g *Gate) RegisterDeliverer(protocol string, d Deliverer) func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	reg := &delivererReg{d: d}
	g.deliverers[protocol] = reg
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.deliverers[protocol] == reg {
			delete(g.deliverers, protocol)
		}
	}
}

// ArmedTest returns the armed test and true, or the zero arm and false when
// the gate is not armed. It is the cheap default-closed query a pull-protocol
// parser makes before answering a poll from a stash.
func (g *Gate) ArmedTest() (models.ConsumerArm, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhaseArmed {
		return models.ConsumerArm{}, false
	}
	return g.arm, true
}

// Arm opens the delivery window for one test and hands its trigger to the
// registered Deliverer.
//
// Ordering inside Arm matters: the phase moves to armed BEFORE Deliver is
// called, because a push deliverer can reach the application synchronously and
// a pull deliverer can find a poll already waiting — so the window has to be
// open by the time either of them runs.
//
// Arm ADOPTS every effect observed since the previous test completed (see
// Gate.obs). It clears nothing.
func (g *Gate) Arm(ctx context.Context, arm models.ConsumerArm, trigger *models.Mock) error {
	if arm.TestID == "" {
		return errors.New("consumer gate: refused to arm a test with no id")
	}
	g.mu.Lock()
	if g.phase == PhaseArmed {
		armed := g.arm.TestID
		g.mu.Unlock()
		return fmt.Errorf("%w: %s is armed, %s tried to arm", ErrAlreadyArmed, armed, arm.TestID)
	}
	if g.completedFor[arm.TestID] {
		g.mu.Unlock()
		return fmt.Errorf("consumer gate: refused to re-arm %s, which has already completed; a consumer's fetch position and producer sequence do not rewind between attempts, so a repeat pass is not a repeat of the first (%s)", arm.TestID, models.CategoryConsumerRepeatPassUnsupported)
	}
	reg, ok := g.deliverers[arm.Protocol]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("%w %q", ErrNoDeliverer, arm.Protocol)
	}
	d := reg.d
	g.phase = PhaseArmed
	g.arm = arm
	g.armedAt = g.clock.Now()
	g.delivered = false
	g.accepted = false
	g.refusal, g.refusalDetl, g.endReason, g.refusedTest = "", "", "", ""
	armedAt := g.armedAt
	g.mu.Unlock()

	// A FAILED ARM MUST NOT LEAVE THE GATE OPEN. Nothing reached the
	// application, so the window is over before it began; leaving the phase
	// armed would let the next poll be served a trigger for a test the
	// caller has already given up on.
	if trigger == nil {
		g.abortArm(arm.TestID, "the test was armed with no trigger mock, so nothing could be delivered")
		return errors.New("consumer gate: refused to arm with a nil trigger mock")
	}

	if err := g.deliver(ctx, d, arm.Protocol, trigger); err != nil {
		g.abortArm(arm.TestID, "the deliverer failed to hand the trigger to the application: "+err.Error())
		return err
	}

	if g.logger != nil {
		g.logger.Debug("consumer gate armed",
			zap.String("test_id", arm.TestID),
			zap.String("test_set_id", arm.TestSetID),
			zap.String("protocol", arm.Protocol),
			zap.Time("armed_at", armedAt),
			zap.Int("expect_effects", arm.Completion.ExpectEffects),
		)
	}
	return nil
}

// abortArm closes a window that never opened: it names the failure and returns
// the gate to draining so nothing else can be delivered against this test.
//
// THE CATEGORY IS TriggerNotDelivered, NOT TriggerDiscarded. Both paths that
// reach here — a nil trigger mock, and a Deliverer that returned an error —
// mean NOTHING WAS EVER WRITTEN to the application. "Discarded" is the
// opposite claim (keploy wrote the bytes and the client threw them away), and
// it sends whoever reads the report to debug a fetch position or a session id
// instead of the missing mock or the failing parser that actually caused it.
// The end reason set here has always been ConsumerEndReasonTriggerNotDelivered;
// the category used to contradict it inside this one call.
// THE IDENTITY CONJUNCT BELOW IS DEFENSIVE, AND AN EQUIVALENT MUTANT. Arm is
// the only caller and it always passes the test it has just installed, so
// `g.arm.TestID == testID` is true whenever `g.phase == PhaseArmed` is — a
// mutation sweep cannot distinguish it and no test can either. It is kept
// because the phase check alone would let a future caller end SOMEONE ELSE'S
// window, which is the mis-attribution this whole file's identity guards
// exist to prevent; the reachable ones are pinned by
// TestARefusalNamingAnotherTestDoesNotEndThisTestsWindow.
func (g *Gate) abortArm(testID, detail string) {
	g.refuse(testID, models.CategoryConsumerTriggerNotDelivered, models.ConsumerEndReasonTriggerNotDelivered, detail)
	g.mu.Lock()
	if g.phase == PhaseArmed && g.arm.TestID == testID {
		g.phase = PhaseDraining
	}
	g.mu.Unlock()
}

// Deliver hands m to the application through the Deliverer registered for
// protocol. It is the ONLY way a recorded payload reaches a consumer, and it
// is DEFAULT-CLOSED in three independent ways: refused unless the gate is
// armed, refused unless the armed test's protocol matches, and refused once
// the armed test's trigger has already gone out.
//
// A parser that gets ErrNotArmed must answer its poll with a synthesized empty
// response. That is not an error path: an unarmed poll happens before test-1,
// between every pair of tests, and after the last one.
func (g *Gate) Deliver(ctx context.Context, protocol string, m *models.Mock) error {
	g.mu.Lock()
	reg, ok := g.deliverers[protocol]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("%w %q", ErrNoDeliverer, protocol)
	}
	d := reg.d
	g.mu.Unlock()
	return g.deliver(ctx, d, protocol, m)
}

// deliver is the guarded delivery path shared by Arm and Deliver. The phase
// check and the single-shot flag are taken together under one lock so two
// concurrent deliveries cannot both see "not yet delivered".
func (g *Gate) deliver(ctx context.Context, d Deliverer, protocol string, m *models.Mock) error {
	g.mu.Lock()
	switch {
	case g.phase != PhaseArmed:
		phase := g.phase
		g.mu.Unlock()
		return fmt.Errorf("%w (phase %s)", ErrNotArmed, phase)
	case g.arm.Protocol != protocol:
		armed := g.arm.Protocol
		g.mu.Unlock()
		return fmt.Errorf("%w: %s is armed, not %s", ErrNotArmed, armed, protocol)
	case g.delivered:
		test := g.arm.TestID
		g.mu.Unlock()
		return fmt.Errorf("%w (%s)", ErrAlreadyDelivered, test)
	}
	g.delivered = true
	g.mu.Unlock()

	if err := d.Deliver(ctx, m); err != nil {
		// Hand the permit back: nothing reached the application, so a
		// retry inside the same window must not be refused as a repeat.
		g.mu.Lock()
		g.delivered = false
		g.mu.Unlock()
		return err
	}
	return nil
}

// ObserveEffect records one live view. It NEVER judges: the comparator on the
// replay side is the only thing that decides whether an effect was expected,
// and it deliberately shares no code with the mock matcher.
//
// Views observed while the gate is not armed are recorded too, not dropped:
// they are the prefetch/overspill cases the next Arm adopts.
func (g *Gate) ObserveEffect(v models.EffectView) {
	g.mu.Lock()
	g.epoch++
	now := g.clock.Now()
	g.obs = append(g.obs, observation{epoch: g.epoch, at: now, view: v})
	g.lastObsAt = now
	phase := g.phase
	g.mu.Unlock()

	if g.logger != nil && phase != PhaseArmed {
		g.logger.Debug("consumer gate observed an effect outside an armed window; it will be adopted by the next test",
			zap.String("phase", string(phase)),
			zap.String("protocol", v.Protocol),
			zap.String("op", v.Op),
			zap.String("target", v.Target),
		)
	}
	g.wake()
}

// MarkTriggerAccepted records the POSITIVE delivery check: the client took the
// trigger and did not come back asking for the same position again.
//
// It is deliberately not "MarkDelivered". Bytes on the wire are not delivery:
// a Kafka client silently discards an incremental fetch response whose session
// id it does not recognise, and a batch below its fetch position, and in both
// cases keploy wrote bytes and the application received zero records. Without
// a positive check that case reports as "the worker stopped producing".
//
// CALLING IT IS AN OBLIGATION, NOT AN OPTIMISATION, for any parser whose
// workers may produce no protocol effects at all. When a test expects zero
// effects — the consume-and-write-to-a-database shape — this is the ONLY
// evidence the gate can have that the application ever received the message,
// so Complete refuses to close such a window on the count rule until it
// arrives and falls through to the timeout backstop, which reports
// trigger_not_delivered. A parser that never calls this turns every
// zero-effect test into a timeout; one that calls it unconditionally, without
// an actual positive check, gives back the false green it exists to remove.
func (g *Gate) MarkTriggerAccepted(testID string) {
	g.mu.Lock()
	if g.arm.TestID == testID {
		g.accepted = true
	}
	g.mu.Unlock()
	g.wake()
}

// MarkTriggerDiscarded records that the client threw the trigger away. It ends
// the window immediately with a named refusal instead of waiting out the
// timeout and reporting a missing effect.
func (g *Gate) MarkTriggerDiscarded(testID, detail string) {
	g.refuse(testID, models.CategoryConsumerTriggerDiscarded, models.ConsumerEndReasonTriggerDiscarded, detail)
}

// Refuse raises a named refusal against testID. It is how a parser reports
// something v1 cannot honestly judge — an un-modelled wire version, a payload
// it declined to guess at — without inventing a verdict.
func (g *Gate) Refuse(testID string, category models.FailureCategory, detail string) {
	g.refuse(testID, category, models.ConsumerEndReasonInternalError, detail)
}

func (g *Gate) refuse(testID string, category models.FailureCategory, reason models.ConsumerEndReason, detail string) {
	g.mu.Lock()
	// First refusal wins: it is the one closest to the cause, and a later
	// one is usually its consequence.
	if g.refusal == "" || g.refusedTest != testID {
		g.refusal, g.endReason, g.refusalDetl, g.refusedTest = category, reason, detail, testID
	}
	g.mu.Unlock()
	if g.logger != nil {
		g.logger.Warn("consumer gate refused a test",
			zap.String("test_id", testID),
			zap.String("category", string(category)),
			zap.String("detail", detail),
		)
	}
	g.wake()
}

// wake nudges a blocked Complete. Non-blocking: the channel has capacity 1 and
// a pending signal already means "recompute".
func (g *Gate) wake() {
	select {
	case g.notify <- struct{}{}:
	default:
	}
}

// Complete blocks until testID's window closes and returns what was observed
// inside it. It is the gate's only blocking call.
//
// THE COMPLETION RULE IS COUNT PLUS A GRACE DRAIN, NEVER AN IDLE TIMER:
//
//	complete  <=>  observed >= expected  AND  there is positive evidence the
//	               application ran (it produced something, or the parser
//	               confirmed it took the trigger)  AND  grace has elapsed
//	               since the later of the arm and the last observation
//	timeout   =>   FAILED, with a NAMED end reason
//
// The grace drain is mandatory and is never skipped. Without it an N+1
// over-production arriving twenty milliseconds after the count is satisfied is
// attributed to the NEXT test and this one passes — which would make the
// over-produce regression uncatchable, and catching it is half the reason this
// exists. An idle timer alone cannot do the job either: it cannot tell "done"
// from "slow", and it cannot see an extra at all.
//
// THE GRACE IS ANCHORED AT max(armedAt, lastObservation), not at the last
// observation alone. Effects adopted from before the arm have timestamps that
// may be arbitrarily old; anchoring on them would let a window close before
// the application had any chance to react to the trigger it was just handed,
// and report count_reached for work the worker never did.
func (g *Gate) Complete(ctx context.Context, testID string) models.ConsumerResult {
	g.mu.Lock()
	if g.phase != PhaseArmed || g.arm.TestID != testID {
		phase, armed := g.phase, g.arm.TestID
		refusal, reason, detail := g.refusal, g.endReason, g.refusalDetl
		refusedTest := g.refusedTest
		g.mu.Unlock()
		if refusedTest == testID && refusal != "" {
			// Arm failed for this very test and already named why.
			return models.ConsumerResult{
				TestID:        testID,
				EndReason:     reason,
				Refusal:       refusal,
				RefusalDetail: detail,
			}
		}
		return models.ConsumerResult{
			TestID:        testID,
			EndReason:     models.ConsumerEndReasonInternalError,
			Refusal:       models.CategoryConsumerUnsupportedAgent,
			RefusalDetail: fmt.Sprintf("Complete(%s) was called while the gate was in phase %q armed for %q; the caller must Arm a test before awaiting it", testID, phase, armed),
		}
	}
	expect := g.arm.Completion.ExpectEffects
	grace := g.arm.Completion.Grace()
	timeoutDeadline := g.armedAt.Add(g.arm.Completion.Timeout())
	// THE BACKSTOP MUST NOT CUT A DRAIN THAT IS ALREADY RUNNING. The two
	// deadlines are anchored differently — the timeout on the arm, the grace
	// on max(arm, last observation) — so an effect landing later than
	// Timeout-Grace after the arm would have its drain truncated: the loop
	// would reach the timeout case with the grace deadline still in the
	// future and close the window with end_reason=timeout, which the judge
	// turns into CONSUMER_COMPLETION_TIMEOUT and a FAILED test even though
	// the expected count was reached. A slow-but-correct worker would go red.
	//
	// So once the count is satisfied the backstop yields to the drain, and
	// hardDeadline bounds how far: at most ONE extra grace window past the
	// timeout. Without that bound a worker producing continuously would keep
	// re-anchoring the grace and the window would never close at all.
	hardDeadline := timeoutDeadline.Add(grace)
	g.mu.Unlock()

	for {
		g.mu.Lock()
		observed, _ := g.pendingLocked()
		anchor := g.armedAt
		if g.lastObsAt.After(anchor) {
			anchor = g.lastObsAt
		}
		graceDeadline := anchor.Add(grace)
		refusal, refusedTest := g.refusal, g.refusedTest
		reason, detail := g.endReason, g.refusalDetl
		accepted := g.accepted
		now := g.clock.Now()
		g.mu.Unlock()

		// POSITIVE EVIDENCE THAT THE APPLICATION ACTUALLY RAN. The count
		// rule alone is satisfied at t=0 for a test that expects zero
		// effects, and a test that expects zero effects is not a corner:
		// it is exactly what the recorder mints for a
		// consume-and-write-to-a-database worker, one of the two most
		// common consumer shapes. Closing such a window on the count
		// would report count_reached for an application that crashed at
		// boot, joined the wrong group or never subscribed — design §5's
		// false-pass row 2, "0 of 0 reads green".
		//
		// The evidence is either of two things, and one of them is always
		// available: the worker produced something (which it cannot do
		// without the message), or the parser confirmed the client TOOK
		// the trigger. Without either, this falls through to the timeout
		// backstop, which reports trigger_not_delivered by name.
		//
		// A parser whose workers may produce nothing therefore MUST call
		// MarkTriggerAccepted — see that method and the Deliverer
		// contract.
		evidence := accepted || observed > 0

		switch {
		case refusal != "" && refusedTest == testID:
			return g.finish(testID, expect, reason, refusal, detail)

		case observed >= expect && evidence && !now.Before(graceDeadline):
			return g.finish(testID, expect, models.ConsumerEndReasonCountReached, "", "")

		case !now.Before(backstopOf(timeoutDeadline, hardDeadline, graceDeadline, observed >= expect && evidence)):
			// A timeout with no positive delivery check is a
			// DIFFERENT failure from a timeout with one: "keploy
			// never got the message to the app" and "the app got
			// the message and produced nothing" need opposite
			// remedies, and a bare "0 effects observed" cannot
			// tell them apart.
			if !accepted {
				return g.finish(testID, expect, models.ConsumerEndReasonTriggerNotDelivered, "",
					"the completion timeout elapsed and the application never took the trigger")
			}
			return g.finish(testID, expect, models.ConsumerEndReasonTimeout, "",
				fmt.Sprintf("the completion timeout elapsed with %d of %d expected effects observed", observed, expect))
		}

		wake := backstopOf(timeoutDeadline, hardDeadline, graceDeadline, observed >= expect && evidence)
		if observed >= expect && evidence && graceDeadline.Before(wake) {
			wake = graceDeadline
		}
		timer, stop := g.clock.Until(wake)
		select {
		case <-timer:
		case <-g.notify:
		case <-ctx.Done():
			stop()
			// A CANCELLED RUN IS NOT AN AGENT THAT LACKS CONSUMER
			// SUPPORT. This path used to report
			// CONSUMER_UNSUPPORTED_AGENT, which sends whoever reads
			// the report looking for a missing capability instead of
			// noticing that someone pressed Ctrl-C or that the run was
			// torn down. The category is the one whose whole reason
			// for existing is to name this.
			return g.finish(testID, expect, models.ConsumerEndReasonInternalError, models.CategoryConsumerRunCancelled,
				"the run was cancelled while waiting for this test's effects: "+ctx.Err().Error())
		}
		stop()
	}
}

// backstopOf returns the instant the completion backstop fires.
//
// It is the arm-anchored timeout, EXCEPT while a grace drain is already under
// way (the expected count is satisfied and there is evidence the application
// ran), in which case it yields to that drain's own deadline — bounded by
// hard, which is one grace window past the timeout. See Complete for why the
// two deadlines would otherwise contradict each other.
func backstopOf(timeout, hard, grace time.Time, draining bool) time.Time {
	if !draining || !grace.After(timeout) {
		return timeout
	}
	if grace.After(hard) {
		return hard
	}
	return grace
}

// pendingLocked returns the record count and the views observed since the last
// completed test. Caller holds g.mu.
//
// A PRESENCE VIEW IS RETURNED BUT NOT COUNTED. The recorder writes
// ExpectEffects from produced records only — it deliberately excludes mapped
// writes, because nothing on the replay side calls ObserveEffect for a
// database write, so counting them at record time would make every
// consume-and-write worker wait out its whole timeout. Counting them HERE
// would break the same arithmetic from the other end, in two ways that both
// redden a healthy test:
//
//  1. one produce plus one presence view reports 2 observed against 1
//     expected, and the judge's count assertion fires EFFECT_UNEXPECTED for a
//     worker that did exactly what it recorded;
//  2. worse, with expect=1 a presence view arriving at t+10ms SATISFIES the
//     count, the grace anchors on it, and the window closes before the
//     produce lands at t+300ms — so this test fails EFFECT_MISSING and the
//     next one fails EFFECT_UNEXPECTED. That is precisely the truncation
//     design §5 deleted idlePollObserved to avoid.
//
// They are still returned in views (nothing observed is ever thrown away; the
// judge filters them out of its pairing) and they still move lastObsAt, which
// only ever EXTENDS the grace — the worker was demonstrably still working.
func (g *Gate) pendingLocked() (records int, views []models.EffectView) {
	for _, o := range g.obs {
		if o.epoch <= g.attributedThrough {
			continue
		}
		if !o.view.IsPresenceOnly() {
			records += o.view.RecordCount()
		}
		views = append(views, o.view)
	}
	return records, views
}

// finish closes the window, attributes the pending observations to testID and
// builds the result.
//
// The attribution snapshot is taken under the lock at THIS instant, not at the
// instant the loop decided to stop, so an effect that arrives in between is
// still counted against this test — it did arrive before the window closed.
// expect is passed in rather than re-read from g.arm: a Reset racing a
// Complete would otherwise produce a result claiming zero expected effects for
// a test that expected several, which reads as a pass.
func (g *Gate) finish(testID string, expect int, reason models.ConsumerEndReason, refusal models.FailureCategory, detail string) models.ConsumerResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	records, views := g.pendingLocked()
	g.attributedThrough = g.epoch

	// Compact only what has now been attributed. This is not the clearing
	// that Arm is forbidden to do: every entry dropped here has just been
	// handed to a completed test, and entries observed after this instant
	// carry a higher epoch and are untouched.
	keep := g.obs[:0]
	for _, o := range g.obs {
		if o.epoch > g.attributedThrough {
			keep = append(keep, o)
		}
	}
	for i := len(keep); i < len(g.obs); i++ {
		g.obs[i] = observation{}
	}
	g.obs = keep

	res := models.ConsumerResult{
		TestID:          testID,
		TriggerAccepted: g.accepted,
		ExpectEffects:   expect,
		ObservedEffects: records,
		EndReason:       reason,
		Effects:         views,
		Refusal:         refusal,
		RefusalDetail:   detail,
	}
	// Only this test's own window is closed. A Reset that already moved the
	// gate back to boot (a new test set) must not be undone here.
	if g.phase == PhaseArmed && g.arm.TestID == testID {
		g.phase = PhaseDraining
	}
	g.completedFor[testID] = true

	if g.logger != nil {
		g.logger.Debug("consumer gate closed a window",
			zap.String("test_id", testID),
			zap.String("end_reason", string(reason)),
			zap.String("refusal", string(refusal)),
			zap.Bool("trigger_accepted", res.TriggerAccepted),
			zap.Int("expected_effects", res.ExpectEffects),
			zap.Int("observed_effects", res.ObservedEffects),
		)
	}
	return res
}

// Reset returns the gate to boot for a new test set.
//
// It is the ONLY place the observation buffer is emptied, and it is safe
// precisely because no window is open at a test-set boundary. It exists
// because --keep-app-alive reuses one application process across test sets: a
// gate left armed, or an adopted effect carried across the boundary, would
// leak one set's state into the next one's first test.
//
// It RETURNS the number of unattributed effect records it dropped, and that
// return value is the only way an over-production after the LAST test of a set
// is ever seen. Within a set such an effect fails the next test as an extra —
// loud, and correct at suite level even though the blame is one window out —
// but the last test has no next test, so without a caller that fails on this
// count a worker that starts emitting an N+1 message just outside the final
// window produces no row, no log line and no non-zero exit.
func (g *Gate) Reset() int {
	g.mu.Lock()
	records, _ := g.pendingLocked()
	phase := g.phase
	armed := g.arm.TestID
	g.phase = PhaseBoot
	g.arm = models.ConsumerArm{}
	g.armedAt = time.Time{}
	g.delivered = false
	g.accepted = false
	g.refusal, g.endReason, g.refusalDetl, g.refusedTest = "", "", "", ""
	g.obs = nil
	g.attributedThrough = g.epoch
	g.lastObsAt = time.Time{}
	g.completedFor = map[string]bool{}
	g.mu.Unlock()

	if g.logger != nil && (records > 0 || phase == PhaseArmed) {
		g.logger.Warn("consumer gate reset with state still open",
			zap.String("phase", string(phase)),
			zap.String("armed_test", armed),
			zap.Int("unattributed_effect_records", records),
			zap.String("next_step", "effects observed after the last test of a set completed are reported here rather than carried into the next set; if this count is non-zero the worker produced after its window closed, which the following set would otherwise have blamed on its own first test"),
		)
	}
	return records
}
