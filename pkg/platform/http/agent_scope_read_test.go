package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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
	t.Setenv(provenswitch.Env, "")
	if got, err := a.GetScopeWindows(ctx); err != nil || len(got) != 1 {
		t.Fatalf("switch off: got %+v, %v", got, err)
	}
	if len(queries) != 2 || queries[0] != "final=1" || queries[1] != "" {
		t.Fatalf("queries %q, want final=1 under the switch, then the plain read with it off", queries)
	}

	status = http.StatusNotFound
	t.Setenv(provenswitch.Env, "")
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
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// The recorded test windows go with them, in the same request.
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil, []models.TestWindow{{TestCase: "o::TestA", Start: t0, End: t0.Add(time.Second)}}, nil); err != nil {
		t.Fatal(err)
	}
	// The values each test gave when it was recorded ride the same request.
	given := map[string][]models.GivenValue{"o::TestA": {{Name: "given:nonce", Value: "1761", Source: "a_test.go:9"}}}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, nil, nil, given); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]}}`,
		`{"mappings":{"o::TestA":[]},"pool":{}}`,
		`{"mappings":{"o::TestA":[]},"pool":{},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}]}`,
		`{"mappings":{"o::TestA":["mock-3"]},"pool":{},"given":{"o::TestA":[{"name":"given:nonce","value":"1761","source":"a_test.go:9"}]}}`,
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("posted %q, want %q", bodies, want)
	}
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusInternalServerError} {
		status = s
		if err := a.PushProvenTable(ctx, nil, nil, nil, nil); err == nil {
			t.Fatalf("status %d: want an error", s)
		}
	}
}

// A restarted agent is handed the table again (the session's scope-table
// step), the recorded test windows and the values each test gave when it was
// recorded with it, as they were pushed: without the windows its learn step
// would read every earlier recording of the set as the paired one's place
// again, and without the givens every given of the replay would bind with no
// recorded value, and nothing would say so.
func TestAgentClient_PushProvenTableIsReappliedWithItsWindows(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scope/table" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sessions.Delete(srv.URL) })
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	a.session().configure(true)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	windows := []models.TestWindow{{TestCase: "o::TestA", Start: t0, End: t0.Add(time.Second)}}
	given := map[string][]models.GivenValue{"o::TestA": {{Name: "given:nonce", Value: "1761", Source: "a_test.go:9"}}}
	if err := a.PushProvenTable(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1"}}, windows, given); err != nil {
		t.Fatal(err)
	}
	windows[0].TestCase = "o::Changed" // the caller's slice is its own
	given["o::TestA"] = nil            // and so is its map
	if err := a.session().reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	want := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}],"given":{"o::TestA":[{"name":"given:nonce","value":"1761","source":"a_test.go:9"}]}}`
	if !slices.Equal(bodies, []string{want, want}) {
		t.Fatalf("posted %q, want the table, its windows and its givens twice: %q", bodies, want)
	}
}
