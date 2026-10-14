package routes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.uber.org/zap"
)

// oneContainer resolves every destination to one postgres container per
// destination port: an agent that reads Docker itself.
type oneContainer struct{}

func (oneContainer) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{ID: fmt.Sprint(dst.Port()), Image: "postgres:16", Port: 5432}, true, nil
}

func (oneContainer) Bind(context.Context)               {}
func (oneContainer) Watch(context.Context, *zap.Logger) {}
func (oneContainer) Endpoint() string                   { return "unix:///fake.sock" }

// A docker-mode agent says it takes the CLI's container list and names
// container destinations from it; an agent that reads Docker itself says
// it does not and refuses a list; a body that is not a list is refused.
func TestHandleContainers(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	a := &Agent{logger: zap.NewNop()}
	call := func(method, body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/agent/containers", strings.NewReader(body))
		if method == http.MethodGet {
			a.HandleTakesContainers(rec, req)
		} else {
			a.HandleContainers(rec, req)
		}
		return rec.Code
	}
	list := `{"source":"cli","seq":1,"containers":[{"id":"pg","image":"postgres:16","ips":["172.18.0.2"]}]}`

	containerkey.Default = containerkey.NewKeyer(oneContainer{})
	if code := call(http.MethodGet, ""); code != http.StatusConflict {
		t.Fatalf("an agent that reads Docker answered %d to the question, want 409", code)
	}
	if code := call(http.MethodPost, list); code != http.StatusConflict {
		t.Fatalf("an agent that reads Docker answered %d to a list, want 409", code)
	}

	containerkey.UsePushed()
	if code := call(http.MethodGet, ""); code != http.StatusNoContent {
		t.Fatalf("a docker-mode agent answered %d to the question, want 204", code)
	}
	if code := call(http.MethodPost, `{"containers":`); code != http.StatusBadRequest {
		t.Fatalf("a broken list answered %d, want 400", code)
	}
	if code := call(http.MethodPost, list); code != http.StatusNoContent {
		t.Fatalf("a docker-mode agent answered %d to a list, want 204", code)
	}
	if _, key, _ := containerkey.Default.Name(zap.NewNop(), "1", "172.18.0.2:5432"); key != "container:postgres:16:5432#1" {
		t.Fatalf("after the list the agent named %q", key)
	}
}

// The container routes exist only under the proven switch: without it the
// agent answers as it did before them.
func TestContainerRoutesOnlyUnderTheSwitch(t *testing.T) {
	for _, tc := range []struct {
		proven string
		want   int
	}{{"", http.StatusNotFound}, {"1", http.StatusConflict}} {
		t.Setenv("KEPLOY_PROVEN", tc.proven)
		r := chi.NewRouter()
		DefaultRoutes{}.New(r, nil, zap.NewNop())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/containers", nil))
		if rec.Code != tc.want {
			t.Fatalf("KEPLOY_PROVEN=%q: GET /agent/containers answered %d, want %d", tc.proven, rec.Code, tc.want)
		}
	}
}
