package http_test

// The agent keeps the consumed history between /updatemockparams calls
// (models.ConsumedSync), so the client sends only what was written since its
// last call. These run the real routes against the real client: the service
// must be handed, at every call, exactly the map a full send would carry,
// while what goes on the wire stays the size of one test's consumption.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// historySvc records the consumed history each filter hands the service.
type historySvc struct {
	stubSvc
	mu   sync.Mutex
	seen []map[string]models.MockState
}

func (s *historySvc) UpdateMockParams(_ context.Context, p models.MockFilterParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, maps.Clone(p.TotalConsumedMocks))
	return nil
}

func (s *historySvc) last(t *testing.T) map[string]models.MockState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("the service was handed no filter")
	}
	return s.seen[len(s.seen)-1]
}

// perTestHistorySvc reads the history for its per-test mocks only, the way the
// agent service answers for itself (models.ConsumedScopeHeader).
type perTestHistorySvc struct{ historySvc }

func (s *perTestHistorySvc) ReadsConsumedForPerTestOnly(svc agentsvc.Service) bool {
	return svc == agentsvc.Service(s)
}

// wire records the consumed entries each /updatemockparams request carried.
type wire struct {
	mu      sync.Mutex
	entries []int
	syncs   []*models.ConsumedSync
}

func (w *wire) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/updatemockparams" {
			body, _ := io.ReadAll(r.Body)
			var req models.UpdateMockParamsReq
			_ = json.Unmarshal(body, &req)
			w.mu.Lock()
			w.entries = append(w.entries, len(req.FilterParams.TotalConsumedMocks))
			w.syncs = append(w.syncs, req.ConsumedSync)
			w.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(rw, r)
	})
}

func (w *wire) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}

// agentAt serves svc behind the real routes; swap replaces the agent behind
// the same address, as docker compose starting it again does.
type agentAt struct {
	mu sync.Mutex
	h  http.Handler
}

func (a *agentAt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	h := a.h
	a.mu.Unlock()
	h.ServeHTTP(w, r)
}

func (a *agentAt) swap(svc agentsvc.Service) {
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	a.mu.Lock()
	a.h = r
	a.mu.Unlock()
}

func serveHistory(t *testing.T, svc agentsvc.Service) (*agentAt, *wire, string) {
	t.Helper()
	at := &agentAt{}
	at.swap(svc)
	w := &wire{}
	srv := httptest.NewServer(w.wrap(at))
	t.Cleanup(srv.Close)
	return at, w, srv.URL + "/agent"
}

// consume writes n entries of test i into h, the way a test's drain does, and
// re-writes one entry of an earlier test (a mock used again, now Deleted).
func consume(h *models.ConsumedHistory, i, n int) {
	for j := 0; j < n; j++ {
		h.Set(models.MockState{Name: fmt.Sprintf("mock-%d-%d", i, j), Usage: models.Updated, SortOrder: int64(i*10 + j)})
	}
	if i > 0 {
		h.Set(models.MockState{Name: fmt.Sprintf("mock-%d-0", i-1), Usage: models.Deleted, SortOrder: int64(i * 10)})
	}
}

// sameHistory reports whether a and b name the same states; no map and an
// empty one are the same history.
func sameHistory(a, b map[string]models.MockState) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func filterFor(h *models.ConsumedHistory, region map[string]struct{}) models.MockFilterParams {
	return models.MockFilterParams{TotalConsumedMocks: h.Map(), ConsumedHistory: h, ConsumedRegion: region}
}

func TestTheAgentKeepsTheConsumedHistoryAndIsSentOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	svc := &historySvc{}
	_, w, uri := serveHistory(t, svc)
	c := newClient(t, uri)

	const tests, perTest = 300, 4
	h := models.NewConsumedHistory(0)
	for i := 0; i < tests; i++ {
		consume(h, i, perTest)
		if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
			t.Fatalf("test %d: UpdateMockParams: %v", i, err)
		}
		if got, want := svc.last(t), h.Map(); !sameHistory(got, want) {
			t.Fatalf("test %d: the service filtered with %d entries, the history has %d", i, len(got), len(want))
		}
	}
	if w.calls() != tests {
		t.Fatalf("%d calls for %d filters: none should have been sent twice", w.calls(), tests)
	}
	// The first call starts the stream with the whole history; every later
	// one carries the entries written since: one test's, whatever the index.
	for i, n := range w.entries {
		want := perTest + 1
		if i == 0 {
			want = perTest
		}
		if n != want {
			t.Fatalf("call %d carried %d consumed entries; want %d, the entries its test wrote (the history then held %d)", i, n, want, len(svc.seen[i]))
		}
		if s := w.syncs[i]; s == nil || s.Full != (i == 0) {
			t.Fatalf("call %d: sync %+v; want a full send only first", i, s)
		}
	}
}

func TestAnAgentStartedAgainIsSentTheWholeHistory(t *testing.T) {
	ctx := context.Background()
	first := &historySvc{}
	at, w, uri := serveHistory(t, first)
	c := newClient(t, uri)

	h := models.NewConsumedHistory(0)
	for i := 0; i < 3; i++ {
		consume(h, i, 2)
		if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
	}
	again := &historySvc{}
	at.swap(again)
	consume(h, 3, 2)
	before := w.calls()
	if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
		t.Fatalf("UpdateMockParams to the agent started again: %v", err)
	}
	if got := w.calls() - before; got != 2 {
		t.Fatalf("%d requests; want 2: the send built on the old agent's state, refused, then the whole history", got)
	}
	if got, want := again.last(t), h.Map(); !sameHistory(got, want) {
		t.Fatalf("the agent started again filtered with %d entries; the history has %d", len(got), len(want))
	}
	if n := len(again.seen); n != 1 {
		t.Fatalf("the agent started again applied %d filters; want 1, the refused send applies nothing", n)
	}
}

