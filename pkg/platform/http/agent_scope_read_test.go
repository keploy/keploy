package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// PushProvenTable sends the per-test table, the pool slices and the worker
// scope in one request, an empty pool as {}, the time default only when it
// is on, and any answer but 200 is an error.
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
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1", "mock-2"}}, map[string][]string{"o": {"mock-2"}}, nil, nil, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// The recorded test windows go with them, in the same request.
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil, nil, nil, nil, []models.TestWindow{{TestCase: "o::TestA", Start: t0, End: t0.Add(time.Second)}}, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// The values each test gave when it was recorded ride the same request.
	given := map[string][]models.GivenValue{"o::TestA": {{Name: "given:nonce", Value: "1761", Source: "a_test.go:9"}}}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, nil, nil, nil, nil, nil, given, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// So do each test's repeated reads.
	repeats := map[string][]models.RepeatRead{"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-3", "mock-4"}}}}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3", "mock-4"}}, nil, nil, nil, nil, nil, nil, repeats, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// The test each worker recording trails rides it with the worker scope.
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}, "o::TestB": {}}, map[string][]string{"o": {"mock-2"}}, map[string][]string{"o": {"mock-2"}}, nil, map[string]string{"mock-2": "o::TestA"}, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// The names each test's own list marks served, not required, too.
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, nil, nil, nil, nil, nil, nil, nil, map[string][]string{"o::TestA": {"mock-3"}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// So do the names of the worker scope that are the reads of tests the
	// replay's gate skips.
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, map[string][]string{"o": {"mock-2"}}, map[string][]string{"o": {"mock-2"}}, map[string]string{"mock-2": "o::TestCRUD"}, nil, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1","mock-2"]},"worker":{"o":["mock-2"]}}`,
		`{"mappings":{"o::TestA":[]},"pool":{}}`,
		`{"mappings":{"o::TestA":[]},"pool":{},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}]}`,
		`{"mappings":{"o::TestA":["mock-3"]},"pool":{},"given":{"o::TestA":[{"name":"given:nonce","value":"1761","source":"a_test.go:9"}]}}`,
		`{"mappings":{"o::TestA":["mock-3","mock-4"]},"pool":{},"repeatReads":{"o::TestA":[{"name":"onboarding","mocks":["mock-3","mock-4"]}]}}`,
		`{"mappings":{"o::TestA":["mock-3"],"o::TestB":[]},"pool":{"o":["mock-2"]},"worker":{"o":["mock-2"]},"trails":{"mock-2":"o::TestA"}}`,
		`{"mappings":{"o::TestA":["mock-3"]},"pool":{},"notRequired":{"o::TestA":["mock-3"]}}`,
		`{"mappings":{"o::TestA":[]},"pool":{"o":["mock-2"]},"worker":{"o":["mock-2"]},"skippedReads":{"mock-2":"o::TestCRUD"}}`,
		`{"mappings":{"o::TestA":[]},"pool":{},"timeForm":true}`,
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("posted %q, want %q", bodies, want)
	}
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusInternalServerError} {
		status = s
		if err := a.PushProvenTable(ctx, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false); err == nil {
			t.Fatalf("status %d: want an error", s)
		}
	}
}

