package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The HTTP replay scale shape: a recording of n calls to one upstream, half
// GET /item/K (one path per call) and half POST /item with a JSON body that
// differs per call, as an app's own JSON client writes them. Every mock is a
// per-test mock inside the serving window, as a mock replay stages the set.

const scalePad = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func scaleReq(i int) (method, path, body string) {
	if i%2 == 0 {
		return "GET", "/item/" + strconv.Itoa(i/2), ""
	}
	return "POST", "/item", fmt.Sprintf(`{"id":"b%05d-%d","pad":%q,"r":%d}`, i/8, i%8, scalePad, i/8)
}

func scaleMock(i int, at time.Time) *models.Mock {
	method, path, body := scaleReq(i)
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

func scaleRaw(i int) string {
	method, path, body := scaleReq(i)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: upstream\r\nUser-Agent: Go-http-client/1.1\r\n", method, path)
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\nContent-Type: application/json\r\n", len(body))
	}
	b.WriteString("Accept-Encoding: gzip\r\n\r\n")
	b.WriteString(body)
	return b.String()
}

// scaleCalls is how many calls each test of view "tests" makes: half of them
// GETs, half POSTs, as the workload this shape is taken from does.
const scaleCalls = 8

// scaleTestName is the name of test t of the set: view "test" has one, t 0,
// that owns every recording; view "tests" has n/scaleCalls, test t owning
// recordings scaleCalls*t to scaleCalls*t+scaleCalls-1.
func scaleTestName(t int) string { return fmt.Sprintf("TestScale/b%05d", t) }

// stageScale installs a fresh copy of the n-mock set as the serving window
// and returns its staged copies by position. Under the proven switch keploy
// test and keploy mock replay serve the set exactly: with view "test" or
// "tests" the set's recorded test windows are seeded and its pool (empty) is
// served, as the agent stages a set; each test is then opened by openScale.
// With view "none" no exact view is installed and every call reads the
// store, as with the switch off.
func stageScale(mm *MockManager, n int, at time.Time, view string) []*models.Mock {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = scaleMock(i, at)
	}
	start, end := at.Add(-time.Second), at.Add(time.Duration(n)*time.Millisecond+time.Second)
	mm.SetMocksWithWindow(mocks, nil, start, end)
	if view == "none" {
		return nil
	}
	var windows []models.TestWindow
	if view == "test" {
		windows = []models.TestWindow{{TestCase: scaleTestName(0), Start: start, End: end}}
	} else {
		for t := 0; t*scaleCalls < n; t++ {
			first := at.Add(time.Duration(t*scaleCalls) * time.Millisecond)
			windows = append(windows, models.TestWindow{TestCase: scaleTestName(t), Start: first.Add(-250 * time.Microsecond), End: first.Add(time.Duration(scaleCalls)*time.Millisecond - 250*time.Microsecond)})
		}
	}
	mm.SeedRecordedWindows(windows)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	byName := map[string]*models.Mock{}
	for _, m := range mm.StagedMocks() {
		byName[m.Name] = m
	}
	staged := make([]*models.Mock, n)
	for i := range staged {
		staged[i] = byName["mock-"+strconv.Itoa(i)]
	}
	return staged
}

// openScale opens test t of the staged set, as the agent opens a test when
// its first call comes: the pool served again (the previous test's end),
// then the test, owning its recordings (ServeExactTest).
func openScale(mm *MockManager, staged []*models.Mock, t, owns int) {
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest(scaleTestName(t), staged[t*owns:(t+1)*owns])
}

// serveScale sends recorded call i through the HTTP integration's replay path
// and returns the served response body.
func serveScale(h integrations.Integrations, mm integrations.MockMemDb, i int) (string, error) {
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agent.Close()
	}()
	go func() { _, _ = app.Write([]byte(scaleRaw(i))) }()
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		_ = app.Close()
		return "", fmt.Errorf("call %d: %w (integration: %v)", i, err, <-done)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	return string(body), err
}

// BenchmarkHTTPReplayPerCall is one replayed call against a staged set of n
// recordings.
//
// It runs under the proven switch off (KEPLOY_PROVEN unset: no exact view)
// and on. Under the switch, view=tests is a replay of n/8 recorded tests of 8
// calls each, as keploy mock replay runs a Go test of many subtests: the set
// staged once, each test opened at its first call (openScale, timed apart as
// view-ns and counted in ns/op), its calls made in recorded order; view=test
// is one test owning all n recordings; view=none installs no exact view, so
// every call reads the store (the path a miss takes). Without a view the set
// is restaged (untimed) every 256 calls, so the pool stays near n, and calls
// are taken in a stride through the set, so each one serves a different
// recording. The exact view's verify mode, which this package's rows run
// under (TestMain), compares every recording on every call by design and is
// off here: what is measured is what a replay pays.
//
// It reports the mean time of a GET (one recording per path) and of a POST
// (every POST recording shares the path) apart, as get-ns and post-ns.
func BenchmarkHTTPReplayPerCall(b *testing.B) {
	verify := exactViewVerify.Load()
	exactViewVerify.Store(false)
	b.Cleanup(func() { exactViewVerify.Store(verify) })
	modes := []struct{ name, proven, view string }{
		{"proven=0", "0", "none"},
		{"proven=1/view=tests", "1", "tests"},
		{"proven=1/view=test", "1", "test"},
		{"proven=1/view=none", "1", "none"},
	}
	for _, mode := range modes {
		for _, n := range []int{1000, 10000} {
			b.Run(mode.name+"/n="+strconv.Itoa(n), func(b *testing.B) {
				b.Setenv("KEPLOY_PROVEN", mode.proven)
				const per = 256
				at := time.Unix(1_700_000_000, 0)
				h := httpint.New(zap.NewNop())
				mm := NewMockManager(nil, nil, zap.NewNop())
				defer mm.Close()
				var took [2]time.Duration
				var calls [2]int
				var viewTook time.Duration
				var views int
				var staged []*models.Mock
				b.ReportAllocs()
				for k := 0; k < b.N; k++ {
					var i int
					switch mode.view {
					case "tests":
						if i = k % n; i == 0 {
							b.StopTimer()
							staged = stageScale(mm, n, at, mode.view)
							b.StartTimer()
						}
						if i%scaleCalls == 0 {
							t0 := time.Now()
							openScale(mm, staged, i/scaleCalls, scaleCalls)
							viewTook += time.Since(t0)
							views++
						}
					default:
						if k%per == 0 {
							b.StopTimer()
							if staged = stageScale(mm, n, at, mode.view); mode.view == "test" {
								openScale(mm, staged, 0, n)
							}
							b.StartTimer()
						}
						i = ((k % per) * 7919) % n
					}
					t0 := time.Now()
					body, err := serveScale(h, mm, i)
					took[i%2] += time.Since(t0)
					calls[i%2]++
					if err != nil {
						b.Fatal(err)
					}
					if want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i); body != want {
						b.Fatalf("call %d served %q, want %q", i, body, want)
					}
				}
				for kind, name := range []string{"get-ns", "post-ns"} {
					if calls[kind] > 0 {
						b.ReportMetric(float64(took[kind].Nanoseconds())/float64(calls[kind]), name)
					}
				}
				if views > 0 {
					b.ReportMetric(float64(viewTook.Nanoseconds())/float64(views), "view-ns")
				}
			})
		}
	}
}
