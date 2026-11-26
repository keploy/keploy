package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// scaleBgProxy forwards SeedBackgroundRecordings into a real MockManager so
// scale tests measure actual MockManager.background residency and run real
// parser background-miss matching against it.
type scaleBgProxy struct {
	pacingProxy
	mm *proxyPkg.MockManager
}

func (p *scaleBgProxy) SeedBackgroundRecordings(recs []models.BackgroundRecording) {
	p.mm.SeedBackgroundRecordings(recs)
}

func probeHTTPMiss(t *testing.T, mm *proxyPkg.MockManager, host, path string) *models.MockMismatchReport {
	t.Helper()
	return probeHTTPMissWithCtx(t, context.Background(), mm, host, path)
}

func probeHTTPMissWithCtx(t *testing.T, ctx context.Context, mm *proxyPkg.MockManager, host, path string) *models.MockMismatchReport {
	t.Helper()
	var reported error
	ctx = models.WithMockMismatchReporter(ctx, func(err error) { reported = err })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- httpparser.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: "127.0.0.1:8200"}, mm, models.OutgoingOptions{})
	}()
	raw := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Go-http-client/1.1\r\n\r\n", path, host)
	if _, err := io.WriteString(app, raw); err != nil {
		t.Fatalf("write request %s: %v", path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("read response %s: %v", path, err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("parser did not return after miss for %s", path)
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
		t.Fatalf("miss for %s reported %v without MismatchReport", path, reported)
	}
	return withReport.MismatchReport()
}

func scaleClockCtx(proven bool, setAt, now time.Time) context.Context {
	realNow := time.Now()
	clock := func(real time.Time) (time.Time, bool) {
		return now.Add(real.Sub(realNow)), true
	}
	if proven {
		tr := learn.NewTracker()
		tr.SetAppClock(clock)
		tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return setAt, true })
		tr.NewSetAt(setAt)
		return learn.WithTracker(context.Background(), tr)
	}
	sc := &learn.ServeClock{}
	sc.SetClock(clock)
	sc.NewSetAt(setAt)
	return learn.WithServeClock(context.Background(), sc)
}

