package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch every answer of the agent names its process
// (models.AgentInstanceHeader): one that answers, one that refuses, one for a
// route that is not there, and the same name from every router the process
// builds. A client tells an agent started again in this one's place by its
// answers alone. With the switch off no answer carries it.
func TestEveryAnswerNamesTheAgentProcessUnderTheSwitch(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run(provenswitch.Env+"="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			for build := 1; build <= 2; build++ {
				for _, tc := range []struct {
					name string
					svc  lossSvc
					path string
					code int
				}{
					{"an answer", lossSvc{}, "/agent/capturelosses", http.StatusOK},
					{"a refusal", lossSvc{err: errors.New("bad file descriptor")}, "/agent/capturelosses", http.StatusInternalServerError},
					{"no such route", lossSvc{}, "/agent/no-such-route", http.StatusNotFound},
				} {
					r := chi.NewRouter()
					DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
					rec := httptest.NewRecorder()
					r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
					if rec.Code != tc.code {
						t.Fatalf("router %d, %s: status %d, want %d", build, tc.name, rec.Code, tc.code)
					}
					got := rec.Header().Get(models.AgentInstanceHeader)
					want := ""
					if proven == "1" {
						want = processInstance
					}
					if got != want {
						t.Fatalf("router %d, %s: %s %q, want %q", build, tc.name, models.AgentInstanceHeader, got, want)
					}
				}
			}
		})
	}
	if processInstance == "" {
		t.Fatal("the agent process has no instance")
	}
}

// printInstanceEnv makes TestHelperPrintsItsAgentInstance, in the test binary
// run as a child, print the child's own process instance.
const printInstanceEnv = "KEPLOY_TEST_PRINT_AGENT_INSTANCE"

// TestHelperPrintsItsAgentInstance is the child of
// TestAnotherAgentProcessNamesAnotherInstance; run otherwise, it does nothing.
func TestHelperPrintsItsAgentInstance(t *testing.T) {
	if os.Getenv(printInstanceEnv) != "1" {
		t.Skip("the child of TestAnotherAgentProcessNamesAnotherInstance")
	}
	_, _ = os.Stdout.WriteString("instance=" + processInstance + "\n")
}

// Another agent process, as one started again in this one's place is, names
// another instance.
func TestAnotherAgentProcessNamesAnotherInstance(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestHelperPrintsItsAgentInstance$")
	cmd.Env = append(os.Environ(), printInstanceEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the test binary as another process: %v: %s", err, out)
	}
	var other string
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "instance="); ok {
			other = v
		}
	}
	if other == "" || other == processInstance {
		t.Fatalf("another process names instance %q, this one %q: a process started again must name another", other, processInstance)
	}
}
