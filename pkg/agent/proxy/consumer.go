package proxy

import (
	"context"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The agent-side half of consumer replay.
//
// pkg/service/replay declares what a consumer test needs from an agent
// (replay.ConsumerInstrumentation: arm, await, reset) and reaches it by type
// assertion. THIS FILE IS WHERE THAT CAPABILITY ACTUALLY LIVES for the native
// in-process agent: pkg/service/agent.Agent forwards to these three methods,
// the agent routes expose them over HTTP, and pkg/platform/http.AgentClient
// calls them from the replayer. Without this file the interface is a seam with
// nothing behind it — every consumer test would be refused
// CONSUMER_UNSUPPORTED_AGENT in every deployment, which is a different and
// much worse thing than "inert because OSS ships no consumer protocol parser".
//
// The gate itself is protocol-blind and lives in
// pkg/agent/proxy/integrations/consumer. What this file adds is the one piece
// that needs the proxy: turning "arm test-7" into "hand THIS mock to the
// parser", by finding the recorded trigger in the resident per-test mock pool.

// ArmConsumerTrigger opens the delivery window for one test and hands its
// recorded trigger to the parser that registered a Deliverer for the protocol.
//
// The trigger is resolved from the RESIDENT per-test mock pool rather than
// carried in the arm, because the arm crosses a process boundary (the replayer
// talks to the agent over HTTP) and the payload that has to reach the
// application is the recorded mock's own bytes — the whole point of §8's
// "editing spec.trigger.body does not change what the app receives".
//
// THE DELIVERER OWNS THE CONSUMPTION BOOKKEEPING. Resolving a mock here does
// NOT mark it used: a parser serves the trigger through its own response path
// and is the only thing that knows whether the mock was actually spent. It
// MUST run the served trigger — and every effect mock it answers — through the
// normal DeleteFilteredMock / GetConsumedMocks bookkeeping, or the trigger
// appears in the test's expected mock set and never in its consumed set, and
// the mock-set mismatch fires for EVERY consumer test.
//
// AND THE CONSEQUENCE OF GETTING IT WRONG IS QUIET, NOT LOUD. Read this before
// deciding it can wait. The two halves of the verdict treat an unconsumed
// trigger differently on purpose (pkg/service/replay/depresult.go):
//
//   - replay.neverDemotableKind is keyed on Kind alone, so a consumer test the
//     JUDGE failed stays FAILED and still reddens the set. Nothing is lost
//     there.
//   - replay.missingEffectMockPromotes — the arm that turns a PASSING test red
//     — deliberately skips role=trigger, because an undelivered trigger is
//     keploy failing to deliver rather than the worker failing to produce.
//
// So a consumer test that PASSES with its trigger never marked consumed stays
// PASSED, and the divergence is a Debug line. The suite is green and the
// mapping is silently wrong on every test: harder to notice than a red suite,
// and it degrades the mock-set assertion to nothing for the whole set. Run the
// bookkeeping.
func (p *Proxy) ArmConsumerTrigger(ctx context.Context, arm models.ConsumerArm) error {
	gate := p.ConsumerGate()
	if gate == nil {
		return fmt.Errorf("%s: this proxy has no consumer delivery gate", models.CategoryConsumerUnsupportedAgent)
	}
	trigger, err := p.resolveConsumerTrigger(arm)
	if err != nil {
		return err
	}
	return gate.Arm(ctx, arm, trigger)
}

// AwaitConsumerEffects blocks until testID's delivery window closes under the
// completion rule or its backstop, and returns what was observed inside it.
//
// A refusal is a RESULT, not an error: it is a named FAILED verdict the judge
// renders, and returning it as an error would route the test through
// CreateFailedTestResult with nothing naming the cause.
func (p *Proxy) AwaitConsumerEffects(ctx context.Context, testID string) (*models.ConsumerResult, error) {
	gate := p.ConsumerGate()
	if gate == nil {
		return nil, fmt.Errorf("%s: this proxy has no consumer delivery gate", models.CategoryConsumerUnsupportedAgent)
	}
	res := gate.Complete(ctx, testID)
	return &res, nil
}

// ResetConsumerGate returns the delivery gate to its default-closed boot phase
// at a test-set boundary and reports how many effect records were left
// unattributed by it.
//
// THE LEFTOVER COUNT IS A VALUE, NOT AN ERROR. Effects observed after the last
// test of a set completed are the over-production regression landing one
// window too late: within a set they fail the NEXT test as an extra, which is
// loud and suite-correct, but the last test of the last set has no next test.
// Returning them here is what gives that case somewhere to land.
//
// It used to be returned as an error, and that was wrong in a way that matters
// downstream: the caller then could not tell "the worker over-produced" from
// "the agent is unreachable" or "this agent predates the route", so an
// infrastructure failure was reported to the user as an application regression
// with a remedy pointing at their worker. A count crosses the HTTP boundary as
// data; a transport failure stays an error.
func (p *Proxy) ResetConsumerGate(_ context.Context, _ string) (int, error) {
	gate := p.ConsumerGate()
	if gate == nil {
		return 0, nil
	}
	return gate.Reset(), nil
}

// resolveConsumerTrigger finds the recorded trigger for an armed test in the
// resident per-test mock pool.
//
// EXACTLY ONE MOCK IS EXPECTED and anything else is refused rather than
// guessed. Mapping-based filtering narrows the pool to one test's mocks before
// a consumer test is armed (a consumer set without mappings is refused
// outright, see refuseUnmappedConsumerSet), so a second role=trigger mock in
// the pool means the pool is not the one this test was armed against — and
// delivering the wrong recorded message would produce a failure that blames
// the worker for keploy's mistake.
func (p *Proxy) resolveConsumerTrigger(arm models.ConsumerArm) (*models.Mock, error) {
	mgr := p.getMockManager()
	if mgr == nil {
		return nil, fmt.Errorf("%s: no mock pool is resident, so test %s has no recorded message to deliver", models.CategoryConsumerTriggerNotDelivered, arm.TestID)
	}
	mocks, err := mgr.GetFilteredMocks()
	if err != nil {
		return nil, fmt.Errorf("%s: the resident mock pool could not be read while arming test %s: %w", models.CategoryConsumerTriggerNotDelivered, arm.TestID, err)
	}

	var candidates []*models.Mock
	for _, m := range mocks {
		if m == nil || m.Spec.Metadata[models.MetaKeyRole] != models.RoleTrigger {
			continue
		}
		candidates = append(candidates, m)
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("%s: test %s has no mock carrying %s=%s in its resident pool, so there is no recorded message to hand to the worker", models.CategoryConsumerTriggerNotDelivered, arm.TestID, models.MetaKeyRole, models.RoleTrigger)
	case 1:
		return candidates[0], nil
	}

	// More than one. Narrow by the op and target the spec names — a
	// last-resort disambiguation, and still a refusal when it does not
	// resolve to one.
	var narrowed []*models.Mock
	for _, m := range candidates {
		meta := m.Spec.Metadata
		if !metaMatches(meta[models.MetaKeyOp], arm.Trigger.Op) || !metaMatches(meta[models.MetaKeyTarget], arm.Trigger.Target) {
			continue
		}
		narrowed = append(narrowed, m)
	}
	if len(narrowed) == 1 {
		return narrowed[0], nil
	}
	p.logger.Error("more than one recorded trigger is resident for a consumer test",
		zap.String("test_id", arm.TestID),
		zap.String("test_set_id", arm.TestSetID),
		zap.Int("candidates", len(candidates)),
		zap.String("next_step", "this test set's per-test mock mapping does not narrow the pool to one message; regenerate it with --update-test-mapping, or re-record the set"))
	return nil, fmt.Errorf("%s: %d mocks carrying %s=%s are resident while arming test %s, so which recorded message the worker would receive is undefined", models.CategoryConsumerTriggerNotDelivered, len(candidates), models.MetaKeyRole, models.RoleTrigger, arm.TestID)
}

// metaMatches compares a mock's metadata value with the spec's, treating an
// absent value on either side as "no claim" rather than as a mismatch. A
// parser that stamps role but not op is still usable; a parser that stamps a
// DIFFERENT op is not.
func metaMatches(meta, want string) bool {
	meta, want = strings.TrimSpace(meta), strings.TrimSpace(want)
	return meta == "" || want == "" || meta == want
}
