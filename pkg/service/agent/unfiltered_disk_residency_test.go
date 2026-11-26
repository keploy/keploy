package agent

import (
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// streamMocksViaPipe encodes filtered+unfiltered mocks one-by-one over an
// io.Pipe so the test harness itself never buffers the full serialized stream
// in RAM while StoreMocksStream is running.
func streamMocksViaPipe(t *testing.T, a *Agent, filteredCount, unfilteredCount int, genFiltered, genUnfiltered func(i int) *models.Mock) {
	t.Helper()
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		enc := gob.NewEncoder(pw)
		if err := enc.Encode(models.MockStreamHeader{
			FilteredCount:   filteredCount,
			UnfilteredCount: unfilteredCount,
		}); err != nil {
			_ = pw.CloseWithError(err)
			errCh <- err
			return
		}
		for i := 0; i < filteredCount; i++ {
			if err := enc.Encode(genFiltered(i)); err != nil {
				_ = pw.CloseWithError(err)
				errCh <- err
				return
			}
		}
		for i := 0; i < unfilteredCount; i++ {
			if err := enc.Encode(genUnfiltered(i)); err != nil {
				_ = pw.CloseWithError(err)
				errCh <- err
				return
			}
		}
		errCh <- pw.Close()
	}()

	dec := gob.NewDecoder(pr)
	var header models.MockStreamHeader
	if err := dec.Decode(&header); err != nil {
		t.Fatalf("decode stream header: %v", err)
	}
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatalf("StoreMocksStream: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stream writer: %v", err)
	}
}

func readMemAfterGC() runtime.MemStats {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms
}

