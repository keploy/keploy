package routes

import (
	"context"
	"encoding/json"
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
	return containerkey.Container{ID: fmt.Sprint(dst.Port()), Image: "postgres:16", Port: 5432, Fingerprint: "0123456789ab"}, true, nil
}

func (oneContainer) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (oneContainer) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (oneContainer) AwaitReadsOf(context.Context, []string)             {}

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
	list := `{"source":"cli","seq":1,"containers":[{"id":"pg","image":"postgres:16","ips":["172.18.0.2"],"fingerprint":"0123456789ab"}]}`

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
	if _, key, _ := containerkey.Default.Name(zap.NewNop(), "1", "172.18.0.2:5432"); key != "container:postgres:5432@0123456789ab" {
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
		// The CLI's ask for settles too: the agent's Docker is its own here,
		// so with the switch on it refuses at once.
		for _, path := range []string{"/agent/containers", "/agent/containers/settle?after=0"} {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != tc.want {
				t.Fatalf("KEPLOY_PROVEN=%q: GET %s answered %d, want %d", tc.proven, path, rec.Code, tc.want)
			}
		}
	}
}

// The CLI's ask for settles is held open until the docker-mode agent asks
// one, and is answered with its count and run; an agent that reads Docker
// itself refuses it, and a count that is not one is refused.
func TestHandleSettleContainers(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	a := &Agent{logger: zap.NewNop()}
	ask := func(ctx context.Context, query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleSettleContainers(rec, httptest.NewRequest(http.MethodGet, "/agent/containers/settle"+query, nil).WithContext(ctx))
		return rec
	}
	containerkey.Default = containerkey.NewKeyer(oneContainer{})
	if rec := ask(t.Context(), "?after=0"); rec.Code != http.StatusConflict {
		t.Fatalf("an agent that reads Docker answered %d, want 409", rec.Code)
	}
	containerkey.UsePushed()
	if rec := ask(t.Context(), "?after=x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a count that is not one answered %d, want 400", rec.Code)
	}
	if err := containerkey.ApplySnapshot(containerkey.Snapshot{Source: "cli", Seq: 1, Settles: true, Containers: []containerkey.SnapshotContainer{}}); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if rec := ask(gone, "?after=0"); rec.Body.Len() != 0 {
		t.Fatalf("an ask whose CLI went away was answered %q", rec.Body.String())
	}
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- ask(t.Context(), "?after=0&boot=") }()
	containerkey.Default.Name(zap.NewNop(), "c1", "172.18.0.9:5432")
	settled := make(chan struct{})
	go func() {
		// A first use that finds nothing placed asks its CLI for a list.
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		_, _ = containerkey.StateFromContext(containerkey.WithConn(ctx, containerkey.Default, "c1"))
		close(settled)
	}()
	rec := <-got
	var answer containerkey.SettleAsk
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); rec.Code != http.StatusOK || err != nil || answer.Asked != 1 || answer.Boot == "" {
		t.Fatalf("the ask was answered %d %q (%v), want the agent's first settle", rec.Code, rec.Body.String(), err)
	}
	if err := containerkey.ApplySnapshot(containerkey.Snapshot{Source: "cli", Seq: 2, Settles: true, Settled: 1, SettledBoot: answer.Boot, Containers: []containerkey.SnapshotContainer{}}); err != nil {
		t.Fatal(err)
	}
	<-settled
}
