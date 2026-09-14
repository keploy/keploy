package proxy

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	"go.keploy.io/server/v3/pkg/models"
)

// The agent-side implementation of replay.ConsumerInstrumentation.
//
// Before these methods existed the interface was a seam with nothing behind
// it: nothing in the repository implemented it, so SimulateRequest's type
// assertion failed for every consumer test in every deployment and reported
// CONSUMER_UNSUPPORTED_AGENT — which is not "inert because OSS ships no
// protocol parser" but "the seam is welded shut". These tests pin the three
// things that make it work: the gate that gets armed is the one the parsers
// see, the recorded trigger is resolved from the resident pool, and every
// ambiguity is a named refusal rather than a guess.

func triggerPoolMock(name string, meta map[string]string) *models.Mock {
	m := consumerfake.Mock(consumerfake.MockOptions{
		Name:  name,
		Role:  models.RoleTrigger,
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	})
	for k, v := range meta {
		m.Spec.Metadata[k] = v
	}
	return m
}

func armFor(testID string) models.ConsumerArm {
	return models.ConsumerArm{
		TestID:     testID,
		TestSetID:  "test-set-0",
		Protocol:   consumerfake.Protocol,
		Trigger:    consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`),
		Completion: models.ConsumerCompletion{ExpectEffects: 1, GraceMs: 1, TimeoutMs: 50},
	}
}

// residentPool puts mocks into the proxy's per-test filtered pool, which is
// where a mapping-based replay leaves the armed test's mocks.
func residentPool(t *testing.T, p *Proxy, mocks ...*models.Mock) {
	t.Helper()
	m := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger)
	p.setMockManager(m)
	t.Cleanup(m.Close)
	m.SetFilteredMocks(mocks)
}

// ARMING HERE OPENS THE WINDOW THE PARSERS DELIVER THROUGH. It is the same
// gate instance in both places, and the recorded mock — not the spec's view of
// it — is what reaches the deliverer, because the spec's body is a
// human-readable projection and the application needs the recorded bytes.
func TestArmConsumerTriggerDeliversTheResidentRecordedMock(t *testing.T) {
	p := newTestProxy(t)
	trigger := triggerPoolMock("mock-41", nil)
	residentPool(t, p, trigger, consumerfake.Mock(consumerfake.MockOptions{Name: "mock-42"}))

	var delivered []*models.Mock
	unregister := p.ConsumerGate().RegisterDeliverer(consumerfake.Protocol,
		consumer.DelivererFunc(func(_ context.Context, m *models.Mock) error {
			delivered = append(delivered, m)
			return nil
		}))
	defer unregister()

	if err := p.ArmConsumerTrigger(context.Background(), armFor("test-1")); err != nil {
		t.Fatalf("ArmConsumerTrigger: %v", err)
	}
	if len(delivered) != 1 || delivered[0] != trigger {
		t.Fatalf("the deliverer received %d mocks; the resident role=trigger mock is what carries the recorded bytes", len(delivered))
	}
	if p.ConsumerGate().Phase() != consumer.PhaseArmed {
		t.Fatalf("phase %q after arming, want armed", p.ConsumerGate().Phase())
	}

	// And the window closes through the same seam.
	p.ConsumerGate().MarkTriggerAccepted("test-1")
	p.ConsumerGate().ObserveEffect(consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`))
	res, err := p.AwaitConsumerEffects(context.Background(), "test-1")
	if err != nil {
		t.Fatalf("AwaitConsumerEffects: %v", err)
	}
	if res.EndReason != models.ConsumerEndReasonCountReached || res.ObservedEffects != 1 {
		t.Fatalf("window closed as %+v", res)
	}
}

