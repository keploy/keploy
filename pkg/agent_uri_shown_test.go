package pkg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
// that parses, its one @ ending its host's userinfo, shows the password as
// xxxxx; any other with an @ shows all of what comes before its last @ as
// xxxxx, past its scheme's ://; one with no @ is shown as it is.
func TestShownAgentURI(t *testing.T) {
	cases := map[string]struct{ uri, want string }{
		"parses":                               {"http://:s3aled-t0ken@host:1/agent", "http://:xxxxx@host:1/agent"},
		"parses, with no password":             {"http://host:1/agent", "http://host:1/agent"},
		"does not parse (a bad escape)":        {"keploy-agent://:s3aled%zzt0ken@host:1/agent", "keploy-agent://xxxxx@host:1/agent"},
		"does not parse, an @ in its password": {"keploy-agent://u:s3aled%zz@t0ken@host:1/agent", "keploy-agent://xxxxx@host:1/agent"},
		"no host part (scheme:rest)":           {"agent:s3aled-t0ken@host:1", "xxxxx@host:1"},
		"does not parse, no @":                 {"http://[::1:16789", "http://[::1:16789"},
		"digits, then a / in its password":     {"keploy-agent://:123/sec@host:1/agent", "keploy-agent://xxxxx@host:1/agent"},
		"digits, then a ? in its password":     {"http://:123?sec@host:1/agent", "http://xxxxx@host:1/agent"},
		"digits, then a # in its password":     {"http://:123#sec@host:1/agent", "http://xxxxx@host:1/agent"},
		"a raw # in its password":              {"http://u:s3aled#t0ken@host:1/agent", "http://xxxxx@host:1/agent"},
		"two @ in its userinfo":                {"http://u:s3aled@t0ken@host:1/agent", "http://xxxxx@host:1/agent"},
		"userinfo, and an @ past its host":     {"http://u:s3aled@host:1/x@y", "http://xxxxx@y"},
		"a :// in the password, no host part":  {"agent:s3aled://t0ken@host:1", "xxxxx@host:1"},
		"a / before a :// in the password":     {"a/b://s3aled@host:1", "xxxxx@host:1"},
		"a ? before a :// in the password":     {"a?b://s3aled@host:1", "xxxxx@host:1"},
		"a # before a :// in the password":     {"a#b://s3aled@host:1", "xxxxx@host:1"},
		"an @, then a / in its password":       {"http://:s3@aled/t0ken@host:1/agent", "http://xxxxx@host:1/agent"},
		"nothing before its ://":               {"://s3aled@host:1", "xxxxx@host:1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ShownAgentURI(tc.uri); got != tc.want {
				t.Fatalf("ShownAgentURI(%q) = %q; want %q", tc.uri, got, tc.want)
			}
		})
	}
}

// failingTransport fails each request with err, as a dial that fails does.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// doError is client.Do's error for a GET of uri, its transport failing with
// inner.
func doError(t *testing.T, uri string, inner error) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, uri, nil)
	if err != nil {
		t.Fatalf("the URI %s did not parse: %v", uri, err)
	}
	client := http.Client{Transport: failingTransport{inner}}
	res, err := client.Do(req)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("the request did not fail")
	}
	return err
}

// url.Parse's error for an agent URI, as a request made for it gets it, is
// said with none of its text: it names the URI, whose password a raw # cuts
// off before its @, and its inner error quotes it (a bad port, a bad escape).
func TestShownAgentURIErrorOfAURIThatDoesNotParse(t *testing.T) {
	cases := map[string]string{
		"a bad escape":    "keploy-agent://:s3aled%zzt0ken@host:1/agent/health",
		"a raw /":         "keploy-agent://:s3aled/t0ken@127.0.0.1:1/agent/health",
		"a raw ?":         "keploy-agent://:s3aled?t0ken@127.0.0.1:1/agent/health",
		"a raw #":         "http://u:s3aled#t0ken@127.0.0.1:1/agent/health",
		"no @ left in it": "http://u:s3aled",
		"a space in it":   "http://u:s3 aled@host:1/agent",
	}
	for name, uri := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := http.NewRequestWithContext(context.Background(), http.MethodGet, uri, nil)
			if err == nil {
				t.Fatal("the URI parsed")
			}
			if got, want := ShownAgentURIError(err).Error(), "the agent URI does not parse"; got != want {
				t.Fatalf("ShownAgentURIError(%v) said %q; want %q", err, got, want)
			}
			if got := ShownAgentURIError(fmt.Errorf("build the request for %s: %w", uri, err)).Error(); strings.Contains(got, "s3") {
				t.Fatalf("a wrapped error said %q, with a part of the password", got)
			}
		})
	}
}

