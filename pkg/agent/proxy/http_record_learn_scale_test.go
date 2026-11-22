package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The HTTP record full-round (learn step, opts.Learn=true) scale shape:
// n calls to one upstream, half GET /item/K (unique path per call) and half
// POST /item with distinct JSON bodies, plus an identical-call shape (n
// identical POST /same recordings) and a query-distinct shape (n GET
// /items?id=K recordings).
const recScalePad = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func recScaleReq(shape string, i int) (method, path, body string) {
	switch shape {
	case "identical":
		return "POST", "/same", fmt.Sprintf(`{"kind":"same","pad":%q}`, recScalePad)
	case "query":
		return "GET", "/items?id=" + strconv.Itoa(i), ""
	default:
		if i%2 == 0 {
			return "GET", "/item/" + strconv.Itoa(i/2), ""
		}
		return "POST", "/item", fmt.Sprintf(`{"id":"b%05d-%d","pad":%q,"r":%d}`, i/8, i%8, recScalePad, i/8)
	}
}

func recScaleMock(shape string, i int, at time.Time) *models.Mock {
	method, path, body := recScaleReq(shape, i)
	hdr := map[string]string{"Host": "upstream", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
	if body != "" {
		hdr["Content-Type"] = "application/json"
		hdr["Content-Length"] = strconv.Itoa(len(body))
	}
	respBody := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i)
	m := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-" + strconv.Itoa(i),
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method), ProtoMajor: 1, ProtoMinor: 1,
				URL: "http://upstream" + path, Header: hdr, Body: body,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(respBody))},
				Body:       respBody,
			},
			ReqTimestampMock: at.Add(time.Duration(i) * time.Millisecond),
			ResTimestampMock: at.Add(time.Duration(i)*time.Millisecond + 500*time.Microsecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

func recScaleRaw(shape string, i int) string {
	method, path, body := recScaleReq(shape, i)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: upstream\r\nUser-Agent: Go-http-client/1.1\r\n", method, path)
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\nContent-Type: application/json\r\n", len(body))
	}
	b.WriteString("Accept-Encoding: gzip\r\n\r\n")
	b.WriteString(body)
	return b.String()
}

const recScaleCallsPerTest = 8

func recScaleTestName(t int) string { return fmt.Sprintf("TestRecScale/b%05d", t) }

func stageRecScale(mm *MockManager, tr *learn.Tracker, shape string, n int, at time.Time, view string) []*models.Mock {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = recScaleMock(shape, i, at)
	}
	start, end := at.Add(-time.Second), at.Add(time.Duration(n)*time.Millisecond+time.Second)
	mm.LearnBoundary(tr, func(t *learn.Tracker) { t.NewSet() })
	var windows []models.TestWindow
	switch view {
	case "test":
		windows = []models.TestWindow{{TestCase: recScaleTestName(0), Start: start, End: end}}
	case "tests":
		for t := 0; t*recScaleCallsPerTest < n; t++ {
			first := at.Add(time.Duration(t*recScaleCallsPerTest) * time.Millisecond)
			windows = append(windows, models.TestWindow{
				TestCase: recScaleTestName(t),
				Start:    first.Add(-250 * time.Microsecond),
				End:      first.Add(time.Duration(recScaleCallsPerTest)*time.Millisecond - 250*time.Microsecond),
			})
		}
	case "pool":
		names := make([]string, n)
		for i, m := range mocks {
			names[i] = m.Name
		}
		mm.SetMocksWithWindow(nil, mocks, start, end)
		mm.bumpStagingEpoch()
		mm.SeedRecordedWindows(nil)
		mm.ServeExactPool(models.PoolKeyNone, names)
		return nil
	}
	mm.SetMocksWithWindow(mocks, nil, start, end)
	mm.bumpStagingEpoch()
	mm.SeedRecordedWindows(windows)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	byName := make(map[string]*models.Mock, n)
	for _, m := range mm.StagedMocks() {
		byName[m.Name] = m
	}
	staged := make([]*models.Mock, n)
	for i := range staged {
		staged[i] = byName["mock-"+strconv.Itoa(i)]
	}
	return staged
}

func openRecScale(mm *MockManager, tr *learn.Tracker, staged []*models.Mock, t, owns int) {
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })
	mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
	mm.ServeExactTest(recScaleTestName(t), staged[t*owns:(t+1)*owns])
}

func serveRecScale(h integrations.Integrations, mm integrations.MockMemDb, tr *learn.Tracker, shape string, i int, learnPool bool) (string, error) {
	return serveRecRaw(h, mm, tr, recScaleRaw(shape, i), learnPool)
}

func serveRecRaw(h integrations.Integrations, mm integrations.MockMemDb, tr *learn.Tracker, raw string, learnPool bool) (string, error) {
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(learn.WithTracker(context.Background(), tr), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{
			Learn:     true,
			LearnPool: learnPool,
		})
		_ = agent.Close()
	}()
	go func() { _, _ = app.Write([]byte(raw)) }()
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		_ = app.Close()
		return "", fmt.Errorf("read response: %w (integration: %v)", err, <-done)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	return string(body), err
}

// BenchmarkHTTPRecordLearnPerCall measures steady-state per-call cost of
// record's full round (opts.Learn=true, KEPLOY_PROVEN=1) from 1k to 10k
// recordings after the view's one-time index build (2 warmup calls), and
// BenchmarkHTTPRecordLearnFullSet measures the amortised per-call cost across
// an entire 1k vs 10k recording set including index build and bucket filing.
func BenchmarkHTTPRecordLearnPerCall(b *testing.B) {
	verify := exactViewVerify.Load()
	exactViewVerify.Store(false)
	b.Cleanup(func() { exactViewVerify.Store(verify) })
	modes := []struct{ name, view, shape string }{
		{"view=test", "test", "mixed"},
		{"view=tests", "tests", "mixed"},
		{"view=pool", "pool", "mixed"},
		{"view=pool/identical", "pool", "identical"},
		{"view=test/identical", "test", "identical"},
		{"view=test/query", "test", "query"},
	}
	for _, mode := range modes {
		for _, n := range []int{1000, 10000} {
			b.Run(mode.name+"/n="+strconv.Itoa(n), func(b *testing.B) {
				b.Setenv(provenswitch.Env, "1")
				const per = 256
				at := time.Unix(1_700_000_000, 0)
				h := httpint.New(zap.NewNop())
				mm := NewMockManager(nil, nil, zap.NewNop())
				defer mm.Close()
				tr := learn.NewTracker()
				mm.SetServedObserver(tr.ServedMock)
				mm.SetTrackerSetAt(tr.SetAt)
				var took [2]time.Duration
				var calls [2]int
				var staged []*models.Mock
				warmOffset := 0
				b.ReportAllocs()
				for k := 0; k < b.N; k++ {
					var i int
					switch mode.view {
					case "tests":
						if k == 0 || (k+2)%n == 0 {
							b.StopTimer()
							staged = stageRecScale(mm, tr, mode.shape, n, at, mode.view)
							// Warm one test's shared index build before timing steady-state calls.
							openRecScale(mm, tr, staged, 0, recScaleCallsPerTest)
							for w := 0; w < 2; w++ {
								if _, err := serveRecScale(h, mm, tr, mode.shape, w, false); err != nil {
									b.Fatalf("tests warmup %d: %v", w, err)
								}
							}
							b.StartTimer()
						}
						i = (k + 2) % n
						if i%recScaleCallsPerTest == 0 {
							openRecScale(mm, tr, staged, i/recScaleCallsPerTest, recScaleCallsPerTest)
						}
					default:
						if k%per == 0 {
							b.StopTimer()
							staged = stageRecScale(mm, tr, mode.shape, n, at, mode.view)
							if mode.view == "test" {
								openRecScale(mm, tr, staged, 0, n)
							}
							// Warm one GET + one POST (or 2 calls) so the
							// steady-state per-call measurement isolates
							// lookup cost; full-set amortised cost including
							// build is measured by BenchmarkHTTPRecordLearnFullSet.
							for w := 0; w < 2; w++ {
								if _, err := serveRecScale(h, mm, tr, mode.shape, w, mode.view == "pool"); err != nil {
									b.Fatalf("warmup %d: %v", w, err)
								}
							}
							warmOffset = 2
							b.StartTimer()
						}
						if mode.shape == "identical" {
							i = warmOffset + (k % per)
						} else {
							i = warmOffset + (((k % per) * 7919) % (n - warmOffset))
						}
					}
					t0 := time.Now()
					body, err := serveRecScale(h, mm, tr, mode.shape, i, mode.view == "pool")
					took[i%2] += time.Since(t0)
					calls[i%2]++
					if err != nil {
						b.Fatal(err)
					}
					want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i)
					if body != want {
						b.Fatalf("call %d served %q, want %q", i, body, want)
					}
				}
				if mode.shape == "mixed" {
					for kind, name := range []string{"get-ns", "post-ns"} {
						if calls[kind] > 0 {
							b.ReportMetric(float64(took[kind].Nanoseconds())/float64(calls[kind]), name)
						}
					}
				}
			})
		}
	}
}

