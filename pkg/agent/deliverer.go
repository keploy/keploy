package agent

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/models"
)

// WebhookDeliverer sends a replayed test's recorded incoming requests (a
// provider's callback) to the application, each once the test's mock it waits
// for was answered (models.Delivery). A downstream build registers one
// (RegisterWebhookDeliverer); without one the agent refuses a replay table
// that has deliveries, so a test whose callback cannot come never runs as if
// it would.
type WebhookDeliverer interface {
	// Install replaces the deliveries of the set being replayed, by test ID
	// (models.ScopeTableReq.Deliver). nil or empty: none.
	Install(ctx context.Context, deliveries map[string][]models.DeliveryCase) error
	// Served is told that mock's answer was written to the application
	// while test was the open test. It must not block: it runs on a
	// parser's goroutine.
	Served(test, mock string)
	// Settle waits until every delivery of test that Served started has
	// been answered, or ctx ends. It returns whether test has deliveries,
	// and why each that failed did, in its deliver: order: one never sent
	// (its after: mock was never answered, or its body could not be sent
	// as signed), one the application answered other than recorded. It
	// forgets test's deliveries' progress, so a test run again starts
	// afresh.
	Settle(ctx context.Context, test string) (has bool, failed []string)
}

// ActiveWebhookDeliverer is the registered WebhookDeliverer, nil when none.
var ActiveWebhookDeliverer WebhookDeliverer

// RegisterWebhookDeliverer registers d as the agent's WebhookDeliverer.
func RegisterWebhookDeliverer(d WebhookDeliverer) {
	ActiveWebhookDeliverer = d
}

// ErrNoWebhookDeliverer is a replay table with deliveries given to an agent
// that has no WebhookDeliverer: the replay is refused before any test runs.
var ErrNoWebhookDeliverer = errors.New("this keploy agent cannot deliver recorded webhooks (deliver:): replay them with a keploy build that can")