// TestDerivedBackgroundRecordings_BoundedMemoryAndSinglePass reproduces the
// 12-hour continuous-recording shape: 20,000+ outside-window mocks parked in
// DiskMocks alongside 11 late test windows. Derivation MUST stream in a single
// pass without double-scanning DiskMocks, MUST bound MockManager.background
// residency (count and live heap delta) well below OOM thresholds, and MUST
// preserve background-miss matching semantics in both KEPLOY_PROVEN modes.
func TestDerivedBackgroundRecordings_BoundedMemoryAndSinglePass(t *testing.T) {
	const (
		totalOutsideMocks = 20000
		numLateTests      = 11
		host              = "svc.internal:8200"
		maxRetainedRecs   = 512
		maxLiveHeapDelta  = int64(16 << 20) // 16 MiB ceiling vs 50-150+ MiB unbounded
	)

	// 3 KiB request body and 3 KiB response body stay below DiskMocks's 8 KiB
	// response-spill threshold so every retained mock carries realistic decoded
	// payload weight if held unboundedly in RAM.
	reqPad := strings.Repeat("r", 3*1024)
	respPad := strings.Repeat("b", 3*1024)

	mkHTTP := func(name, path string, at time.Time, withBody bool) *models.Mock {
		reqBody := ""
		respBody := `{"ok":true}`
		if withBody {
			reqBody = `{"pad":"` + reqPad + `"}`
			respBody = `{"pad":"` + respPad + `"}`
		}
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    name,
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "mocks",
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					"destAddr":                 "127.0.0.1:8200",
				},
				HTTPReq: &models.HTTPReq{
					Method: models.Method("GET"),
					URL:    "http://" + host + path,
					Header: map[string]string{
						"Host":       host,
						"User-Agent": "Go-http-client/1.1",
					},
					Body: reqBody,
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "application/json"},
					Body:       respBody,
				},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(2 * time.Millisecond),
			},
		}
	}

	mkHTTP2 := func(name, path string, at time.Time) *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    name,
			Kind:    models.HTTP2,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "mocks",
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					"destAddr":                 "127.0.0.1:9090",
				},
				HTTP2Req: &models.HTTP2Req{
					Method:    models.Method("POST"),
					Authority: "grpc.internal:9090",
					URL:       path,
					Headers:   map[string]string{"content-type": "application/grpc"},
					Body:      reqPad,
				},
				HTTP2Resp: &models.HTTP2Resp{
					StatusCode: 200,
					Body:       respPad,
				},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(2 * time.Millisecond),
			},
		}
	}

	mkMongo := func(name, section string, at time.Time) *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    name,
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "mocks",
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					"destAddr":                 "127.0.0.1:27017",
				},
				MongoRequests: []models.MongoRequest{
					{Message: &models.MongoOpMessage{Sections: []string{section}}},
				},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(2 * time.Millisecond),
			},
		}
	}

	for _, proven := range []string{"", "1"} {
		mode := "proven=unset"
		if proven == "1" {
			mode = "proven=1"
		}
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)

			disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatalf("NewDiskMocks: %v", err)
			}
			t.Cleanup(func() { _ = disk.Close() })

			base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

			// 1. Hour 0 boot call (must survive high-cardinality traffic).
			bootInit := mkHTTP("boot-init", "/v1/sys/init", base.Add(1*time.Second), false)
			bootInit.DeriveLifetime()
			if err := disk.Add(bootInit); err != nil {
				t.Fatalf("add boot-init: %v", err)
			}

			// 2. 19,999 outside-window mocks spanning Hours 0..10:
			//    - periodic HTTP health polls (/v1/sys/health)
			//    - periodic epoch-bearing HTTP polls (/v1/worker/poll?since=<epoch>)
			//    - high-cardinality unique HTTP paths (/api/v1/orders/<id>)
			//    - high-cardinality unique HTTP2 paths (/svc.Orders/Get/<id>)
			//    - periodic Mongo background polls (findAndModify ptw_endpoints)
			const mongoPoll = `{ SectionSingle msg: {"findAndModify":"ptw_endpoints","new":true,"query":{"dirtyAt":{"$gt":{"$numberLong":"0"}}}} }`
			for i := 1; i < totalOutsideMocks; i++ {
				at := base.Add(time.Duration(i+1) * time.Second)
				var m *models.Mock
				switch i % 5 {
				case 0:
					m = mkHTTP(fmt.Sprintf("health-%d", i), "/v1/sys/health", at, false)
				case 1:
					m = mkHTTP(fmt.Sprintf("epoch-poll-%d", i), fmt.Sprintf("/v1/worker/poll?since=%d", at.Unix()), at, false)
				case 2:
					m = mkHTTP(fmt.Sprintf("order-http-%d", i), fmt.Sprintf("/api/v1/orders/%d", i), at, true)
				case 3:
					m = mkHTTP2(fmt.Sprintf("order-h2-%d", i), fmt.Sprintf("/svc.Orders/Get/%d", i), at)
				case 4:
					m = mkMongo(fmt.Sprintf("mongo-poll-%d", i), mongoPoll, at)
				}
				m.DeriveLifetime()
				if err := disk.Add(m); err != nil {
					t.Fatalf("add mock %d: %v", i, err)
				}
			}

			// 3. 11 late test windows in Hour 11, each with an in-window mock,
			//    plus late between-test background polls of HTTP, HTTP2, Mongo.
			hour11 := base.Add(11 * time.Hour)
			windows := make([]models.TestWindow, 0, numLateTests)
			inWindowNames := make(map[string]bool, numLateTests)
			var test1MockAt, test3MockAt, test5MockAt time.Time
			for i := 0; i < numLateTests; i++ {
				wStart := hour11.Add(time.Duration(i*10) * time.Second)
				wEnd := wStart.Add(4 * time.Second)
				windows = append(windows, models.TestWindow{
					TestCase: fmt.Sprintf("tc-%d", i+1),
					Start:    wStart,
					End:      wEnd,
				})
				name := fmt.Sprintf("in-test-%d", i+1)
				inWindowNames[name] = true
				at := wStart.Add(1 * time.Second)
				switch i {
				case 0:
					test1MockAt = at
				case 2:
					test3MockAt = at
				case 4:
					test5MockAt = at
				}
				m := mkHTTP(name, fmt.Sprintf("/api/v1/test-%d", i+1), at, false)
				m.DeriveLifetime()
				if err := disk.Add(m); err != nil {
					t.Fatalf("add in-test mock %d: %v", i+1, err)
				}
			}

			lateHTTP := mkHTTP("late-http-poll", "/v1/late-poll", hour11.Add(6*time.Second), false)
			lateHTTP2 := mkHTTP2("late-http2-poll", "/svc.Health/Check", hour11.Add(26*time.Second))
			lateMongo := mkMongo("late-mongo-poll", mongoPoll, hour11.Add(46*time.Second))
			for _, m := range []*models.Mock{lateHTTP, lateHTTP2, lateMongo} {
				m.DeriveLifetime()
				if err := disk.Add(m); err != nil {
					t.Fatalf("add %s: %v", m.Name, err)
				}
			}
			disk.Finalize()

			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			// Stage an empty epoch so BackgroundRecordingsIn matches live misses.
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, hour11.Add(time.Hour))

			p := &scaleBgProxy{mm: mm}
			a := &Agent{logger: zap.NewNop(), Proxy: p}
			a.clientMocks.Store(uint64(0), &ClientMockStorage{diskMocks: disk})

			// Measure baseline live heap AFTER DiskMocks is populated.
			runtime.GC()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			staging := models.MockFilterParams{
				AfterTime:                   models.BaseTime,
				BeforeTime:                  hour11.Add(time.Hour),
				FirstRecordedTestStart:      windows[0].Start,
				RecordedWindows:             windows,
				DeriveBackgroundFromWindows: true,
			}
			if err := a.UpdateMockParams(context.Background(), staging); err != nil {
				t.Fatalf("UpdateMockParams staging: %v", err)
			}

			runtime.GC()
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(mm)
			runtime.KeepAlive(disk)

			liveDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			httpRecs := mm.BackgroundRecordings(models.HTTP)
			h2Recs := mm.BackgroundRecordings(models.HTTP2)
			mongoRecs := mm.BackgroundRecordings(models.Mongo)
			totalRetained := len(httpRecs) + len(h2Recs) + len(mongoRecs)
			reads := disk.RecordedReads()

			t.Logf("[%s] diskLen=%d recordedReads=%d retainedTotal=%d (http=%d http2=%d mongo=%d) beforeHeap=%d afterHeap=%d liveHeapDelta=%d (%.2f MiB)",
				mode, disk.Len(), reads, totalRetained, len(httpRecs), len(h2Recs), len(mongoRecs),
				before.HeapAlloc, after.HeapAlloc, liveDelta, float64(liveDelta)/(1<<20))

			// 1. Single-pass invariant: must NOT double-scan DiskMocks.
			if reads > int64(disk.Len()) {
				t.Errorf("DiskMocks was scanned more than once: recordedReads=%d > diskLen=%d", reads, disk.Len())
			}

			// 2. Hard entry ceiling invariant: must NOT retain all 20k outside-window mocks.
			if totalRetained > maxRetainedRecs {
				t.Errorf("MockManager.background retained %d recordings across kinds; ceiling is %d", totalRetained, maxRetainedRecs)
			}

			// 3. Hard live-heap delta ceiling invariant.
			if liveDelta > maxLiveHeapDelta {
				t.Errorf("live heap grew by %d bytes (%.2f MiB); ceiling is %d bytes (%.2f MiB)",
					liveDelta, float64(liveDelta)/(1<<20), maxLiveHeapDelta, float64(maxLiveHeapDelta)/(1<<20))
			}

			// 4. Semantic invariants:
			//    - In-window mocks must NEVER appear in BackgroundRecordings.
			//    - Hour-0 boot call, periodic health poll, epoch-tracked poll (>= MinEpochReads
			//      distinct timestamps with non-zero From), and Hour-11 between-test polls
			//      must all be present with exact From spans.
			byName := make(map[string]models.BackgroundRecording, totalRetained)
			var epochPollWithFrom int
			epochSeen := make(map[int64]bool)
			for _, list := range [][]models.BackgroundRecording{httpRecs, h2Recs, mongoRecs} {
				for _, r := range list {
					if r.Mock == nil {
						t.Fatalf("nil mock in BackgroundRecordings")
					}
					if inWindowNames[r.Mock.Name] {
						t.Errorf("in-window test mock %q leaked into BackgroundRecordings", r.Mock.Name)
					}
					byName[r.Mock.Name] = r
					if strings.HasPrefix(r.Mock.Name, "epoch-poll-") && !r.From.IsZero() {
						if !epochSeen[r.Mock.Spec.ReqTimestampMock.Unix()] {
							epochSeen[r.Mock.Spec.ReqTimestampMock.Unix()] = true
							epochPollWithFrom++
						}
					}
				}
			}

			if _, ok := byName["boot-init"]; !ok {
				t.Errorf("Hour-0 boot recording boot-init was evicted from BackgroundRecordings")
			}
			if epochPollWithFrom < learn.MinEpochReads {
				t.Errorf("epoch-poll retained %d distinct-second samples with non-zero From; need >= %d for learn.EpochTrack",
					epochPollWithFrom, learn.MinEpochReads)
			}
			for _, want := range []struct {
				name string
				from time.Time
			}{
				{"late-http-poll", test1MockAt},
				{"late-http2-poll", test3MockAt},
				{"late-mongo-poll", test5MockAt},
			} {
				r, ok := byName[want.name]
				if !ok {
					t.Errorf("late between-test background poll %q missing from BackgroundRecordings", want.name)
					continue
				}
				if !r.From.Equal(want.from) {
					t.Errorf("%q From = %v; want %v", want.name, r.From, want.from)
				}
			}

			// 5. End-to-end HTTP background miss matching against the bounded MockManager.
			for _, bgPath := range []string{"/v1/sys/init", "/v1/sys/health", "/v1/late-poll"} {
				rep := probeHTTPMiss(t, mm, host, bgPath)
				if !rep.Background || rep.CountsAgainstTest() {
					t.Errorf("live miss for background path %s got Background=%v CountsAgainstTest=%v; want true/false",
						bgPath, rep.Background, rep.CountsAgainstTest())
				}
			}
			ownRep := probeHTTPMiss(t, mm, host, "/api/v1/test-1")
			if ownRep.Background || !ownRep.CountsAgainstTest() {
				t.Errorf("live miss for in-window test path /api/v1/test-1 got Background=%v CountsAgainstTest=%v; want false/true",
					ownRep.Background, ownRep.CountsAgainstTest())
			}

			// 6. Live epoch-bearing background poll miss must still be recognized
			//    via learn.EpochTrack from the bounded per-shape representatives.
			liveNow := hour11.Add(2 * time.Hour)
			clockCtx := scaleClockCtx(proven == "1", time.Now().Add(-5*time.Second), liveNow)
			epochRep := probeHTTPMissWithCtx(t, clockCtx, mm, host, fmt.Sprintf("/v1/worker/poll?since=%d", liveNow.Unix()))
			if !epochRep.Background || epochRep.CountsAgainstTest() {
				t.Errorf("live epoch-bearing background poll miss got Background=%v CountsAgainstTest=%v; want true/false",
					epochRep.Background, epochRep.CountsAgainstTest())
			}
		})
	}
}