// BenchmarkHTTPRecordLearnFullSet runs an entire 1k or 10k set end-to-end
// (including index build and lazy bucket filing) and reports amortised
// ns/call and allocs/call across all n calls, proving O(1) amortised scaling.
func BenchmarkHTTPRecordLearnFullSet(b *testing.B) {
	verify := exactViewVerify.Load()
	exactViewVerify.Store(false)
	b.Cleanup(func() { exactViewVerify.Store(verify) })
	for _, n := range []int{1000, 10000} {
		b.Run("view=test/n="+strconv.Itoa(n), func(b *testing.B) {
			b.Setenv(provenswitch.Env, "1")
			at := time.Unix(1_700_000_000, 0)
			h := httpint.New(zap.NewNop())
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			tr := learn.NewTracker()
			mm.SetServedObserver(tr.ServedMock)
			mm.SetTrackerSetAt(tr.SetAt)
			b.ReportAllocs()
			b.ResetTimer()
			var totalDur time.Duration
			for iter := 0; iter < b.N; iter++ {
				b.StopTimer()
				staged := stageRecScale(mm, tr, "mixed", n, at, "test")
				openRecScale(mm, tr, staged, 0, n)
				b.StartTimer()
				t0 := time.Now()
				for i := 0; i < n; i++ {
					body, err := serveRecScale(h, mm, tr, "mixed", i, false)
					if err != nil {
						b.Fatal(err)
					}
					if want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i); body != want {
						b.Fatalf("call %d served %q, want %q", i, body, want)
					}
				}
				totalDur += time.Since(t0)
			}
			b.ReportMetric(float64(totalDur.Nanoseconds())/float64(b.N*n), "ns/call")
		})
	}
}

// TestHTTPRecordLearnPerCallAllocsStayConstant deterministically pins per-call
// allocations and allocated bytes of record's full round flat between n=100
// and n=2000 (20x scale) without relying on wall-clock timing. On origin/main
// without full-round indexing, allocs/call grow ~20x from n=100 to n=2000 and
// this test fails. Also covers Mock.Noise on every recording (wild cursor)
// and an unrelated staging TakeVoided (per-mock voided check).
func TestHTTPRecordLearnPerCallAllocsStayConstant(t *testing.T) {
	verify := exactViewVerify.Load()
	exactViewVerify.Store(false)
	t.Cleanup(func() { exactViewVerify.Store(verify) })
	t.Setenv(provenswitch.Env, "1")

	measure := func(view, shape, variant string, n int) (allocs, bytes float64) {
		at := time.Unix(1_700_000_000, 0)
		h := httpint.New(zap.NewNop())
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		tr := learn.NewTracker()
		mm.SetServedObserver(tr.ServedMock)
		mm.SetTrackerSetAt(tr.SetAt)
		mocks := make([]*models.Mock, n)
		for i := range mocks {
			mocks[i] = recScaleMock(shape, i, at)
			if variant == "mocknoise" {
				mocks[i].Noise = []string{"tok-[0-9a-f]{8}"}
			}
		}
		start, end := at.Add(-time.Second), at.Add(time.Duration(n)*time.Millisecond+time.Second)
		mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
		if view == "pool" {
			names := make([]string, n)
			for i, m := range mocks {
				names[i] = m.Name
			}
			mm.SetMocksWithWindow(nil, mocks, start, end)
			mm.bumpStagingEpoch()
			mm.SeedRecordedWindows(nil)
			mm.ServeExactPool(models.PoolKeyNone, names)
		} else {
			mm.SetMocksWithWindow(mocks, nil, start, end)
			mm.bumpStagingEpoch()
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: recScaleTestName(0), Start: start, End: end}})
			mm.ServeExactPool(models.PoolKeyNone, nil)
			byName := make(map[string]*models.Mock, n)
			for _, m := range mm.StagedMocks() {
				byName[m.Name] = m
			}
			staged := make([]*models.Mock, n)
			for i := range staged {
				staged[i] = byName[mocks[i].Name]
			}
			openRecScale(mm, tr, staged, 0, n)
		}
		if variant == "voided" {
			mm.exact.round.mu.Lock()
			mm.takeLocked(integrations.Take{How: integrations.TakeVoided, Mock: &models.Mock{Name: "elsewhere"}})
			mm.exact.round.mu.Unlock()
		}
		// Warm 2 calls so one-time index build / bucket filing completes.
		for w := 0; w < 2; w++ {
			if _, err := serveRecScale(h, mm, tr, shape, w, view == "pool"); err != nil {
				t.Fatalf("%s/%s/%s n=%d warmup %d: %v", view, shape, variant, n, w, err)
			}
		}
		const runs = 20
		idx := 2
		var msBefore, msAfter runtime.MemStats
		runtime.ReadMemStats(&msBefore)
		allocs = testing.AllocsPerRun(runs, func() {
			i := idx
			idx++
			body, err := serveRecScale(h, mm, tr, shape, i, view == "pool")
			if err != nil {
				t.Fatalf("%s/%s/%s n=%d call %d: %v", view, shape, variant, n, i, err)
			}
			if want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i); body != want {
				t.Fatalf("%s/%s/%s n=%d call %d served %q, want %q", view, shape, variant, n, i, body, want)
			}
		})
		runtime.ReadMemStats(&msAfter)
		// AllocsPerRun runs 1 warmup + runs iterations = runs+1 calls.
		bytes = float64(msAfter.TotalAlloc-msBefore.TotalAlloc) / float64(runs+1)
		return allocs, bytes
	}

	cases := []struct{ name, view, shape, variant string }{
		{"test/mixed", "test", "mixed", ""},
		{"pool/mixed", "pool", "mixed", ""},
		{"pool/identical", "pool", "identical", ""},
		{"test/identical", "test", "identical", ""},
		{"test/query", "test", "query", ""},
		{"test/mocknoise", "test", "mixed", "mocknoise"},
		{"test/voided", "test", "mixed", "voided"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a100, b100 := measure(tc.view, tc.shape, tc.variant, 100)
			a2000, b2000 := measure(tc.view, tc.shape, tc.variant, 2000)
			aratio := a2000 / a100
			bratio := b2000 / b100
			t.Logf("%s: n=100 %.0f allocs (%.0f B), n=2000 %.0f allocs (%.0f B) (ratio %.2fx allocs, %.2fx B)",
				tc.name, a100, b100, a2000, b2000, aratio, bratio)
			if aratio > 1.5 {
				t.Fatalf("per-call allocations grew with recording count: n=100 %.0f, n=2000 %.0f (%.2fx > 1.5x)", a100, a2000, aratio)
			}
			if bratio > 1.5 {
				t.Fatalf("per-call allocated bytes grew with recording count: n=100 %.0f B, n=2000 %.0f B (%.2fx > 1.5x)", b100, b2000, bratio)
			}
		})
	}
}

// disableRoundMeta turns off MockManager.RoundMeta via its onRead seam while
// keeping the real *MockManager and every optional capability intact, giving a
// faithful production full-path reference oracle.
func disableRoundMeta(mm *MockManager) {
	mm.exact.round.mu.Lock()
	mm.exact.round.onRead = func(string) {}
	mm.exact.round.mu.Unlock()
}

