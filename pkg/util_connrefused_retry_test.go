package pkg

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// connRefusedErr builds the exact error chain net/http returns for a dial-time
// "connection refused": *url.Error -> *net.OpError -> *os.SyscallError ->
// syscall.ECONNREFUSED, so errors.Is(err, syscall.ECONNREFUSED) is true.
func connRefusedErr(u string) error {
	return &url.Error{Op: "Get", URL: u, Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
}

// connResetErr is the mid-response "connection reset by peer" chain — ambiguous
// (the app may already have consumed single-use mocks), so it must NOT be retried.
func connResetErr(u string) error {
	return &url.Error{Op: "Get", URL: u, Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
}

// scriptedRT returns a scripted sequence of errors (attempts 1..len(errs)) then a
// 200; it records the attempt count and the body bytes seen on the served attempt
// so a test can assert the request body was rewound between attempts.
type scriptedRT struct {
	errs     []error
	attempts int32
	lastBody string
}

func (rt *scriptedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	n := int(atomic.AddInt32(&rt.attempts, 1))
	if n <= len(rt.errs) {
		return nil, rt.errs[n-1]
	}
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		rt.lastBody = string(b)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
}

// A transient pre-response connection-refused must be re-sent so the suite
// obtains the REAL response instead of a false status_code=0.
func TestDoRequestWithConnRefusedRetry_RecoversTransientRefusal(t *testing.T) {
	rt := &scriptedRT{errs: []error{connRefusedErr("http://x/a"), connRefusedErr("http://x/a")}}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/a", nil)
	resp, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req)
	if err != nil {
		t.Fatalf("expected recovery after 2 refusals, got error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&rt.attempts); got != 3 {
		t.Fatalf("expected 3 attempts (2 refused + 1 served), got %d", got)
	}
}

// A genuinely unreachable app must fail FAST after the bounded retries — never
// fabricate a success.
func TestDoRequestWithConnRefusedRetry_StopsAfterMaxWithoutFabricating(t *testing.T) {
	errs := make([]error, maxConnRefusedRetries+5)
	for i := range errs {
		errs[i] = connRefusedErr("http://x/a")
	}
	rt := &scriptedRT{errs: errs}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/a", nil)
	_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req)
	if err == nil {
		t.Fatal("expected an error after exhausting retries — must not fabricate a result")
	}
	if got := int(atomic.LoadInt32(&rt.attempts)); got != maxConnRefusedRetries+1 {
		t.Fatalf("expected %d attempts, got %d", maxConnRefusedRetries+1, got)
	}
}

// A mid-response reset is ambiguous about mock/state consumption — must NOT be
// retried (else we'd re-run non-idempotent logic against exhausted mocks).
func TestDoRequestWithConnRefusedRetry_DoesNotRetryReset(t *testing.T) {
	rt := &scriptedRT{errs: []error{connResetErr("http://x/a")}}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/a", nil)
	_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req)
	if err == nil {
		t.Fatal("expected the reset error to propagate without retry")
	}
	if got := int(atomic.LoadInt32(&rt.attempts)); got != 1 {
		t.Fatalf("ECONNRESET must not be retried: expected 1 attempt, got %d", got)
	}
}

// The retried request must carry the FULL recorded body (rewound via GetBody),
// never a truncated/empty one — otherwise a retry would fabricate a wrong result.
func TestDoRequestWithConnRefusedRetry_RewindsBody(t *testing.T) {
	const body = "the-full-recorded-body"
	rt := &scriptedRT{errs: []error{connRefusedErr("http://x/a")}}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://x/a", strings.NewReader(body))
	if _, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.lastBody != body {
		t.Fatalf("retried request body not rewound: want %q, got %q", body, rt.lastBody)
	}
}
