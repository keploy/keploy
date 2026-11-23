package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
)

// clearRunEnv clears every variable InternalWhy and detectCIOrInvoker read,
// so a test reads only what it sets, also in keploy's own CI.
func clearRunEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CI", InternalEnv, InvokerEnv} {
		t.Setenv(k, "")
	}
	for _, p := range ciProviders {
		t.Setenv(p.envVar, "")
	}
	for _, k := range ciRepoVars {
		t.Setenv(k, "")
	}
}

func TestIsDevBuild(t *testing.T) {
	for v, want := range map[string]bool{
		utils.DevVersion:    true,
		" 3-dev ":           true,
		"0.0.0-pipeline-7":  true,
		"v0.0.0-pipeline-7": true,
		"0.0.0":             true,
		"abc1234":           true,
		"733e7f7605d1b2c3d4e5f60718293a4b5c6d7e8f": true,
		"3.9.4":         false,
		"v3.9.4":        false,
		"3.9.4-abc1234": false, // a customer's build of a release
		"3.9.15-rc.1":   false,
		"1.0.0-beta10":  false,
		"":              false, // says nothing
		"abc123":        false, // too short for a commit
		"ABC1234":       false, // git writes hashes in lower case
		"733e7f7605d1b2c3d4e5f60718293a4b5c6d7e8f0": false,
	} {
		if got := IsDevBuild(v); got != want {
			t.Errorf("IsDevBuild(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestInternalWhy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		version string
		want    string
	}{
		{name: "a user's release", version: "3.9.4"},
		{name: "KEPLOY_INTERNAL=1", env: map[string]string{InternalEnv: "1"}, version: "3.9.4", want: InternalWhyEnv},
		{name: "KEPLOY_INTERNAL=true", env: map[string]string{InternalEnv: "true"}, version: "3.9.4", want: InternalWhyEnv},
		{name: "KEPLOY_INTERNAL= TRUE ", env: map[string]string{InternalEnv: " TRUE "}, version: "3.9.4", want: InternalWhyEnv},
		{name: "KEPLOY_INTERNAL=0", env: map[string]string{InternalEnv: "0"}, version: "3.9.4"},
		{name: "KEPLOY_INTERNAL=yes is not clearly set", env: map[string]string{InternalEnv: "yes"}, version: "3.9.4"},
		{name: "env first, over a dev build", env: map[string]string{InternalEnv: "1"}, version: utils.DevVersion, want: InternalWhyEnv},
		{name: "keploy's Woodpecker", env: map[string]string{"CI": "woodpecker", "CI_REPO": "keploy/enterprise"}, version: "3.9.4", want: InternalWhyCIRepo},
		{name: "keploy's GitHub Actions", env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "Keploy/keploy"}, version: "3.9.4", want: InternalWhyCIRepo},
		{name: "a customer's CI", env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop"}, version: "3.9.4"},
		{name: "a customer's fork of a keploy sample", env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/samples-go"}, version: "3.9.4"},
		// A prospect's Jenkins job that checks a keploy sample out names it
		// in GIT_URL: a user, not keploy's CI.
		{name: "a prospect's Jenkins building a keploy sample", env: map[string]string{"JENKINS_URL": "https://ci.acme.test/", "GIT_URL": "https://github.com/keploy/samples-go.git"}, version: "3.9.4"},
		// A Codespace of a keploy sample sets GITHUB_REPOSITORY and no
		// runner's variable: its user is a user.
		{name: "a Codespace of a keploy sample", env: map[string]string{"CODESPACES": "true", "GITHUB_REPOSITORY": "keploy/samples-go"}, version: "3.9.4"},
		{name: "a keploy/ repository a CI job runs (KEPLOY_INVOKER=ci)", env: map[string]string{InvokerEnv: "ci", "GITHUB_REPOSITORY": "keploy/api-server"}, version: "3.9.4", want: InternalWhyCIRepo},
		{name: "a dev build", version: utils.DevVersion, want: InternalWhyDevBuild},
		{name: "a commit build", version: "733e7f7605", want: InternalWhyDevBuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRunEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := InternalWhy(tc.version); got != tc.want {
				t.Fatalf("InternalWhy(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

// A user who cloned a keploy sample app is a user: InternalWhy reads the CI
// provider's variables, never the checkout's remote.
func TestInternalWhyIgnoresTheCheckoutsRemote(t *testing.T) {
	clearRunEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/keploy/samples-go.git"}} {
		if out, err := gitCmd(dir, args...); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	if got := InternalWhy("3.9.4"); got != "" {
		t.Fatalf("InternalWhy in a clone of keploy/samples-go = %q, want a user's run", got)
	}
}

func TestDetectCIOrInvoker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantCI       bool
		wantProvider string
	}{
		{name: "neither"},
		{name: "KEPLOY_INVOKER=ci with no runner variable", env: map[string]string{InvokerEnv: "ci"}, wantCI: true, wantProvider: "unknown"},
		{name: "KEPLOY_INVOKER=ci on Woodpecker", env: map[string]string{InvokerEnv: "ci", "CI": "woodpecker"}, wantCI: true, wantProvider: "woodpecker"},
		{name: "Woodpecker alone", env: map[string]string{"CI": "woodpecker"}, wantCI: true, wantProvider: "woodpecker"},
		{name: "KEPLOY_INVOKER=vscode-extension", env: map[string]string{InvokerEnv: "vscode-extension"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRunEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			ci, provider := detectCIOrInvoker()
			if ci != tc.wantCI || provider != tc.wantProvider {
				t.Fatalf("detectCIOrInvoker() = %v, %q; want %v, %q", ci, provider, tc.wantCI, tc.wantProvider)
			}
		})
	}
}

func TestAgentGetsTheMarkItCannotReadItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "a user's run"},
		{name: "KEPLOY_INTERNAL=1", env: map[string]string{InternalEnv: "1"}, want: true},
		{name: "a CI run of a keploy/ repository", env: map[string]string{"CI": "woodpecker", "CI_REPO": "keploy/k8s-proxy"}, want: true},
		{name: "a customer's CI", env: map[string]string{"CI": "woodpecker", "CI_REPO": "acme/shop"}},
		{name: "a Codespace of a keploy sample", env: map[string]string{"GITHUB_REPOSITORY": "keploy/samples-go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRunEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			args, env := AgentArgs(), AgentEnv()
			if tc.want {
				if !slices.Equal(args, []string{"--" + AgentFlag}) || env != InternalEnv+"=1" {
					t.Fatalf("AgentArgs() = %q, AgentEnv() = %q; want the mark", args, env)
				}
				return
			}
			if args != nil || env != "" {
				t.Fatalf("AgentArgs() = %q, AgentEnv() = %q; want none for a user's run", args, env)
			}
		})
	}
}

func TestMarkInternal(t *testing.T) {
	clearRunEnv(t)
	MarkInternal()
	if got := InternalWhy("3.9.4"); got != InternalWhyEnv {
		t.Fatalf("after MarkInternal, InternalWhy = %q, want %q", got, InternalWhyEnv)
	}
}

// A test binary never sends to keploy's own sink, however its URL is spelled:
// a test's run would count as a user's. A sink a test names gets its events.
func TestTestBinarySendsOnlyToASinkItNames(t *testing.T) {
	for _, u := range []string{defaultTeleURL, "https://telemetry.keploy.io/analytics/", "https://Telemetry.Keploy.io:443/analytics", "http://telemetry.keploy.io/x", "://bad"} {
		if sendsTo(u) {
			t.Errorf("a test binary may send to %q", u)
		}
	}
	if !sendsTo("http://127.0.0.1:1/analytics") {
		t.Fatal("a test binary may not send to a sink it named")
	}
}

// countingTransport counts the requests a client would have sent, and sends
// none.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// With keploy's own sink, a test binary's Telemetry posts nothing. The tracked
// events (TestRun, and the batched counters Shutdown flushes) are what
// Shutdown waits for, so a post of theirs has landed when it returns, however
// slow the machine; the untracked Ping is checked as well, after them.
func TestTestBinaryTelemetryNeverPostsToKeploysSink(t *testing.T) {
	clearRunEnv(t)
	prev := teleURL
	teleURL = defaultTeleURL
	t.Cleanup(func() { teleURL = prev })
	tel := NewTelemetry(nil, Options{Enabled: true, Version: "3.9.4", InstallationID: "test-install"})
	rt := &countingTransport{}
	tel.client.Transport = rt
	tel.SendTelemetry("Ping")
	tel.TestRun(1, 0, 1, 0, "pass", nil)
	tel.RecordedTestAndMocks()
	tel.Shutdown()
	if n := rt.n.Load(); n != 0 {
		t.Fatalf("a test binary posted %d event(s) to %s", n, defaultTeleURL)
	}
}

// Every event a run that is keploy's own sends says so, with why; a user's
// says nothing of it; and a run KEPLOY_INVOKER=ci started is CI.
func TestSendEventMarksKeploysOwnRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		version  string
		wantWhy  string
		wantIsCI bool
	}{
		{name: "a user's", version: "3.9.4"},
		{name: "KEPLOY_INTERNAL=1", env: map[string]string{InternalEnv: "1"}, version: "3.9.4", wantWhy: InternalWhyEnv},
		{name: "a dev build", version: utils.DevVersion, wantWhy: InternalWhyDevBuild},
		{name: "keploy's CI", env: map[string]string{"CI": "woodpecker", "CI_REPO": "keploy/integrations"}, version: "3.9.4", wantWhy: InternalWhyCIRepo, wantIsCI: true},
		{name: "invoker=ci", env: map[string]string{InvokerEnv: "ci"}, version: "3.9.4", wantIsCI: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRunEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var (
				mu   sync.Mutex
				got  []map[string]interface{}
				done = make(chan struct{}, 1)
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var ev map[string]interface{}
				_ = json.Unmarshal(body, &ev)
				mu.Lock()
				got = append(got, ev)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				done <- struct{}{}
			}))
			t.Cleanup(srv.Close)
			prev := teleURL
			teleURL = srv.URL
			t.Cleanup(func() { teleURL = prev })

			tel := NewTelemetry(nil, Options{Enabled: true, Version: tc.version, InstallationID: "test-install"})
			tel.SendTelemetry("TestEvent", map[string]interface{}{"k": "v"})
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("no event reached the test's sink")
			}
			mu.Lock()
			defer mu.Unlock()
			ev := got[0]
			meta, _ := ev["meta"].(map[string]interface{})
			if tc.wantWhy == "" {
				if _, ok := meta["internal"]; ok {
					t.Fatalf("a user's event carries internal: %v", meta)
				}
				if _, ok := meta["internal_why"]; ok {
					t.Fatalf("a user's event carries internal_why: %v", meta)
				}
			} else if meta["internal"] != true || meta["internal_why"] != tc.wantWhy {
				t.Fatalf("meta = %v, want internal true, internal_why %q", meta, tc.wantWhy)
			}
			if isCI, _ := ev["is_ci"].(bool); isCI != tc.wantIsCI {
				t.Fatalf("is_ci = %v, want %v", ev["is_ci"], tc.wantIsCI)
			}
			if meta["k"] != "v" {
				t.Fatalf("the event lost its own meta: %v", meta)
			}
		})
	}
}

// gitCmd runs git in dir, for a test that needs a checkout.
func gitCmd(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	return cmd.CombinedOutput()
}
