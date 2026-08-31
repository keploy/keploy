package http

import (
	"net/url"
	"testing"
)

// These guard the url-noise hot path. MatchURLPath and QueryParamsMatch both run
// once per CANDIDATE MOCK per proxied request, so compiling the configured
// patterns inline showed up as ~12us and ~18KB of garbage per call with five
// patterns configured. With compileURLNoise memoizing (and QueryParamsMatch
// compiling lazily) the exact-match path is allocation-free again.
var benchNoise = []string{`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`, `/users/[0-9]+`, `[0-9]{10,}`, `token=[A-Za-z0-9]+`, `\.txt`}

func BenchmarkQueryParamsMatch_NoNoise(b *testing.B) {
	h := newHTTP()
	m := map[string]string{"id": "A", "page": "2"}
	q := url.Values{"id": {"A"}, "page": {"2"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.QueryParamsMatch(m, q, nil)
	}
}

func BenchmarkQueryParamsMatch_NoiseUnused(b *testing.B) {
	h := newHTTP()
	m := map[string]string{"id": "A", "page": "2"}
	q := url.Values{"id": {"A"}, "page": {"2"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.QueryParamsMatch(m, q, benchNoise)
	}
}

// Worst case for the fix: a value actually differs, so noise IS needed.
func BenchmarkQueryParamsMatch_NoiseUsed(b *testing.B) {
	h := newHTTP()
	m := map[string]string{"id": "A", "page": "2"}
	q := url.Values{"id": {"B"}, "page": {"2"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.QueryParamsMatch(m, q, benchNoise)
	}
}

func BenchmarkMatchURLPath_NoiseMiss(b *testing.B) {
	h := newHTTP()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.MatchURLPath("http://svc/orders/abc", "/customers/xyz", benchNoise, false)
	}
}