func TestDerivedBgCollector_ByteCeilingPerShapeAndNoResidentMutation(t *testing.T) {
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	mkHTTP := func(name, path, reqBody, respBody string, sec int) *models.Mock {
		return &models.Mock{
			Name: name,
			Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "mocks",
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
				},
				HTTPReq: &models.HTTPReq{
					Method: models.Method("POST"),
					URL:    "http://svc.internal:8080" + path,
					Header: map[string]string{"Host": "svc.internal:8080"},
					Body:   reqBody,
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Body:       respBody,
				},
				ReqTimestampMock: base.Add(time.Duration(sec) * time.Second),
				ResTimestampMock: base.Add(time.Duration(sec)*time.Second + time.Millisecond),
			},
		}
	}

	// 1. Resident mock responses must NOT be mutated by stripDerivedResponse.
	resident := mkHTTP("res-1", "/boot", "req", "important-resident-response", 1)
	col := newDerivedBgCollector(16, 16<<10, 3, 4, 4<<10)
	col.add(resident, time.Time{})
	recs := col.recordings()
	if len(recs) != 1 {
		t.Fatalf("retained %d recs; want 1", len(recs))
	}
	if recs[0].Mock.Spec.HTTPResp != nil {
		t.Fatalf("derived copy retained HTTPResp; want elided nil")
	}
	if resident.Spec.HTTPResp == nil || resident.Spec.HTTPResp.Body != "important-resident-response" {
		t.Fatalf("resident mock was mutated: %+v", resident.Spec.HTTPResp)
	}

	// 2. Per-shape cap keeps only the latest maxPerShape samples; byte cap and
	//    entry cap are never exceeded and byShape never leaks empty buckets.
	col2 := newDerivedBgCollector(12, 12<<10, 3, 3, 3<<10)
	// Seed 2 boot mocks into protected head reserve.
	col2.add(mkHTTP("boot-a", "/boot/a", "x", "resp", 1), time.Time{})
	col2.add(mkHTTP("boot-b", "/boot/b", "x", "resp", 2), base.Add(time.Second))
	// Push 50 copies of the same poll shape -> capped at 3.
	for i := 0; i < 50; i++ {
		col2.add(mkHTTP(fmt.Sprintf("poll-%d", i), fmt.Sprintf("/poll?t=%d", i), "p", "resp", 10+i), base.Add(time.Duration(9+i)*time.Second))
	}
	// Push 100 distinct shapes with 2 KiB request bodies -> strictly bounded by maxBytes (12 KiB) and maxEntries (12).
	bigReq := strings.Repeat("q", 2*1024)
	for i := 0; i < 100; i++ {
		col2.add(mkHTTP(fmt.Sprintf("uniq-%d", i), fmt.Sprintf("/uniq/%d", i), bigReq, "resp", 100+i), base.Add(time.Duration(99+i)*time.Second))
	}
	// Push one monster mock larger than maxBytes -> ignored cleanly without wiping boot prefix.
	col2.add(mkHTTP("monster", "/monster", strings.Repeat("m", 20*1024), "resp", 500), base.Add(499*time.Second))

	if col2.totalCount() > 12 {
		t.Fatalf("totalCount=%d exceeded maxEntries=12", col2.totalCount())
	}
	if col2.totalBytes > 12<<10 {
		t.Fatalf("totalBytes=%d exceeded maxBytes=%d", col2.totalBytes, 12<<10)
	}
	if len(col2.byShape) > col2.totalCount() {
		t.Fatalf("byShape leaked empty buckets: len(byShape)=%d > totalCount=%d", len(col2.byShape), col2.totalCount())
	}
	final := col2.recordings()
	names := make(map[string]bool, len(final))
	for _, r := range final {
		names[r.Mock.Name] = true
	}
	if !names["boot-a"] || !names["boot-b"] {
		t.Fatalf("protected boot prefix lost: %v", names)
	}
	if !names["uniq-99"] {
		t.Fatalf("most recent recording uniq-99 missing: %v", names)
	}
	if names["monster"] {
		t.Fatalf("oversized monster mock should not be admitted")
	}

	// 3. Outlier with size in (maxBytes - headBytes, maxBytes] must be rejected
	//    BEFORE any eviction, leaving both headList and tailList intact for
	//    both a brand-new shape and a shape already at maxPerShape in headList.
	col3 := newDerivedBgCollector(12, 16<<10, 2, 3, 4<<10)
	// Fill headList with 2 entries of shape /boot/dup (~1 KiB each) + 1 entry of /boot/other (~1 KiB) -> headBytes ~ 3 KiB.
	pad512 := strings.Repeat("h", 512)
	col3.add(mkHTTP("head-dup-1", "/boot/dup", pad512, "r", 1), time.Time{})
	col3.add(mkHTTP("head-dup-2", "/boot/dup", pad512, "r", 2), base.Add(time.Second))
	col3.add(mkHTTP("head-other", "/boot/other", pad512, "r", 3), base.Add(2*time.Second))
	// Seal headList and populate tailList with two recent polls.
	col3.add(mkHTTP("tail-1", "/tail/1", pad512, "r", 10), base.Add(9*time.Second))
	col3.add(mkHTTP("tail-2", "/tail/2", pad512, "r", 11), base.Add(10*time.Second))

	wantHeadCount := col3.headList.count
	wantTailCount := col3.tailList.count
	wantTotalBytes := col3.totalBytes
	if wantHeadCount != 3 || wantTailCount != 2 {
		t.Fatalf("precondition failed: headCount=%d tailCount=%d; want 3, 2", wantHeadCount, wantTailCount)
	}

	// Choose outlier body so estimated size is ~14.5 KiB: <= maxBytes (16 KiB)
	// but > maxBytes - headBytes (~13 KiB) AND > maxBytes - (headBytes - oneHead) (~14 KiB).
	outlierBody := strings.Repeat("o", 14*1024)
	// 3a. New shape outlier.
	col3.add(mkHTTP("outlier-new", "/outlier/new", outlierBody, "r", 20), base.Add(19*time.Second))
	// 3b. Same-shape outlier matching a full maxPerShape=2 bucket in headList.
	col3.add(mkHTTP("outlier-dup", "/boot/dup", outlierBody, "r", 21), base.Add(20*time.Second))

	if col3.headList.count != wantHeadCount || col3.tailList.count != wantTailCount || col3.totalBytes != wantTotalBytes {
		t.Fatalf("rejected outlier mutated collector: head=%d (want %d), tail=%d (want %d), bytes=%d (want %d)",
			col3.headList.count, wantHeadCount, col3.tailList.count, wantTailCount, col3.totalBytes, wantTotalBytes)
	}
	got3 := col3.recordings()
	got3Names := make(map[string]bool, len(got3))
	for _, r := range got3 {
		got3Names[r.Mock.Name] = true
	}
	for _, keep := range []string{"head-dup-1", "head-dup-2", "head-other", "tail-1", "tail-2"} {
		if !got3Names[keep] {
			t.Fatalf("existing entry %q was evicted by rejected outlier; got %v", keep, got3Names)
		}
	}
	if got3Names["outlier-new"] || got3Names["outlier-dup"] {
		t.Fatalf("outliers should not be admitted: %v", got3Names)
	}
}