// TestHTTPRecordLearnFullRoundByteIdentical runs an identical mixed workload
// (unique GETs, distinct POSTs, identical POST repeats, query GETs, a drifting
// POST that learns noise, a PUT+GET read-back pair, and a spent-exact miss)
// against both the indexed fast path and the faithful full-path reference on
// the same real *MockManager (via disableRoundMeta), in both KEPLOY_PROVEN
// modes, asserting byte-identical responses, misses, and consumed MockStates.
func TestHTTPRecordLearnFullRoundByteIdentical(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)

	makeRaw := func(method, path, body string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: upstream\r\nUser-Agent: Go-http-client/1.1\r\n", method, path)
		if body != "" {
			fmt.Fprintf(&b, "Content-Length: %d\r\nContent-Type: application/json\r\n", len(body))
		}
		b.WriteString("Accept-Encoding: gzip\r\n\r\n")
		b.WriteString(body)
		return b.String()
	}
	makeMock := func(name string, off int, method, path, reqBody, respBody string) *models.Mock {
		hdr := map[string]string{"Host": "upstream", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
		if reqBody != "" {
			hdr["Content-Type"] = "application/json"
			hdr["Content-Length"] = strconv.Itoa(len(reqBody))
		}
		m := &models.Mock{
			Version: models.GetVersion(),
			Name:    name,
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": models.HTTPClient},
				HTTPReq: &models.HTTPReq{
					Method: models.Method(method), ProtoMajor: 1, ProtoMinor: 1,
					URL: "http://upstream" + path, Header: hdr, Body: reqBody,
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(respBody))},
					Body:       respBody,
				},
				ReqTimestampMock: at.Add(time.Duration(off) * time.Millisecond),
				ResTimestampMock: at.Add(time.Duration(off)*time.Millisecond + 500*time.Microsecond),
			},
		}
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
		return m
	}

	type callSpec struct{ method, path, body string }
	var calls []callSpec
	buildMocks := func() []*models.Mock {
		var ms []*models.Mock
		off := 0
		for i := 0; i < 20; i++ {
			ms = append(ms, makeMock(fmt.Sprintf("get-%d", i), off, "GET", fmt.Sprintf("/item/%d", i), "", fmt.Sprintf(`{"g":%d}`, i)))
			off++
		}
		for i := 0; i < 20; i++ {
			b := fmt.Sprintf(`{"id":"p-%d","n":%d}`, i, i)
			ms = append(ms, makeMock(fmt.Sprintf("post-%d", i), off, "POST", "/item", b, fmt.Sprintf(`{"p":%d}`, i)))
			off++
		}
		for i := 0; i < 5; i++ {
			ms = append(ms, makeMock(fmt.Sprintf("same-%d", i), off, "POST", "/same", `{"k":"dup"}`, fmt.Sprintf(`{"s":%d}`, i)))
			off++
		}
		for i := 0; i < 10; i++ {
			ms = append(ms, makeMock(fmt.Sprintf("q-%d", i), off, "GET", fmt.Sprintf("/search?id=%d&tag=x", i), "", fmt.Sprintf(`{"q":%d}`, i)))
			off++
		}
		// Drifted POST (learns noise on req_id).
		ms = append(ms, makeMock("drift-0", off, "POST", "/drift", `{"req_id":"rec-uuid-1","op":"go"}`, `{"ok":true}`))
		off++
		// PUT + GET read-back pair.
		ms = append(ms, makeMock("put-kv", off, "PUT", "/kv/k1", `{"val":"rec-val-1"}`, `{"saved":true}`))
		off++
		ms = append(ms, makeMock("get-kv", off, "GET", "/kv/k1", "", `{"val":"rec-val-1"}`))
		return ms
	}
	for i := 19; i >= 0; i-- { // out-of-order GETs
		calls = append(calls, callSpec{"GET", fmt.Sprintf("/item/%d", i), ""})
	}
	for i := 0; i < 20; i++ {
		calls = append(calls, callSpec{"POST", "/item", fmt.Sprintf(`{"n":%d,"id":"p-%d"}`, i, i)}) // reordered JSON keys
	}
	for i := 0; i < 5; i++ {
		calls = append(calls, callSpec{"POST", "/same", `{"k":"dup"}`})
	}
	for i := 9; i >= 0; i-- {
		calls = append(calls, callSpec{"GET", fmt.Sprintf("/search?tag=x&id=%d", i), ""})
	}
	calls = append(calls, callSpec{"POST", "/drift", `{"req_id":"live-uuid-2","op":"go"}`})
	calls = append(calls, callSpec{"PUT", "/kv/k1", `{"val":"live-val-2"}`})
	calls = append(calls, callSpec{"GET", "/kv/k1", ""})
	// Extra call (spent-exact miss).
	calls = append(calls, callSpec{"GET", "/item/0", ""})

	for _, proven := range []string{"1", ""} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			run := func(useFast bool) ([]string, []string, []bool, []models.MockState) {
				mocks := buildMocks()
				mm := NewMockManager(nil, nil, zap.NewNop())
				defer mm.Close()
				tr := learn.NewTracker()
				mm.SetServedObserver(tr.ServedMock)
				mm.SetTrackerSetAt(tr.SetAt)
				start, end := at.Add(-time.Second), at.Add(time.Minute)
				mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
				mm.SetMocksWithWindow(mocks, nil, start, end)
				mm.bumpStagingEpoch()
				mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "T1", Start: start, End: end}})
				mm.ServeExactPool(models.PoolKeyNone, nil)
				mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
				mm.ServeExactTest("T1", mm.StagedMocks())

				if !useFast {
					disableRoundMeta(mm)
				}
				h := httpint.New(zap.NewNop())
				opts := models.OutgoingOptions{Learn: true}
				out := make([]string, len(calls))
				reports := make([]string, len(calls))
				errs := make([]bool, len(calls))
				for i, c := range calls {
					body, rep, err := serveDiffCallWithReport(h, mm, tr, makeRaw(c.method, c.path, c.body), opts)
					if i < len(calls)-1 && err != nil {
						t.Fatalf("useFast=%v call %d (%s %s): %v", useFast, i, c.method, c.path, err)
					}
					out[i] = body
					reports[i] = rep
					errs[i] = err != nil
				}
				states := mm.GetConsumedMocks()
				for i := range states {
					states[i].SortOrder = 0
					states[i].Timestamp = 0
				}
				return out, reports, errs, states
			}

			fastResps, fastReps, fastErrs, fastStates := run(true)
			refResps, refReps, refErrs, refStates := run(false)
			if !reflect.DeepEqual(fastErrs, refErrs) {
				t.Fatalf("call error presence differs:\nfast: %v\nref:  %v", fastErrs, refErrs)
			}
			if !reflect.DeepEqual(fastResps, refResps) {
				for i := range fastResps {
					if fastResps[i] != refResps[i] {
						t.Fatalf("call %d response mismatch:\nfast: %q\nref:  %q", i, fastResps[i], refResps[i])
					}
				}
			}
			missReports := 0
			for i := range fastReps {
				if fastReps[i] != "" {
					missReports++
				}
				if fastReps[i] != refReps[i] {
					t.Fatalf("call %d miss report mismatch:\nfast: %s\nref:  %s", i, fastReps[i], refReps[i])
				}
			}
			if missReports == 0 {
				t.Fatal("expected at least one captured miss report on spent-exact miss")
			}
			if !reflect.DeepEqual(fastStates, refStates) {
				t.Fatalf("consumed MockStates differ:\nfast: %+v\nref:  %+v", fastStates, refStates)
			}
		})
	}
}

type diffCall struct {
	method, path, body string
	hdr                [][2]string
}

func (c diffCall) raw() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: upstream\r\nUser-Agent: Go-http-client/1.1\r\n", c.method, c.path)
	for _, h := range c.hdr {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	if c.body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\nContent-Type: application/json\r\n", len(c.body))
	}
	b.WriteString("Accept-Encoding: gzip\r\n\r\n")
	b.WriteString(c.body)
	return b.String()
}

type diffCorpus struct {
	owned     [][]*models.Mock
	ownedCall [][]diffCall
	pool      []*models.Mock
	poolCall  []diffCall
	windows   []models.TestWindow
	calls     [][]diffCall
	gaps      [][]diffCall
	opts      models.OutgoingOptions
}

