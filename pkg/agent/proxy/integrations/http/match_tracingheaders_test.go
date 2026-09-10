package http

import (
	"net/http"
	"testing"
)

// TestHeadersContainKeys_ConditionalDatadogTagsHeader reproduces a miss seen in
// the field.
//
// A Ruby service fetches its JWKS from an identity provider. At record time
// dd-trace-rb emitted X-Datadog-Tags because the trace carried _dd.p.* tags
// (128-bit trace id, sampling decision). At replay the trace carried none, so
// the tracer emitted no X-Datadog-Tags at all.
//
// HeadersContainKeys is presence-only, so one absent recorded header rejects
// the candidate before any value is compared. The whole HTTP mock pool was
// still intact, the JWKS mock among it, yet the match reported
// "no_schema_candidates". The app got no keys back, JWT verification raised
// "No verification key available", and the request returned 401 — which reads
// as an application auth bug, not a mock miss.
//
// Only a header that vanishes ENTIRELY is fatal here, which is why value noise
// cannot cover this case and the key has to be in the allowlist.
func TestHeadersContainKeys_ConditionalDatadogTagsHeader(t *testing.T) {
	h := newHTTP()

	// Recorded while the trace carried 128-bit ids and a sampling decision, so
	// the tracer emitted _dd.p.tid and _dd.p.dm — and therefore X-Datadog-Tags.
	recorded := map[string]string{
		"Accept":                      "*/*",
		"Accept-Encoding":             "gzip;q=1.0,deflate;q=0.6,identity;q=0.3",
		"Baggage":                     "sentry-trace_id=1111111111111111aaaaaaaaaaaaaaaa,sentry-environment=sandbox,sentry-release=1a2b3c4d",
		"Connection":                  "close",
		"Host":                        "identity.example.com",
		"Sentry-Trace":                "1111111111111111aaaaaaaaaaaaaaaa-1111111111111111",
		"Traceparent":                 "00-1111111111111111aaaaaaaaaaaaaaaa-1111111111111111-01",
		"Tracestate":                  "dd=p:1111111111111111;s:1;t.dm:-1",
		"User-Agent":                  "Ruby",
		"X-B3-Sampled":                "1",
		"X-B3-Spanid":                 "1111111111111111",
		"X-B3-Traceid":                "1111111111111111aaaaaaaaaaaaaaaa",
		"X-Datadog-Parent-Id":         "1229782938247303441",
		"X-Datadog-Sampling-Priority": "1",
		"X-Datadog-Tags":              "_dd.p.dm=-1,_dd.p.tid=1111111111111111",
		"X-Datadog-Trace-Id":          "1229782938247303441",
	}

	// Replayed while the trace carried neither: note the all-zero high half of
	// the trace id (no _dd.p.tid) and the Tracestate without t.dm (no _dd.p.dm).
	// With no propagation tags to carry, the tracer emits no X-Datadog-Tags at
	// all. Every OTHER header here is present but differs in value, and none of
	// those matter — presence-only matching lets a changed value through.
	live := http.Header{
		"Accept":                      {"*/*"},
		"Accept-Encoding":             {"gzip;q=1.0,deflate;q=0.6,identity;q=0.3"},
		"Baggage":                     {"sentry-trace_id=00000000000000002222222222222222,sentry-environment=sandbox,sentry-release=5e6f7a8b"},
		"Connection":                  {"close"},
		"Host":                        {"identity.example.com"},
		"Sentry-Trace":                {"00000000000000002222222222222222-2222222222222222"},
		"Traceparent":                 {"00-00000000000000002222222222222222-2222222222222222-01"},
		"Tracestate":                  {"dd=p:2222222222222222;s:1"},
		"User-Agent":                  {"Ruby"},
		"X-B3-Sampled":                {"1"},
		"X-B3-Spanid":                 {"2222222222222222"},
		"X-B3-Traceid":                {"00000000000000002222222222222222"},
		"X-Datadog-Parent-Id":         {"2459565876494606882"},
		"X-Datadog-Sampling-Priority": {"1"},
		"X-Datadog-Trace-Id":          {"2459565876494606882"},
		// X-Datadog-Tags: absent — this is the whole test.
	}

	noise := make(map[string][]string, len(flakyHeaders))
	for _, hdr := range flakyHeaders {
		noise[hdr] = []string{}
	}

	if !h.HeadersContainKeys(recorded, live, noise) {
		t.Fatal("the JWKS mock must still match when the tracer omits X-Datadog-Tags: " +
			"the header's presence is per-request, so it belongs in the auto-noise allowlist")
	}
}

// TestFlakyHeaders_CoversConditionallyEmittedPropagationHeaders pins the
// membership itself. A propagation header that some requests carry and others
// do not must be in the allowlist, because presence-only matching turns a
// missing recorded header into an unconditional rejection.
func TestFlakyHeaders_CoversConditionallyEmittedPropagationHeaders(t *testing.T) {
	present := make(map[string]struct{}, len(flakyHeaders))
	for _, hdr := range flakyHeaders {
		present[hdr] = struct{}{}
	}

	for _, hdr := range []string{
		"x-datadog-tags", // Datadog: only when the trace has _dd.p.* tags
		"baggage",        // W3C Baggage: only when the context has entries
		"sentry-trace",   // Sentry: only when the transaction is traced
		"x-b3-flags",     // B3: only on debug-sampled traces
	} {
		if _, ok := present[hdr]; !ok {
			t.Errorf("%q is emitted conditionally by its tracer and must be in flakyHeaders; "+
				"without it a recording made while the header was present can never match a replay made while it is absent", hdr)
		}
	}
}