// TestStoreMocksStreamAndUpdateMockParams_UnfilteredMemoryCeiling proves that:
//  1. Streaming 20,000 unfiltered/session mocks via StoreMocksStream does NOT
//     keep all decoded *models.Mock objects resident in storage.unfiltered
//     (they spill via DiskMocks with bounded resident memory).
//  2. Running initial staging + 10 per-test UpdateMockParams calls does NOT
//     DeepCopy the unfiltered pool or rebuild the unfiltered trees on every
//     test, keeping both resident HeapAlloc and per-test allocations bounded.
func TestStoreMocksStreamAndUpdateMockParams_UnfilteredMemoryCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 20k unfiltered memory ceiling test in -short mode")
	}

	const (
		unfilteredCount = 20_000
		numTests        = 10
		filteredPerTest = 10
		filteredCount   = numTests * filteredPerTest
		httpBodyBytes   = 12 << 10 // 12 KiB -> ~240 MiB raw HTTP bodies across 20k mocks
	)

	cases := []struct {
		name             string
		makeUnfiltered   func(base time.Time, i int) *models.Mock
		maxPostStoreHeap uint64
		maxPostStageHeap uint64
		maxPeakHeap      uint64
		maxAllocPerTest  uint64
	}{
		{
			name: "http-session-20k-12KiB-bodies",
			makeUnfiltered: func(base time.Time, i int) *models.Mock {
				ts := base.Add(-time.Minute).Add(time.Duration(i) * time.Microsecond)
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    fmt.Sprintf("cfg-http-%05d", i),
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata: map[string]string{
							"type":     "config",
							"destAddr": "10.0.0.10:8080",
						},
						ReqTimestampMock: ts,
						ResTimestampMock: ts.Add(time.Millisecond),
						HTTPReq: &models.HTTPReq{
							Method: models.Method("GET"),
							URL:    fmt.Sprintf("http://cfg-svc:8080/api/v1/config/%d", i),
							Header: map[string]string{"Host": "cfg-svc:8080", "Accept": "application/json"},
						},
						HTTPResp: &models.HTTPResp{
							StatusCode: 200,
							Header:     map[string]string{"Content-Type": "application/json"},
							Body:       fmt.Sprintf(`{"id":%d,"pad":"%s"}`, i, strings.Repeat("a", httpBodyBytes)),
						},
					},
				}
			},
			maxPostStoreHeap: 64 << 20,  // 64 MiB (on main: ~290+ MiB)
			maxPostStageHeap: 120 << 20, // 120 MiB (on main: ~580+ MiB)
			maxPeakHeap:      120 << 20, // 120 MiB (on main: ~600-850+ MiB)
			maxAllocPerTest:  4 << 20,   // 4 MiB/test (on main: ~290+ MiB/test)
		},
		{
			name: "small-session-20k-no-body-spill",
			makeUnfiltered: func(base time.Time, i int) *models.Mock {
				ts := base.Add(-time.Minute).Add(time.Duration(i) * time.Microsecond)
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    fmt.Sprintf("cfg-small-%05d", i),
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata: map[string]string{
							"type":     "config",
							"destAddr": "10.0.0.11:8080",
						},
						ReqTimestampMock: ts,
						ResTimestampMock: ts.Add(time.Millisecond),
						HTTPReq: &models.HTTPReq{
							Method: models.Method("GET"),
							URL:    fmt.Sprintf("http://cfg-svc:8080/ping/%d", i),
							Header: map[string]string{"Host": "cfg-svc:8080", "X-Idx": fmt.Sprintf("%d", i)},
						},
						HTTPResp: &models.HTTPResp{
							StatusCode: 200,
							Header:     map[string]string{"Content-Type": "text/plain"},
							Body:       fmt.Sprintf("ok-%d", i),
						},
					},
				}
			},
			maxPostStoreHeap: 48 << 20,  // 48 MiB (on main: ~45-65 MiB)
			maxPostStageHeap: 110 << 20, // 110 MiB (on main: ~130+ MiB)
			maxPeakHeap:      110 << 20, // 110 MiB (on main: ~150+ MiB)
			maxAllocPerTest:  2 << 20,   // 2 MiB/test (on main: ~45-60 MiB/test)
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			prevHooks := ActiveHooks
			ch := &captureHook{}
			ActiveHooks = ch
			t.Cleanup(func() { ActiveHooks = prevHooks })

			cfg := &config.Config{
				Agent: config.Agent{Mode: models.MODE_TEST},
				Test:  config.Test{StrictMockWindow: true},
			}
			realProxy := proxyPkg.New(zap.NewNop(), nil, cfg)
			if err := realProxy.Record(context.Background(), make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
				t.Fatalf("proxy.Record init: %v", err)
			}
			a := &Agent{
				logger: zap.NewNop(),
				Proxy:  realProxy,
				config: cfg,
			}
			t.Cleanup(func() {
				a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
			})

			base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
			baseline := readMemAfterGC()

			streamMocksViaPipe(t, a, filteredCount, unfilteredCount,
				func(i int) *models.Mock {
					testIdx := i / filteredPerTest
					offset := i % filteredPerTest
					ts := base.Add(time.Duration(testIdx)*10*time.Second + time.Duration(offset)*10*time.Millisecond)
					return &models.Mock{
						Version: "api.keploy.io/v1beta1",
						Name:    fmt.Sprintf("pt-%02d-%02d", testIdx, offset),
						Kind:    models.Mongo,
						Spec: models.MockSpec{
							Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
							ReqTimestampMock: ts,
							ResTimestampMock: ts.Add(time.Millisecond),
						},
					}
				},
				func(i int) *models.Mock {
					return tc.makeUnfiltered(base, i)
				},
			)

			st := loadStorage(t, a)
			postStore := readMemAfterGC()
			postStoreDelta := uint64(0)
			if postStore.HeapAlloc > baseline.HeapAlloc {
				postStoreDelta = postStore.HeapAlloc - baseline.HeapAlloc
			}

			// Call 1: Initial staging (BaseTime .. +2h)
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:              models.BaseTime,
				BeforeTime:             base.Add(2 * time.Hour),
				FirstRecordedTestStart: base,
				StrictMockWindow:       true,
			}); err != nil {
				t.Fatalf("initial staging UpdateMockParams: %v", err)
			}
			postStage := readMemAfterGC()
			postStageDelta := uint64(0)
			if postStage.HeapAlloc > baseline.HeapAlloc {
				postStageDelta = postStage.HeapAlloc - baseline.HeapAlloc
			}

			// Test 1 establishes the first real test window (drops initial staging's startup tree).
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:        base,
				BeforeTime:       base.Add(5 * time.Second),
				StrictMockWindow: true,
			}); err != nil {
				t.Fatalf("test 0 UpdateMockParams: %v", err)
			}

			beforeLoop := readMemAfterGC()
			peakHeapDelta := postStageDelta
			for testIdx := 1; testIdx < numTests; testIdx++ {
				start := base.Add(time.Duration(testIdx) * 10 * time.Second)
				end := start.Add(5 * time.Second)
				if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
					AfterTime:        start,
					BeforeTime:       end,
					StrictMockWindow: true,
				}); err != nil {
					t.Fatalf("test %d UpdateMockParams: %v", testIdx, err)
				}
				cur := readMemAfterGC()
				if cur.HeapAlloc > baseline.HeapAlloc {
					if d := cur.HeapAlloc - baseline.HeapAlloc; d > peakHeapDelta {
						peakHeapDelta = d
					}
				}
			}
			afterLoop := readMemAfterGC()
			allocPerTest := (afterLoop.TotalAlloc - beforeLoop.TotalAlloc) / uint64(numTests-1)

			t.Logf("KEPLOY_PROVEN=%v case=%s: residentUnfiltered=%d postStoreHeapDelta=%.1fMiB postStageHeapDelta=%.1fMiB peakHeapDelta=%.1fMiB allocPerTest=%.2fMiB",
				provenswitch.On(), tc.name, len(st.unfiltered),
				float64(postStoreDelta)/(1<<20),
				float64(postStageDelta)/(1<<20),
				float64(peakHeapDelta)/(1<<20),
				float64(allocPerTest)/(1<<20))

			if len(st.unfiltered) != 0 {
				t.Errorf("storage.unfiltered kept %d decoded mocks resident in RAM; want 0 (must spill via DiskMocks)", len(st.unfiltered))
			}
			if st.unfilteredDisk == nil || st.unfilteredDisk.Len() != unfilteredCount {
				t.Errorf("unfilteredDisk count = %v, want %d", st.unfilteredDisk, unfilteredCount)
			}
			a.scopeMu.Lock()
			gotLoaded := a.loadedMocks
			a.scopeMu.Unlock()
			if gotLoaded != filteredCount+unfilteredCount {
				t.Errorf("loadedMocks = %d, want %d", gotLoaded, filteredCount+unfilteredCount)
			}
			wantAnchor := base.Add(-time.Minute)
			if !ch.called || !ch.anchor.Equal(wantAnchor) {
				t.Errorf("freeze anchor = %v (called=%v), want %v", ch.anchor, ch.called, wantAnchor)
			}
			if tc.name == "http-session-20k-12KiB-bodies" {
				if st.unfilteredDisk != nil && st.unfilteredDisk.SpilledResponses() != unfilteredCount {
					t.Errorf("unfilteredDisk.SpilledResponses = %d, want %d", st.unfilteredDisk.SpilledResponses(), unfilteredCount)
				}
				if st.unfilteredDisk != nil && st.unfilteredDisk.DiskBytes() == 0 {
					t.Errorf("unfilteredDisk.DiskBytes = 0, want > 0 (must spill beyond resident budget)")
				}
				// Verify lazy response hydration on a sampled mock from unfilteredDisk.
				sample, err := st.unfilteredDisk.LoadByNames([]string{"cfg-http-00042"})
				if err != nil || len(sample) != 1 {
					t.Fatalf("LoadByNames(cfg-http-00042) = %d, %v", len(sample), err)
				}
				if !sample[0].HasSpilledResponse() || sample[0].Spec.HTTPResp != nil {
					t.Errorf("expected cfg-http-00042 response to stay spilled before hydration")
				}
				hydrated := sample[0].ShallowCopy()
				if err := hydrated.HydrateResponse(); err != nil {
					t.Fatalf("HydrateResponse: %v", err)
				}
				if hydrated.Spec.HTTPResp == nil || !strings.HasPrefix(hydrated.Spec.HTTPResp.Body, `{"id":42,"pad":"aaaa`) || len(hydrated.Spec.HTTPResp.Body) < httpBodyBytes {
					t.Errorf("hydrated response body corrupted: resp=%v", hydrated.Spec.HTTPResp)
				}
			}
			if postStoreDelta > tc.maxPostStoreHeap {
				t.Errorf("post-StoreMocksStream HeapAlloc delta = %.1f MiB exceeds ceiling %.1f MiB",
					float64(postStoreDelta)/(1<<20), float64(tc.maxPostStoreHeap)/(1<<20))
			}
			if postStageDelta > tc.maxPostStageHeap {
				t.Errorf("post-initial-staging HeapAlloc delta = %.1f MiB exceeds ceiling %.1f MiB",
					float64(postStageDelta)/(1<<20), float64(tc.maxPostStageHeap)/(1<<20))
			}
			if peakHeapDelta > tc.maxPeakHeap {
				t.Errorf("peak per-test HeapAlloc delta = %.1f MiB exceeds ceiling %.1f MiB",
					float64(peakHeapDelta)/(1<<20), float64(tc.maxPeakHeap)/(1<<20))
			}
			if allocPerTest > tc.maxAllocPerTest {
				t.Errorf("per-test UpdateMockParams TotalAlloc = %.2f MiB/test exceeds ceiling %.2f MiB/test (unfiltered DeepCopy/rebuild leak)",
					float64(allocPerTest)/(1<<20), float64(tc.maxAllocPerTest)/(1<<20))
			}
		})
	}
}

