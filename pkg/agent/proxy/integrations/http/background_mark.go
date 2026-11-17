package http

import (
	"context"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A MISS THE APPLICATION'S BACKGROUND WORK MADE (models.MockMismatchReport.
// Background), as keploy's HTTP parser reads it.
//
// A call no recording answered is still a miss, answered with keploy's miss
// answer. When it is the same request as one of the set's HTTP background
// recordings (its mappings.yaml worker: and startup: mocks, of the set the
// call was marked in, served or not, as the store hands them:
// integrations.BackgroundRecordingsAt), it is one more run of the
// application's own background work: a liveness probe or a poll on a timer
// made once more than a recording was left to answer, or a declared series'
// call made again. Its miss is marked Background: listed with the test's
// misses, it counts against no test and fails none
// (models.UnmatchedCall.CountsAgainstTest), so judging learns the test's
// noise past it as past no miss.
//
// The same request is the strict comparison's, and no other (strictFits):
// equal to the recording in every part it compares, but keploy.yml's noise,
// the noise learned onto the recording, its obfuscated values, and the
// places the learn step learned, or a declared rule names, as reads of the
// application's clock, each checked against the call's own window on that
// clock (clockDiffs). A declared series' call that differs from its
// recording only at the clocks its rules declare is one. A call that differs
// anywhere else (a version read back from an earlier answer, an id the test
// minted, another path or query) is not, and stays its test's miss, as is
// every miss of a store that hands no background recordings.

// markBackground marks report Background when the call (input, its
// connection's ctx as it came, which tells its destination and its window
// on the application's clock) is the same request as one of the HTTP
// background recordings of the set it was marked in (call), read with n: the
// run's noise and the store's declared rules. It changes nothing else of the
// report.
func (h *HTTP) markBackground(ctx context.Context, report *models.MockMismatchReport, input *req, mockDb integrations.MockMemDb, call integrations.CallRound, n strictNoise) {
	if report == nil {
		return
	}
	recs := integrations.BackgroundRecordingsAt(mockDb, call, models.HTTP)
	pool := make([]*models.Mock, 0, len(recs))
	for _, r := range recs {
		if r.Mock != nil {
			pool = append(pool, r.Mock)
		}
	}
	if fits, _ := strictFits(ctx, input, liveDestination(ctx, input), pool, n, h.verifying()); len(fits) > 0 {
		report.Background = true
	}
}
