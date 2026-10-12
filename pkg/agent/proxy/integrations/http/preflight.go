package http

import (
	"net/http"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// preflightCandidates narrows the candidates for a CORS preflight to the
// recordings of the same preflight.
//
// A preflight (OPTIONS carrying Access-Control-Request-Method) asks whether a
// request may be sent, and its answer is about that request: the
// Access-Control-Allow-Headers of the reply lists the headers it was asked
// about. A preflight has no body, so the exact match (ExactBodyMatch) holds for
// every recorded preflight of the URL, and the next one in line answered. When
// one page sends the same URL with different headers, that can be the answer
// to another preflight. Measured on an enterprise-ui replay: the page's own
// GraphQL calls (Content-Type) and the test's (Authorization, Content-Type) both
// preflight POST /query; the test's commit was answered with the page's
// preflight, Chrome refused to send it ("Request header field authorization is
// not allowed by Access-Control-Allow-Headers in preflight response"), and the
// test went on without the write it had recorded.
//
// The recordings kept are those whose Origin, Access-Control-Request-Method and
// Access-Control-Request-Headers (as a set, case-insensitive) are the live
// preflight's. When none is, the candidates are returned as they were: the
// preflight is matched as before rather than missed.
func preflightCandidates(input *req, candidates []*models.Mock) []*models.Mock {
	if input == nil || input.method != http.MethodOptions {
		return candidates
	}
	method := input.header.Get("Access-Control-Request-Method")
	if method == "" {
		return candidates
	}
	headers := headerNameSet(input.header.Get("Access-Control-Request-Headers"))
	origin := input.header.Get("Origin")
	var kept []*models.Mock
	for _, m := range candidates {
		if m == nil || m.Spec.HTTPReq == nil {
			continue
		}
		rec := m.Spec.HTTPReq.Header
		if !strings.EqualFold(headerValue(rec, "Access-Control-Request-Method"), method) ||
			headerNameSet(headerValue(rec, "Access-Control-Request-Headers")) != headers ||
			headerValue(rec, "Origin") != origin {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return candidates
	}
	return kept
}

// headerNameSet is a comma-separated list of header names as a canonical set:
// lower case, trimmed, sorted, deduplicated.
func headerNameSet(list string) string {
	seen := map[string]struct{}{}
	var names []string
	for _, n := range strings.Split(list, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
