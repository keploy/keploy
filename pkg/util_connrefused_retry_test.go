package pkg

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	resp, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req, nil, 0)
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
	_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req, nil, 0)
	if err == nil {
		t.Fatal("expected an error after exhausting retries — must not fabricate a result")
	}
	if got := int(atomic.LoadInt32(&rt.attempts)); got != maxConnRefusedRetries+1 {
		t.Fatalf("expected %d attempts, got %d", maxConnRefusedRetries+1, got)
	}
}

// IsTransportConnReset must classify the docker-proxy / mid-exchange reset
// family (ECONNRESET, broken pipe, bare EOF/unexpected-EOF) and nothing else,
// so the replay orchestration can mock-consumption-gate a re-send of exactly
// this transport class.
func TestIsTransportConnReset(t *testing.T) {
	resetLike := []error{
		connResetErr("http://x/a"),
		&url.Error{Op: "Post", URL: "http://x/a", Err: &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}},
		&url.Error{Op: "Get", URL: "http://x/a", Err: io.EOF},
		&url.Error{Op: "Get", URL: "http://x/a", Err: io.ErrUnexpectedEOF},
	}
	for _, e := range resetLike {
		if !IsTransportConnReset(e) {
			t.Errorf("expected reset classification for: %v", e)
		}
	}

	notReset := []error{
		nil,
		connRefusedErr("http://x/a"), // refused is handled by its own path, not this one
		errors.New("response body mismatch"),
		context.DeadlineExceeded,
	}
	for _, e := range notReset {
		if IsTransportConnReset(e) {
			t.Errorf("did NOT expect reset classification for: %v", e)
		}
	}
}

// A mid-response reset is ambiguous about mock/state consumption — must NOT be
// retried (else we'd re-run non-idempotent logic against exhausted mocks).
func TestDoRequestWithConnRefusedRetry_DoesNotRetryReset(t *testing.T) {
	rt := &scriptedRT{errs: []error{connResetErr("http://x/a")}}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/a", nil)
	_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req, nil, 0)
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
	if _, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), client, req, nil, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.lastBody != body {
		t.Fatalf("retried request body not rewound: want %q, got %q", body, rt.lastBody)
	}
}

// answerReach is an AppPortReachability with a fixed answer that records the
// addresses it was asked about, and the app ports.
type answerReach struct {
	reason   string
	asked    []string
	appPorts []uint16
}

func (a *answerReach) UnreachableAppPort(_ context.Context, host string, port, appPort uint16) string {
	a.asked = append(a.asked, net.JoinHostPort(host, strconv.Itoa(int(port))))
	a.appPorts = append(a.appPorts, appPort)
	return a.reason
}

// Asking whether the app can be reached changes nothing when it can: the app
// is still starting, and the refusal is re-sent exactly as without the
// question. The question is asked once, about the address dialed, with the
// scheme's port when the URL names none.
func TestDoRequestWithConnRefusedRetry_ReachablePortStillRetries(t *testing.T) {
	for _, u := range []struct{ url, asked string }{
		{"http://x:8095/a", "x:8095"},
		{"http://x/a", "x:80"},
		{"https://x/a", "x:443"},
	} {
		rt := &scriptedRT{errs: []error{connRefusedErr(u.url), connRefusedErr(u.url)}}
		reach := &answerReach{}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", u.url, nil)
		resp, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), &http.Client{Transport: rt}, req, reach, 0)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: expected recovery after 2 refusals, got (%v, %v)", u.url, resp, err)
		}
		if got := atomic.LoadInt32(&rt.attempts); got != 3 {
			t.Errorf("%s: expected 3 attempts, got %d", u.url, got)
		}
		if len(reach.asked) != 1 || reach.asked[0] != u.asked {
			t.Errorf("%s: asked %v, want [%s]", u.url, reach.asked, u.asked)
		}
	}
}

// A refusal at an address the app can never be reached at is not re-sent, and
// the error says why while still being the refusal underneath. It names the
// app's port the test was recorded on, which reach is asked about too, and the
// host port the request went to when that is another one (test.port sent it
// there); a test that does not say its app port is named by the host port.
func TestDoRequestWithConnRefusedRetry_UnreachablePortFailsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		appPort uint16
		want    string
	}{
		{8096, "the app's port 8096 cannot be reached from the host: it is not published on the host: "},
		{8080, "the app's port 8080 cannot be reached from the host at port 8096: it is not published on the host: "},
		{0, "the app cannot be reached from the host at port 8096: it is not published on the host: "},
	} {
		rt := &scriptedRT{errs: []error{connRefusedErr("http://localhost:8096/a"), connRefusedErr("http://localhost:8096/a")}}
		reach := &answerReach{reason: "it is not published on the host"}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://localhost:8096/a", nil)
		_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), &http.Client{Transport: rt}, req, reach, tc.appPort)
		var unreachable *UnreachableAppPortError
		if !errors.As(err, &unreachable) || unreachable.Port != 8096 || unreachable.AppPort != tc.appPort {
			t.Fatalf("app port %d: want an UnreachableAppPortError for 8096, got %v", tc.appPort, err)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("app port %d: the refusal is lost from the chain: %v", tc.appPort, err)
		}
		if !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("error %q does not start with %q", err, tc.want)
		}
		if got := atomic.LoadInt32(&rt.attempts); got != 1 {
			t.Errorf("app port %d: re-sent to an unreachable port: %d attempts", tc.appPort, got)
		}
		if len(reach.appPorts) != 1 || reach.appPorts[0] != tc.appPort {
			t.Errorf("reach asked for app ports %v, want [%d]", reach.appPorts, tc.appPort)
		}
	}
}
