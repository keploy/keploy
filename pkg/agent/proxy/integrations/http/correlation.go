package http

import (
	"maps"
	"strings"

	"github.com/tidwall/gjson"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// rootRelPath strips the kind-agnostic "body." prefix from a correlation path,
// yielding the dotted JSON path that gjson and ChangedJSONFieldPaths use.
func rootRelPath(p string) string {
	return strings.TrimPrefix(p, "body.")
}

// correlationMatch selects a mock whose request carries an app-random value the
// dependency echoes back (Spec.Correlations). On replay the app mints a NEW
// value, so the live request never byte-matches the recording — this compares
// every NON-correlated field exactly (treating each correlated RequestPath as
// ignorable) and, on a full match, captures the live value at each correlated
// path. It returns the matched mock and the bindings (RequestPath -> live
// value) the response render uses. JSON request bodies only (v1), matching the
// detector's scope; non-JSON or non-correlated mocks fall through to the normal
// cascade.
func (h *HTTP) correlationMatch(liveBody []byte, schemaMatched []*models.Mock, enabled bool) (bool, *models.Mock, map[string]string) {
	if !enabled || !pkg.IsJSON(liveBody) {
		return false, nil, nil
	}
	for _, m := range schemaMatched {
		if m == nil || len(m.Spec.Correlations) == 0 || m.Spec.HTTPReq == nil {
			continue
		}
		recBody := m.Spec.HTTPReq.Body
		if !pkg.IsJSON([]byte(recBody)) {
			continue
		}
		// Ignore the correlated paths when comparing the rest of the body; any
		// OTHER differing field rejects this candidate.
		known := make(map[string][]string, len(m.Spec.Correlations))
		for _, c := range m.Spec.Correlations {
			known[rootRelPath(c.RequestPath)] = []string{}
		}
		if drift := matcher.ChangedJSONFieldPaths(recBody, string(liveBody), known, nil, false, nil); len(drift) > 0 {
			continue
		}
		// Capture the live value at each correlated path; all must be present to
		// bind (a vanished correlated field is not a match).
		bindings := make(map[string]string, len(m.Spec.Correlations))
		complete := true
		for _, c := range m.Spec.Correlations {
			res := gjson.GetBytes(liveBody, rootRelPath(c.RequestPath))
			if !res.Exists() {
				complete = false
				break
			}
			bindings[c.RequestPath] = res.String()
		}
		if !complete {
			continue
		}
		return true, m, bindings
	}
	return false, nil, nil
}

// renderCorrelations returns a serve-safe copy of served with each correlated
// RecordedValue replaced by the captured live value in the response body and
// headers. Value substitution (not a JSON path-set) is correct and robust here
// because correlated values are app-random and effectively unique (see
// appRandomClass): a mis-replacement elsewhere is astronomically unlikely, and
// it naturally covers echoes in arrays, headers (Location/ETag/Set-Cookie) and
// non-JSON response bodies. It copies HTTPResp and its Header before mutating so
// the pooled mock (which withResponse may return directly) is never touched.
func renderCorrelations(served *models.Mock, bindings map[string]string) *models.Mock {
	if served == nil || len(bindings) == 0 || len(served.Spec.Correlations) == 0 || served.Spec.HTTPResp == nil {
		return served
	}
	out := served.ShallowCopy()
	rc := *out.Spec.HTTPResp
	rc.Header = maps.Clone(rc.Header)
	changed := false
	for _, c := range out.Spec.Correlations {
		live, ok := bindings[c.RequestPath]
		if !ok || c.RecordedValue == "" || live == "" || live == c.RecordedValue {
			continue
		}
		rc.Body = strings.ReplaceAll(rc.Body, c.RecordedValue, live)
		for k, v := range rc.Header {
			rc.Header[k] = strings.ReplaceAll(v, c.RecordedValue, live)
		}
		changed = true
	}
	if !changed {
		return served
	}
	out.Spec.HTTPResp = &rc
	return out
}
