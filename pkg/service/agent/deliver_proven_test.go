package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// fakeDeliverer records what the agent tells it, and settles each test with
// the answer set for it.
type fakeDeliverer struct {
	a         *Agent
	installed map[string][]models.DeliveryCase
	served    []string
	settled   []string
	openAt    []string
	settledAt []time.Time
	// mayWait is, for each Settle, whether its ctx let it wait.
	mayWait []bool
	answers map[string][]string
	err     error
}

func (d *fakeDeliverer) Install(_ context.Context, ds map[string][]models.DeliveryCase) error {
	d.installed = ds
	return d.err
}

func (d *fakeDeliverer) Served(test, mock string) { d.served = append(d.served, test+" "+mock) }

func (d *fakeDeliverer) Settle(ctx context.Context, test string) (bool, []string) {
	d.settled = append(d.settled, test)
	d.mayWait = append(d.mayWait, ctx.Err() == nil)
	d.openAt = append(d.openAt, d.a.openTest())
	d.settledAt = append(d.settledAt, time.Now())
	failed, has := d.answers[test]
	return has, failed
}

func deliverWith(t *testing.T, d coreAgent.WebhookDeliverer) {
	prev := coreAgent.ActiveWebhookDeliverer
	coreAgent.ActiveWebhookDeliverer = d
	t.Cleanup(func() {
		coreAgent.ActiveWebhookDeliverer = prev
		integrations.SetAnswerWritten(nil)
	})
}

var payCallback = map[string][]models.DeliveryCase{"o::TestA": {{
	Delivery: models.Delivery{Case: "test-2", After: "mock-3"},
	Req:      models.HTTPReq{Method: models.Method("POST"), URL: "http://127.0.0.1:18110/webhooks/pay", Body: `{"order":"o-1"}`},
	Resp:     models.HTTPResp{StatusCode: 200},
}}}

// An agent with no deliverer refuses a replay table with any delivery: those
// tests would wait for a callback that never comes. A table with none (or
// only empty lists) is taken.
func TestADeliveryNeedsAnAgentThatCanSendIt(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	deliverWith(t, nil)
	ctx := context.Background()
	require.True(t, errors.Is(a.SetScopeDeliver(ctx, payCallback), coreAgent.ErrNoWebhookDeliverer))
	require.NoError(t, a.SetScopeDeliver(ctx, nil))
	require.NoError(t, a.SetScopeDeliver(ctx, map[string][]models.DeliveryCase{"o::TestA": {}}))

	// A deliverer that cannot take them refuses the set with its reason.
	deliverWith(t, &fakeDeliverer{a: a, err: errors.New("case test-2 has no request")})
	require.EqualError(t, a.SetScopeDeliver(ctx, payCallback), "case test-2 has no request")
}

// The deliverer is given the set's deliveries, and told each answer written
// to the application with the test open then: none when no test is open. A
// test's end waits for its deliveries with its window still open, and a
// delivery that failed fails the test with the deliverer's reason; one that
// passed fails nothing. A test with no delivery ends at its harness's end.
func TestAWrittenAnswerReachesTheOpenTestsDeliveries(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	const notDelivered = "test-2 (POST /webhooks/pay) was not delivered: its after: mock-3 was never called in o::TestA; set after: to the call that triggers it"
	d := &fakeDeliverer{a: a, answers: map[string][]string{"o::TestA": {notDelivered}, "o::TestB": nil}}
	deliverWith(t, d)
	ctx := context.Background()
	require.NoError(t, a.SetScopeDeliver(ctx, payCallback))
	require.Equal(t, payCallback, d.installed)
	claimSelf(t, a)

	integrations.AnswerWritten(&models.Mock{Name: "mock-3"})
	require.Empty(t, d.served, "an answer written while no test is open is no test's")

	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	integrations.AnswerWritten(&models.Mock{Name: "mock-3"})
	integrations.AnswerWritten(nil)
	require.Equal(t, []string{"o::TestA mock-3"}, d.served)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []string{"o::TestA"}, d.settled)
	require.Equal(t, []string{"o::TestA"}, d.openAt, "a test's deliveries settle while its window is open")
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: "+notDelivered), e.Reason)
	reasonA := e.Reason

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a delivery answered as recorded fails nothing")
	// Read last: reading the windows ends the run.
	wa, wb := windowOf(t, a, "o::TestA"), windowOf(t, a, "o::TestB")
	require.Equal(t, reasonA, wa.Evidence.Reason)
	require.Equal(t, models.ScopeOutcomePassed, wb.Outcome)
	// A test with deliveries ends once they were answered, not at its
	// harness's end: the application's calls handling them are inside.
	require.False(t, wa.End.Before(d.settledAt[0]), "%v ended before its deliveries settled at %v", wa.End, d.settledAt[0])
	require.False(t, wb.End.Before(d.settledAt[1]), "%v ended before its deliveries settled at %v", wb.End, d.settledAt[1])
}

// A test the run's end finds still open (its process waited for a callback
// that never came, until its own framework stopped it) fails naming each
// recorded callback not delivered, with its case and the mock it waited for,
// before the reap's reason; the run's end waits for no answer. A test that
// ended is not settled again.
func TestARunsEndNamesTheCallbacksOfATestLeftOpen(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	const notDelivered = "test-2 (POST /webhooks/pay) was not delivered: its after: mock-3 was never called in o::TestA; set after: to the call that triggers it"
	d := &fakeDeliverer{a: a, answers: map[string][]string{"o::TestA": {notDelivered}, "o::TestB": nil}}
	deliverWith(t, d)
	ctx := context.Background()
	require.NoError(t, a.SetScopeDeliver(ctx, payCallback))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)

	// Reading the windows is the run's end.
	wa := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, wa.Outcome)
	require.True(t, strings.HasPrefix(wa.Evidence.Reason, "keploy: "+notDelivered+"; "), wa.Evidence.Reason)
	require.Contains(t, wa.Evidence.Reason, "outlived the test command")
	require.Equal(t, []string{"o::TestB", "o::TestA"}, d.settled)
	require.Equal(t, []bool{true, false}, d.mayWait, "the run's end waits for no callback's answer")
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "o::TestB").Outcome)
}

// A test left open with no delivery that failed fails with the reap's
// reason alone.
func TestARunsEndWithNoUndeliveredCallbackKeepsItsReason(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	d := &fakeDeliverer{a: a, answers: map[string][]string{"o::TestA": nil}}
	deliverWith(t, d)
	ctx := context.Background()
	require.NoError(t, a.SetScopeDeliver(ctx, payCallback))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	wa := windowOf(t, a, "o::TestA")
	require.Equal(t, []string{"o::TestA"}, d.settled)
	require.True(t, strings.HasPrefix(wa.Evidence.Reason, "keploy: the test was still open"), wa.Evidence.Reason)
	require.NotContains(t, wa.Evidence.Reason, ";")
}

// A record sends no delivery: its provider sends the callback itself. Its
// tests' ends never wait on the deliverer.
func TestARecordSettlesNoDelivery(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	d := &fakeDeliverer{a: a, answers: map[string][]string{"o::TestA": {"not delivered"}}}
	deliverWith(t, d)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, d.settled)
	require.NotEqual(t, models.ScopeActionFail, e.Action)
}
