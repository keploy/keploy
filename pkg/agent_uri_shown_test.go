package pkg

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An agent URI is said with no password in it (a token an embedder sealed
// into it, as k8s-proxy does in keploy-agent://), whatever its shape: one
// that parses shows the password as xxxxx; one that does not parse, or has
// no host part, shows all of what comes before its last @ as xxxxx, past its
// scheme's ://; one with nothing before an @ is shown as it is.
func TestShownAgentURI(t *testing.T) {
	cases := map[string]struct{ uri, want string }{
		"parses":                               {"http://:s3aled-t0ken@host:1/agent", "http://:xxxxx@host:1/agent"},
		"parses, with no password":             {"http://host:1/agent", "http://host:1/agent"},
		"does not parse (a bad escape)":        {"keploy-agent://:s3aled%zzt0ken@host:1/agent", "keploy-agent://xxxxx@host:1/agent"},
		"does not parse, an @ in its password": {"keploy-agent://u:s3aled%zz@t0ken@host:1/agent", "keploy-agent://xxxxx@host:1/agent"},
		"no host part (scheme:rest)":           {"agent:s3aled-t0ken@host:1", "xxxxx@host:1"},
		"does not parse, no @":                 {"http://[::1:16789", "http://[::1:16789"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ShownAgentURI(tc.uri); got != tc.want {
				t.Fatalf("ShownAgentURI(%q) = %q; want %q", tc.uri, got, tc.want)
			}
		})
	}
}

// url.Parse's error for an agent URI, as a request made for it gets it,
// names the whole URI: it is said with the URI shown as ShownAgentURI shows
// it, and a bad escape (which names the escape, perhaps a part of the
// password) with only that it is one. Any other error is said as it is.
func TestShownAgentURIError(t *testing.T) {
	_, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "keploy-agent://:s3aled%zzt0ken@host:1/agent/health", nil)
	if err == nil {
		t.Fatal("the URI parsed")
	}
	if got, want := ShownAgentURIError(err).Error(), `parse "keploy-agent://xxxxx@host:1/agent/health": invalid URL escape`; got != want {
		t.Fatalf("ShownAgentURIError said %q; want %q", got, want)
	}
	other := errors.New("connection refused")
	if got := ShownAgentURIError(other); got != other {
		t.Fatalf("ShownAgentURIError(%v) = %v; want it as it is", other, got)
	}
}

// keploy's own health check (AgentHealthTicker) never says a part of the
// agent URI's password, at any level: not as it checks, not when a check runs
// out of time, not in the error of a URI that does not parse, and not when it
// hands the healthy agent to its control-plane check.
func TestAgentHealthTickerNeverSaysTheAgentURIsPassword(t *testing.T) {
	resetProbeOnce(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 1 {
			// The first check runs out of time (agentHealthFirstWait).
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cases := map[string]struct {
		uri     string
		healthy bool
		mustLog []string
	}{
		"a URI that parses": {
			uri:     "http://:s3aled-t0ken@" + srv.Listener.Addr().String() + "/agent",
			healthy: true,
			mustLog: []string{"Checking agent health", "agent health check ran out of time", "not checking the control-plane authentication"},
		},
		"a URI that does not parse": {
			uri:     "keploy-agent://:s3aled%zzt0ken@127.0.0.1:1/agent",
			mustLog: []string{"Checking agent health", "Failed to create health check request"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if !tc.healthy {
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			ready := make(chan bool, 1)
			go AgentHealthTicker(ctx, zap.New(core), tc.uri, ready, 5*time.Millisecond)
			if ok := <-ready; ok != tc.healthy {
				t.Fatalf("the agent was found healthy: %v; want %v", ok, tc.healthy)
			}
			// The control-plane check runs off the ticker's goroutine.
			deadline := time.Now().Add(10 * time.Second)
			for _, m := range tc.mustLog {
				for logs.FilterMessageSnippet(m).Len() == 0 {
					if time.Now().After(deadline) {
						t.Fatalf("logged %v; want a line that says %q", logs.All(), m)
					}
					time.Sleep(time.Millisecond)
				}
			}
			for _, l := range logs.All() {
				said := l.Message + " " + fmt.Sprint(l.ContextMap())
				for _, part := range []string{"s3aled", "t0ken", "%zz"} {
					if strings.Contains(said, part) {
						t.Fatalf("logged %q, with a part of the URI's password (%s)", said, part)
					}
				}
			}
		})
	}
}