// A request's error shows its URI as ShownAgentURI shows it. Its inner error
// (a dial's says a host and a port) is said only for a URI whose @ ends its
// userinfo: where a password took the place of a path or a host, the host and
// port a dial names can be parts of it. errors.Is still reads what was hidden.
func TestShownAgentURIErrorOfARequest(t *testing.T) {
	dial := errors.New("dial tcp :123: connect: connection refused")
	cases := map[string]struct {
		uri  string
		want string
	}{
		"its @ ends its userinfo": {"http://u:s3aled@host:1/agent/health", `Get "http://u:xxxxx@host:1/agent/health": ` + dial.Error()},
		"no host part":            {"agent:s3aled-t0ken@host:1/agent/health", `Get "xxxxx@host:1/agent/health": the request failed`},
		"digits, then a /":        {"http://:123/s3aled@host:1/agent/health", `Get "http://xxxxx@host:1/agent/health": the request failed`},
		"digits, then a #":        {"http://:123#s3aled@host:1/agent/health", `Get "http://xxxxx@host:1/agent/health": the request failed`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ShownAgentURIError(doError(t, tc.uri, dial))
			if got.Error() != tc.want {
				t.Fatalf("ShownAgentURIError said %q; want %q", got, tc.want)
			}
			if !errors.Is(got, dial) {
				t.Fatalf("%v no longer wraps the request's own error", got)
			}
			var ue *url.Error
			if !errors.As(got, &ue) || ue.Op != "Get" {
				t.Fatalf("%v is no url.Error of a GET", got)
			}
		})
	}
	// Shown twice, it says the same.
	once := ShownAgentURIError(doError(t, "agent:s3aled-t0ken@host:1/agent/health", dial))
	if twice := ShownAgentURIError(once); twice.Error() != once.Error() {
		t.Fatalf("shown again it says %q; want %q", twice, once)
	}
}

// An error with nothing to hide is said as it is, the very error: one with no
// url.Error in it, one for a URI with no @, and no error at all.
func TestShownAgentURIErrorLeavesTheRestAsTheyAre(t *testing.T) {
	if got := ShownAgentURIError(nil); got != nil {
		t.Fatalf("ShownAgentURIError(nil) = %v", got)
	}
	other := errors.New("connection refused")
	if got := ShownAgentURIError(other); got != other {
		t.Fatalf("ShownAgentURIError(%v) = %v; want it as it is", other, got)
	}
	plain := doError(t, "http://127.0.0.1:1/agent/health", other)
	if got := ShownAgentURIError(plain); got != plain {
		t.Fatalf("ShownAgentURIError(%v) = %v; want it as it is", plain, got)
	}
}

// keploy's check of an agent's control plane says the agent's URI as shown,
// and each of the errors its probe can end in goes through the mask.
func TestTheControlPlaneCheckNeverSaysTheAgentURIsPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	cases := map[string]struct{ uri, line string }{
		"the agent refuses the wrong token": {"http://:s3aled-t0ken@" + addr + "/agent", "agent control-plane authentication verified"},
		"the URI does not parse":            {"keploy-agent://:s3aled/t0ken@" + addr + "/agent", "could not build the agent authentication probe"},
		"the probe does not complete":       {"agent:s3aled-t0ken@" + addr + "/agent", "agent authentication probe did not complete"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resetProbeOnce(t)
			recordLaunch(t, tc.uri)
			core, logs := observer.New(zapcore.DebugLevel)
			verifyControlPlaneGuarded(context.Background(), zap.New(core), tc.uri)
			if logs.FilterMessageSnippet(tc.line).Len() != 1 {
				t.Fatalf("logged %v; want the line %q", logs.All(), tc.line)
			}
			for _, l := range logs.All() {
				said := l.Message + " " + fmt.Sprint(l.ContextMap())
				if strings.Contains(said, "s3aled") || strings.Contains(said, "t0ken") {
					t.Fatalf("logged %q, with a part of the URI's password", said)
				}
			}
		})
	}
}

// No line of keploy says an agent URI as it is. A line in the middle of a run
// (record's and mock's wait for a compose agent) has no row that reaches it
// cheaply, so the row reads the source: a zap field or a line of its own in an
// error's arguments that is the URI, unless it goes through ShownAgentURI.
func TestNoLineSaysAnAgentURIUnmasked(t *testing.T) {
	raw := regexp.MustCompile(`zap\.(String|Any)\("[^"]*",\s*[\w.]*(AgentURI|agentURI|url|uri)\)|^\s*[\w.]*\.AgentURI,\s`)
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		for i, line := range strings.Split(string(src), "\n") {
			if raw.MatchString(line) && !strings.Contains(line, "ShownAgentURI(") && agentLine(path) {
				t.Errorf("%s:%d says an agent URI as it is: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil || checked == 0 {
		t.Fatalf("read %d files: %v", checked, err)
	}
}

// agentLine reports whether the source at path talks to the agent: what its
// zap fields call a url or a uri elsewhere is another service's.
func agentLine(path string) bool {
	for _, dir := range []string{"platform/http/", "service/record/", "service/mock/", "service/replay/", "service/runner/"} {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return path == "util.go"
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
		"a raw / in the password": {
			uri:     "keploy-agent://:s3aled/t0ken@127.0.0.1:1/agent",
			mustLog: []string{"Checking agent health", "Failed to create health check request"},
		},
		"a raw # in the password": {
			uri:     "http://u:s3aled#t0ken@127.0.0.1:1/agent",
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
