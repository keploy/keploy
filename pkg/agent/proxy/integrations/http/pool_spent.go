package http

import (
	"context"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// spentPoolRecording is, for a request no mock answered under exact serving
// (the proven switch), the start-up or between-tests recording that is this
// very call (the strict comparison, strictDiff, as notOwnedRecording reads
// it) which another call of the replay already used up
// (integrations.SpentPoolReader), with where it went: the first in recorded
// order. ok is false when there is none, or the store keeps no such account.
// ctx carries the request's destination as it came (liveDestination).
func spentPoolRecording(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (integrations.PoolSpend, bool) {
	r, ok := mockDb.(integrations.SpentPoolReader)
	if !ok {
		return integrations.PoolSpend{}, false
	}
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	target := strings.Join(input.header.Values(amzTargetHeader), ",")
	live := liveDestination(ctx, input)
	for _, s := range r.SpentPool(models.HTTP, RequestKeysOf(input.method, path, target, n.loose())) {
		if s.Mock != nil && s.Mock.Spec.HTTPReq != nil && len(strictDiff(ctx, input, live, s.Mock, n)) == 0 {
			return s, true
		}
	}
	return integrations.PoolSpend{}, false
}

// markPoolSpent makes report the miss of a call that is s.Mock, a start-up
// or between-tests recording another call already used
// (spentPoolRecording): integrations.MarkPoolSpent, with s.Mock's rendered
// request as the closest's.
func markPoolSpent(report *models.MockMismatchReport, s integrations.PoolSpend) {
	integrations.MarkPoolSpent(report, s)
	report.ClosestMockReq = renderMockRequest(s.Mock.Spec.HTTPReq, s.Mock.Noise)
}