// retainingMgrProxy wraps a real *proxyPkg.MockManager and exposes
// WindowedProxy + UnfilteredRetainingWindowedProxy + FirstWindowStartReader +
// ConsumedStateReader + BackgroundRecordingsSeeder + RecordedDestinationsSeeder +
// StatementCatalogSeeder so tests can inspect the live MockManager trees and
// retention counts directly.
type retainingMgrProxy struct {
	coreAgent.Proxy
	mm                  *proxyPkg.MockManager
	fullStageCalls      int
	retainCalls         int
	retainDeclinedCalls int
	beforeRetainHook    func()
	bgRecs              []models.BackgroundRecording
	seededDests         []*models.Mock
	seededStmts         []*models.Mock
}

func newRetainingMgrProxy() *retainingMgrProxy {
	return &retainingMgrProxy{mm: proxyPkg.NewMockManager(nil, nil, zap.NewNop())}
}

func (p *retainingMgrProxy) SetMocks(_ context.Context, f, u []*models.Mock) error {
	p.mm.SetFilteredMocks(f)
	p.mm.SetUnFilteredMocks(u)
	return nil
}

func (p *retainingMgrProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, s, e time.Time) error {
	p.fullStageCalls++
	p.mm.SetMocksWithWindow(f, u, s, e)
	return nil
}

func (p *retainingMgrProxy) SetFilteredMocksWithWindow(_ context.Context, f []*models.Mock, s, e time.Time) error {
	if h := p.beforeRetainHook; h != nil {
		p.beforeRetainHook = nil
		h()
	}
	if !p.mm.SetFilteredMocksWithWindow(f, s, e) {
		p.retainDeclinedCalls++
		return coreAgent.ErrUnfilteredNotRetained
	}
	p.retainCalls++
	return nil
}

func (p *retainingMgrProxy) OpenTestFilteredWithWindow(_ context.Context, f []*models.Mock, s, e time.Time, given []models.VariableBinding) error {
	if h := p.beforeRetainHook; h != nil {
		p.beforeRetainHook = nil
		h()
	}
	if !p.mm.OpenTestFilteredWithWindow(f, s, e, given) {
		p.retainDeclinedCalls++
		return coreAgent.ErrUnfilteredNotRetained
	}
	p.retainCalls++
	return nil
}

