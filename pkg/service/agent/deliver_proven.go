package agent

import (
	"context"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// Deliveries (models.Delivery, deliver: in a test's mapping): in a replay,
// keploy sends a test's recorded incoming request (a provider's callback) to
// the application once the application was answered the test's after: mock,
// and the test's end waits for the application's answer to it. The sending
// is the registered coreAgent.WebhookDeliverer's; the agent installs the
// set's deliveries, tells it which test each written answer belongs to, and
// fails a test with the reasons its deliveries failed.

// SetScopeDeliver installs, for a replay, each test's deliveries
// (models.ScopeTableReq.Deliver). An agent with no deliverer refuses a set
// that has any (coreAgent.ErrNoWebhookDeliverer): its callbacks would never
// come, and its tests would fail on calls their application never got.
func (a *Agent) SetScopeDeliver(ctx context.Context, deliveries map[string][]models.DeliveryCase) error {
	d := coreAgent.ActiveWebhookDeliverer
	if d == nil {
		for _, ds := range deliveries {
			if len(ds) > 0 {
				return coreAgent.ErrNoWebhookDeliverer
			}
		}
		return nil
	}
	if err := d.Install(ctx, deliveries); err != nil {
		return err
	}
	integrations.SetAnswerWritten(func(mk *models.Mock) {
		if open := a.openTest(); open != "" {
			d.Served(open, mk.Name)
		}
	})
	return nil
}

// openTest is the open test's ID, "" when none is open.
func (a *Agent) openTest() string {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	return a.proven.open
}

// settleDeliveries waits, in a replay, for test's deliveries to be answered.
// It says whether test has any, and why each that failed did (none when none
// did). The test's window is still open: the calls the application makes
// while handling a delivery are the test's.
func (a *Agent) settleDeliveries(ctx context.Context, test string) (bool, []string) {
	d := coreAgent.ActiveWebhookDeliverer
	if d == nil || !a.replaying() {
		return false, nil
	}
	return d.Settle(ctx, test)
}