func buildDiffCorpus(seed int64) diffCorpus {
	r := rand.New(rand.NewSource(seed))
	paths := []string{"/a/1", "/a/2", "/a", "/q?x=1&y=2", "/q?x=1&x=2", "/k/v1", "/k/v2"}
	bodies := []string{
		`{"a":1,"b":"x"}`, `{"a":2,"b":"x"}`, `{"a":1.0,"b":"x"}`, `{"b":[1,2]}`, `{"b":[2,1]}`,
		`{"a":{"b":1}}`, `{"a.b":1}`, `text-body`, ``, `{}`, `[]`, `null`,
		`{"tok":"secret-1"}`, `{"tok":"secret-2"}`, `{"a":"1","b":"x"}`,
	}
	xids := []string{"i1", "i2"}
	pickStr := func(xs []string) string { return xs[r.Intn(len(xs))] }
	randCall := func() diffCall {
		c := diffCall{method: pickStr([]string{"GET", "POST", "PUT"}), path: pickStr(paths)}
		if c.method != "GET" {
			c.body = pickStr(bodies)
		}
		c.hdr = [][2]string{{"X-Id", pickStr(xids)}}
		return c
	}
	mutateCall := func(c diffCall) diffCall {
		out := c
		out.hdr = append([][2]string(nil), c.hdr...)
		switch r.Intn(10) {
		case 0:
			out.hdr = [][2]string{{"X-Id", "live-" + strconv.Itoa(r.Intn(3))}}
		case 1:
			out.body = strings.Replace(c.body, `{"a":1,"b":"x"}`, `{"b":"x","a":1}`, 1)
		case 2:
			out.body = strings.Replace(c.body, `"a":1,`, `"a":1.0,`, 1)
		case 3:
			sep := "?"
			if strings.Contains(out.path, "?") {
				sep = "&"
			}
			out.path += sep + "KeployTs=" + strconv.Itoa(r.Intn(9))
		case 4:
			out.path = strings.Replace(out.path, "x=1&x=2", "x=2&x=1", 1)
		case 5:
			out.body = strings.Replace(c.body, "secret-1", "secret-9", 1)
		}
		return out
	}
	mkMock := func(name string, at time.Time, c diffCall) *models.Mock {
		hdr := map[string]string{"Host": "upstream", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
		for _, h := range c.hdr {
			hdr[h[0]] = h[1]
		}
		if c.body != "" {
			hdr["Content-Type"] = "application/json"
			hdr["Content-Length"] = strconv.Itoa(len(c.body))
		}
		resp := fmt.Sprintf(`{"m":%q}`, name)
		m := &models.Mock{
			Version: models.GetVersion(),
			Name:    name,
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": models.HTTPClient},
				HTTPReq: &models.HTTPReq{
					Method: models.Method(c.method), ProtoMajor: 1, ProtoMinor: 1,
					URL: "http://upstream" + c.path, Header: hdr, Body: c.body,
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))},
					Body:       resp,
				},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(200 * time.Microsecond),
			},
		}
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
		switch r.Intn(10) {
		case 0:
			m.Noise = []string{"secret-1"}
		case 1:
			m.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
		}
		return m
	}

	base := time.Unix(1_700_000_000, 0)
	var c diffCorpus
	tests := 2 + r.Intn(5)
	off := 0
	for g := 0; g <= tests; g++ {
		for i := 0; i < r.Intn(4); i++ {
			call := randCall()
			at := base.Add(time.Duration(off) * time.Millisecond)
			if r.Intn(3) > 0 {
				off++
			}
			m := mkMock(fmt.Sprintf("p%d-%d", g, i), at, call)
			if r.Intn(3) == 0 {
				m.TestModeInfo.Lifetime = models.LifetimeSession
			}
			c.pool = append(c.pool, m)
			c.poolCall = append(c.poolCall, call)
		}
		off += 2
		if g == tests {
			break
		}
		start := base.Add(time.Duration(off)*time.Millisecond - 500*time.Microsecond)
		var own []*models.Mock
		var ownCall []diffCall
		for i := 0; i < 3+r.Intn(8); i++ {
			call := randCall()
			if i > 0 && r.Intn(3) == 0 {
				call = ownCall[r.Intn(len(ownCall))]
			}
			at := base.Add(time.Duration(off) * time.Millisecond)
			if r.Intn(10) == 0 {
				// Placed outside every window so ownership() routes to pool.
				at = start.Add(-200 * time.Microsecond)
			} else if r.Intn(3) > 0 {
				off++
			}
			own = append(own, mkMock(fmt.Sprintf("t%d-%d", g, i), at, call))
			ownCall = append(ownCall, call)
		}
		off++
		end := base.Add(time.Duration(off)*time.Millisecond - 500*time.Microsecond)
		c.windows = append(c.windows, models.TestWindow{TestCase: fmt.Sprintf("T%d", g), Start: start, End: end})
		c.owned = append(c.owned, own)
		c.ownedCall = append(c.ownedCall, ownCall)
		off += 2
	}
	pickCall := func(own []diffCall) diffCall {
		switch k := r.Intn(10); {
		case k < 6 && len(own) > 0:
			return mutateCall(own[r.Intn(len(own))])
		case k < 8 && len(c.poolCall) > 0:
			return mutateCall(c.poolCall[r.Intn(len(c.poolCall))])
		default:
			return randCall()
		}
	}
	for g := 0; g <= tests; g++ {
		var gap []diffCall
		for i := 0; i < r.Intn(4); i++ {
			gap = append(gap, pickCall(nil))
		}
		c.gaps = append(c.gaps, gap)
		if g == tests {
			break
		}
		var calls []diffCall
		for i := 0; i < len(c.ownedCall[g])+r.Intn(4)-1; i++ {
			calls = append(calls, pickCall(c.ownedCall[g]))
		}
		c.calls = append(c.calls, calls)
	}
	c.opts = models.OutgoingOptions{Learn: true, LearnPool: r.Intn(2) == 0}
	switch r.Intn(5) {
	case 0:
		c.opts.NoiseConfig = map[string]map[string][]string{"requestbody": {"a": {}}}
	case 1:
		c.opts.NoiseConfig = map[string]map[string][]string{"header": {"x-id": {}}}
	case 2:
		c.opts.NoiseConfig = map[string]map[string][]string{"url": {"v[0-9]+": {}}}
	}
	return c
}

type mismatchReportProvider interface {
	MismatchReport() *models.MockMismatchReport
}

// serveDiffCallWithReport serves a single HTTP call through h.MockOutgoing and
// also captures any persisted MockMismatchReport (with its At timestamp zeroed)
// serialized as JSON, so differential tests verify miss diagnostics
// (candidate_count, closest, destination_scope, reason, next steps) in
// addition to the HTTP response body.
func serveDiffCallWithReport(h integrations.Integrations, db integrations.MockMemDb, tr *learn.Tracker, raw string, opts models.OutgoingOptions) (string, string, error) {
	app, agent := net.Pipe()
	var mu sync.Mutex
	var reports []string
	note := func(err error) {
		var r mismatchReportProvider
		if errors.As(err, &r) && r.MismatchReport() != nil {
			c := *r.MismatchReport()
			c.At = time.Time{}
			b, _ := json.Marshal(c)
			mu.Lock()
			reports = append(reports, string(b))
			mu.Unlock()
		} else if err != nil {
			mu.Lock()
			reports = append(reports, "ERR "+err.Error())
			mu.Unlock()
		}
	}
	ctx := models.WithMockMismatchReporter(learn.WithTracker(context.Background(), tr), note)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, db, opts)
		_ = agent.Close()
	}()
	go func() { _, _ = app.Write([]byte(raw)) }()
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		_ = app.Close()
		return "", "", fmt.Errorf("read response: %w (integration: %v)", err, <-done)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	if derr := <-done; derr != nil {
		var r mismatchReportProvider
		if errors.As(derr, &r) && !models.IsMismatchReported(derr) {
			note(derr)
		}
	}
	mu.Lock()
	rep := strings.Join(reports, "\n")
	mu.Unlock()
	return string(body), rep, err
}

func serveDiffCall(h integrations.Integrations, db integrations.MockMemDb, tr *learn.Tracker, raw string, opts models.OutgoingOptions) (string, error) {
	body, _, err := serveDiffCallWithReport(h, db, tr, raw, opts)
	return body, err
}

// TestHTTPRecordLearnDifferentialCorpus compares the fast path (*MockManager
// with RoundMeta + fullRoundIndexes) against the faithful full-path reference
// (the same real *MockManager with RoundMeta disabled via onRead, preserving
// every optional capability) across a seeded multi-test randomized corpus
// (including shared candidates via ServeExactTestConcurrent), comparing every
// response, every miss's MockMismatchReport (candidate_count, closest, reason,
// destination_scope), and every consumed MockState.
func TestHTTPRecordLearnDifferentialCorpus(t *testing.T) {
	seeds := 150
	if s := os.Getenv("KEPLOY_DIFF_SEEDS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			seeds = v
		}
	}

	runSeed := func(t *testing.T, c diffCorpus, fast, withCands bool) ([]string, []string, []models.MockState) {
		t.Helper()
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		tr := learn.NewTracker()
		mm.SetServedObserver(tr.ServedMock)
		mm.SetTrackerSetAt(tr.SetAt)
		var owned []*models.Mock
		for _, own := range c.owned {
			owned = append(owned, own...)
		}
		start := c.windows[0].Start.Add(-time.Hour)
		end := c.windows[len(c.windows)-1].End.Add(time.Hour)
		mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
		mm.SetMocksWithWindow(owned, c.pool, start, end)
		mm.bumpStagingEpoch()
		mm.SeedRecordedWindows(c.windows)
		byName := make(map[string]*models.Mock, len(owned)+len(c.pool))
		for _, m := range mm.StagedMocks() {
			byName[m.Name] = m
		}
		poolNames := make([]string, 0, len(c.pool))
		for _, m := range c.pool {
			poolNames = append(poolNames, m.Name)
		}
		if !fast {
			disableRoundMeta(mm)
		}
		h := httpint.New(zap.NewNop())
		var answers, reports []string
		serve := func(call diffCall) {
			body, rep, err := serveDiffCallWithReport(h, mm, tr, call.raw(), c.opts)
			if err != nil {
				body = "ERR " + err.Error()
			}
			answers = append(answers, call.method+" "+call.path+" -> "+body)
			reports = append(reports, rep)
		}
		for g := range c.gaps {
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })
			for _, call := range c.gaps[g] {
				serve(call)
			}
			if g == len(c.owned) {
				break
			}
			mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
			own := make([]*models.Mock, 0, len(c.owned[g]))
			for _, m := range c.owned[g] {
				own = append(own, byName[m.Name])
			}
			var cands []*models.Mock
			if withCands && g+1 < len(c.owned) {
				for i, m := range c.owned[g+1] {
					if i%3 == 0 {
						cands = append(cands, byName[m.Name])
					}
				}
			}
			mm.ServeExactTestConcurrent(c.windows[g].TestCase, own, cands)
			for _, call := range c.calls[g] {
				serve(call)
			}
		}
		states := mm.GetConsumedMocks()
		for i := range states {
			states[i].SortOrder = 0
			states[i].Timestamp = 0
		}
		sort.SliceStable(states, func(i, j int) bool { return states[i].Name < states[j].Name })
		return answers, reports, states
	}

	modes := []struct {
		name      string
		proven    string
		verify    bool
		withCands bool
		n         int
	}{
		{"proven=1", "1", false, false, seeds},
		{"proven=1/cands", "1", false, true, min(seeds, 100)},
		{"proven=1/verify", "1", true, false, min(seeds, 50)},
		{"proven=unset", "", false, false, min(seeds, 40)},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, m.proven)
			was := integrations.ExactViewVerify.Load()
			integrations.ExactViewVerify.Store(m.verify)
			t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
			reportsSeen := 0
			for seed := int64(1); seed <= int64(m.n); seed++ {
				c := buildDiffCorpus(seed)
				fastAns, fastRep, fastStates := runSeed(t, c, true, m.withCands)
				refAns, refRep, refStates := runSeed(t, c, false, m.withCands)
				if !reflect.DeepEqual(fastAns, refAns) {
					for i := range fastAns {
						if i >= len(refAns) || fastAns[i] != refAns[i] {
							t.Fatalf("seed %d call %d differs:\nfast: %s\nref:  %s\nopts=%+v", seed, i, fastAns[i], refAns[i], c.opts)
						}
					}
				}
				for i := range fastRep {
					if fastRep[i] != "" {
						reportsSeen++
					}
					if fastRep[i] != refRep[i] {
						t.Fatalf("seed %d call %d (%s) miss report differs:\nfast: %s\nref:  %s\nopts=%+v", seed, i, fastAns[i], fastRep[i], refRep[i], c.opts)
					}
				}
				if !reflect.DeepEqual(fastStates, refStates) {
					t.Fatalf("seed %d consumed states differ:\nfast: %+v\nref:  %+v", seed, fastStates, refStates)
				}
			}
			if reportsSeen == 0 {
				t.Fatalf("%s: expected captured miss reports across %d seeds, got 0", m.name, m.n)
			}
		})
	}
}

