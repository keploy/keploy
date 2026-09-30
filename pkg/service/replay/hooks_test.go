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

func (u *unpublishedPortInstr) UnreachableAppPort(_ context.Context, host string, port uint16) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asked = append(u.asked, net.JoinHostPort(host, strconv.Itoa(int(port))))
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
			for _, e := range logs.All() {
				assert.NotContains(t, e.Message, "not yet accepting", "misleading refusal message logged")
				assert.NotContains(t, e.Message, "may still be coming up", "misleading refusal message logged")
			}
			instr.mu.Lock()
			defer instr.mu.Unlock()
			assert.Equal(t, []string{authority}, instr.asked,
				"asked once, about the address the request was sent to, and not re-sent")
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
			instr.mu.Lock()
			defer instr.mu.Unlock()
			assert.Equal(t, []string{authority}, instr.asked, "asked once, about the address the request was sent to")
		})
	}
}
