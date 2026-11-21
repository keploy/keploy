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

// stageScale installs a fresh copy of the n-mock set as the serving window.
func stageScale(mm *MockManager, n int, at time.Time) {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = scaleMock(i, at)
	}
	mm.SetMocksWithWindow(mocks, nil, at.Add(-time.Second), at.Add(time.Duration(n)*time.Millisecond+time.Second))
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
// recordings; the set is restaged (untimed) every `per` calls so the pool
// stays near n. Calls are taken in a stride through the set, so each one
// serves a different recording.
//
// It runs under the proven switch off and on (KEPLOY_PROVEN), and reports the
// mean time of a GET (one recording per path) and of a POST (every POST
// recording shares the path) apart, as get-ns and post-ns.
func BenchmarkHTTPReplayPerCall(b *testing.B) {
	for _, proven := range []string{"0", "1"} {
		for _, n := range []int{1000, 10000} {
			b.Run("proven="+proven+"/n="+strconv.Itoa(n), func(b *testing.B) {
				b.Setenv("KEPLOY_PROVEN", proven)
				const per = 256
				at := time.Unix(1_700_000_000, 0)
				h := httpint.New(zap.NewNop())
				mm := NewMockManager(nil, nil, zap.NewNop())
				defer mm.Close()
				var took [2]time.Duration
				var calls [2]int
				b.ReportAllocs()
				for k := 0; k < b.N; k++ {
					if k%per == 0 {
						b.StopTimer()
						stageScale(mm, n, at)
						b.StartTimer()
					}
					i := ((k % per) * 7919) % n
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
			})
		}
	}
}