func (p *retainingMgrProxy) CanRetainUnfiltered() bool {
	return p.mm.CanRetainUnfiltered()
}

func (p *retainingMgrProxy) FirstTestWindowStart() time.Time {
	return p.mm.FirstTestWindowStart()
}

func (p *retainingMgrProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}

func (p *retainingMgrProxy) SeedBackgroundRecordings(recs []models.BackgroundRecording) {
	p.bgRecs = recs
}

func (p *retainingMgrProxy) SeedRecordedDestinations(mocks []*models.Mock) {
	p.seededDests = mocks
}

func (p *retainingMgrProxy) SeedStatementCatalog(mocks []*models.Mock) {
	p.seededStmts = mocks
}

// TestUpdateMockParams_UnfilteredRetentionSemanticsAndInvalidation exercises
// retention reuse, DeleteUnFilteredMock invalidation, derived ReplacedBy
// omission/restoration, metadata/catalog/background/byName reads from
// unfilteredDisk, lax-mode large-pool spilling, set-boundary cleanup, and
// concurrent access under -race.
func TestUpdateMockParams_UnfilteredRetentionSemanticsAndInvalidation(t *testing.T) {
	ctx := context.Background()
	prevHooks := ActiveHooks
	ActiveHooks = &captureHook{}
	t.Cleanup(func() { ActiveHooks = prevHooks })

	base := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	t.Run("retention-invalidation-replacement-and-disk-readers", func(t *testing.T) {
		p := newRetainingMgrProxy()
		t.Cleanup(p.mm.Close)
		cfg := &config.Config{
			Agent: config.Agent{Mode: models.MODE_TEST},
			Test:  config.Test{StrictMockWindow: true},
		}
		a := &Agent{logger: zap.NewNop(), Proxy: p, config: cfg}
		t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

		u1 := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "unf-1",
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config", "destAddr": "10.1.2.3:3306"},
				ReqTimestampMock: base.Add(-20 * time.Second),
				ResTimestampMock: base.Add(-20*time.Second + time.Millisecond),
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/u1"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "u1"},
			},
		}
		u2 := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "unf-2",
			Kind:    models.PostgresV3,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "config", "destAddr": "10.1.2.4:5432"},
				PostgresV3: &models.PostgresV3Spec{
					Type:  models.PostgresV3TypeQuery,
					Query: &models.PostgresV3QuerySpec{SQLAstHash: "sha256:unf-stmt", InvocationID: "unf-2"},
				},
				ReqTimestampMock: base.Add(-15 * time.Second),
				ResTimestampMock: base.Add(-15*time.Second + time.Millisecond),
			},
		}
		pt1 := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "pt-1",
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
				ReqTimestampMock: base.Add(1 * time.Second),
				ResTimestampMock: base.Add(1*time.Second + time.Millisecond),
			},
		}
		// Derived mock in Test 2 window that replaces unf-1.
		pt2Derived := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "pt-2-derived",
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
				},
				Authored:         &models.Authored{How: models.AuthoredDerived, Reason: "override", Source: "pkg::Test2/unf-1"},
				ReqTimestampMock: base.Add(11 * time.Second),
				ResTimestampMock: base.Add(11*time.Second + time.Millisecond),
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/u1"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "u1-derived"},
			},
		}
		pt3 := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    "pt-3",
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
				ReqTimestampMock: base.Add(21 * time.Second),
				ResTimestampMock: base.Add(21*time.Second + time.Millisecond),
			},
		}

		header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, []*models.Mock{pt1, pt2Derived, pt3}, []*models.Mock{u1, u2}))
		if err := a.StoreMocksStream(ctx, header, dec); err != nil {
			t.Fatalf("StoreMocksStream: %v", err)
		}
		st1 := loadStorage(t, a)
		if st1.unfilteredDisk == nil || st1.unfilteredDisk.Len() != 2 || len(st1.unfiltered) != 0 {
			t.Fatalf("expected 2 mocks in unfilteredDisk and 0 resident, got disk=%v resident=%d", st1.unfilteredDisk, len(st1.unfiltered))
		}
		// Seeded destinations & statement catalog must include unfilteredDisk mocks.
		hasUnfDest := false
		for _, d := range p.seededDests {
			if d != nil && d.Name == "unf-1" {
				hasUnfDest = true
				break
			}
		}
		if !hasUnfDest {
			t.Fatalf("expected destinations from unfilteredDisk to include unf-1, got %v", sigs(p.seededDests))
		}
		if len(p.seededStmts) != 1 || p.seededStmts[0].Name != "unf-2" {
			t.Fatalf("expected statement catalog from unfilteredDisk to contain unf-2, got %v", p.seededStmts)
		}
		// mocksByName must resolve names stored in unfilteredDisk.
		byName, err := a.mocksByName([]string{"pt-1", "unf-1", "unf-2"})
		if err != nil || len(byName) != 3 {
			t.Fatalf("mocksByName = %d, %v; want 3", len(byName), err)
		}

		// Initial staging (BaseTime) — full stage #1, also seeds background from unfilteredDisk.
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:              models.BaseTime,
			BeforeTime:             base.Add(time.Hour),
			FirstRecordedTestStart: base,
			BackgroundMocks:        []string{"unf-1"},
			StrictMockWindow:       true,
		}); err != nil {
			t.Fatalf("initial staging: %v", err)
		}
		if p.fullStageCalls != 1 || p.retainCalls != 0 {
			t.Fatalf("after initial staging: full=%d retain=%d, want 1, 0", p.fullStageCalls, p.retainCalls)
		}
		if len(p.bgRecs) != 1 || p.bgRecs[0].Mock.Name != "unf-1" {
			t.Fatalf("expected background recording unf-1 from unfilteredDisk, got %v", p.bgRecs)
		}

		// Test 1: retains unfiltered tier without reload!
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base,
			BeforeTime:       base.Add(5 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 1: %v", err)
		}
		if p.fullStageCalls != 1 || p.retainCalls != 1 {
			t.Fatalf("after test 1: full=%d retain=%d, want 1, 1", p.fullStageCalls, p.retainCalls)
		}
		gotUnf, _ := p.mm.GetUnFilteredMocks()
		if len(gotUnf) != 2 {
			t.Fatalf("test 1 unfiltered count = %d, want 2", len(gotUnf))
		}

		// Test 2: derived mock replaces unf-1 -> must fall back to full stage and omit unf-1.
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base.Add(10 * time.Second),
			BeforeTime:       base.Add(15 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 2: %v", err)
		}
		if p.fullStageCalls != 2 || p.retainCalls != 1 {
			t.Fatalf("after test 2 (with replacement): full=%d retain=%d, want 2, 1", p.fullStageCalls, p.retainCalls)
		}
		gotUnf, _ = p.mm.GetUnFilteredMocks()
		if len(gotUnf) != 1 || gotUnf[0].Name != "unf-2" {
			t.Fatalf("test 2 unfiltered = %v, want only [unf-2] (unf-1 replaced)", sigs(gotUnf))
		}

		// Test 3: no replacement -> reloads full unfiltered pool restoring unf-1.
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base.Add(20 * time.Second),
			BeforeTime:       base.Add(25 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 3: %v", err)
		}
		if p.fullStageCalls != 3 || p.retainCalls != 1 {
			t.Fatalf("after test 3 (restoring): full=%d retain=%d, want 3, 1", p.fullStageCalls, p.retainCalls)
		}
		gotUnf, _ = p.mm.GetUnFilteredMocks()
		if len(gotUnf) != 2 {
			t.Fatalf("test 3 unfiltered count = %d, want 2 (unf-1 restored)", len(gotUnf))
		}

		// DeleteUnFilteredMock invalidates retention so the next test reloads & restores.
		if !p.mm.DeleteUnFilteredMock(*gotUnf[0]) {
			t.Fatalf("DeleteUnFilteredMock failed")
		}
		if p.CanRetainUnfiltered() {
			t.Fatalf("CanRetainUnfiltered() must be false after DeleteUnFilteredMock")
		}
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base.Add(20 * time.Second),
			BeforeTime:       base.Add(25 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 3 retry after delete: %v", err)
		}
		if p.fullStageCalls != 4 {
			t.Fatalf("after delete invalidation: full=%d, want 4", p.fullStageCalls)
		}
		gotUnf, _ = p.mm.GetUnFilteredMocks()
		if len(gotUnf) != 2 {
			t.Fatalf("after reload post-delete: unfiltered count = %d, want 2", len(gotUnf))
		}
		// Next test retains again!
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base.Add(20 * time.Second),
			BeforeTime:       base.Add(25 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 4 retain: %v", err)
		}
		if p.retainCalls != 2 {
			t.Fatalf("after test 4: retain=%d, want 2", p.retainCalls)
		}

		// TOCTOU race: CanRetainUnfiltered() returns true, then a concurrent
		// DeleteUnFilteredMock fires before SetFilteredMocksWithWindow enters
		// MockManager. SetFilteredMocksWithWindow declines cleanly with
		// ErrUnfilteredNotRetained and Agent falls back to full staging,
		// restoring all unfiltered mocks instead of wiping m.unfiltered.
		p.beforeRetainHook = func() {
			cur, _ := p.mm.GetUnFilteredMocks()
			if len(cur) > 0 {
				p.mm.DeleteUnFilteredMock(*cur[0])
			}
		}
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:        base.Add(20 * time.Second),
			BeforeTime:       base.Add(25 * time.Second),
			StrictMockWindow: true,
		}); err != nil {
			t.Fatalf("test 5 TOCTOU fallback: %v", err)
		}
		if p.retainDeclinedCalls != 1 || p.fullStageCalls != 5 {
			t.Fatalf("TOCTOU fallback counts: declined=%d full=%d, want 1, 5", p.retainDeclinedCalls, p.fullStageCalls)
		}
		if gotUnf, _ = p.mm.GetUnFilteredMocks(); len(gotUnf) != 2 {
			t.Fatalf("after TOCTOU fallback: unfiltered count = %d, want 2 (full pool restored)", len(gotUnf))
		}

		// Publishing set 2 must close set 1's unfilteredDisk and invalidate retention.
		oldUnfDisk := st1.unfilteredDisk
		header2, dec2 := readHeaderAndDecoder(t, encodeStreamBody(t, nil, []*models.Mock{u2}))
		if err := a.StoreMocksStream(ctx, header2, dec2); err != nil {
			t.Fatalf("set 2 StoreMocksStream: %v", err)
		}
		if _, err := oldUnfDisk.LoadAll(); err == nil {
			t.Fatalf("expected superseded unfilteredDisk to be closed")
		}
	})

	t.Run("lax-mode-large-unfiltered-pool-spills-to-disk", func(t *testing.T) {
		p := newRetainingMgrProxy()
		t.Cleanup(p.mm.Close)
		a := strictAgent(false)
		a.Proxy = p
		t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

		const n = unfilteredResidentLimit + 100
		streamMocksViaPipe(t, a, 0, n,
			func(int) *models.Mock { return nil },
			func(i int) *models.Mock {
				ts := base.Add(time.Duration(i) * time.Millisecond)
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    fmt.Sprintf("lax-cfg-%04d", i),
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata:         map[string]string{"type": "config"},
						ReqTimestampMock: ts,
						ResTimestampMock: ts.Add(time.Millisecond),
					},
				}
			},
		)
		st := loadStorage(t, a)
		if st.unfilteredDisk == nil || st.unfilteredDisk.Len() != n {
			t.Fatalf("lax mode >limit: expected %d mocks in unfilteredDisk, got %v", n, st.unfilteredDisk)
		}
		if len(st.unfiltered) != 0 {
			t.Fatalf("lax mode >limit: expected 0 resident unfiltered mocks, got %d", len(st.unfiltered))
		}
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  base,
			BeforeTime: base.Add(time.Hour),
		}); err != nil {
			t.Fatalf("lax UpdateMockParams: %v", err)
		}
		got, _ := p.mm.GetUnFilteredMocks()
		if len(got) != n {
			t.Fatalf("lax staged unfiltered count = %d, want %d", len(got), n)
		}
	})

	t.Run("concurrent-update-and-reads-race-free", func(t *testing.T) {
		p := newRetainingMgrProxy()
		t.Cleanup(p.mm.Close)
		cfg := &config.Config{
			Agent: config.Agent{Mode: models.MODE_TEST},
			Test:  config.Test{StrictMockWindow: true},
		}
		a := &Agent{logger: zap.NewNop(), Proxy: p, config: cfg}
		t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

		streamMocksViaPipe(t, a, 20, 200,
			func(i int) *models.Mock {
				ts := base.Add(time.Duration(i) * time.Second)
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    fmt.Sprintf("race-pt-%d", i),
					Kind:    models.Mongo,
					Spec: models.MockSpec{
						Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
						ReqTimestampMock: ts,
						ResTimestampMock: ts.Add(time.Millisecond),
					},
				}
			},
			func(i int) *models.Mock {
				ts := base.Add(-time.Minute).Add(time.Duration(i) * time.Millisecond)
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    fmt.Sprintf("race-u-%d", i),
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata:         map[string]string{"type": "config"},
						ReqTimestampMock: ts,
						ResTimestampMock: ts.Add(time.Millisecond),
						HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: fmt.Sprintf("http://svc/%d", i)},
						HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
					},
				}
			},
		)

		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:              models.BaseTime,
			BeforeTime:             base.Add(time.Hour),
			FirstRecordedTestStart: base,
			StrictMockWindow:       true,
		}); err != nil {
			t.Fatalf("race initial stage: %v", err)
		}

		stop := make(chan struct{})
		done := make(chan struct{}, 2)
		for g := 0; g < 2; g++ {
			go func() {
				defer func() { done <- struct{}{} }()
				for {
					select {
					case <-stop:
						return
					default:
						u, _ := p.mm.GetUnFilteredMocks()
						_, _ = p.mm.GetSessionScopedMocks()
						_, _ = p.mm.GetStartupMocks()
						_, _ = p.mm.GetStatelessMocks(models.HTTP, "race-u-0")
						if len(u) > 0 {
							cp := u[0].ShallowCopy()
							p.mm.UpdateUnFilteredMock(u[0], cp)
						}
					}
				}
			}()
		}
		for i := 0; i < 10; i++ {
			s := base.Add(time.Duration(i*2) * time.Second)
			if err := a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime:        s,
				BeforeTime:       s.Add(2 * time.Second),
				StrictMockWindow: true,
			}); err != nil {
				close(stop)
				t.Fatalf("race UpdateMockParams %d: %v", i, err)
			}
		}
		close(stop)
		<-done
		<-done
	})

	t.Run("exact-proven-disk-readers-and-recorded-dests-include-unfilteredDisk", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			strict bool
			unfN   int
		}{
			{name: "strict-both-disks", strict: true, unfN: 2},
			{name: "lax-unfiltered-disk-only", strict: false, unfN: unfilteredResidentLimit + 5},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := newRetainingMgrProxy()
				t.Cleanup(p.mm.Close)
				a := strictAgent(tc.strict)
				a.Proxy = p
				t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

				streamMocksViaPipe(t, a, 1, tc.unfN,
					func(int) *models.Mock {
						return &models.Mock{
							Version: "api.keploy.io/v1beta1",
							Name:    "pt-disk-1",
							Kind:    models.HTTP,
							Spec: models.MockSpec{
								Metadata: map[string]string{
									models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
									"destAddr":                 "127.0.0.1:19001",
								},
								ReqTimestampMock: base.Add(time.Second),
								ResTimestampMock: base.Add(time.Second + time.Millisecond),
								HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://127.0.0.1:19001/pt"},
								HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "pt"},
							},
						}
					},
					func(i int) *models.Mock {
						ts := base.Add(-time.Minute).Add(time.Duration(i) * time.Millisecond)
						addr := "10.9.9.9:8080"
						if i == 0 {
							addr = "[::1]:19002"
						}
						return &models.Mock{
							Version: "api.keploy.io/v1beta1",
							Name:    fmt.Sprintf("unf-disk-%d", i),
							Kind:    models.HTTP,
							Spec: models.MockSpec{
								Metadata: map[string]string{
									"type":     "config",
									"destAddr": addr,
								},
								ReqTimestampMock: ts,
								ResTimestampMock: ts.Add(time.Millisecond),
								HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: fmt.Sprintf("http://svc/u/%d", i)},
								HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
							},
						}
					},
				)

				// 1. diskRecordingTimes must include unf-disk-0 from unfilteredDisk.
				rt := a.diskRecordingTimes()
				if rt == nil {
					t.Fatal("diskRecordingTimes() = nil, want non-nil")
				}
				sawUnfTime := false
				if err := rt(func(name string, _ time.Time, _ bool, _ string) bool {
					if name == "unf-disk-0" {
						sawUnfTime = true
						return false
					}
					return true
				}); err != nil {
					t.Fatalf("diskRecordingTimes: %v", err)
				}
				if !sawUnfTime {
					t.Fatal("diskRecordingTimes did not visit unf-disk-0 in unfilteredDisk")
				}

				// 2. diskRecordedBefore must include unf-disk-0 from unfilteredDisk.
				rb := a.diskRecordedBefore()
				if rb == nil {
					t.Fatal("diskRecordedBefore() = nil, want non-nil")
				}
				sawUnfBefore := false
				if err := rb(base.Add(time.Hour), func(m *models.Mock) bool {
					if m != nil && m.Name == "unf-disk-0" {
						sawUnfBefore = true
						return false
					}
					return true
				}); err != nil {
					t.Fatalf("diskRecordedBefore: %v", err)
				}
				if !sawUnfBefore {
					t.Fatal("diskRecordedBefore did not visit unf-disk-0 in unfilteredDisk")
				}

				// 3. diskRecordingSource must find unf-disk-0 by request key.
				rs := a.diskRecordingSource()
				if rs == nil {
					t.Fatal("diskRecordingSource() = nil, want non-nil")
				}
				sawUnfKey := false
				if err := rs(models.HTTP, models.RequestKeys(&models.Mock{
					Kind: models.HTTP,
					Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/u/0"}},
				}), func(name string, _ func() (*models.Mock, error)) bool {
					if name == "unf-disk-0" {
						sawUnfKey = true
						return false
					}
					return true
				}); err != nil {
					t.Fatalf("diskRecordingSource: %v", err)
				}
				if !sawUnfKey {
					t.Fatal("diskRecordingSource did not visit unf-disk-0 in unfilteredDisk")
				}

				// 4. diskRecordedOfKind must be non-nil and (under proven) visit unf-disk-0.
				rk := a.diskRecordedOfKind()
				if rk == nil {
					t.Fatal("diskRecordedOfKind() = nil, want non-nil")
				}
				if provenswitch.On() {
					sawUnfKind := false
					if err := rk(models.HTTP, func(name string, _ func() (*models.Mock, error)) bool {
						if name == "unf-disk-0" {
							sawUnfKind = true
							return false
						}
						return true
					}); err != nil {
						t.Fatalf("diskRecordedOfKind: %v", err)
					}
					if !sawUnfKind {
						t.Fatal("diskRecordedOfKind did not visit unf-disk-0 in unfilteredDisk")
					}
				}

				// 5. asRecorded() must rewrite 127.0.0.1:19002 -> [::1]:19002 from
				//    unfilteredDisk in both KEPLOY_PROVEN modes (via StoreMocksStream
				//    and also via fallback recordedDests scan when s.dests == nil).
				got := a.asRecorded(models.UnmatchedCall{Destination: "127.0.0.1:19002"})
				if got.Destination != "[::1]:19002" || got.LiveDestination != "127.0.0.1:19002" {
					t.Fatalf("asRecorded(streamed) = (%q, live=%q), want ([::1]:19002, 127.0.0.1:19002)", got.Destination, got.LiveDestination)
				}
				st := loadStorage(t, a)
				st.mu.Lock()
				st.dests = nil
				st.mu.Unlock()
				gotFallback := a.asRecorded(models.UnmatchedCall{Destination: "127.0.0.1:19002"})
				if gotFallback.Destination != "[::1]:19002" || gotFallback.LiveDestination != "127.0.0.1:19002" {
					t.Fatalf("asRecorded(disk fallback) = (%q, live=%q), want ([::1]:19002, 127.0.0.1:19002)", gotFallback.Destination, gotFallback.LiveDestination)
				}
			})
		}
	})

	t.Run("untimed-unfiltered-mock-does-not-corrupt-freeze-anchor", func(t *testing.T) {
		prevHooks := ActiveHooks
		ch := &captureHook{}
		ActiveHooks = ch
		t.Cleanup(func() { ActiveHooks = prevHooks })

		p := newRetainingMgrProxy()
		t.Cleanup(p.mm.Close)
		a := strictAgent(true)
		a.Proxy = p
		t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

		wantAnchor := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)
		streamMocksViaPipe(t, a, 1, 2,
			func(int) *models.Mock {
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    "pt-timed",
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
						ReqTimestampMock: wantAnchor.Add(time.Minute),
						ResTimestampMock: wantAnchor.Add(time.Minute + time.Millisecond),
						HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/pt"},
						HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
					},
				}
			},
			func(i int) *models.Mock {
				if i == 0 {
					// Untimed config mock: ReqTimestampMock is zero.
					return &models.Mock{
						Version: "api.keploy.io/v1beta1",
						Name:    "unf-untimed",
						Kind:    models.HTTP,
						Spec: models.MockSpec{
							Metadata: map[string]string{"type": "config"},
							HTTPReq:  &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/cfg"},
							HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "cfg"},
						},
					}
				}
				return &models.Mock{
					Version: "api.keploy.io/v1beta1",
					Name:    "unf-timed",
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata:         map[string]string{"type": "config"},
						ReqTimestampMock: wantAnchor,
						ResTimestampMock: wantAnchor.Add(time.Millisecond),
						HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/timed"},
						HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
					},
				}
			},
		)

		st := loadStorage(t, a)
		if st.unfilteredDisk == nil {
			t.Fatal("expected unfilteredDisk to be populated")
		}
		if got := st.unfilteredDisk.EarliestReqTs(); !got.Equal(wantAnchor) {
			t.Fatalf("unfilteredDisk.EarliestReqTs() = %v, want %v", got, wantAnchor)
		}
		if !ch.called || !ch.anchor.Equal(wantAnchor) {
			t.Fatalf("SetFreezeAnchor = %v (called=%v), want %v", ch.anchor, ch.called, wantAnchor)
		}
	})

	t.Run("diskRecordedBefore-merges-both-disks-ascending-and-dedups", func(t *testing.T) {
		p := newRetainingMgrProxy()
		t.Cleanup(p.mm.Close)
		a := strictAgent(true)
		a.Proxy = p
		t.Cleanup(func() { a.finalizeClientMocks(ctx, &ClientMockStorage{}) })

		// Interleave timestamps across filtered diskMocks (+1s, +3s, +5s) and
		// unfilteredDisk (+2s, +3s duplicate name, +4s).
		mkAt := func(name string, offset time.Duration, perTest bool) *models.Mock {
			md := map[string]string{"destAddr": "127.0.0.1:8200"}
			if perTest {
				md[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			} else {
				md["type"] = "config"
			}
			ts := base.Add(offset)
			return &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Name:    name,
				Kind:    models.HTTP,
				Spec: models.MockSpec{
					Metadata:         md,
					ReqTimestampMock: ts,
					ResTimestampMock: ts.Add(time.Millisecond),
					HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://svc/" + name},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
				},
			}
		}
		fltMocks := []*models.Mock{
			mkAt("m-1s", 1*time.Second, true),
			mkAt("m-3s-dup", 3*time.Second, true),
			mkAt("m-5s", 5*time.Second, true),
		}
		unfMocks := []*models.Mock{
			mkAt("m-2s", 2*time.Second, false),
			mkAt("m-3s-dup", 3*time.Second, false),
			mkAt("m-4s", 4*time.Second, false),
		}
		streamMocksViaPipe(t, a, len(fltMocks), len(unfMocks),
			func(i int) *models.Mock { return fltMocks[i] },
			func(i int) *models.Mock { return unfMocks[i] },
		)

		rb := a.diskRecordedBefore()
		if rb == nil {
			t.Fatal("diskRecordedBefore() = nil, want non-nil")
		}
		var names []string
		var prev time.Time
		if err := rb(base.Add(time.Minute), func(m *models.Mock) bool {
			names = append(names, m.Name)
			if !prev.IsZero() && m.Spec.ReqTimestampMock.Before(prev) {
				t.Fatalf("out-of-order mock %q (%v < %v)", m.Name, m.Spec.ReqTimestampMock, prev)
			}
			prev = m.Spec.ReqTimestampMock
			return true
		}); err != nil {
			t.Fatalf("diskRecordedBefore: %v", err)
		}
		want := []string{"m-1s", "m-2s", "m-3s-dup", "m-4s", "m-5s"}
		if len(names) != len(want) {
			t.Fatalf("diskRecordedBefore visited %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("diskRecordedBefore[%d] = %q, want %q (all=%v)", i, names[i], want[i], names)
			}
		}

		// Early stop must halt cleanly without error or iterator leak.
		var stopped []string
		if err := rb(base.Add(time.Minute), func(m *models.Mock) bool {
			stopped = append(stopped, m.Name)
			return len(stopped) < 2
		}); err != nil {
			t.Fatalf("diskRecordedBefore early stop: %v", err)
		}
		if len(stopped) != 2 || stopped[0] != "m-1s" || stopped[1] != "m-2s" {
			t.Fatalf("diskRecordedBefore early stop visited %v, want [m-1s m-2s]", stopped)
		}
	})
}
