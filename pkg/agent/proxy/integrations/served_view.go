package integrations

import (
	"context"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TellServed tells the learn step's tracker what F's serve of m, a
// recording an adapter just consumed, gave the application, as see says it:
// as recorded (learn.Tracker.ServedMockAsRecorded) where see's asRecorded is
// true, else the view it gives, under its key (learn.Tracker.ServedMockLive;
// nil, "": a view keploy cannot read, which fails closed). Without a
// statement a later call of the set reads m as a serve no adapter stated,
// and refuses every changed value that may stand there. An adapter tells it
// once for each serve, after the consume that numbered it. Only under the
// proven switch: with it off nothing is told, and see is not run.
func TellServed(ctx context.Context, m *models.Mock, see func() (view func() (*models.Mock, error), key string, asRecorded bool)) {
	if m == nil || !provenswitch.On() {
		return
	}
	t := learn.TrackerFrom(ctx)
	if t == nil {
		return
	}
	view, key, asRecorded := see()
	if asRecorded {
		t.ServedMockAsRecorded(m)
		return
	}
	t.ServedMockLive(m, key, view)
}
