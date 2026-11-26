package http

import (
	"context"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// tryReadThrough attempts an exact-match fallback against MockManager's
// deduplicated session read-through tier strictly AFTER the primary per-test,
// session, background, OwnRepeat, extra-call, and AnswerRepeatRead passes miss.
//
// Invariants:
//   - Only read-only HTTP methods (GET, HEAD) are ever queried; writes never hit.
//   - Never masks an already-exhausted stateful walk (diag.answered != nil or
//     lmiss.answered != nil), a variable mismatch (diag.variable != nil), a
//     changed call against one of the test's own recordings (diag.phase ==
//     MatchPhaseStrict or full-round learnMiss), or a moved round (lmiss.moved).
//   - Propagates TLS scheme so live HTTPS calls match recorded https:// mocks
//     without crossing HTTP/HTTPS scheme boundaries.
//   - Matches strictly by canonical request key (exact scheme, host, method,
//     escaped path, sorted query params, and non-flaky headers).
func (h *HTTP) tryReadThrough(ctx context.Context, input *req, mockDb integrations.MockMemDb, diag *matchDiag, lmiss *learnMiss) (*models.Mock, bool) {
	if input == nil || mockDb == nil {
		return nil, false
	}
	if diag != nil && (diag.answered != nil || diag.variable != nil || diag.phase == models.MatchPhaseStrict) {
		return nil, false
	}
	if lmiss != nil && (lmiss.moved || lmiss.answered != nil || lmiss.closest != nil || lmiss.res.Reason != models.MissUnmockedCall) {
		return nil, false
	}
	method := strings.ToUpper(strings.TrimSpace(input.method))
	if method != "GET" && method != "HEAD" {
		return nil, false
	}
	if input.url == nil {
		return nil, false
	}
	headers := make(map[string]string, len(input.header))
	for k, vals := range input.header {
		if len(vals) > 0 {
			headers[k] = strings.Join(vals, ",")
		}
	}
	dest := liveDestination(ctx, input).host
	isTLS := isHTTPExchangeTLS(nil, input.client, input.tls, input.url)
	var meta map[string]string
	if dest != "" {
		meta = map[string]string{"destAddr": dest}
	}
	if isTLS {
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["tls"] = "true"
	}
	live := &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    input.url.String(),
				Header: headers,
				Body:   string(input.body),
			},
		},
	}
	matched, ok := integrations.MatchReadThrough(mockDb, live)
	if !ok || matched == nil {
		return nil, false
	}
	if h.Logger != nil {
		h.Logger.Debug("http: served exact-match fallback from session read-through tier",
			zap.String("mockName", matched.Name),
			zap.String("method", method),
			zap.String("path", input.url.Path))
	}
	return matched, true
}