func makeMovedClaimMock(name string, at time.Time, method, path string, lifetime models.Lifetime) *models.Mock {
	hdr := map[string]string{"Host": "upstream", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
	resp := fmt.Sprintf(`{"m":%q}`, name)
	m := &models.Mock{
		Version: models.GetVersion(),
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method), ProtoMajor: 1, ProtoMinor: 1,
				URL: "http://upstream" + path, Header: hdr,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))},
				Body:       resp,
			},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(200 * time.Microsecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = lifetime, true
	return m
}

// TestHTTPRecordLearnMovedUnderClaimRealStore verifies moved-round miss
// diagnostics on the REAL *MockManager when T1 ends and T2 opens between a
// call's fast-path pick and its claim (via MockManager.OnConsume), comparing
// fast vs disableRoundMeta across consumed owned, owned-with-variables, shared
// recordings (with one spent in T1), pool-with-variables, and worker-scope
// variable recordings (pinning varPerTest and varSession reconstruction).
func TestHTTPRecordLearnMovedUnderClaimRealStore(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	type variant struct {
		name       string
		cands      bool // T1 is served with shared recordings, one spent in T1
		vars       bool // T1 holds a recording with variables
		learnPool  bool
		poolVars   bool // pool holds recordings with variables
		workerVars bool // worker scope exposes variable recordings in open test's SharedMocks
	}
	variants := []variant{
		{"plain", false, false, false, false, false},
		{"plain/pool", false, false, true, false, false},
		{"vars", false, true, true, false, false},
		{"cands", true, false, true, false, false},
		{"cands+vars", true, true, true, false, false},
		{"cands+vars/nopool", true, true, false, false, false},
		{"poolvars", false, false, true, true, false},
		{"poolvars/nopool", false, false, false, true, false},
		{"cands+poolvars", true, true, true, true, false},
		{"worker-vars", false, true, true, true, true},
		{"cands+worker-vars", true, true, true, true, true},
	}
	for _, proven := range []string{"1", ""} {
		for _, vr := range variants {
			t.Run("proven="+proven+"/"+vr.name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				run := func(fast bool) ([]string, []string, []models.MockState) {
					t1done := makeMovedClaimMock("t1-done", ms(1), "GET", "/item/0", models.LifetimePerTest)
					t1get := makeMovedClaimMock("t1-get", ms(2), "GET", "/item/1", models.LifetimePerTest)
					t1var := makeMovedClaimMock("t1-var", ms(3), "GET", "/item/2", models.LifetimePerTest)
					if vr.vars {
						t1var.Spec.Variables = models.MockVariables{"v": {Recorded: "2", Use: []string{"url.path[1]"}}}
					}
					t1late := makeMovedClaimMock("t1-late", ms(4), "GET", "/item/1", models.LifetimePerTest)
					t2get := makeMovedClaimMock("t2-get", ms(12), "GET", "/item/1", models.LifetimePerTest)
					t2sh0 := makeMovedClaimMock("t2-sh0", ms(13), "GET", "/shared/0", models.LifetimePerTest)
					t2sh1 := makeMovedClaimMock("t2-sh1", ms(14), "GET", "/shared/1", models.LifetimePerTest)
					pSess := makeMovedClaimMock("p-sess", ms(-5), "GET", "/item/1", models.LifetimeSession)
					pPT := makeMovedClaimMock("p-pt", ms(-4), "GET", "/item/1", models.LifetimePerTest)
					pOther := makeMovedClaimMock("p-other", ms(-3), "GET", "/pool/9", models.LifetimeSession)
					owned := []*models.Mock{t1done, t1get, t1var, t1late, t2get, t2sh0, t2sh1}
					pool := []*models.Mock{pSess, pPT, pOther}
					poolNames := []string{"p-sess", "p-pt", "p-other"}
					var workerNames []string
					if vr.poolVars {
						pv1 := makeMovedClaimMock("pv-pt", ms(-2), "GET", "/item/8", models.LifetimePerTest)
						pv1.Spec.Variables = models.MockVariables{"v": {Recorded: "8", Use: []string{"url.path[1]"}}}
						pv2 := makeMovedClaimMock("pv-sess", ms(-1), "GET", "/item/9", models.LifetimeSession)
						pv2.Spec.Variables = models.MockVariables{"w": {Recorded: "9", Use: []string{"url.path[1]"}}}
						if vr.workerVars {
							// Stage pv-pt in filtered (per-test tree) and pv-sess in
							// unfiltered (session tree), both exposed via worker scope
							// so SharedMocks() populates both varPerTest and varSession.
							owned = append(owned, pv1)
							pool = append(pool, pv2)
							poolNames = append(poolNames, "pv-pt", "pv-sess")
							workerNames = append(workerNames, "pv-pt", "pv-sess")
						} else {
							pool = append(pool, pv1, pv2)
							poolNames = append(poolNames, "pv-pt", "pv-sess")
						}
					}
					windows := []models.TestWindow{
						{TestCase: "T1", Start: ms(0).Add(-500 * time.Microsecond), End: ms(10)},
						{TestCase: "T2", Start: ms(11), End: ms(20)},
					}
					mm := NewMockManager(nil, nil, zap.NewNop())
					defer mm.Close()
					tr := learn.NewTracker()
					mm.SetServedObserver(tr.ServedMock)
					mm.SetTrackerSetAt(tr.SetAt)
					mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
					mm.SetMocksWithWindow(owned, pool, base.Add(-time.Hour), base.Add(time.Hour))
					mm.bumpStagingEpoch()
					mm.SeedRecordedWindows(windows)
					if len(workerNames) > 0 {
						mm.ServeExactWorker(workerNames)
					}
					by := map[string]*models.Mock{}
					for _, m := range mm.StagedMocks() {
						by[m.Name] = m
					}
					mm.ServeExactPool(models.PoolKeyNone, poolNames)
					mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })
					mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
					var cands []*models.Mock
					if vr.cands {
						cands = []*models.Mock{by["t2-sh0"], by["t2-sh1"]}
					}
					mm.ServeExactTestConcurrent("T1", []*models.Mock{by["t1-done"], by["t1-get"], by["t1-var"], by["t1-late"]}, cands)
					if !fast {
						disableRoundMeta(mm)
					}
					h := httpint.New(zap.NewNop())
					opts := models.OutgoingOptions{Learn: true, LearnPool: vr.learnPool}
					var answers, reports []string
					serve := func(method, path string) {
						body, rep, err := serveDiffCallWithReport(h, mm, tr, diffCall{method: method, path: path}.raw(), opts)
						if err != nil {
							body = "ERR " + err.Error()
						}
						answers = append(answers, method+" "+path+" -> "+body)
						reports = append(reports, rep)
					}
					serve("GET", "/item/0") // t1-done answered earlier in T1
					if vr.cands {
						serve("GET", "/shared/0") // t2-sh0 spent in T1
					}
					var once sync.Once
					moved := false
					mm.OnConsume(func(name string) {
						if name != "t1-get" {
							return
						}
						once.Do(func() {
							moved = true
							mm.ServeExactPool(models.PoolKeyNone, poolNames)
							mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })
							mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
							mm.ServeExactTestConcurrent("T2", []*models.Mock{by["t2-get"], by["t2-sh0"], by["t2-sh1"]}, nil)
						})
					})
					serve("GET", "/item/1") // picks t1-get; round moves under its claim
					mm.OnConsume(nil)
					if !moved {
						t.Fatalf("fast=%v: the claim of t1-get never ran (answers %v)", fast, answers)
					}
					serve("GET", "/item/1") // T2's own
					serve("GET", "/shared/1")
					states := mm.GetConsumedMocks()
					for i := range states {
						states[i].SortOrder = 0
						states[i].Timestamp = 0
					}
					sort.SliceStable(states, func(i, j int) bool { return states[i].Name < states[j].Name })
					return answers, reports, states
				}
				fa, fr, fs := run(true)
				ra, rr, rs := run(false)
				if !reflect.DeepEqual(fa, ra) {
					t.Errorf("answers differ:\nfast: %v\nref:  %v", fa, ra)
				}
				movedReports := 0
				for i := range fr {
					if fr[i] != "" {
						movedReports++
					}
					if fr[i] != rr[i] {
						t.Errorf("call %d (%s) miss report differs:\nfast: %s\nref:  %s", i, fa[i], fr[i], rr[i])
					}
				}
				if proven == "1" && movedReports == 0 {
					t.Fatalf("expected non-empty moved-miss report, got %v", fr)
				}
				if !reflect.DeepEqual(fs, rs) {
					t.Errorf("consumed states differ:\nfast: %+v\nref:  %+v", fs, rs)
				}
			})
		}
	}
}

