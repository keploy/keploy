package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// httpMockOnHost builds a recorded HTTP mock the way the recorder writes one:
// a path-only URL with the upstream authority in the Host header.
func httpMockOnHost(name, method, rawURL, host string) *models.Mock {
	header := map[string]string{"Accept": "application/json"}
	if host != "" {
		header["Host"] = host
	}
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    rawURL,
				Header: header,
			},
		},
	}
}

func reqToHost(method, path, host string) *http.Request {
	return &http.Request{
		Method: method,
		URL:    &url.URL{Path: path},
		Host:   host,
		Header: http.Header{},
	}
}

// End-to-end over the real report builder, in the shape of the reported
// bundle: a sidecar container's call to 192.0.2.20 that none of the compared
// mocks target, against a pool recorded from the one container the user named.
// Until this check existed the report claimed the request structure had
// changed and led with a diff against a mock on a different host.
func TestBuildHTTPMismatchReport_DestinationScope(t *testing.T) {
	recorded := []*models.Mock{
		httpMockOnHost("mock-1", "GET", "/api/orders?id=90", "192.0.2.10"),
		httpMockOnHost("mock-2", "GET", "/api/clients?code=demo", "192.0.2.10"),
		httpMockOnHost("mock-3", "POST", "/oauth/token?grant_type=client_credentials", "192.0.2.30:9090"),
		httpMockOnHost("mock-4", "GET", "/api/vendors?name=vendor-a", "192.0.2.40"),
	}
	// One mock whose upstream cannot be read: a path-only URL with no Host
	// header. It could be the mock that targeted the live call, so it has to
	// veto the whole verdict.
	unreadable := append(append([]*models.Mock{}, recorded...),
		httpMockOnHost("mock-5", "GET", "/internal/health", ""))

	tests := []struct {
		name        string
		mocks       []*models.Mock
		request     *http.Request
		diag        *matchDiag
		wantScope   string
		wantPhase   string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:      "sidecar destination absent from the compared set",
			mocks:     recorded,
			request:   reqToHost("GET", "/v1/metrics", "192.0.2.20"),
			diag:      &matchDiag{phase: models.MatchPhaseSchema, candidates: len(recorded)},
			wantScope: models.DestinationScopeNotInComparedSet,
			wantPhase: models.MatchPhaseSchema,
			wantContain: []string{
				"No recorded HTTP mock in the compared set targets 192.0.2.20",
			},
			// The shared causes are rendered once per test from
			// models.OutOfScopeDestinationCauses, never per call.
			wantAbsent: []string{"Request structure changed", "sidecar", "ONE container per session"},
		},
		{
			name:        "destination in the compared set, drifted request keeps today's message",
			mocks:       recorded,
			request:     reqToHost("GET", "/api/orders/renamed", "192.0.2.10"),
			diag:        &matchDiag{phase: models.MatchPhaseSchema, candidates: len(recorded)},
			wantScope:   models.DestinationScopeInComparedSet,
			wantPhase:   models.MatchPhaseSchema,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
		{
			// One mock in the pool has no readable upstream, so the pool
			// proves nothing about any destination.
			name:        "one unreadable mock in the pool falls back",
			mocks:       unreadable,
			request:     reqToHost("GET", "/v1/metrics", "192.0.2.20"),
			diag:        &matchDiag{phase: models.MatchPhaseSchema, candidates: len(unreadable)},
			wantScope:   models.DestinationScopeUnknown,
			wantPhase:   models.MatchPhaseSchema,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
		{
			// No Host header and no URL authority: the live destination is
			// unknown, so nothing can be said about it.
			name:        "unknown live destination falls back",
			mocks:       recorded,
			request:     makeReqWithQuery("GET", "/v1/metrics", ""),
			diag:        &matchDiag{phase: models.MatchPhaseSchema, candidates: len(recorded)},
			wantScope:   models.DestinationScopeUnknown,
			wantPhase:   models.MatchPhaseSchema,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
		{
			// An empty pool is MatchPhaseNoMocks' case, and it keeps it.
			name:        "empty mock set keeps no_mocks",
			mocks:       nil,
			request:     reqToHost("GET", "/v1/metrics", "192.0.2.20"),
			diag:        nil,
			wantScope:   models.DestinationScopeUnknown,
			wantPhase:   models.MatchPhaseNoMocks,
			wantContain: []string{"No recorded mocks were available"},
			wantAbsent:  []string{"compared set", "sidecar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHTTP()
			db := &mockMemDb{mocks: tt.mocks}
			report := h.buildHTTPMismatchReport(tt.request, nil, db, nil, nil, nil, tt.diag)

			if report.DestinationScope != tt.wantScope {
				t.Errorf("DestinationScope = %q, want %q", report.DestinationScope, tt.wantScope)
			}
			if report.MatchPhase != tt.wantPhase {
				t.Errorf("MatchPhase = %q, want %q", report.MatchPhase, tt.wantPhase)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(report.NextSteps, want) {
					t.Errorf("NextSteps missing %q:\n%s", want, report.NextSteps)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(report.NextSteps, absent) {
					t.Errorf("NextSteps must not contain %q:\n%s", absent, report.NextSteps)
				}
			}
		})
	}
}

// THE reason the claim is local. Every mock for 192.0.2.10 has been served and
// permanently stripped from the pool (updateMock -> DeleteFilteredMock, then
// the agent's filterOutDeleted for every later test), so the pool the matcher
// just failed against mentions the host nowhere — even though it is the
// application's own upstream and was recorded all along.
//
// The verdict may say what it can see: nothing here targets that host. It may
// NOT dress that up as "never recorded", which is what four earlier cuts of
// this diagnostic did and what put a false claim in front of a user.
func TestBuildHTTPMismatchReport_ConsumedHostMakesNoGlobalClaim(t *testing.T) {
	h := newHTTP()
	// What survives: one mock, on a different host entirely.
	survivors := []*models.Mock{httpMockOnHost("mock-4", "GET", "/api/vendors?name=vendor-a", "192.0.2.40")}
	db := &mockMemDb{mocks: survivors}

	report := h.buildHTTPMismatchReport(
		reqToHost("GET", "/api/orders?id=91", "192.0.2.10"), nil, db, survivors, nil, nil,
		&matchDiag{phase: models.MatchPhaseSchema, candidates: len(survivors)})

	if report.DestinationScope != models.DestinationScopeNotInComparedSet {
		t.Fatalf("DestinationScope = %q, want %q", report.DestinationScope, models.DestinationScopeNotInComparedSet)
	}
	for _, forbidden := range []string{"never recorded", "was not recorded", "outside the recorded scope"} {
		if strings.Contains(strings.ToLower(report.NextSteps), forbidden) {
			t.Errorf("a consumed host must not be claimed unrecorded (%q):\n%s", forbidden, report.NextSteps)
		}
	}
	// And the guidance the user actually reads says out loud why they should
	// not read it as one. The caveat lives in the once-per-test block, and it
	// has to cover BOTH ways a recorded mock leaves the compared set:
	// consumption earlier in the run, and a per-test window that excluded it.
	if !strings.Contains(models.OutOfScopeDestinationCauses,
		"Mocks already served earlier in this run, or recorded outside this test's mock window, are no") {
		t.Errorf("the consumption/windowing caveat must be stated, got:\n%s", models.OutOfScopeDestinationCauses)
	}
}

// THE coverage gap this diagnostic used to have. On the schema-survivor path
// buildHTTPMismatchReport deliberately does not reload the pool — it diffs
// against the survivors — so it held only a SUBSET of the compared set and had
// to stay silent. That silence landed on exactly the worst case: an
// out-of-scope call whose method+path schema-matches an application mock
// (shared paths like /health, /metrics, /oauth/token on a different host), left
// with the old "Request structure changed" message and a closest-mock diff
// against another upstream.
//
// matchDiag.pool carries the set match() actually walked, so the verdict is
// reachable there too. The evidence for the DIFF is unchanged: the closest mock
// still comes from the survivors, exactly as before.
func TestBuildHTTPMismatchReport_SchemaSurvivorPathIsStillScored(t *testing.T) {
	h := newHTTP()
	// /health recorded for the app's own upstream; the live /health goes to a
	// sidecar's. Same method, same path — a schema match, on a different host.
	appHealth := httpMockOnHost("mock-1", "GET", "/health", "192.0.2.10")
	pool := []*models.Mock{
		appHealth,
		httpMockOnHost("mock-2", "GET", "/api/orders?id=90", "192.0.2.10"),
	}
	survivors := []*models.Mock{appHealth}
	db := &mockMemDb{mocks: pool}

	report := h.buildHTTPMismatchReport(
		reqToHost("GET", "/health", "192.0.2.20"), nil, db, nil, nil, nil,
		&matchDiag{
			phase:         models.MatchPhaseBody,
			candidates:    len(pool),
			schemaMatched: survivors,
			pool:          pool,
		})

	if report.DestinationScope != models.DestinationScopeNotInComparedSet {
		t.Fatalf("DestinationScope = %q, want %q on the schema-survivor path",
			report.DestinationScope, models.DestinationScopeNotInComparedSet)
	}
	if !strings.Contains(report.NextSteps, "in the compared set targets 192.0.2.20") {
		t.Errorf("expected the out-of-scope guidance, got:\n%s", report.NextSteps)
	}
	// Diagnostic-only: the candidate the matcher was diffing against is
	// untouched, and so is the cascade stop.
	if report.ClosestMock != "mock-1" {
		t.Errorf("ClosestMock = %q, want the schema survivor mock-1", report.ClosestMock)
	}
	if report.MatchPhase != models.MatchPhaseBody || report.CandidateCount != len(pool) {
		t.Errorf("phase/candidates disturbed: %q / %d", report.MatchPhase, report.CandidateCount)
	}
}

// The diag pool is the compared set, so it also decides IN-set correctly on the
// survivor path — the check must not have become a one-way "always out of
// scope" stamp for schema survivors.
func TestBuildHTTPMismatchReport_SchemaSurvivorPathInScope(t *testing.T) {
	h := newHTTP()
	appHealth := httpMockOnHost("mock-1", "GET", "/health", "192.0.2.10")
	pool := []*models.Mock{appHealth, httpMockOnHost("mock-2", "GET", "/api/orders?id=90", "192.0.2.10")}
	db := &mockMemDb{mocks: pool}

	report := h.buildHTTPMismatchReport(
		reqToHost("GET", "/health", "192.0.2.10"), nil, db, nil, nil, nil,
		&matchDiag{
			phase:         models.MatchPhaseBody,
			candidates:    len(pool),
			schemaMatched: []*models.Mock{appHealth},
			pool:          pool,
		})

	if report.DestinationScope != models.DestinationScopeInComparedSet {
		t.Fatalf("DestinationScope = %q, want %q", report.DestinationScope, models.DestinationScopeInComparedSet)
	}
	if strings.Contains(report.NextSteps, "in the compared set targets") {
		t.Errorf("an in-scope miss must keep its ordinary guidance:\n%s", report.NextSteps)
	}
}

// When a caller hands over fewer mocks than the matcher reported comparing and
// supplies no diag.pool, what is in hand is a SUBSET of the compared set, and
// "none of them targets it" would overreach on the reader's behalf (the report
// says "candidates: 19" right next to it).
//
// This is a guard for callers, not a live production branch: the production
// caller (decode.go) always passes httpMocks == nil, and match() always
// attaches diag.pool, whose length equals diag.candidates by construction. It
// still fires for a hand-built diag like this one, for a future parser reusing
// the builder, and for the pool-reload path if a concurrent consumption on
// another connection shrank the pool between the match and the report.
func TestBuildHTTPMismatchReport_SubsetOfComparedPoolMakesNoClaim(t *testing.T) {
	h := newHTTP()
	survivors := []*models.Mock{httpMockOnHost("mock-4", "GET", "/api/vendors?name=vendor-a", "192.0.2.40")}
	db := &mockMemDb{mocks: survivors}

	report := h.buildHTTPMismatchReport(
		reqToHost("GET", "/v1/metrics", "192.0.2.20"), nil, db, survivors, nil, nil,
		&matchDiag{phase: models.MatchPhaseSchema, candidates: 19, schemaMatched: survivors})

	if report.DestinationScope != models.DestinationScopeUnknown {
		t.Errorf("DestinationScope = %q, want unset when only a subset of the compared pool is in hand", report.DestinationScope)
	}
	if !strings.Contains(report.NextSteps, "Request structure changed since recording") {
		t.Errorf("expected the unchanged fallback message, got %q", report.NextSteps)
	}
}

// The live destination is read from the Host header first and the URL
// authority second, mirroring how recorded mocks store it — so the same
// upstream is recognised whichever way the live request carries it.
func TestBuildHTTPMismatchReport_LiveDestinationFromURL(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{httpMockOnHost("mock-1", "GET", "/api/orders?id=90", "192.0.2.10")}
	db := &mockMemDb{mocks: mocks}

	// No Host field; the authority sits on the URL, as it does for a request
	// captured through a forward proxy.
	request := &http.Request{
		Method: "GET",
		URL:    &url.URL{Host: "192.0.2.20", Path: "/v1/metrics"},
		Header: http.Header{},
	}
	report := h.buildHTTPMismatchReport(request, nil, db, mocks, nil, nil,
		&matchDiag{phase: models.MatchPhaseSchema, candidates: 1})

	if report.Destination != "192.0.2.20" {
		t.Fatalf("Destination = %q, want the URL authority", report.Destination)
	}
	if report.DestinationScope != models.DestinationScopeNotInComparedSet {
		t.Errorf("DestinationScope = %q, want %q", report.DestinationScope, models.DestinationScopeNotInComparedSet)
	}
}

// comparedDestinations is the whole evidence source; its undecidable arms are
// the safety property, so they are pinned directly.
func TestComparedDestinations(t *testing.T) {
	readable := []*models.Mock{
		httpMockOnHost("a", "GET", "/x", "192.0.2.10"),
		httpMockOnHost("b", "GET", "/y", "192.0.2.10"), // duplicate host collapses
		httpMockOnHost("c", "GET", "/z", "192.0.2.40"),
	}

	if got := comparedDestinations(nil, 0); got != nil {
		t.Errorf("empty pool must be undecidable, got %v", got)
	}
	if got := comparedDestinations(readable, 19); got != nil {
		t.Errorf("subset of the compared pool must be undecidable, got %v", got)
	}
	withUnreadable := append(append([]*models.Mock{}, readable...), httpMockOnHost("d", "GET", "/w", ""))
	if got := comparedDestinations(withUnreadable, len(withUnreadable)); got != nil {
		t.Errorf("one unreadable mock must veto the set, got %v", got)
	}
	got := comparedDestinations(readable, len(readable))
	if len(got) != 2 {
		t.Fatalf("comparedDestinations = %v, want the 2 distinct hosts", got)
	}
	want := map[string]bool{"192.0.2.10": true, "192.0.2.40": true}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected destination %q in %v", d, got)
		}
	}
}

// The plumbing that makes the schema-survivor path decidable at all: match()
// must hand the compared set out on EVERY miss return, and len(pool) must equal
// the candidate count it reports — comparedDestinations refuses to judge a pool
// smaller than that count, so a diag that reports 19 candidates and carries 3
// mocks would silently disable the diagnostic again.
func TestMatchDiagCarriesTheComparedPool(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	postMock := httpMockOnHost("mock-2", "POST", "http://192.0.2.10/api/orders", "192.0.2.10")
	postMock.Spec.HTTPReq.Body = `{"id":"recorded"}`
	pool := []*models.Mock{
		httpMockOnHost("mock-1", "GET", "http://192.0.2.10/api/orders?id=90", "192.0.2.10"),
		postMock,
	}
	db := &mockMemDb{mocks: pool}

	// Both miss shapes: the no-schema-candidate return, and the
	// schema-SURVIVOR return, which is the one the builder does not reload a
	// pool for and where the diagnostic used to be blind.
	cases := []struct {
		name      string
		live      *req
		wantPhase string
		wantSurv  bool
	}{
		{
			name: "no schema candidate",
			live: &req{
				method: "GET",
				url:    &url.URL{Scheme: "http", Host: "192.0.2.20", Path: "/v1/metrics"},
				header: http.Header{},
				raw:    []byte("GET /v1/metrics HTTP/1.1\r\nHost: 192.0.2.20\r\n\r\n"),
			},
			wantPhase: models.MatchPhaseSchema,
		},
		{
			// Same method+path as a recorded mock on ANOTHER host: schema
			// matches, the body rules it out, and the survivor comes back on
			// the diag. This is the shape the gap hid in.
			name: "schema survivor, body mismatch",
			live: &req{
				method: "POST",
				url:    &url.URL{Scheme: "http", Host: "192.0.2.20", Path: "/api/orders"},
				header: http.Header{
					"Content-Type": []string{"application/json"},
					"Accept":       []string{"application/json"},
					"Host":         []string{"192.0.2.20"},
				},
				// A DIFFERENT top-level key, not just a different value: the
				// body schema match is key-based, so a same-key body would
				// MATCH here and be served instead of missing.
				body: []byte(`{"unrelated":"live"}`),
				raw:  []byte("POST /api/orders HTTP/1.1\r\nHost: 192.0.2.20\r\n\r\n{\"unrelated\":\"live\"}"),
			},
			wantPhase: models.MatchPhaseBody,
			wantSurv:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, _, diag, err := h.match(ctx, tc.live, db, nil, nil, nil, true, false, false)
			if err != nil || matched {
				t.Fatalf("match() = (%v, %v), want a clean miss", matched, err)
			}
			if diag == nil {
				t.Fatal("miss returned no diag")
			}
			if diag.phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q (the fixture no longer exercises this return)", diag.phase, tc.wantPhase)
			}
			if tc.wantSurv != (len(diag.schemaMatched) > 0) {
				t.Fatalf("schemaMatched = %d, want survivors: %v", len(diag.schemaMatched), tc.wantSurv)
			}
			if len(diag.pool) != diag.candidates {
				t.Fatalf("diag.pool has %d mocks but reports %d candidates — comparedDestinations would refuse to judge it",
					len(diag.pool), diag.candidates)
			}
			if len(diag.pool) != len(pool) {
				t.Fatalf("diag.pool = %d mocks, want the whole compared set (%d)", len(diag.pool), len(pool))
			}

			// And end to end: the report the builder makes from that diag
			// reaches a verdict instead of falling back.
			report := h.buildHTTPMismatchReport(
				reqToHost(tc.live.method, tc.live.url.Path, "192.0.2.20"),
				tc.live.body, db, nil, nil, nil, diag)
			if report.DestinationScope != models.DestinationScopeNotInComparedSet {
				t.Errorf("DestinationScope = %q, want %q", report.DestinationScope, models.DestinationScopeNotInComparedSet)
			}
		})
	}
}