// A restarted agent is handed the table again (the session's scope-table
// step), the recorded test windows and the values each test gave when it was
// recorded with it, as they were pushed: without the windows its learn step
// would read every earlier recording of the set as the paired one's place
// again, and without the givens every given of the replay would bind with no
// recorded value, and nothing would say so. Without its not-required marks
// the restarted agent would charge each test for a marked call it did not
// make, and without its background recordings' names it would charge each
// test for one more run of the application's own polls.
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
	repeats := map[string][]models.RepeatRead{"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-3", "mock-3"}}}}
	trails := map[string]string{"mock-1": "o::TestA"}
	notRequired := map[string][]string{"o::TestA": {"mock-3"}}
	skipped := map[string]string{"mock-1": "o::TestCRUD"}
	background := []string{"mock-1"}
	if err := a.PushProvenTable(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1"}}, map[string][]string{"o": {"mock-1"}}, skipped, trails, windows, given, repeats, notRequired, background, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	windows[0].TestCase = "o::Changed" // the caller's slice is its own
	given["o::TestA"] = nil            // and so is its map
	repeats["o::TestA"] = nil          // and its repeated reads
	trails["mock-1"] = "o::Changed"    // and its trails
	notRequired["o::TestA"] = nil      // and its marks
	skipped["mock-1"] = "o::Changed"   // and its skipped reads
	background[0] = "mock-9"           // and its background recordings
	if err := a.session().reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	want := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}],"given":{"o::TestA":[{"name":"given:nonce","value":"1761","source":"a_test.go:9"}]},"worker":{"o":["mock-1"]},"trails":{"mock-1":"o::TestA"},"timeForm":true,"repeatReads":{"o::TestA":[{"name":"onboarding","mocks":["mock-3","mock-3"]}]},"notRequired":{"o::TestA":["mock-3"]},"skippedReads":{"mock-1":"o::TestCRUD"},"background":["mock-1"]}`
	if !slices.Equal(bodies, []string{want, want}) {
		t.Fatalf("posted %q, want the table, its windows, its givens, its worker scope, its trails, its repeated reads, its not-required marks, its skipped reads, its background recordings and its time default twice: %q", bodies, want)
	}
}

// A table with series is refused by an agent whose answer does not
// acknowledge them: an older agent answers 200 with no body or with no
// served field, and would serve each recording of a series once. The CLI
// errors (ErrAgentIgnoresRules), so the replay does not run on it. An agent
// that names series is taken, and a table with no series takes any 200. An
// answer that names series but does not decode as the ack (a field of
// another type) is no ack. The table posted carries the series.
func TestScopeTableWithoutAckIsRefused(t *testing.T) {
	var answer string
	var posted models.ScopeTableReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = models.ScopeTableReq{}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Errorf("the posted table does not decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()
	series := map[string][]string{"sweep/o::TestA/1": {"mock-2"}}
	push := func(series map[string][]string) error {
		return a.PushProvenTable(ctx, map[string][]string{"o::TestA": {}}, map[string][]string{"o": {"mock-2"}}, map[string][]string{"o": {"mock-2"}}, nil, nil, nil, nil, nil, nil, nil, series, nil, false)
	}
	for _, old := range []string{"", `{"status":"ok"}`, `{"status":"ok","served":["trails"]}`, `not json`, `{"status":"ok","served":["series"],`, `{"status":1,"served":["series"]}`} {
		answer = old
		if err := push(series); !errors.Is(err, ErrAgentIgnoresRules) {
			t.Fatalf("answer %q to a table with series: got %v, want ErrAgentIgnoresRules", old, err)
		}
		if err := push(nil); err != nil {
			t.Fatalf("answer %q to a table with no series: %v", old, err)
		}
	}
	answer = `{"status":"ok","served":["series"]}`
	if err := push(series); err != nil {
		t.Fatalf("an agent that acknowledges series was refused: %v", err)
	}
	if !reflect.DeepEqual(posted.Series, series) {
		t.Fatalf("the posted table's series %v, want %v", posted.Series, series)
	}
}

// TestScopeTableWithoutSourcesAckIsRefused: a table with declared sources
// (models.ScopeTableReq.Sources) pushed to an agent whose answer does not
// name them, an agent that names only series among them, is refused with
// ErrAgentIgnoresRules: it would learn a declared field as if no rule
// declared it. An agent that names sources is taken, and the table posted
// carries them; a table with series and sources needs both named.
func TestScopeTableWithoutSourcesAckIsRefused(t *testing.T) {
	var answer string
	var posted models.ScopeTableReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = models.ScopeTableReq{}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Errorf("the posted table does not decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()
	sources := map[string][]models.DeclaredSource{"mock-3": {{Rule: "audit-at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.docs.msgs.0.at", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}}}
	series := map[string][]string{"sweep/o::TestA/1": {"mock-2"}}
	push := func(series map[string][]string, sources map[string][]models.DeclaredSource) error {
		return a.PushProvenTable(ctx, map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-2"}}, map[string][]string{"o": {"mock-2"}}, nil, nil, nil, nil, nil, nil, nil, series, sources, false)
	}
	for _, old := range []string{"", `{"status":"ok"}`, `{"status":"ok","served":["series"]}`, `not json`, `{"status":1,"served":["sources"]}`} {
		answer = old
		if err := push(nil, sources); !errors.Is(err, ErrAgentIgnoresRules) {
			t.Fatalf("answer %q to a table with sources: got %v, want ErrAgentIgnoresRules", old, err)
		}
	}
	answer = `{"status":"ok","served":["sources"]}`
	if err := push(nil, sources); err != nil {
		t.Fatalf("an agent that acknowledges sources was refused: %v", err)
	}
	if !reflect.DeepEqual(posted.Sources, sources) {
		t.Fatalf("the posted table's sources %v, want %v", posted.Sources, sources)
	}
	if err := push(series, sources); !errors.Is(err, ErrAgentIgnoresRules) {
		t.Fatalf("an agent that acknowledges sources only took a table with series: got %v", err)
	}
	answer = `{"status":"ok","served":["series","sources"]}`
	if err := push(series, sources); err != nil {
		t.Fatalf("an agent that acknowledges both was refused: %v", err)
	}
}