// TestHTTPRecordLearnMovedUnderClaimPoolView verifies moved-round miss
// diagnostics on the REAL *MockManager in a pool view (between tests) when T1
// opens under a pool recording's claim, including per-test and session variable
// recordings in the pool view (pinning pool-view varPerTest and varSession
// reconstruction).
func TestHTTPRecordLearnMovedUnderClaimPoolView(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	for _, proven := range []string{"1", ""} {
		for _, learnPool := range []bool{true, false} {
			for _, consumed := range []bool{false, true} {
				name := fmt.Sprintf("proven=%s/learnPool=%v/consumedFirst=%v", proven, learnPool, consumed)
				t.Run(name, func(t *testing.T) {
					t.Setenv(provenswitch.Env, proven)
					run := func(fast bool) ([]string, []string, []models.MockState) {
						t1get := makeMovedClaimMock("t1-get", ms(2), "GET", "/item/1", models.LifetimePerTest)
						t1b := makeMovedClaimMock("t1-b", ms(3), "GET", "/item/3", models.LifetimePerTest)
						// Staged in filtered and listed in poolNames so GetPerTestMocksInWindow
						// returns a variable-bearing per-test mock in the pool view (pins varPerTest).
						pVarPT := makeMovedClaimMock("p-var-pt", ms(-7), "GET", "/item/6", models.LifetimePerTest)
						pVarPT.Spec.Variables = models.MockVariables{"vp": {Recorded: "6", Use: []string{"url.path[1]"}}}
						pA := makeMovedClaimMock("p-a", ms(-6), "GET", "/item/1", models.LifetimePerTest)
						pB := makeMovedClaimMock("p-b", ms(-5), "GET", "/item/1", models.LifetimePerTest)
						pS := makeMovedClaimMock("p-s", ms(-4), "GET", "/item/1", models.LifetimeSession)
						pVar := makeMovedClaimMock("p-var", ms(-3), "GET", "/item/7", models.LifetimePerTest)
						pVar.Spec.Variables = models.MockVariables{"v": {Recorded: "7", Use: []string{"url.path[1]"}}}
						// Two LifetimePerTest warmup recordings so serving them in the pool
						// view does not bump StagingGen via updateUnFilteredTier, allowing
						// poolIx's two-call build gate to fire for both consumedFirst modes.
						pWarm1 := makeMovedClaimMock("p-warm-1", ms(-2), "GET", "/pool/9", models.LifetimePerTest)
						pWarm2 := makeMovedClaimMock("p-warm-2", ms(-1), "GET", "/pool/9", models.LifetimePerTest)
						owned := []*models.Mock{t1get, t1b, pVarPT}
						pool := []*models.Mock{pA, pB, pS, pVar, pWarm1, pWarm2}
						windows := []models.TestWindow{{TestCase: "T1", Start: ms(0).Add(-500 * time.Microsecond), End: ms(10)}}
						mm := NewMockManager(nil, nil, zap.NewNop())
						defer mm.Close()
						tr := learn.NewTracker()
						mm.SetServedObserver(tr.ServedMock)
						mm.SetTrackerSetAt(tr.SetAt)
						mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
						mm.SetMocksWithWindow(owned, pool, base.Add(-time.Hour), base.Add(time.Hour))
						mm.bumpStagingEpoch()
						mm.SeedRecordedWindows(windows)
						by := map[string]*models.Mock{}
						for _, m := range mm.StagedMocks() {
							by[m.Name] = m
						}
						poolNames := []string{"p-var-pt", "p-a", "p-b", "p-s", "p-var", "p-warm-1", "p-warm-2"}
						mm.ServeExactPool(models.PoolKeyNone, poolNames)
						mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })
						if !fast {
							disableRoundMeta(mm)
						}
						h := httpint.New(zap.NewNop())
						opts := models.OutgoingOptions{Learn: true, LearnPool: learnPool}
						var answers, reports []string
						serve := func(method, path string) {
							body, rep, err := serveDiffCallWithReport(h, mm, tr, diffCall{method: method, path: path}.raw(), opts)
							if err != nil {
								body = "ERR " + err.Error()
							}
							answers = append(answers, method+" "+path+" -> "+body)
							reports = append(reports, rep)
						}
						// Two calls of the pool view first: pool index builds on its
						// second call at one generation.
						serve("GET", "/pool/9")
						serve("GET", "/pool/9")
						if consumed {
							serve("GET", "/item/1") // p-a spent in the pool view
						}
						var once sync.Once
						moved := false
						mm.OnConsume(func(name string) {
							if !strings.HasPrefix(name, "p-") || strings.HasPrefix(name, "p-warm-") {
								return
							}
							once.Do(func() {
								moved = true
								mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
								mm.ServeExactTest("T1", []*models.Mock{by["t1-get"], by["t1-b"]})
							})
						})
						serve("GET", "/item/1") // a pool recording; T1 opens under its claim
						mm.OnConsume(nil)
						if !moved {
							t.Fatalf("fast=%v: no pool claim ran (answers %v)", fast, answers)
						}
						serve("GET", "/item/1")
						serve("GET", "/item/3")
						states := mm.GetConsumedMocks()
						for i := range states {
							states[i].SortOrder = 0
							states[i].Timestamp = 0
						}
						sort.SliceStable(states, func(i, j int) bool { return states[i].Name < states[j].Name })
						return answers, reports, states
					}
					fa, fr, fs := run(true)
					ra, rr, rs := run(false)
					if !reflect.DeepEqual(fa, ra) {
						t.Errorf("answers differ:\nfast: %v\nref:  %v", fa, ra)
					}
					movedReports := 0
					for i := range fr {
						if fr[i] != "" {
							movedReports++
						}
						if fr[i] != rr[i] {
							t.Errorf("call %d (%s) miss report differs:\nfast: %s\nref:  %s", i, fa[i], fr[i], rr[i])
						}
					}
					if proven == "1" && movedReports == 0 {
						t.Fatalf("expected non-empty moved-miss report, got %v", fr)
					}
					if !reflect.DeepEqual(fs, rs) {
						t.Errorf("consumed states differ:\nfast: %+v\nref:  %+v", fs, rs)
					}
				})
			}
		}
	}
}

// TestHTTPRecordLearnConcurrentIdenticalOwn fires more concurrent identical
// calls than the open test recorded, verifying each recording answers exactly
// once with zero skips or double claims on both fast and reference paths.
func TestHTTPRecordLearnConcurrentIdenticalOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, fast := range []bool{true, false} {
		for rep := 0; rep < 20; rep++ {
			const k = 12
			base := time.Unix(1_700_000_000, 0)
			call := diffCall{method: "POST", path: "/same", body: `{"k":"dup"}`, hdr: [][2]string{{"X-Id", "i1"}}}
			owned := make([]*models.Mock, k)
			for i := 0; i < k; i++ {
				m := recScaleMock("identical", i, base)
				m.Name = fmt.Sprintf("s-%02d", i)
				m.Spec.HTTPReq.Header["X-Id"] = "i1"
				m.Spec.HTTPReq.Body = call.body
				m.Spec.HTTPReq.Header["Content-Length"] = strconv.Itoa(len(call.body))
				owned[i] = m
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			tr := learn.NewTracker()
			mm.SetServedObserver(tr.ServedMock)
			mm.SetTrackerSetAt(tr.SetAt)
			start, end := base.Add(-time.Second), base.Add(time.Minute)
			mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
			mm.SetMocksWithWindow(owned, nil, start, end)
			mm.bumpStagingEpoch()
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "T1", Start: start, End: end}})
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.Begin() })
			mm.ServeExactTest("T1", mm.StagedMocks())
			if !fast {
				disableRoundMeta(mm)
			}
			h := httpint.New(zap.NewNop())
			opts := models.OutgoingOptions{Learn: true}
			var mu sync.Mutex
			got := map[string]int{}
			misses := 0
			var wg sync.WaitGroup
			for i := 0; i < k+3; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					body, err := serveDiffCall(h, mm, tr, call.raw(), opts)
					mu.Lock()
					defer mu.Unlock()
					if err != nil || strings.Contains(body, "no matching mock") {
						misses++
						return
					}
					got[body]++
				}()
			}
			wg.Wait()
			mm.Close()
			if len(got) != k || misses != 3 {
				t.Fatalf("fast=%v rep %d: %d distinct answers (want %d), %d misses (want 3): %v", fast, rep, len(got), k, misses, got)
			}
			for b, n := range got {
				if n != 1 {
					t.Fatalf("fast=%v rep %d: answer %s served %d times", fast, rep, b, n)
				}
			}
		}
	}
}

