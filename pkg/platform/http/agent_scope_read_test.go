package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// GetScopeWindows is the CLI's read once its command has exited, so under
// the switch it is always the run's last (?final=1): the agent fails a test
// still open then, where a plain read would leave it out of the verdict.
// With the switch off it is the plain read keploy made before the switch.
// Under the switch an agent without the read is an error, never "no tests".
func TestAgentClient_GetScopeWindowsIsTheFinalRead(t *testing.T) {
	var queries []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/scope/windows" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`[{"name":"TestA"}]`))
		}
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()

	t.Setenv(provenswitch.Env, "1")
	got, err := a.GetScopeWindows(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "TestA" {
		t.Fatalf("got %+v, %v", got, err)
	}
	t.Setenv(provenswitch.Env, "0")
	if got, err := a.GetScopeWindows(ctx); err != nil || len(got) != 1 {
		t.Fatalf("switch off: got %+v, %v", got, err)
	}
	if len(queries) != 2 || queries[0] != "final=1" || queries[1] != "" {
		t.Fatalf("queries %q, want final=1 under the switch, then the plain read with it off", queries)
	}

	status = http.StatusNotFound
	t.Setenv(provenswitch.Env, "0")
	if got, err := a.GetScopeWindows(ctx); err != nil || got != nil {
		t.Fatalf("switch off, an older agent: got %+v, %v, want no windows and no error", got, err)
	}
	t.Setenv(provenswitch.Env, "1")
	if _, err := a.GetScopeWindows(ctx); err == nil {
		t.Fatal("switch on, an agent without the read: want an error")
	}
}

// PushProvenTable sends the per-test table and the pool slices in one
// request, an empty pool as {}, and any answer but 200 is an error.
func TestAgentClient_PushProvenTable(t *testing.T) {
	var bodies []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scope/table" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]}}`, `{"mappings":{"o::TestA":[]},"pool":{}}`}
	if len(bodies) != 2 || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("posted %q, want %q", bodies, want)
	}
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusInternalServerError} {
		status = s
		if err := a.PushProvenTable(ctx, nil, nil); err == nil {
			t.Fatalf("status %d: want an error", s)
		}
	}
}
