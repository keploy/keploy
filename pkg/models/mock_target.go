package models

import (
	"net/url"
	"strings"
)

// MockTargetFromSpec derives a human-meaningful destination for a mock.
//
// Neither MockEntry (the mapping / expected side) nor MockState
// (the consumed side) records a destination, so the loaded *Mock is the
// only place one exists. Priority order, MOST DISCRIMINATING first — the row
// name has to tell five outgoing calls to the same service apart:
//
//  1. HTTP: method + host + path. A bare host collapses every call to one
//     service into indistinguishable rows; `GET api.internal:8080/orders` is
//     the string a human acts on and an agent keys off.
//  2. Metadata["destAddr"] ("host:port", written by the MySQL and Postgres
//     recorders) PLUS the mock's operation token when it has one, so the two
//     protocols that route every call through a single address still produce
//     distinct rows ("mysql:3306 COM_QUERY").
//  3. The protocol-generic summary (MockSummaryFromSpec), which is what
//     the report UI already shows for MatchedCalls. It beats the bare
//     `operation` token below because for Mongo/MySQL/Postgres it is
//     "<protocol> <operation>" rather than a naked verb.
//  4. The operation token on its own — the only identifying thing some v1
//     mocks carry.
//
// Returns "" when none is available; DepRowName renders correctly without it.
func MockTargetFromSpec(mock *Mock) string {
	if mock == nil {
		return ""
	}
	if d := mock.onDisk; d != nil {
		if d.longTarget {
			return d.display(mock, MockTargetFromSpec, "")
		}
		return d.target
	}
	if t := httpTargetFromReq(mock.Spec.HTTPReq); t != "" {
		return t
	}
	md := mock.Spec.Metadata
	op := mockOperationToken(md)
	if v := strings.TrimSpace(md["destAddr"]); v != "" {
		if op != "" {
			return v + " " + op
		}
		return v
	}
	// MockSummaryFromSpec falls back to the bare Kind when a mock carries
	// nothing identifying. That is already the row's `type` token, so using it
	// as the target too would render "deps[0] redis Redis (presence)".
	if s := strings.TrimSpace(MockSummaryFromSpec(mock)); s != "" && s != string(mock.Kind) {
		return s
	}
	return op
}

// mockOperationToken returns the most specific operation verb a mock's
// metadata carries.
//
// "operation" is the key every v1 recorder writes and the only one
// MockSummaryFromSpec reads. The MySQL v2 recorder writes
// "requestOperation"/"responseOperation" instead (see
// integrations/mysql/recorder/record_v2.go), which is why MockSummaryFromSpec
// returns a bare "MySQL" for those mocks — and why every MySQL dependency row
// would otherwise collapse to the shared "mysql:3306" address with only the
// index to tell them apart. The response side is deliberately not consulted:
// it names the server's reply status ("OK"), not the call.
func mockOperationToken(md map[string]string) string {
	for _, key := range []string{"operation", "requestOperation"} {
		if v := strings.TrimSpace(md[key]); v != "" {
			return v
		}
	}
	return ""
}

// httpTargetFromReq renders an outgoing HTTP call as "<METHOD> <host><path>".
// The scheme is dropped (it is implied by the port and adds noise to a name
// that is already long) but the path is not: the path is the only thing that
// distinguishes two calls to the same service.
func httpTargetFromReq(req *HTTPReq) string {
	if req == nil {
		return ""
	}
	method := strings.TrimSpace(string(req.Method))
	raw := strings.TrimSpace(req.URL)
	target := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		target = u.Host + u.EscapedPath()
	}
	switch {
	case method == "" && target == "":
		return ""
	case method == "":
		return target
	case target == "":
		return method
	}
	return method + " " + target
}