// TestRoundMetaMatchesRoundSnapshot verifies that MockManager.RoundMeta and
// scopedMockDb.RoundMeta agree with RoundSnapshot across EVERY round phase
// (startup, in-test, between, between-with-ahead, boundary/store-ended-to-pool,
// boundary/store-ended-to-next-test, boundary/view-alone, boundary/tracker-ahead-pool,
// boundary/tracker-ahead-test, boundary/set-unstaged), respect the onRead test
// seam, track per-mock TakeVoided, bump StagingGenerationByKind(HTTP) and
// re-serve restored pool recordings on RestartExactKey, and advance
// unspentFrom safely across leading consumed prefixes only.
func TestRoundMetaMatchesRoundSnapshot(t *testing.T) {
	s := newRoundStore(t)
	check := func(label string) integrations.RoundMeta {
		t.Helper()
		snap, sok := s.mm.RoundSnapshot(integrations.RoundQuery{Tracker: s.tr})
		meta, mok := s.mm.RoundMeta(s.tr)
		if sok != mok {
			t.Fatalf("%s: ok mismatch snap=%v meta=%v", label, sok, mok)
		}
		if !mok {
			return integrations.RoundMeta{}
		}
		if meta.At != snap.At || meta.Phase != snap.Phase || meta.Open != snap.Open ||
			meta.Test != snap.Test || meta.Ahead != snap.Ahead || meta.Fired != snap.Fired ||
			meta.Background != snap.Background || meta.Staging != snap.Staging ||
			meta.Ended != snap.Ended || meta.ViewGen() != snap.ViewGen() ||
			meta.NoOneRound() != snap.NoOneRound() || meta.ViewAlone() != snap.ViewAlone() ||
			meta.TrackerAhead() != snap.TrackerAhead() || meta.SetUnstaged() != snap.SetUnstaged() ||
			meta.PoolsCall() != snap.PoolsCall() || meta.ExactBackground() != snap.ExactBackground() ||
			meta.Learn.Bound != snap.Learn.Bound || meta.Learn.Open != snap.Learn.Open ||
			meta.Learn.Number != snap.Learn.Number || meta.Learn.SetAhead != snap.Learn.SetAhead ||
			meta.Learn.Awaiting != snap.Learn.Awaiting {
			t.Fatalf("%s: meta=%+v != snap=%+v", label, meta, snap)
		}
		// Verify CallRound metadata and Held(name) parity across every staged
		// recording plus an unstaged name in every phase.
		mcr, scr := meta.CallRound(), snap.CallRound()
		if mcr.Epoch() != scr.Epoch() || mcr.Test() != scr.Test() ||
			mcr.Staging() != scr.Staging() || mcr.HoldsEveryRecording() != scr.HoldsEveryRecording() {
			t.Fatalf("%s: CallRound mismatch meta=%+v snap=%+v", label, mcr, scr)
		}
		for name := range s.ms {
			if mcr.Held(name) != scr.Held(name) {
				t.Fatalf("%s: CallRound.Held(%s) meta=%v snap=%v", label, name, mcr.Held(name), scr.Held(name))
			}
		}
		if mcr.Held("unstaged-other") != scr.Held("unstaged-other") {
			t.Fatalf("%s: CallRound.Held(unstaged-other) meta=%v snap=%v", label, mcr.Held("unstaged-other"), scr.Held("unstaged-other"))
		}
		// Verify scopedMockDb forwards RoundMeta identically.
		sc := &scopedMockDb{MockMemDb: s.mm}
		smeta, smok := sc.RoundMeta(s.tr)
		if !smok || smeta.At != meta.At || smeta.Phase != meta.Phase || smeta.Test != meta.Test {
			t.Fatalf("%s: scopedMockDb.RoundMeta mismatch: %+v vs %+v", label, smeta, meta)
		}
		// Verify InBackground, InPoolSlice, IsWorker, InPool, InFlightIntoOpenTest,
		// and window helpers match RoundSnapshot for every mock.
		for name, mk := range s.ms {
			if meta.InBackground(name) != snap.InBackground(name) {
				t.Fatalf("%s: InBackground(%s) meta=%v snap=%v", label, name, meta.InBackground(name), snap.InBackground(name))
			}
			if meta.InPoolSlice(name) != snap.InPoolSlice(name) {
				t.Fatalf("%s: InPoolSlice(%s) meta=%v snap=%v", label, name, meta.InPoolSlice(name), snap.InPoolSlice(name))
			}
			if meta.IsWorker(name) != snap.IsWorker(name) {
				t.Fatalf("%s: IsWorker(%s) meta=%v snap=%v", label, name, meta.IsWorker(name), snap.IsWorker(name))
			}
			mp, mpk := meta.InPool(mk)
			sp, spk := snap.InPool(mk)
			if mp != sp || mpk != spk {
				t.Fatalf("%s: InPool(%s) meta=(%v,%v) snap=(%v,%v)", label, name, mp, mpk, sp, spk)
			}
			if meta.InFlightIntoOpenTest(mk) != snap.InFlightIntoOpenTest(mk) {
				t.Fatalf("%s: InFlightIntoOpenTest(%s) meta=%v snap=%v", label, name, meta.InFlightIntoOpenTest(mk), snap.InFlightIntoOpenTest(mk))
			}
			mb, mbk := meta.BeforeTestWindow(mk.Spec.ReqTimestampMock)
			sb, sbk := snap.BeforeTestWindow(mk.Spec.ReqTimestampMock)
			if mb != sb || mbk != sbk {
				t.Fatalf("%s: BeforeTestWindow(%s) meta=(%v,%v) snap=(%v,%v)", label, name, mb, mbk, sb, sbk)
			}
		}
		return meta
	}

	// 1. Before any view is installed (!ok), then RoundStartUp once pool view is installed
	check("no-view")
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if m := check("startup"); m.Phase != integrations.RoundStartUp {
		t.Fatalf("startup: phase=%v, want RoundStartUp", m.Phase)
	}

	// 2. RoundBoundary / TrackerAhead on pool view (PoolsCall == true)
	s.begin()
	if m := check("boundary-tracker-ahead-pool"); !m.TrackerAhead() || !m.PoolsCall() {
		t.Fatalf("boundary-tracker-ahead-pool: TrackerAhead=%v PoolsCall=%v, want true/true", m.TrackerAhead(), m.PoolsCall())
	}

	// 3. RoundInTest (including shared/in-flight/shared-taken owners via ServeExactTestConcurrent)
	s.mm.ServeExactTest("test-a", []*models.Mock{s.ms["own"]})
	if m := check("in-test"); m.Phase != integrations.RoundInTest || m.ViewAlone() != "" || m.NoOneRound() != "" {
		t.Fatalf("in-test: phase=%v viewAlone=%q noOne=%q", m.Phase, m.ViewAlone(), m.NoOneRound())
	} else if cr := m.CallRound(); !cr.Held("own") || cr.Held("other") {
		t.Fatalf("in-test: CallRound.Held(own)=%v Held(other)=%v, want true/false", cr.Held("own"), cr.Held("other"))
	}
	// Shared/candidate and in-flight recordings must also be held identically by
	// RoundMeta.CallRound().Held and RoundSnapshot.CallRound().Held.
	s.mm.ServeExactTestConcurrent("test-a", []*models.Mock{s.ms["own"]}, []*models.Mock{s.ms["own2"], s.ms["other"]})
	if m := check("in-test-concurrent-shared"); !m.CallRound().Held("own") || !m.CallRound().Held("own2") || !m.CallRound().Held("other") || m.CallRound().Held("pool") {
		t.Fatalf("in-test-concurrent-shared: unexpected Held flags: own=%v own2=%v other=%v pool=%v",
			m.CallRound().Held("own"), m.CallRound().Held("own2"), m.CallRound().Held("other"), m.CallRound().Held("pool"))
	}
	s.mm.ServeExactTest("test-a", []*models.Mock{s.ms["own"]})

	// TakeVoided must mark the voided mock via meta.IsVoided (when held by the
	// view) without flagging unrelated mocks.
	s.mm.exact.round.mu.Lock()
	s.mm.takeLocked(integrations.Take{How: integrations.TakeVoided, Mock: s.ms["own"]})
	s.mm.takeLocked(integrations.Take{How: integrations.TakeVoided, Mock: &models.Mock{Name: "elsewhere"}})
	s.mm.exact.round.mu.Unlock()
	vmeta, vok := s.mm.RoundMeta(s.tr)
	if !vok || !vmeta.IsVoided("own") || vmeta.IsVoided("elsewhere") || vmeta.IsVoided("other") {
		t.Fatalf("after TakeVoided: ok=%v own=%v elsewhere=%v other=%v, want true/true/false/false",
			vok, vmeta.IsVoided("own"), vmeta.IsVoided("elsewhere"), vmeta.IsVoided("other"))
	}

	// 4. RoundBoundary / Ended != "" with Test != "" (store moved to next test before tracker ended)
	s.mm.ServeExactTest("test-b", []*models.Mock{s.ms["next"]})
	if m := check("boundary-store-ended-to-next-test"); m.NoOneRound() == "" {
		t.Fatalf("boundary-store-ended-to-next-test: NoOneRound empty")
	}

	// 5. RoundBoundary / Ended != "" with Test == "" (store moved to pool before tracker ended)
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"])
	if m := check("boundary-store-ended-to-pool"); m.NoOneRound() == "" {
		t.Fatalf("boundary-store-ended-to-pool: NoOneRound empty")
	}

	// 6. RoundBetween (with ahead)
	s.end()
	if m := check("between-with-ahead"); m.Phase != integrations.RoundBetween {
		t.Fatalf("between-with-ahead: phase=%v, want RoundBetween", m.Phase)
	}

	// 7. RoundBetween (plain pool, no ahead)
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if m := check("between-plain"); m.Phase != integrations.RoundBetween || !m.InPoolSlice("pool") {
		t.Fatalf("between-plain: phase=%v inPoolSlice=%v, want RoundBetween/true", m.Phase, m.InPoolSlice("pool"))
	}

	// 8. RoundBoundary / ViewAlone (store shows test view while tracker has no test open)
	s.mm.ServeExactTest("test-c", []*models.Mock{s.ms["own"]})
	if m := check("boundary-view-alone"); m.ViewAlone() == "" {
		t.Fatalf("boundary-view-alone: ViewAlone empty, want non-empty boundary wording")
	}

	// 9. RoundBoundary / TrackerAhead on previous test's view (PoolsCall == false)
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	s.begin()
	s.mm.ServeExactTest("test-d", []*models.Mock{s.ms["own"]})
	check("in-test-d")
	s.end()
	s.begin()
	if m := check("boundary-tracker-ahead-test"); !m.TrackerAhead() || m.PoolsCall() {
		t.Fatalf("boundary-tracker-ahead-test: TrackerAhead=%v PoolsCall=%v, want true/false", m.TrackerAhead(), m.PoolsCall())
	}

	// 10. RoundBoundary / SetUnstaged (tracker began NewSet that store has not staged)
	s.mm.LearnBoundary(s.tr, func(lt *learn.Tracker) { lt.NewSet() })
	if m := check("boundary-set-unstaged"); !m.SetUnstaged() || m.NoOneRound() == "" {
		t.Fatalf("boundary-set-unstaged: SetUnstaged=%v NoOneRound=%q, want true/non-empty", m.SetUnstaged(), m.NoOneRound())
	}

	// RestartExactKey must bump StagingGenerationByKind(HTTP) (meta.StagingGen)
	// and meta.At.Gen so both pool cursors and learnIx re-key immediately.
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	genBefore, _ := s.mm.RoundMeta(s.tr)
	httpGenBefore := s.mm.StagingGenerationByKind(models.HTTP)
	s.mm.RestartExactKey("k", []string{"pool"})
	genAfter, _ := s.mm.RoundMeta(s.tr)
	httpGenAfter := s.mm.StagingGenerationByKind(models.HTTP)
	if genAfter.At.Gen == genBefore.At.Gen {
		t.Fatalf("RestartExactKey did not move meta.At.Gen: before=%d after=%d", genBefore.At.Gen, genAfter.At.Gen)
	}
	if httpGenAfter == httpGenBefore || genAfter.StagingGen == genBefore.StagingGen {
		t.Fatalf("RestartExactKey did not bump HTTP staging generation: httpGen %d->%d meta.StagingGen %d->%d",
			httpGenBefore, httpGenAfter, genBefore.StagingGen, genAfter.StagingGen)
	}

	// onRead seam must cause RoundMeta to return false so interleaving tests
	// continue driving RoundSnapshot.
	s.mm.exact.round.mu.Lock()
	s.mm.exact.round.onRead = func(string) {}
	s.mm.exact.round.mu.Unlock()
	if _, ok := s.mm.RoundMeta(s.tr); ok {
		t.Fatal("RoundMeta returned ok=true with onRead seam installed")
	}
	s.mm.exact.round.mu.Lock()
	s.mm.exact.round.onRead = nil
	s.mm.exact.round.mu.Unlock()
	if _, ok := s.mm.RoundMeta(s.tr); !ok {
		t.Fatal("RoundMeta returned ok=false after clearing onRead seam")
	}

	// exactView.unspentFrom must advance nextFrom across leading consumed
	// entries only, never past an unconsumed entry claimed by another test
	// (even when followed by another consumed entry), both directly and via posMap.
	var ep atomic.Uint64
	ep.Store(7)
	claims := &exactClaims{}
	claims.claim(7, "c1", "other-test")
	v := &exactView{
		test:     "my-test",
		epoch:    &ep,
		owned:    []*models.Mock{{Name: "c0"}, {Name: "c1"}, {Name: "c2"}, {Name: "c3"}},
		consumed: []bool{true, false, true, false},
		cand:     []bool{false, true, false, false},
		claims:   claims,
	}
	idx, nextFrom, ok := v.unspentFrom(nil, []int{0, 1, 2, 3}, 0)
	if !ok || idx != 3 || nextFrom != 1 {
		t.Fatalf("unspentFrom(direct) = (%d, %d, %v), want (3, 1, true)", idx, nextFrom, ok)
	}
	idx2, nextFrom2, ok2 := v.unspentFrom([]int{0, 1, 2, 3}, []int{0, 1, 2, 3}, 0)
	if !ok2 || idx2 != 3 || nextFrom2 != 1 {
		t.Fatalf("unspentFrom(posMap) = (%d, %d, %v), want (3, 1, true)", idx2, nextFrom2, ok2)
	}
}