func TestPrevCallTracker_MatchesBatchReference(t *testing.T) {
	at := func(sec int) time.Time { return bgT0.Add(time.Duration(sec) * time.Second) }

	mocks := []*models.Mock{
		{Name: "zero", Kind: models.HTTP},
		bgMock("c1", models.HTTP, 1, 2),
		bgMock("long", models.HTTP, 3, 20), // answered late at 20
		bgMock("c2", models.HTTP, 4, 4),
		bgMock("tie-a", models.HTTP, 6, 6),
		bgMock("tie-b", models.HTTP, 6, 6),     // same instant as tie-a
		bgMock("dns", models.DNS, 7, 7),        // protocol phase, ignored
		bgMock("inv", models.HTTP, 8, 5),       // inverted res < req
		bgMock("after-inv", models.HTTP, 9, 9), // previous is inv(8)
		bgMock("at-long-end", models.HTTP, 20, 21),
	}

	// Reference O(N log N) computation matching original previousCallAt.
	var calls []bgCallTime
	for _, mk := range mocks {
		if !mk.IsProtocolPhase() && !mk.Spec.ReqTimestampMock.IsZero() {
			calls = append(calls, bgCallTime{req: mk.Spec.ReqTimestampMock, res: mk.Spec.ResTimestampMock})
		}
	}
	refPrev := func(to time.Time) time.Time {
		var best time.Time
		for _, c := range calls {
			if c.req.Before(to) && !c.res.After(to) && (best.IsZero() || c.req.After(best)) {
				best = c.req
			}
		}
		return best
	}

	tracker := newPrevCallTracker()
	for _, mk := range mocks {
		got := tracker.advance(mk.Spec.ReqTimestampMock)
		tracker.recordCall(mk)
		want := refPrev(mk.Spec.ReqTimestampMock)
		if !got.Equal(want) {
			t.Errorf("mock %q (req=%v): tracker.advance=%v; want %v", mk.Name, mk.Spec.ReqTimestampMock, got, want)
		}
	}
	// Verify long(3,20) was retired by t=20: a subsequent call at 25 after only
	// long should see at-long-end(20), and a dedicated scenario verifies long
	// itself is chosen once its answer lands.
	t2 := newPrevCallTracker()
	t2.advance(at(1))
	t2.recordCall(bgMock("slow", models.HTTP, 1, 10))
	if got := t2.advance(at(5)); !got.IsZero() {
		t.Fatalf("while slow(1,10) is in flight at 5: got %v; want zero", got)
	}
	if got := t2.advance(at(10)); !got.Equal(at(1)) {
		t.Fatalf("once slow(1,10) answered at 10: got %v; want %v", got, at(1))
	}
}
