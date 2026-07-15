package http

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
)

// compile-time assertions.
var _ async.AsyncParser = (*HTTP)(nil)
var _ async.AsyncAware = (*HTTP)(nil)

// SetAsyncEngine stores the shared engine (setter injection).
func (h *HTTP) SetAsyncEngine(e *async.Engine) { h.asyncEngine = e }

// MatchesLane matches host + path globs (path.Match) from lane.Match against
// the mock's recorded request URL.
func (h *HTTP) MatchesLane(m *models.Mock, lane models.AsyncLane) bool {
	if m == nil || m.Spec.HTTPReq == nil {
		return false
	}
	host, p := hostAndPath(m.Spec.HTTPReq)
	if hg := lane.Match["host"]; hg != "" {
		if ok, _ := path.Match(hg, host); !ok {
			return false
		}
	}
	if pg := lane.Match["path"]; pg != "" {
		if ok, _ := path.Match(pg, p); !ok {
			return false
		}
	}
	return lane.Match["host"] != "" || lane.Match["path"] != ""
}

// MatchRequestShape reuses SchemaMatch against the single recorded mock.
// Volatile query params (lane.VolatileParams) are stripped from BOTH the
// live and the recorded request before comparison: SchemaMatch's query-param
// check (MapsHaveSameKeys) compares the KEY SET of the live request's parsed
// URL query against the recorded mock's URLParams, so stripping only one
// side would leave the key counts unequal (e.g. "cursor" present on the live
// side but absent on the stripped recorded side) and spuriously fail the
// shape match — stripping both sides keeps the key-set comparison honest
// while still ignoring the volatile key's value.
func (h *HTTP) MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (bool, string) {
	if live == nil || live.Spec.HTTPReq == nil || recorded == nil || recorded.Spec.HTTPReq == nil {
		return false, "missing request payload"
	}
	liveStripped := stripVolatile(live, lane.VolatileParams)
	liveReq, err := mockToReq(liveStripped)
	if err != nil {
		return false, "unparseable live request: " + err.Error()
	}
	rec := stripVolatile(recorded, lane.VolatileParams)
	// SchemaMatch does field-by-field request-shape comparison; a non-empty
	// result means the single candidate matched.
	matched, err := h.SchemaMatch(context.Background(), liveReq, []*models.Mock{rec}, flakyHeaderNoise(), nil, true)
	if err != nil {
		return false, "schema match error: " + err.Error()
	}
	if len(matched) == 0 {
		return false, fmt.Sprintf("request shape drift: %s %s vs %s",
			liveReq.method, liveReq.url.Path, recorded.Spec.HTTPReq.URL)
	}
	return true, ""
}

// EmptyResponse is a minimal 204 keep-alive.
func (h *HTTP) EmptyResponse(_ models.AsyncLane) ([]byte, error) {
	return []byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"), nil
}

func hostAndPath(r *models.HTTPReq) (host, p string) {
	if r.Header != nil && r.Header["Host"] != "" {
		host = r.Header["Host"]
	}
	if u, err := url.Parse(r.URL); err == nil {
		if host == "" {
			host = u.Host
		}
		p = u.Path
	}
	return host, p
}

// stripVolatile returns a shallow copy of the mock with volatile query params
// removed from URL + URLParams, so key-set comparison ignores them.
func stripVolatile(m *models.Mock, volatile []string) *models.Mock {
	if len(volatile) == 0 {
		return m
	}
	vol := make(map[string]bool, len(volatile))
	for _, v := range volatile {
		vol[v] = true
	}
	req := *m.Spec.HTTPReq
	if u, err := url.Parse(req.URL); err == nil {
		q := u.Query()
		for k := range vol {
			q.Del(k)
		}
		u.RawQuery = q.Encode()
		req.URL = u.String()
	}
	if req.URLParams != nil {
		np := make(map[string]string, len(req.URLParams))
		for k, v := range req.URLParams {
			if !vol[k] {
				np[k] = v
			}
		}
		req.URLParams = np
	}
	cp := *m
	sp := m.Spec
	sp.HTTPReq = &req
	cp.Spec = sp
	return &cp
}

// mockToReq builds the matcher's `req` value from a recorded/live mock.
func mockToReq(m *models.Mock) (*req, error) {
	u, err := url.Parse(m.Spec.HTTPReq.URL)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	for k, v := range m.Spec.HTTPReq.Header {
		hdr.Set(k, v)
	}
	return &req{
		method: string(m.Spec.HTTPReq.Method),
		url:    u,
		header: hdr,
		body:   []byte(m.Spec.HTTPReq.Body),
	}, nil
}

// flakyHeaderNoise returns the package flaky-header list as a header-noise map.
func flakyHeaderNoise() map[string][]string {
	nm := make(map[string][]string, len(flakyHeaders))
	for _, fh := range flakyHeaders {
		nm[fh] = []string{}
	}
	return nm
}
