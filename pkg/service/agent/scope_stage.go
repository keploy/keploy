package agent

import (
	"context"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// stageScope stages the pool of keploy mock replay, with the proven switch
// off, as a test its harness began (open) or ended (!open) without a worker
// pid: the single global pool every connection reads.
//
// A begin stages the test's own per-test mocks (names, its mapping, which
// may be empty: a test recorded with no call has none) and the reusable tier,
// and opens a test window for them (ScopeStager), so tier-aware parsers route
// the test's calls to the test's own tier and report a miss as one inside a
// test. A call that is answered by a per-test recording in a test is answered
// by one of the test's own: a call the test did not record, or whose only
// recording was made outside every test, misses, and the miss is reported.
// The reusable tier is served in every test, as always; it holds the
// session, config and connection recordings, and, unless
// KEPLOY_STRICT_MOCK_WINDOW is set (the lax rule reads that variable alone,
// whatever test.strictMockWindow says), every data mock of the kinds the
// legacy lax rule files there (models.Mock.DeriveLifetime: HTTP, HTTP/2,
// MySQL, Postgres v1 and v2, Generic, DNS), which a test's mapping therefore
// does not narrow. The test's own mocks are staged afresh, so each run of a
// test is served its whole recording.
//
// This holds for the parsers that read the per-test tier while a window is
// open (Postgres v3, Mongo v2 and the parsers that read the per-test and
// reusable tiers as they are). A parser that re-reads its pool only when the
// test window's recorded-time bounds change (enterprise Pulsar) sees none
// change here, as it saw none before.
//
// An end closes the window and stages the pool between tests as an end
// always did (the whole pool, served as before the first test), less every
// per-test mock served while a test was open, which the proxy leaves out
// (ScopeStager): a test's recording answers no call after its test ended.
//
// That the pool staged now reaches a parser depends on the parser reading its
// tiers as they are staged. A parser that indexes a tier once per connection
// (Postgres v3 before integrations' live tier reads) keeps serving, on a
// connection opened earlier, the pool of that moment.
//
// A proxy that is not a ScopeStager is staged with the models.BaseTime
// sentinel, which opens no window: the pool a begin always staged, and for a
// test recorded with no call, the reusable tier only.
func (a *Agent) stageScope(ctx context.Context, names []string, open bool) error {
	params := models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}
	if open {
		// Exact: an empty mapping is the test's whole recording, not "none
		// supplied" (which would stage the whole pool as the test's own).
		params.MockMapping, params.UseMappingBased, params.ExactMapping = names, true, true
	}
	stager, ok := a.Proxy.(coreAgent.ScopeStager)
	if !ok {
		return a.UpdateMockParams(ctx, params)
	}
	filtered, unfiltered, err := a.mocksForParams(ctx, params)
	if err != nil {
		return err
	}
	if err := stager.SetScopeMocks(ctx, filtered, unfiltered, open); err != nil {
		utils.LogContentError(a.logger, err, "failed to stage the mocks of a test on the proxy")
		return err
	}
	return nil
}
