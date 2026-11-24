package replay

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func perTestHTTPMock(name, method, rawURL string, hdr map[string]string) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":                     "Http",
				"type":                     models.HTTPClient,
				models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
			},
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    rawURL,
				Header: hdr,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 202,
			},
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
		},
	}
	return m
}

type staticPayloadSource struct {
	mock *models.Mock
}

func (s *staticPayloadSource) LoadPayload(_ context.Context, _ models.PayloadAt) (*models.Mock, error) {
	return s.mock, nil
}
func (s *staticPayloadSource) SetPayloadMutator(models.PayloadMutator) {}
func (s *staticPayloadSource) PayloadMutator() models.PayloadMutator   { return nil }

// An old recording made before elastic-apm-v2-events existed holds a per-test
// POST /intake/v2/events mock in the test's mapping. During replay, decodeHTTP
// short-circuits the APM call via the built-in skip rule without consuming the
// mock. The per-test assertion must therefore leave that mock out in both
// KEPLOY_PROVEN=1 and --assert-dependencies modes, while still asserting it
// when the user disables elastic-apm-v2-events by ID and still asserting an
// application's own JSON POST /intake/v2/events on another port.
func TestRunTestSet_OldRecordingElasticAPMMockNotUnconsumed(t *testing.T) {
	apmPort8200 := func() *models.Mock {
		return perTestHTTPMock(
			"mock-apm-1",
			"POST",
			"http://apm-server:8200/intake/v2/events",
			map[string]string{"Host": "apm-server:8200", "Content-Type": "application/x-ndjson"},
		)
	}
	apmNDJSON443 := func() *models.Mock {
		return perTestHTTPMock(
			"mock-apm-ndjson",
			"POST",
			"https://apm.elastic.cloud/intake/v2/events",
			map[string]string{"Host": "apm.elastic.cloud", "Content-Type": "application/x-ndjson; charset=utf-8"},
		)
	}
	appJSON8080 := func() *models.Mock {
		return perTestHTTPMock(
			"mock-app-intake",
			"POST",
			"http://app:8080/intake/v2/events",
			map[string]string{"Host": "app:8080", "Content-Type": "application/json"},
		)
	}
	ordersDep := func() *models.Mock {
		return perTestHTTPMock(
			"mock-orders-1",
			"GET",
			"http://orders:8080/v1/orders",
			map[string]string{"Host": "orders:8080"},
		)
	}
	ordersConsumed := []models.MockState{{
		Name:     "mock-orders-1",
		Kind:     models.HTTP,
		Type:     models.HTTPClient,
		Lifetime: models.LifetimePerTest,
	}}
	disableAPM := []models.PassThroughRule{{ID: "elastic-apm-v2-events", Mode: models.PassThroughOff}}

	cases := []struct {
		name           string
		proven         bool
		assertDeps     bool
		extraMock      func() *models.Mock
		leaveOnDisk    bool
		passThroughOff bool
		wantSetStatus  models.TestSetStatus
		wantTestStatus models.TestStatus
		wantUnconsumed []models.UnconsumedMock
	}{
		{
			name:           "proven on skips old APM port 8200 mock",
			proven:         true,
			extraMock:      apmPort8200,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven on skips old APM NDJSON 443 mock",
			proven:         true,
			extraMock:      apmNDJSON443,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven on skips old APM mock left on disk",
			proven:         true,
			extraMock:      apmPort8200,
			leaveOnDisk:    true,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven off with --assert-dependencies skips old APM port 8200 mock",
			proven:         false,
			assertDeps:     true,
			extraMock:      apmPort8200,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven off with --assert-dependencies skips old APM NDJSON 443 mock",
			proven:         false,
			assertDeps:     true,
			extraMock:      apmNDJSON443,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven off default passes with no missing rows for old APM mock",
			proven:         false,
			assertDeps:     false,
			extraMock:      apmPort8200,
			wantSetStatus:  models.TestSetStatusPassed,
			wantTestStatus: models.TestStatusPassed,
		},
		{
			name:           "proven on with elastic-apm-v2-events off still asserts recorded APM mock",
			proven:         true,
			extraMock:      apmPort8200,
			passThroughOff: true,
			wantSetStatus:  models.TestSetStatusFailed,
			wantTestStatus: models.TestStatusFailed,
			wantUnconsumed: []models.UnconsumedMock{{Name: "mock-apm-1", Kind: string(models.HTTP)}},
		},
		{
			name:           "proven off --assert-dependencies with elastic-apm-v2-events off still asserts recorded APM mock",
			proven:         false,
			assertDeps:     true,
			extraMock:      apmPort8200,
			passThroughOff: true,
			wantSetStatus:  models.TestSetStatusFailed,
			wantTestStatus: models.TestStatusFailed,
		},
		{
			name:           "proven on still asserts app JSON POST /intake/v2/events on 8080",
			proven:         true,
			extraMock:      appJSON8080,
			wantSetStatus:  models.TestSetStatusFailed,
			wantTestStatus: models.TestStatusFailed,
			wantUnconsumed: []models.UnconsumedMock{{Name: "mock-app-intake", Kind: string(models.HTTP)}},
		},
		{
			name:           "proven off --assert-dependencies still asserts app JSON POST /intake/v2/events on 8080",
			proven:         false,
			assertDeps:     true,
			extraMock:      appJSON8080,
			wantSetStatus:  models.TestSetStatusFailed,
			wantTestStatus: models.TestStatusFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.proven)
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.AssertDependencies = tc.assertDeps
			if tc.passThroughOff {
				h.replayer.config.Record.PassThroughHosts = disableAPM
			}
			extra := tc.extraMock()
			if tc.leaveOnDisk {
				copyMock := *extra
				extra.LeavePayloadOnDisk(&staticPayloadSource{mock: &copyMock}, models.PayloadAt{Name: extra.Name})
			}
			mappedTo(h, ordersDep(), extra)
			h.replayer.hookImpl = prHooks{consumed: ordersConsumed}

			status := h.run(t)
			got := resultOf(t, h, "test-1")
			if status != tc.wantSetStatus || got.Status != tc.wantTestStatus {
				t.Fatalf("set=%s test=%s (depsChecked=%v depResult=%+v unconsumed=%+v), want set=%s test=%s",
					status, got.Status, got.Result.DepsChecked, got.Result.DepResult, got.FailureInfo.UnconsumedMocks,
					tc.wantSetStatus, tc.wantTestStatus)
			}
			if tc.wantTestStatus == models.TestStatusPassed {
				if len(got.Result.DepResult) != 0 {
					t.Fatalf("expected no missing dep rows, got %+v", got.Result.DepResult)
				}
				if len(got.FailureInfo.UnconsumedMocks) != 0 {
					t.Fatalf("expected no unconsumed mocks, got %+v", got.FailureInfo.UnconsumedMocks)
				}
				if !got.Result.DepsChecked || got.Result.DepsConsumed != 1 {
					t.Fatalf("expected DepsChecked=true DepsConsumed=1 for ordersDep, got checked=%v consumed=%d",
						got.Result.DepsChecked, got.Result.DepsConsumed)
				}
			}
			if len(tc.wantUnconsumed) > 0 && !reflect.DeepEqual(got.FailureInfo.UnconsumedMocks, tc.wantUnconsumed) {
				t.Fatalf("unconsumed mocks = %+v, want %+v", got.FailureInfo.UnconsumedMocks, tc.wantUnconsumed)
			}
		})
	}
}

