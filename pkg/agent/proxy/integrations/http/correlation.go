package http

import (
	"encoding/json"
	"maps"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// rootRelPath strips the kind-agnostic "body." prefix from a correlation path,
// yielding the dotted JSON path used to compare and capture within the body.
func rootRelPath(p string) string {
	return strings.TrimPrefix(p, "body.")
}

// flattenJSONBody parses a JSON body and returns matcher.Flatten's path→values
// map (index-free dotted paths); nil for an empty or non-JSON body.
func flattenJSONBody(body string) map[string][]string {
	b := strings.TrimSpace(body)
	if b == "" {
		return nil
	}
	var j interface{}
	if err := json.Unmarshal([]byte(b), &j); err != nil {
		return nil
	}
	return matcher.Flatten(j)
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
	liveFlat := flattenJSONBody(string(liveBody))
	if len(liveFlat) == 0 {
		return false, nil, nil
	}
	for _, m := range schemaMatched {
		if m == nil || len(m.Spec.Correlations) == 0 || m.Spec.HTTPReq == nil {
			continue
		}
		recFlat := flattenJSONBody(m.Spec.HTTPReq.Body)
		if len(recFlat) == 0 {
			continue
		}
		// The correlated paths are matched by EXACT equality (a map-key set), not
		// substring — a field named "id" must not swallow "userId"/"orderId".
		// A bare-scalar body flattens to the "" path; refuse to correlate it
		// (ignoring "" would exclude the whole body).
		correlated := make(map[string]bool, len(m.Spec.Correlations))
		bareScalar := false
		for _, c := range m.Spec.Correlations {
			p := rootRelPath(c.RequestPath)
			if p == "" {
				bareScalar = true
				break
			}
			correlated[p] = true
		}
		if bareScalar {
			continue
		}
		// Every NON-correlated field must match exactly, and the live body must
		// carry no extra (non-correlated) field the recording lacks — stricter
		// than the lenient key-schema path, so a correlated mock is never served
		// on a request that differs anywhere but the echoed value.
		if !nonCorrelatedFieldsMatch(recFlat, liveFlat, correlated) {
			continue
		}
		// Capture the live scalar value at each correlated path; all must be a
		// single present value (an absent or array-valued path is not a bind).
		bindings := make(map[string]string, len(m.Spec.Correlations))
		complete := true
		for _, c := range m.Spec.Correlations {
			lv, present := liveFlat[rootRelPath(c.RequestPath)]
			if !present || len(lv) != 1 {
				complete = false
				break
			}
			bindings[c.RequestPath] = lv[0]
		}
		if !complete {
			continue
		}
		return true, m, bindings
	}
	return false, nil, nil
}

// nonCorrelatedFieldsMatch reports whether recorded and live flattened JSON
// bodies are identical on every path EXCEPT the exact correlated paths: each
// recorded non-correlated path must be present in live with equal values, and
// live must carry no non-correlated path absent from the recording (additions
// are rejected — ChangedJSONFieldPaths does not report them, which would make a
// correlated mock match a strictly-larger request).
func nonCorrelatedFieldsMatch(rec, live map[string][]string, correlated map[string]bool) bool {
	for path, recVals := range rec {
		if correlated[path] {
			continue
		}
		if liveVals, ok := live[path]; !ok || !equalStrs(recVals, liveVals) {
			return false
		}
	}
	for path := range live {
		if correlated[path] {
			continue
		}
		if _, ok := rec[path]; !ok {
			return false
		}
	}
	return true
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renderCorrelations returns a serve-safe copy of served with each correlated
// RecordedValue replaced by the captured live value in the response body and
// headers. Value substitution (not a JSON path-set) is correct and robust here
// because correlated values are app-random and effectively unique (see
// appRandomClass): a mis-replacement elsewhere is astronomically unlikely, and
// it naturally covers echoes in arrays, headers (Location/ETag/Set-Cookie) and
// non-JSON response bodies. It copies HTTPResp and its Header before mutating so
// the pooled mock (which Mock.WithResponse may return directly) is never touched.
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
	// An integrity header over the body (Content-MD5, a checksum, an ETag
	// that is its MD5) is recomputed for the body served, where it can be.
	// Where it cannot (an algorithm reSign does not know, a body served
	// encoded), the echo is served as it always was, with the header as
	// recorded.
	if rc.Body != served.Spec.HTTPResp.Body {
		if signed := maps.Clone(rc.Header); reSign(signed, served.Spec.HTTPResp.Body, rc.Body) {
			rc.Header = signed
		}
	}
	out.Spec.HTTPResp = &rc
	return out
}
