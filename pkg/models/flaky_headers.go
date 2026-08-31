package models

import "strings"

// FlakyHeaders lists HTTP header keys (lowercased) that are known to change
// on every request due to cryptographic signatures, timestamps, credential
// rotation, or per-request identifiers. These are automatically treated as
// noise in TWO places, and the list is shared so the two cannot drift:
//
//   - egress mock matching, so a replayed request finds its recorded mock
//     even though these artifacts differ (pkg/agent/proxy/integrations/http);
//   - the recorded response assertion, so a test does not fail merely because
//     the application minted a fresh id or timestamp (pkg/service/replay).
//
// Both are disabled together by --disableAutoHeaderNoise.
//
// It lives in models rather than beside either consumer because importing the
// agent package from the matcher side would invert the layering, and copying
// the list guarantees the two copies diverge.
//
// No single public library maintains such a list. Most recording/replay
// tools (VCR, WireMock, Hoverfly) avoid the problem by not matching on
// headers at all by default. Since Keploy does match on header keys, we
// maintain this list covering the most common sources of non-determinism.
//
// Categories:
//   - Cloud auth/signing:  AWS SigV4, GCP OAuth, Azure HMAC/Bearer
//   - Tracing/correlation: W3C Trace Context, B3, Datadog, X-Request-Id
//   - Webhook signatures:  Stripe, GitHub, Slack, Twilio, Shopify
//   - SDK metadata:        per-call invocation IDs and attempt counters
var FlakyHeaders = []string{
	// ── AWS SigV4 & SDK ──────────────────────────────────────────────
	"authorization",         // signature changes every request (all cloud providers)
	"x-amz-date",            // signing timestamp (yyyymmddThhmmssZ)
	"x-amz-security-token",  // STS/IRSA session token — may appear or disappear
	"x-amz-content-sha256",  // payload hash
	"x-amz-credential",      // credential scope string
	"x-amz-signature",       // explicit signature value (SigV4 query-string variant)
	"x-amz-signedheaders",   // list of signed headers (varies with SDK)
	"x-amz-expires",         // pre-signed URL expiry seconds
	"x-amz-user-agent",      // SDK metadata
	"x-amzn-trace-id",       // AWS X-Ray trace propagation
	"amz-sdk-invocation-id", // unique per-call UUID from AWS SDK
	"amz-sdk-request",       // attempt counter (attempt=1; max=3)
	"date",                  // SigV4 fallback when X-Amz-Date absent. Globally ignored (Date is dynamic for all HTTP); disable per-test via DisableAutoHeaderNoise.

	// ── GCP ──────────────────────────────────────────────────────────
	"x-goog-api-client",     // SDK metadata (version, runtime info)
	"x-goog-request-params", // routing parameters, may change with resource

	// ── Azure ────────────────────────────────────────────────────────
	"x-ms-date",                     // signing timestamp
	"x-ms-client-request-id",        // client-generated UUID per call
	"x-ms-content-sha256",           // body hash for HMAC auth
	"x-ms-return-client-request-id", // echo control flag

	// ── W3C Trace Context / OpenTelemetry ────────────────────────────
	"traceparent", // unique trace-id + span-id per request
	"tracestate",  // vendor-specific trace context

	// ── Zipkin B3 propagation ────────────────────────────────────────
	"x-b3-traceid",
	"x-b3-spanid",
	"x-b3-parentspanid",
	"x-b3-sampled",
	"b3", // single-header compact format

	// ── Datadog ──────────────────────────────────────────────────────
	"x-datadog-trace-id",
	"x-datadog-parent-id",
	"x-datadog-sampling-priority",
	"x-datadog-origin",
	"x-datadog-tags", // _dd.p.* propagation tags — emitted ONLY when the trace carries at least one (128-bit trace id, sampling decision). Absent whenever it carries none, so its very PRESENCE is per-request.

	// ── Conditionally-emitted propagation headers ────────────────────
	// Same class as the entries above, and listed separately because the
	// hazard is different: these are absent outright on some requests rather
	// than merely carrying a fresh value. HeadersContainKeys is presence-only,
	// so a recorded header that the live request omits kills the candidate
	// before any value comparison runs — a value-noise entry would not help.
	"baggage",      // W3C Baggage (OpenTelemetry, Sentry) — present only when the context has baggage entries
	"sentry-trace", // Sentry distributed tracing — present only when tracing is enabled for the transaction
	"x-b3-flags",   // B3 debug flag — present only on debug-sampled traces

	// ── Generic request/correlation IDs ──────────────────────────────
	"x-request-id",     // Nginx, Envoy, HAProxy, AWS ALB, Heroku
	"x-correlation-id", // cross-service correlation
	"request-id",       // ASP.NET Core and others

	// ── Webhook signatures (request-side, inbound webhooks) ──────────
	"stripe-signature",
	"x-hub-signature-256", // GitHub
	"x-hub-signature",     // GitHub (legacy SHA-1)
	"x-twilio-signature",
	"x-shopify-hmac-sha256",
	"x-slack-signature",
	"x-slack-request-timestamp",
	"webhook-signature", // Standard Webhooks spec
	"webhook-timestamp", // Standard Webhooks spec
	"webhook-id",        // Standard Webhooks spec

	// ── Idempotency / CSRF ───────────────────────────────────────────
	"idempotency-key",
	"x-idempotency-key",
	"x-csrf-token",
	"x-xsrf-token",

	// ── GCP trace (legacy) ───────────────────────────────────────────
	"x-cloud-trace-context",
}

