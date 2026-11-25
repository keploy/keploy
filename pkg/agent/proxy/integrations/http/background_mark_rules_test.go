package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

type httpBgRulesStore struct {
	integrations.MockMemDb
	recs  []models.BackgroundRecording
	rules []models.BackgroundHTTPRule
}

func (s *httpBgRulesStore) BackgroundRecordings(kind models.Kind) []models.BackgroundRecording {
	if kind != models.HTTP {
		return nil
	}
	return s.recs
}

func (s *httpBgRulesStore) BackgroundHTTPRules() []models.BackgroundHTTPRule {
	return s.rules
}

func bgHTTPInput(method, host, rawURL string) *req {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	hdr := http.Header{}
	if host != "" {
		hdr.Set("Host", host)
	}
	return &req{
		method: method,
		url:    u,
		header: hdr,
	}
}

func bgHTTPMock(name string, method models.Method, rawURL, host string) *models.Mock {
	hdr := map[string]string{}
	if host != "" {
		hdr["Host"] = host
	}
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: method,
				URL:    rawURL,
				Header: hdr,
			},
		},
	}
}

// TestDeclaredBackgroundHTTPRules verifies that HTTP declared kind: background
// rules behave uniformly with MongoDB:
//   - An unrecorded read-only (GET/HEAD/OPTIONS) call matching a declared rule
//     is marked Background=true with the rule note in NextSteps.
//   - Path segment wildcards ("{}") match exactly one non-empty segment.
//   - Unrecorded HTTP writes (POST/PUT/PATCH/DELETE) are NEVER excused.
//   - Any covering HTTP background recording blocks the fallback so strict
//     matching/clock checks are never bypassed.
//   - An unreadable HTTP background recording fails closed and blocks fallback.
func TestDeclaredBackgroundHTTPRules(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	mark := integrations.CallRound{}

	rules := []models.BackgroundHTTPRule{
		{ID: "openbao-liveness-check", Method: "GET", Host: "localhost:8200", Path: "/v1/sys/health"},
		{ID: "worker-status-poll", Method: "HEAD", Host: "status.internal:8080", Path: "/v1/nodes/{}/ready"},
		{ID: "forbid-write-fallback", Method: "POST", Host: "localhost:8200", Path: "/v1/secret/touch"},
	}

	t.Run("unrecorded GET health check matches declared rule with note", func(t *testing.T) {
		db := &httpBgRulesStore{rules: rules}
		rep := &models.MockMismatchReport{NextSteps: "Existing advice."}
		h.markBackground(ctx, rep, bgHTTPInput("GET", "localhost:8200", "http://localhost:8200/v1/sys/health"), db, db, mark, false, nil, nil, nil)
		if !rep.Background {
			t.Fatal("expected unrecorded GET matching openbao-liveness-check to be marked Background")
		}
		wantNote := "It matched the declared kind: background rule openbao-liveness-check with no covering recording in the set, so it is counted against no test."
		if !strings.Contains(rep.NextSteps, wantNote) {
			t.Fatalf("NextSteps = %q; want %q", rep.NextSteps, wantNote)
		}
	})

	t.Run("segment wildcard matches one non-empty segment only", func(t *testing.T) {
		db := &httpBgRulesStore{rules: rules}
		rep := &models.MockMismatchReport{}
		h.markBackground(ctx, rep, bgHTTPInput("HEAD", "status.internal:8080", "http://status.internal:8080/v1/nodes/node-1/ready"), db, db, mark, false, nil, nil, nil)
		if !rep.Background {
			t.Fatal("expected HEAD matching wildcard segment to be marked Background")
		}
		repExtra := &models.MockMismatchReport{}
		h.markBackground(ctx, repExtra, bgHTTPInput("HEAD", "status.internal:8080", "http://status.internal:8080/v1/nodes/a/b/ready"), db, db, mark, false, nil, nil, nil)
		if repExtra.Background {
			t.Fatal("extra path segment must not match single-segment wildcard")
		}
		repEmpty := &models.MockMismatchReport{}
		h.markBackground(ctx, repEmpty, bgHTTPInput("HEAD", "status.internal:8080", "http://status.internal:8080/v1/nodes//ready"), db, db, mark, false, nil, nil, nil)
		if repEmpty.Background {
			t.Fatal("empty path segment must not match wildcard")
		}
	})

	t.Run("unrecorded HTTP write is never excused even if present in rules", func(t *testing.T) {
		db := &httpBgRulesStore{rules: rules}
		rep := &models.MockMismatchReport{}
		h.markBackground(ctx, rep, bgHTTPInput("POST", "localhost:8200", "http://localhost:8200/v1/secret/touch"), db, db, mark, false, nil, nil, nil)
		if rep.Background {
			t.Fatal("unrecorded POST write must NOT be excused by declared background rule fallback")
		}
	})

	t.Run("all-placeholder catch-all HTTP paths are refused and never excuse misses", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			rulePath string
			reqURL   string
		}{
			{"single placeholder", "/{}", "http://localhost:8200/anything"},
			{"two placeholders", "/{}/{}", "http://localhost:8200/a/b"},
			{"named placeholders", "/{a}/{b}", "http://localhost:8200/{a}/{b}"},
			{"glob placeholder", "/{*}", "http://localhost:8200/{*}"},
			{"root only", "/", "http://localhost:8200/"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := &httpBgRulesStore{
					rules: []models.BackgroundHTTPRule{{
						ID:     "catch-all-http",
						Method: "GET",
						Host:   "localhost:8200",
						Path:   tc.rulePath,
					}},
				}
				rep := &models.MockMismatchReport{}
				h.markBackground(ctx, rep, bgHTTPInput("GET", "localhost:8200", tc.reqURL), db, db, mark, false, nil, nil, nil)
				if rep.Background {
					t.Fatalf("catch-all HTTP rule %q must NOT excuse miss for %s", tc.rulePath, tc.reqURL)
				}
			})
		}
	})

	t.Run("leading placeholder with literal segment still excuses", func(t *testing.T) {
		db := &httpBgRulesStore{
			rules: []models.BackgroundHTTPRule{{
				ID:     "tenant-health",
				Method: "GET",
				Host:   "localhost:8200",
				Path:   "/{}/health",
			}},
		}
		rep := &models.MockMismatchReport{}
		h.markBackground(ctx, rep, bgHTTPInput("GET", "localhost:8200", "http://localhost:8200/t1/health"), db, db, mark, false, nil, nil, nil)
		if !rep.Background {
			t.Fatal("expected rule with at least one literal segment (/{}/health) to excuse miss")
		}
	})

	t.Run("covering HTTP background recording blocks fallback", func(t *testing.T) {
		// Recorded GET /v1/sys/health has query ?standbyok=true; live call
		// has ?standbyok=false so strictDiff fails, and the covering recording
		// must block the declared-rule fallback.
		rec := bgHTTPMock("mock-health-1", models.Method("GET"), "http://localhost:8200/v1/sys/health?standbyok=true", "localhost:8200")
		db := &httpBgRulesStore{
			recs:  []models.BackgroundRecording{{Mock: rec}},
			rules: rules,
		}
		rep := &models.MockMismatchReport{}
		h.markBackground(ctx, rep, bgHTTPInput("GET", "localhost:8200", "http://localhost:8200/v1/sys/health?standbyok=false"), db, db, mark, false, nil, nil, nil)
		if rep.Background {
			t.Fatal("covering HTTP background recording must block declared-rule fallback")
		}
	})

	t.Run("unreadable or nil HTTP background recording fails closed and blocks fallback", func(t *testing.T) {
		for _, rec := range []models.BackgroundRecording{
			{Mock: &models.Mock{Name: "mock-bad", Kind: models.HTTP}},
			{Mock: nil},
		} {
			db := &httpBgRulesStore{
				recs:  []models.BackgroundRecording{rec},
				rules: rules,
			}
			rep := &models.MockMismatchReport{}
			h.markBackground(ctx, rep, bgHTTPInput("GET", "localhost:8200", "http://localhost:8200/v1/sys/health"), db, db, mark, false, nil, nil, nil)
			if rep.Background {
				t.Fatalf("unreadable/nil HTTP background recording (%+v) must fail closed and block fallback", rec)
			}
		}
	})
}