// NO RECORDED TRIGGER IS A NAMED REFUSAL, NOT A SILENT PASS. Hooks turns the
// error into a refusal result carrying CONSUMER_TRIGGER_NOT_DELIVERED, which
// is a FAILED test with a reason — not "the worker stopped producing".
func TestArmConsumerTriggerRefusesWhenNoTriggerIsResident(t *testing.T) {
	p := newTestProxy(t)
	residentPool(t, p, consumerfake.Mock(consumerfake.MockOptions{Name: "mock-42"}))
	defer p.ConsumerGate().RegisterDeliverer(consumerfake.Protocol,
		consumer.DelivererFunc(func(context.Context, *models.Mock) error { return nil }))()

	err := p.ArmConsumerTrigger(context.Background(), armFor("test-1"))
	if err == nil {
		t.Fatal("arming a test with no recorded trigger in its pool must be refused")
	}
	if !strings.Contains(err.Error(), string(models.CategoryConsumerTriggerNotDelivered)) {
		t.Fatalf("the refusal must be named, got %v", err)
	}
	if p.ConsumerGate().Phase() != consumer.PhaseBoot {
		t.Fatalf("a refused arm must leave the gate closed, phase is %q", p.ConsumerGate().Phase())
	}
}

// TWO RESIDENT TRIGGERS MEAN THE POOL IS NOT THIS TEST'S POOL. Delivering
// whichever one happened to come first would produce a failure that blames the
// worker for keploy's mistake, so it is refused unless op/target resolve it.
func TestArmConsumerTriggerRefusesAnAmbiguousPool(t *testing.T) {
	p := newTestProxy(t)
	residentPool(t, p, triggerPoolMock("mock-41", nil), triggerPoolMock("mock-51", nil))
	defer p.ConsumerGate().RegisterDeliverer(consumerfake.Protocol,
		consumer.DelivererFunc(func(context.Context, *models.Mock) error { return nil }))()

	err := p.ArmConsumerTrigger(context.Background(), armFor("test-1"))
	if err == nil {
		t.Fatal("two resident triggers make the delivered message undefined; that must be refused")
	}
	if !strings.Contains(err.Error(), string(models.CategoryConsumerTriggerNotDelivered)) {
		t.Fatalf("the refusal must be named, got %v", err)
	}
}

// ...unless the spec's own op/target picks one out. This is the last-resort
// disambiguation, and it uses only what the RECORDING says — never a
// protocol-shaped guess.
func TestArmConsumerTriggerNarrowsAnAmbiguousPoolByOpAndTarget(t *testing.T) {
	p := newTestProxy(t)
	other := triggerPoolMock("mock-51", map[string]string{models.MetaKeyTarget: "payments"})
	wanted := triggerPoolMock("mock-41", map[string]string{models.MetaKeyTarget: "orders"})
	residentPool(t, p, other, wanted)

	var delivered *models.Mock
	defer p.ConsumerGate().RegisterDeliverer(consumerfake.Protocol,
		consumer.DelivererFunc(func(_ context.Context, m *models.Mock) error {
			delivered = m
			return nil
		}))()

	if err := p.ArmConsumerTrigger(context.Background(), armFor("test-1")); err != nil {
		t.Fatalf("ArmConsumerTrigger: %v", err)
	}
	if delivered != wanted {
		t.Fatalf("the trigger for the armed test's own target must be the one delivered, got %v", delivered)
	}
}

// The reset seam: it returns the gate to boot AND reports what was left
// unattributed, which is the only place an over-production after the last test
// of a set is ever seen.
//
// THE COUNT IS A VALUE, NOT AN ERROR, and the distinction is the finding this
// row also pins: an error return here could not be told apart from an
// unreachable agent or a 501 from a build that predates the route, so every
// infrastructure failure was reported to the user as "your worker produced
// after its window closed" with a remedy pointing at their worker.
func TestResetConsumerGateReportsTrailingEffects(t *testing.T) {
	p := newTestProxy(t)
	trailing, err := p.ResetConsumerGate(context.Background(), "test-set-0")
	if err != nil || trailing != 0 {
		t.Fatalf("a clean boundary reports nothing: trailing=%d err=%v", trailing, err)
	}

	p.ConsumerGate().ObserveEffect(consumerfake.View("produce", "order-events", "o-9", `{}`))
	trailing, err = p.ResetConsumerGate(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("a reset that worked must not report an error: %v", err)
	}
	if trailing != 1 {
		t.Fatal("an effect produced after the last test of the set closed its window must be counted; " +
			"within a set it would fail the NEXT test as an extra, and the last test has no next test")
	}
	if p.ConsumerGate().Phase() != consumer.PhaseBoot {
		t.Fatalf("phase %q after reset, want boot", p.ConsumerGate().Phase())
	}
}