// TestHTTPRecordLearnRestartExactKeyRekeysPoolCursor pins RestartExactKey
// end-to-end on the real *MockManager: a pool recording consumed on the fast
// path, then restored by RestartExactKey, must be served again on the very
// next call (never skipped by the old pool index's advanced cursor).
func TestHTTPRecordLearnRestartExactKeyRekeysPoolCursor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Unix(1_700_000_000, 0)
	p0 := makeMovedClaimMock("p-0", base.Add(-3*time.Millisecond), "GET", "/item/1", models.LifetimePerTest)
	p1 := makeMovedClaimMock("p-1", base.Add(-2*time.Millisecond), "GET", "/item/1", models.LifetimePerTest)
	p2 := makeMovedClaimMock("p-2", base.Add(-1*time.Millisecond), "GET", "/item/1", models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	tr := learn.NewTracker()
	mm.SetServedObserver(tr.ServedMock)
	mm.SetTrackerSetAt(tr.SetAt)
	mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.NewSet() })
	mm.SetMocksWithWindow(nil, []*models.Mock{p0, p1, p2}, base.Add(-time.Hour), base.Add(time.Hour))
	mm.bumpStagingEpoch()
	mm.SeedRecordedWindows(nil)
	mm.ServeExactPool("key-a", []string{"p-0", "p-1", "p-2"})
	mm.LearnBoundary(tr, func(lt *learn.Tracker) { lt.End() })

	h := httpint.New(zap.NewNop())
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	req := diffCall{method: "GET", path: "/item/1"}.raw()

	// Call 1 (1st call at gen -> fallback, serves p-0), Call 2 (builds pool index,
	// serves p-1, advances pool cursor past p-0).
	b0, _, err := serveDiffCallWithReport(h, mm, tr, req, opts)
	if err != nil || b0 != `{"m":"p-0"}` {
		t.Fatalf("call 1 = (%q, %v), want p-0", b0, err)
	}
	b1, _, err := serveDiffCallWithReport(h, mm, tr, req, opts)
	if err != nil || b1 != `{"m":"p-1"}` {
		t.Fatalf("call 2 = (%q, %v), want p-1", b1, err)
	}

	// RestartExactKey restores p-0 and p-1 and bumps HTTP staging generation.
	// The very next call MUST serve p-0 again (never skip to p-2 via stale cursor).
	mm.RestartExactKey("key-a", []string{"p-0", "p-1"})
	b2, _, err := serveDiffCallWithReport(h, mm, tr, req, opts)
	if err != nil || b2 != `{"m":"p-0"}` {
		t.Fatalf("after RestartExactKey = (%q, %v), want restored p-0", b2, err)
	}
}