func TestANewHistoryIsNeverAddedToTheOldOne(t *testing.T) {
	// A retry cycle rewinds the history to its baseline, a new history: the
	// agent must filter with that baseline alone, not with what the cycle
	// before consumed on top of it.
	ctx := context.Background()
	svc := &historySvc{}
	_, _, uri := serveHistory(t, svc)
	c := newClient(t, uri)

	baseline := map[string]models.MockState{"boot": {Name: "boot", Usage: models.Updated}}
	h := models.ConsumedHistoryOf(baseline)
	for i := 0; i < 3; i++ {
		consume(h, i, 2)
		if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
	}
	rewound := models.ConsumedHistoryOf(baseline)
	if err := c.UpdateMockParams(ctx, filterFor(rewound, nil)); err != nil {
		t.Fatalf("UpdateMockParams after the rewind: %v", err)
	}
	if got := svc.last(t); !sameHistory(got, baseline) {
		t.Fatalf("after the rewind the service filtered with %d entries; want the baseline's %d", len(got), len(baseline))
	}
}

func TestAnAgentThatKeepsNoHistoryIsSentAllOfItEveryTime(t *testing.T) {
	// An agent released before ConsumedSync answers without the header.
	ctx := context.Background()
	var mu sync.Mutex
	var seen []map[string]models.MockState
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req models.UpdateMockParamsReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req.FilterParams.TotalConsumedMocks)
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"isSuccess":true}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL+"/agent")

	h := models.NewConsumedHistory(0)
	for i := 0; i < 5; i++ {
		consume(h, i, 2)
		if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		mu.Lock()
		got := seen[len(seen)-1]
		mu.Unlock()
		if !sameHistory(got, h.Map()) {
			t.Fatalf("call %d carried %d entries; an agent that keeps nothing must be sent all %d", i, len(got), h.Len())
		}
	}
}

func TestAnAgentReplacedByOneThatKeepsNoHistoryIsSentItAgain(t *testing.T) {
	// The client built on what the first agent kept, and an agent that keeps
	// nothing answered: it filtered with the entries since alone, so the
	// client sends the same filter again with the whole history.
	ctx := context.Background()
	at, w, uri := serveHistory(t, &historySvc{})
	c := newClient(t, uri)
	h := models.NewConsumedHistory(0)
	consume(h, 0, 3)
	if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
		t.Fatalf("UpdateMockParams: %v", err)
	}
	var mu sync.Mutex
	var seen []map[string]models.MockState
	at.mu.Lock()
	at.h = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req models.UpdateMockParamsReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req.FilterParams.TotalConsumedMocks)
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"isSuccess":true}`))
	})
	at.mu.Unlock()
	consume(h, 1, 3)
	before := w.calls()
	if err := c.UpdateMockParams(ctx, filterFor(h, nil)); err != nil {
		t.Fatalf("UpdateMockParams: %v", err)
	}
	if got := w.calls() - before; got != 2 {
		t.Fatalf("%d requests; want the send since, then the whole history", got)
	}
	if got := seen[len(seen)-1]; !sameHistory(got, h.Map()) {
		t.Fatalf("the last send carried %d entries; want the whole history's %d", len(got), h.Len())
	}
}

func TestANarrowedStartIsSentWholeOnceTheAgentSaysItKeeps(t *testing.T) {
	// An agent that reads per-test entries only (models.ConsumedScopeHeader)
	// and keeps no history is sent the per-test region's entries alone, as
	// before. When one that keeps the history takes its place, the stream
	// starts narrowed; but the region can gain a mock within a set, which
	// that start has no entry of, so the client starts the stream again,
	// whole, and builds on that one.
	ctx := context.Background()
	at, w, uri := serveHistory(t, &historySvc{})
	at.mu.Lock()
	at.h = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rw.Header().Set(models.ConsumedScopeHeader, models.ConsumedScopePerTest)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"isSuccess":true}`))
	})
	at.mu.Unlock()
	c := newClient(t, uri)

	h := models.NewConsumedHistory(0)
	h.Set(models.MockState{Name: "session-boot", Usage: models.Updated})
	region := map[string]struct{}{}
	step := func(i int) {
		t.Helper()
		consume(h, i, 2)
		for j := 0; j < 2; j++ {
			region[fmt.Sprintf("mock-%d-%d", i, j)] = struct{}{}
		}
		if err := c.UpdateMockParams(ctx, filterFor(h, region)); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
	}
	step(0)
	step(1)
	if w.entries[1] != h.Len()-1 {
		t.Fatalf("call 1 carried %d entries; want the per-test region's %d", w.entries[1], h.Len()-1)
	}
	svc := &perTestHistorySvc{}
	at.swap(svc)
	step(2)
	if _, ok := svc.seen[0]["session-boot"]; ok || len(svc.seen[0]) != h.Len()-1 {
		t.Fatalf("the narrowed start handed %d entries; want the per-test region's %d", len(svc.seen[0]), h.Len()-1)
	}
	step(3)
	if s := w.syncs[3]; s == nil || !s.Full {
		t.Fatalf("call 3: sync %+v; want the stream started again, whole: the narrowed start was built on", s)
	}
	if got := svc.last(t); !sameHistory(got, h.Map()) {
		t.Fatalf("call 3: the service filtered with %d entries; the history has %d", len(got), h.Len())
	}
	step(4)
	if s := w.syncs[4]; s == nil || s.Full {
		t.Fatalf("call 4: sync %+v; want only what was written since", s)
	}
	if got := svc.last(t); !sameHistory(got, h.Map()) {
		t.Fatalf("call 4: the service filtered with %d entries; the history has %d", len(got), h.Len())
	}
}
