package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// UpstreamErrorMarker is a response header the recorder adds when it
// synthesizes a mock for an upstream call that never produced a well-formed
// HTTP response (timeout, peer reset, mid-stream EOF, etc.). Operators and
// downstream tooling can grep for this header to distinguish captured-error
// mocks from legitimate upstream replies.
const UpstreamErrorMarker = "Keploy-Recorded-Upstream-Error"

// UpstreamTimeoutMarker is the specific marker value used when the upstream
// call hit a read/connect timeout. Matches the phrasing called out in the
// SAP-demo diagnostic so operators can eyeball captured timeouts at a glance.
const UpstreamTimeoutMarker = "keploy-recorded-upstream-timeout: true"

// classifyUpstreamError maps a raw network error into the
// (statusCode, reasonPhrase, marker-line) tuple used by
// synthesizeUpstreamErrorResponse.
//
//   - context.DeadlineExceeded / net.Error.Timeout() -> 504 Gateway Timeout
//   - io.EOF / io.ErrUnexpectedEOF                   -> 502 Bad Gateway
//   - "connection refused" / reset / unreachable     -> 502 Bad Gateway
//   - anything else                                   -> 502 Bad Gateway
//
// The reason phrase is surfaced in the synthesized status line; the marker
// line is inlined into the body AND emitted as a response header so
// downstream diff tooling (and humans) can grep for it in recorded YAML.
func classifyUpstreamError(err error) (status int, reason, marker string) {
	if err == nil {
		return 502, "Bad Gateway", UpstreamTimeoutMarker
	}

	// Timeouts: DeadlineExceeded or any net.Error whose Timeout() flag is set.
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, "Gateway Timeout", UpstreamTimeoutMarker
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return 504, "Gateway Timeout", UpstreamTimeoutMarker
	}

	// Mid-stream / early EOF: upstream closed the socket before sending a
	// response (or mid-body). This is "bad gateway" territory rather than a
	// timeout — but it is still something the recorder must persist rather
	// than drop, otherwise replay cannot reproduce the observed error.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return 502, "Bad Gateway", "keploy-recorded-upstream-eof: true"
	}

	// Connection-refused / reset / host-unreachable fall into a generic 502
	// with a more specific marker so post-hoc analysis can pivot by error
	// class.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "broken pipe") {
		return 502, "Bad Gateway", "keploy-recorded-upstream-unreachable: true"
	}

	// Everything else still gets persisted — default to 502 so replay sees a
	// deterministic error instead of silently dropping.
	return 502, "Bad Gateway", "keploy-recorded-upstream-error: true"
}

// synthesizeUpstreamErrorResponse builds a well-formed HTTP/1.1 response that
// captures an upstream error the recorder observed. The returned byte slice
// is structured exactly like a real upstream response so that the downstream
// parseFinalHTTP path (which calls net/http.ReadResponse) accepts it without
// special-casing. The body contains:
//
//  1. The marker line (classified from the error) so operators can grep for
//     captured-error mocks in recorded YAML.
//  2. The raw error message, which preserves the exact upstream diagnostic
//     (e.g. "Read timed out", "dial tcp: i/o timeout").
//
// The method/url arguments are unused in the current body format — they
// exist so future changes (e.g. including request metadata in the
// synthesized body) do not need to re-thread arguments through encode.go.
// Callers should always pass them.
func synthesizeUpstreamErrorResponse(_ string, _ string, upstreamErr error) []byte {
	status, reason, marker := classifyUpstreamError(upstreamErr)

	errMsg := "unknown upstream error"
	if upstreamErr != nil {
		errMsg = upstreamErr.Error()
	}

	body := fmt.Sprintf("%s\n%s\n", marker, errMsg)

	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", status, reason)
	fmt.Fprintf(&sb, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&sb, "%s: %s\r\n", UpstreamErrorMarker, marker)
	fmt.Fprintf(&sb, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(&sb, "Connection: close\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)

	return []byte(sb.String())
}