// A streaming test with an old recorded per-test APM mock in its mapping
// passes under KEPLOY_PROVEN=1 when the built-in skip rule is active, and
// fails as unconsumed when elastic-apm-v2-events is turned off by ID.
func TestRunTestSet_StreamedOldRecordingElasticAPMMockNotUnconsumed(t *testing.T) {
	const expected = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`
	for _, disable := range []bool{false, true} {
		name := "builtin active passes"
		if disable {
			name = "disabled by ID fails as unconsumed"
		}
		t.Run(name, func(t *testing.T) {
			setSwitch(t, true)
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, expected+"\n")
				})
			})
			conf := &tmplConf{}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
					StatusCode: 200, StatusMessage: "OK",
					Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
					Body:       expected,
					StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
				}),
			}
			apmMock := perTestHTTPMock(
				"mock-apm-stream",
				"POST",
				"http://apm-server:8200/intake/v2/events",
				map[string]string{"Host": "apm-server:8200", "Content-Type": "application/x-ndjson"},
			)
			h := newTmplHarness(t, conf, cases, nil)
			h.r.mockDB = &prMockDB{filtered: []*models.Mock{apmMock}}
			if disable {
				h.r.config.Record.PassThroughHosts = []models.PassThroughRule{{ID: "elastic-apm-v2-events", Mode: models.PassThroughOff}}
			}
			h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{
				"test-1": {{Name: "mock-apm-stream", Kind: string(models.HTTP)}},
			}}
			h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
			core, _ := observer.New(zap.DebugLevel)
			h.r.logger = zap.New(core)

			st := h.run(t)
			got := h.result(t, "test-1")
			if !disable {
				if st != models.TestSetStatusPassed || got.Status != models.TestStatusPassed {
					t.Fatalf("set=%s test=%s unconsumed=%+v; want PASSED", st, got.Status, got.FailureInfo.UnconsumedMocks)
				}
				return
			}
			if st != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
				t.Fatalf("disabled by ID: set=%s test=%s; want FAILED", st, got.Status)
			}
			want := []models.UnconsumedMock{{Name: "mock-apm-stream", Kind: string(models.HTTP)}}
			if !reflect.DeepEqual(got.FailureInfo.UnconsumedMocks, want) {
				t.Fatalf("unconsumed mocks = %+v, want %+v", got.FailureInfo.UnconsumedMocks, want)
			}
		})
	}
}

// A background Elastic APM mock recorded in the worker scope of an old
// recording is not required by the exact plan when elastic-apm-v2-events is
// active, and IS required when disabled by ID.
func TestNewExactPlanWithExcluded_WorkerScopeElasticAPM(t *testing.T) {
	setSwitch(t, true)
	apmWorker := perTestHTTPMock(
		"mock-apm-worker",
		"POST",
		"http://apm-server:8200/intake/v2/events",
		map[string]string{"Host": "apm-server:8200", "Content-Type": "application/x-ndjson"},
	)
	workerEntries := []models.MockEntry{{Name: "mock-apm-worker", Kind: string(models.HTTP)}}

	excluded := map[string]bool{}
	if apmWorker.AnsweredByPassThrough(nil, nil) {
		excluded[apmWorker.Name] = true
	}
	planActive := newExactPlanWithExcluded(nil, workerEntries, excluded, []*models.Mock{apmWorker})
	if len(planActive.worker) != 0 {
		t.Fatalf("active builtin: worker scope = %+v, want empty", planActive.worker)
	}

	off := []models.PassThroughRule{{ID: "elastic-apm-v2-events", Mode: models.PassThroughOff}}
	excludedOff := map[string]bool{}
	if apmWorker.AnsweredByPassThrough(nil, off) {
		excludedOff[apmWorker.Name] = true
	}
	planOff := newExactPlanWithExcluded(nil, workerEntries, excludedOff, []*models.Mock{apmWorker})
	if len(planOff.worker) != 1 || planOff.worker[0].Name != "mock-apm-worker" {
		t.Fatalf("disabled by ID: worker scope = %+v, want [mock-apm-worker]", planOff.worker)
	}
}
