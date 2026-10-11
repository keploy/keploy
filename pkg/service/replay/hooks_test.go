package replay

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestEffectiveHTTPConfigPort_OptionsPreflightOnSSEPort_UsesSSEPort(t *testing.T) {
	cfg := config.Test{
		Port:    8000,
		SSEPort: 8047,
		Protocol: config.ProtocolConfig{
			"http": {Port: 0},
			"sse":  {Port: 0},
		},
	}

	tc := &models.TestCase{
		Kind:    models.HTTP,
		AppPort: 8047,
		HTTPReq: models.HTTPReq{
			Method: "OPTIONS",
			Header: map[string]string{
				"Accept": "*/*",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				// no Content-Type, typical for a preflight response
			},
		},
	}

	got := effectiveHTTPConfigPort(tc, cfg)
	assert.EqualValues(t, 8047, got)
}

func TestEffectiveHTTPConfigPort_NormalHTTPRequest_UsesHTTPPort(t *testing.T) {
	cfg := config.Test{
		Port:    8000,
		SSEPort: 8047,
		Protocol: config.ProtocolConfig{
			"http": {Port: 0},
			"sse":  {Port: 0},
		},
	}

	tc := &models.TestCase{
		Kind:    models.HTTP,
		AppPort: 8000,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			Header: map[string]string{
				"Accept": "application/json",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "application/json",
			},
		},
	}

	got := effectiveHTTPConfigPort(tc, cfg)
	assert.EqualValues(t, 8000, got)
}

// unpublishedPortInstr is the replay instrumentation in Docker mode with the
// app's port unpublished: UnreachableAppPort explains every address it is asked
// about, as the docker-backed instrumentation does for a port the run command
// does not publish, and records the questions.
type unpublishedPortInstr struct {
	*prInstr
	reason string
	mu     sync.Mutex
	asked  []string
}

func (u *unpublishedPortInstr) UnreachableAppPort(_ context.Context, host string, port, appPort uint16) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asked = append(u.asked, fmt.Sprintf("%s for %d", net.JoinHostPort(host, strconv.Itoa(int(port))), appPort))
	return u.reason
}

// closedLocalPort is a loopback port nothing listens on, so a dial is refused.
func closedLocalPort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

// A test request refused at an app port the host cannot reach is not the app
// "not yet accepting connections": nothing will ever accept there, so keploy
// says what is wrong — the port is not published — and does not re-send. The
// refusal stays a refusal to everything that classifies it (the no-answer
// accounting, mappings.yaml's hold). HTTP and gRPC tests alike.
func TestARefusalAtAnUnreachableAppPortSaysSoAndIsNotRetried(t *testing.T) {
	const reason = "it is not published on the host; add -p 8096:8096"
	port := closedLocalPort(t)
	authority := net.JoinHostPort("localhost", strconv.Itoa(int(port)))

	for _, tc := range []*models.TestCase{
		{
			Name: "post-echo-1", Kind: models.HTTP, AppPort: port,
			HTTPReq: models.HTTPReq{Method: "POST", URL: "http://" + authority + "/echo", Header: map[string]string{}, Body: "{}"},
		},
		{
			Name: "grpc-echo-1", Kind: models.GRPC_EXPORT, AppPort: port,
			GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{
				":authority": authority, ":path": "/echo.Echo/Ping", ":method": "POST", ":scheme": "http",
			}}},
		},
	} {
		t.Run(string(tc.Kind), func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			instr := &unpublishedPortInstr{prInstr: &prInstr{}, reason: reason}
			cfg := &config.Config{Test: config.Test{APITimeout: 5}}
			h := NewHooks(zap.New(core), cfg, instr)

			_, err := h.SimulateRequest(context.Background(), tc, "test-set-0")
			if err == nil {
				t.Fatal("a request to a closed port succeeded")
			}
			wantMsg := fmt.Sprintf("the app's port %d cannot be reached from the host: %s", port, reason)
			assert.Contains(t, err.Error(), wantMsg)
			assert.True(t, neterr.IsConnRefused(err), "the refusal must still classify as one: %v", err)
			assert.Contains(t, err.Error(), "connection refused")
			assert.Contains(t, failedWith(tc, err).FailureInfo.Category, models.AppConnectionError,
				"a test that failed with %v", err)
			for _, e := range logs.All() {
				assert.NotContains(t, e.Message, "not yet accepting", "misleading refusal message logged")
				assert.NotContains(t, e.Message, "may still be coming up", "misleading refusal message logged")
			}
			instr.mu.Lock()
			defer instr.mu.Unlock()
			assert.Equal(t, []string{fmt.Sprintf("%s for %d", authority, port)}, instr.asked,
				"asked once, about the address the request was sent to and the port it was recorded on, and not re-sent")
		})
	}
}