// FlakyHeaderNoise returns FlakyHeaders as a header-noise map: every entry maps
// to an EMPTY pattern list, which the matcher reads as "ignore this header
// unconditionally" rather than "ignore it when it matches a regex".
//
// A fresh map is returned on every call so a caller is free to mutate it; the
// current callers only read, but a shared map would make that a trap.
func FlakyHeaderNoise() map[string][]string {
	nm := make(map[string][]string, len(FlakyHeaders))
	for _, h := range FlakyHeaders {
		nm[h] = []string{}
	}
	return nm
}

// volatileResponseHeaders are response headers a server mints fresh on every
// call, so comparing their VALUES asserts nothing.
//
// Deliberately NOT FlakyHeaders. That list is curated for REQUEST volatility --
// its own comments say "webhook signatures (inbound)" -- and several entries are
// meaningful when they appear on a response: a server issues x-csrf-token and
// ceasing to issue one is a regression worth failing on; idempotency-key echo is
// a correctness assertion in Stripe-style APIs; authorization appears in
// token-refresh responses. Suppressing those would delete real coverage.
//
// This set is therefore only the identifiers and timing values that are, by
// construction, different on every response: the HTTP date, request/correlation
// ids, and distributed-trace ids.
var volatileResponseHeaders = map[string]struct{}{
	"date":                  {},
	"x-request-id":          {},
	"request-id":            {},
	"x-correlation-id":      {},
	"x-runtime":             {}, // Rails: per-request wall time
	"traceparent":           {},
	"tracestate":            {},
	"b3":                    {},
	"x-b3-traceid":          {},
	"x-b3-spanid":           {},
	"x-b3-parentspanid":     {},
	"x-datadog-trace-id":    {},
	"x-datadog-parent-id":   {},
	"x-amzn-trace-id":       {},
	"x-cloud-trace-context": {},
	"sentry-trace":          {},
}

// IsVolatileResponseHeader reports whether name is EXACTLY one of the headers
// whose response VALUE carries no assertable information, compared
// case-insensitively.
//
// Exactness matters and is why callers must not route this through a noise map:
// matcher.SubstringKeyMatch resolves noise keys with strings.Contains, so a
// "date" entry there also suppresses "X-Candidate-Id" and "X-Validate-Token".
// Consumers must compare header names through this function directly.
func IsVolatileResponseHeader(name string) bool {
	_, ok := volatileResponseHeaders[strings.ToLower(name)]
	return ok
}
