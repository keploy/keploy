package http

import (
	"net/http"
	"testing"
)

// A browser XHR and the app's own server-side renderer call the SAME endpoint,
// and the two recordings are NOT interchangeable — Origin on the request is what
// makes the server attach Access-Control-Allow-Origin to the response.
//
// Header sets below are the real ones from an enterprise-ui recording of
// GET /cluster/proxy-origins: 48 copies carried the 15 browser headers and got a
// CORS response, 45 carried only these 8 and got no CORS headers at all.
//
// HeadersContainKeys is a subset test, so the 8-key server-side recording is
// strictly MORE permissive than the 15-key browser one and used to match a
// browser request. The browser then discarded the CORS-less response with
// net::ERR_FAILED while keploy recorded a successful match and reported
// missed: 0 — invisible to any miss-based gate.
var (
	serverSideRecording = map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate",
		"Accept-Language": "*",
		"Connection":      "keep-alive",
		"Cookie":          "session=abc",
		"Host":            "localhost:8083",
		"Sec-Fetch-Mode":  "cors",
		"User-Agent":      "node",
	}

	browserRecording = map[string]string{
		"Accept":             "*/*",
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Accept-Language":    "en-US,en;q=0.9",
		"Connection":         "keep-alive",
		"Cookie":             "session=abc",
		"Host":               "localhost:8083",
		"Origin":             "http://localhost:3000",
		"Referer":            "http://localhost:3000/",
		"Sec-Ch-Ua":          `"Chromium";v="141"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-site",
		"User-Agent":         "Mozilla/5.0",
	}

	browserRequest = http.Header{
		"Accept":             {"*/*"},
		"Accept-Encoding":    {"gzip, deflate, br, zstd"},
		"Accept-Language":    {"en-US,en;q=0.9"},
		"Connection":         {"keep-alive"},
		"Cookie":             {"session=abc"},
		"Host":               {"localhost:8083"},
		"Origin":             {"http://localhost:3000"},
		"Referer":            {"http://localhost:3000/"},
		"Sec-Ch-Ua":          {`"Chromium";v="141"`},
		"Sec-Ch-Ua-Mobile":   {"?0"},
		"Sec-Ch-Ua-Platform": {`"Windows"`},
		"Sec-Fetch-Dest":     {"empty"},
		"Sec-Fetch-Mode":     {"cors"},
		"Sec-Fetch-Site":     {"same-site"},
		"User-Agent":         {"Mozilla/5.0"},
	}

	serverSideRequest = http.Header{
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"*"},
		"Connection":      {"keep-alive"},
		"Cookie":          {"session=abc"},
		"Host":            {"localhost:8083"},
		"Sec-Fetch-Mode":  {"cors"},
		"User-Agent":      {"node"},
	}
)

func TestHeadersContainKeys_OriginPresenceMustAgree(t *testing.T) {
	h := newHTTP()

	for _, tc := range []struct {
		name     string
		mock     map[string]string
		req      http.Header
		expected bool
		why      string
	}{
		{
			name:     "server-side recording must NOT serve a browser request",
			mock:     serverSideRecording,
			req:      browserRequest,
			expected: false,
			why:      "its response carries no Access-Control-Allow-Origin; the browser would discard it",
		},
		{
			name:     "browser recording must NOT serve a server-side request",
			mock:     browserRecording,
			req:      serverSideRequest,
			expected: false,
			why:      "the mock requires Origin, which a server-side call never sends",
		},
		{
			name:     "browser recording serves a browser request",
			mock:     browserRecording,
			req:      browserRequest,
			expected: true,
		},
		{
			name:     "server-side recording serves a server-side request",
			mock:     serverSideRecording,
			req:      serverSideRequest,
			expected: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.HeadersContainKeys(tc.mock, tc.req, nil); got != tc.expected {
				t.Fatalf("HeadersContainKeys = %v, want %v. %s", got, tc.expected, tc.why)
			}
		})
	}
}

// The check is an ordinary header rule, so header noise turns it off for users
// who genuinely want the two shapes interchangeable.
func TestHeadersContainKeys_OriginNoiseDisablesTheCheck(t *testing.T) {
	h := newHTTP()
	noise := map[string][]string{"origin": {}}

	if !h.HeadersContainKeys(serverSideRecording, browserRequest, noise) {
		t.Fatal("with Origin marked as noise the server-side recording should match again")
	}
}