// A test that test.port (--port), or test.grpcPort for gRPC, sends to another
// host port than the one it was recorded on is asked about as that: the host
// port it goes to, for the app's port it was recorded on. A compose file that
// publishes "18067:8080", replayed with --port 18067, is the app's port 8080
// reached through host port 18067; asking about host port 18067 alone made a
// refusal while the app was starting look like a port that can never reach it.
// When the address is unreachable after all, the error names both ports.
func TestARedirectedTestIsAskedAboutForTheAppPortItWasRecordedOn(t *testing.T) {
	const reason = "host port N is published to the app's port 9000 instead of its port 8080"
	port := closedLocalPort(t)
	for _, tc := range []struct {
		tc  *models.TestCase
		cfg config.Test
	}{
		{
			tc: &models.TestCase{
				Name: "get-ping-1", Kind: models.HTTP, AppPort: 8080,
				HTTPReq: models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:18057/ping", Header: map[string]string{}},
			},
			cfg: config.Test{APITimeout: 5, Port: uint32(port)},
		},
		{
			tc: &models.TestCase{
				Name: "grpc-echo-1", Kind: models.GRPC_EXPORT, AppPort: 8080,
				GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{
					":authority": "127.0.0.1:18057", ":path": "/echo.Echo/Ping", ":method": "POST", ":scheme": "http",
				}}},
			},
			cfg: config.Test{APITimeout: 5, GRPCPort: uint32(port)},
		},
	} {
		t.Run(string(tc.tc.Kind), func(t *testing.T) {
			instr := &unpublishedPortInstr{prInstr: &prInstr{}, reason: reason}
			h := NewHooks(zap.NewNop(), &config.Config{Test: tc.cfg}, instr)

			_, err := h.SimulateRequest(context.Background(), tc.tc, "test-set-0")
			if err == nil {
				t.Fatal("a request to a closed port succeeded")
			}
			assert.Contains(t, err.Error(), fmt.Sprintf("the app's port 8080 cannot be reached from the host at port %d: %s", port, reason))
			instr.mu.Lock()
			defer instr.mu.Unlock()
			assert.Equal(t, []string{fmt.Sprintf("localhost:%d for 8080", port)}, instr.asked,
				"asked about the host port the request was sent to, for the app's port it was recorded on")
		})
	}
}

// At a published port where the app listens only on 127.0.0.1 inside its
// container, docker accepts the connection and drops it: a reset, or a gRPC
// call that loses its connection before the server's preface, on every try.
// That is not a transient either, so it fails at once saying why, while still
// classifying as the reset it is.
func TestADroppedConnectionAtAnUnreachableAppPortSaysSo(t *testing.T) {
	const reason = "the app listens on port 8097 only on 127.0.0.1 inside the container"
	_, port := acceptOnlyListener(t)
	authority := net.JoinHostPort("localhost", port) // as replay resolves it
	n, _ := strconv.Atoi(port)

	for _, tc := range []*models.TestCase{
		{
			Name: "post-echo-1", Kind: models.HTTP, AppPort: uint16(n),
			HTTPReq: models.HTTPReq{Method: "POST", URL: "http://" + authority + "/echo", Header: map[string]string{}, Body: "{}"},
		},
		{
			Name: "grpc-echo-1", Kind: models.GRPC_EXPORT, AppPort: uint16(n),
			GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{
				":authority": authority, ":path": "/echo.Echo/Ping", ":method": "POST", ":scheme": "http",
			}}},
		},
	} {
		t.Run(string(tc.Kind), func(t *testing.T) {
			instr := &unpublishedPortInstr{prInstr: &prInstr{}, reason: reason}
			cfg := &config.Config{Test: config.Test{APITimeout: 5}}
			h := NewHooks(zap.NewNop(), cfg, instr)

			_, err := h.SimulateRequest(context.Background(), tc, "test-set-0")
			if err == nil {
				t.Fatal("a request to a port that drops every connection succeeded")
			}
			assert.Contains(t, err.Error(), fmt.Sprintf("the app's port %d cannot be reached from the host: %s", n, reason))
			assert.True(t, pkg.IsUnreachableAppPort(err), "not marked as never reachable: %v", err)
			if tc.Kind == models.HTTP {
				assert.True(t, pkg.IsTransportConnReset(err), "the reset must still classify as one: %v", err)
			}
			// However the drop is reported (net/http's io.EOF or reset, or its
			// errServerClosedIdle, by a race inside it), the test is labelled
			// APP_CONNECTION_ERROR.
			assert.Contains(t, failedWith(tc, err).FailureInfo.Category, models.AppConnectionError,
				"a test that failed with %v", err)
			instr.mu.Lock()
			defer instr.mu.Unlock()
			assert.Equal(t, []string{fmt.Sprintf("%s for %d", authority, n)}, instr.asked, "asked once, about the address the request was sent to")
		})
	}
}
