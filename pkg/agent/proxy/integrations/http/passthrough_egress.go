package http

import (
	"net/url"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// passThroughEgressDecision reports whether an outgoing HTTP request targets a
// configured telemetry / noisy-egress endpoint and, if so, the effective mode
// ("skip" or "recordOne"). Configured rules (opts.PassThroughPorts /
// PassThroughHosts) take precedence; the built-in telemetry-path defaults
// (telemetryEgressPaths, see isTelemetryEgress) apply as a "skip" fallback when
// no configured rule matches. host is the destination authority (request Host
// header preferred, URL host next, dstCfg.Addr last); port comes from dstCfg.
//
// The full defaults-registry merge / precedence hardening lives in T10; this is
// the minimal config-first + legacy-default behavior.
func passThroughEgressDecision(opts models.OutgoingOptions, host string, port uint32, method string, u *url.URL) (models.PassThroughRule, bool) {
	if u == nil {
		return models.PassThroughRule{}, false
	}
	reqPath := u.Path
	// User rules + built-in telemetry defaults, matched by specificity.
	rules := models.MergePassThroughDefaults(opts.PassThroughPorts, opts.PassThroughHosts)
	if rule, ok := models.MatchPassThrough(rules, host, port, reqPath); ok {
		// mode:"off" is an explicit opt-out: the rule matched (so it overrode any
		// built-in default for this target), but the endpoint must NOT be passed
		// through — record/replay it as a normal dependency. Return early so the
		// legacy telemetry bypass below does not re-skip it.
		if rule.Mode == models.PassThroughOff {
			return models.PassThroughRule{}, false
		}
		return rule, true
	}
	// Belt-and-suspenders: the legacy exact-path+POST bypass (subset of defaults).
	if isTelemetryEgress(method, u) {
		return models.PassThroughRule{Mode: models.PassThroughSkip}, true
	}
	return models.PassThroughRule{}, false
}

// serveOnePassThroughMock returns the raw response bytes of ONE recorded HTTP
// mock whose method+path match the request, serving it body-agnostically (no
// query/body/header comparison, no fuzzy match) and WITHOUT consuming it — so
// the same recorded response is served for every matching telemetry call. It
// returns nil when no suitable recorded mock exists (the caller then falls back
// to a synthetic 200). Candidates are drawn from both the per-test and session
// pools; a recordOne mock is tagged type:config (LifetimeSession) at record
// time so it lives in the session pool and is never window-filtered.
func (h *HTTP) serveOnePassThroughMock(mockDb integrations.MockMemDb, input *req, queryKeys []string) []byte {
	if mockDb == nil || input == nil || input.url == nil {
		return nil
	}
	var candidates []*models.Mock
	if per, err := mockDb.GetPerTestMocksInWindow(); err == nil {
		candidates = append(candidates, per...)
	}
	if sess, err := mockDb.GetSessionMocks(); err == nil {
		candidates = append(candidates, sess...)
	}
	var fallback *models.Mock
	for _, m := range candidates {
		if m == nil || m.Kind != models.HTTP || m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
			continue
		}
		if m.Spec.HTTPReq.Method != models.Method(input.method) {
			continue
		}
		if mockURLPath(m.Spec.HTTPReq.URL) != input.url.Path {
			continue
		}
		// Significant-query gate: for query-multiplexed collectors (queryKeys
		// non-empty, e.g. New Relic's ?method=), the recorded mock must agree
		// with the request on every significant param — otherwise a connect
		// request could be served a metric_data mock. Empty queryKeys keeps the
		// default body/query-agnostic behavior.
		if !mockQueryMatches(m.Spec.HTTPReq, input.url, queryKeys) {
			continue
		}
		// Prefer a 2xx response (a warmup error may also have been recorded
		// before the first success under recordOne's first-2xx policy).
		if isSuccessStatus(m.Spec.HTTPResp.StatusCode) {
			if out, err := h.buildMockResponseBytes(m); err == nil {
				return out
			}
		} else if fallback == nil {
			fallback = m
		}
	}
	if fallback != nil {
		if out, err := h.buildMockResponseBytes(fallback); err == nil {
			return out
		}
	}
	return nil
}

func isSuccessStatus(code int) bool { return code >= 200 && code < 300 }

// ptRecordKey identifies a recordOne endpoint for record-time de-duplication.
// The body and non-significant query params are excluded by design; host/port
// select the upstream. queryKeys (from the matched rule, in order) names the
// query params that ARE significant to identity — folded in so query-multiplexed
// collectors (New Relic ?method=) keep one mock per operation instead of
// collapsing to a single mock. Empty queryKeys reproduces the path-only key.
func ptRecordKey(method, host string, port uint32, reqPath string, queryKeys []string, query url.Values) string {
	key := method + "\x00" + host + "\x00" + reqPath + "\x00" + itoaU32(port)
	for _, name := range queryKeys {
		key += "\x00" + name + "=" + query.Get(name)
	}
	return key
}

// mockQueryMatches reports whether a recorded mock and the live request agree on
// every significant query param. Empty queryKeys ⇒ true (query-agnostic default).
func mockQueryMatches(mockReq *models.HTTPReq, reqURL *url.URL, queryKeys []string) bool {
	if len(queryKeys) == 0 {
		return true
	}
	reqQ := reqURL.Query()
	for _, name := range queryKeys {
		if mockQueryGet(mockReq, name) != reqQ.Get(name) {
			return false
		}
	}
	return true
}

// mockQueryGet reads a recorded request's query param, preferring the structured
// URLParams map and falling back to parsing the stored full URL.
func mockQueryGet(mockReq *models.HTTPReq, name string) string {
	if v, ok := mockReq.URLParams[name]; ok {
		return v
	}
	if u, err := url.Parse(mockReq.URL); err == nil {
		return u.Query().Get(name)
	}
	return ""
}

func itoaU32(v uint32) string {
	// small, allocation-light uint32→string
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ptRecorder de-duplicates recordOne captures to at most one representative
// exchange per endpoint key, preferring the first 2xx. Worst case (a warmup
// error precedes the first success) it keeps ≤2 mocks; once a 2xx is seen every
// later capture for that key is dropped. Shared across a parser instance's
// connections; per-app isolation for the proxyless path is handled by the
// enterprise gate (T7), not here.
type ptRecorder struct {
	mu   sync.Mutex
	seen map[string]*ptSeenState
}

type ptSeenState struct {
	recordedAny bool
	recorded2xx bool
}

// shouldRecord reports whether a recordOne capture for key with the given
// response status should be persisted (and marks state accordingly).
func (p *ptRecorder) shouldRecord(key string, statusCode int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = make(map[string]*ptSeenState)
	}
	st := p.seen[key]
	if st == nil {
		st = &ptSeenState{}
		p.seen[key] = st
	}
	if st.recorded2xx {
		return false
	}
	if isSuccessStatus(statusCode) {
		st.recorded2xx = true
		st.recordedAny = true
		return true
	}
	if !st.recordedAny {
		st.recordedAny = true
		return true
	}
	return false
}

// passThroughRecordDecision is the record-side counterpart of
// passThroughEgressDecision: it decides whether an outgoing request being
// recorded targets a telemetry/noisy endpoint and, if so, its mode. host is the
// request Host (falling back to URL host); port is the resolved destination port.
func passThroughRecordDecision(opts models.OutgoingOptions, host string, port uint32, method string, u *url.URL) (models.PassThroughRule, bool) {
	return passThroughEgressDecision(opts, host, port, method, u)
}
