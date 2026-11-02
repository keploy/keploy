package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The own-call check's cost in a long test: a test that owns calls
// recordings, on a view that shows a worker scope, makes each of its calls
// once, all on one kept-alive connection, as an app's client does. Each op is
// the whole test: its view opened again, and every call made; only the calls
// are timed (the connection's set-up and close are not). The check is asked
// of every call (integrations.OwnRepeatRead). Two shapes:
//   - path=distinct: GET /r/0 .. /r/<calls-1>, each its own method and path;
//   - path=shared: GET /r?i=0 .. /r?i=<calls-1>, one method and path, which
//     the HTTP matcher reads as one group of recordings for every call.
func BenchmarkOwnCallCheckInALongTest(b *testing.B) {
	shapes := []struct {
		name string
		path func(i int) string
	}{
		{"path=distinct", func(i int) string { return fmt.Sprintf("/r/%d", i) }},
		{"path=shared", func(i int) string { return fmt.Sprintf("/r?i=%d", i) }},
	}
	for _, shape := range shapes {
		for _, calls := range []int{100, 500, 1000} {
			b.Run(fmt.Sprintf("%s/calls=%d", shape.name, calls), func(b *testing.B) {
				b.Setenv(provenswitch.Env, "1")
				worker := workerProbe("mock-worker-w", time.Second, "/w", "worker")
				own := make([]*models.Mock, calls)
				mocks := []*models.Mock{worker}
				for i := range own {
					own[i] = workerProbe(fmt.Sprintf("mock-own-%d", i), 3*time.Second+time.Duration(i)*time.Microsecond, shape.path(i), "own")
					mocks = append(mocks, own[i])
				}
				mm := NewMockManager(nil, nil, zap.NewNop())
				b.Cleanup(mm.Close)
				mm.SetMocksWithWindow(nil, mocks, models.BaseTime, exactT0.Add(time.Hour))
				mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(2 * time.Second), End: exactT0.Add(5 * time.Second)}})
				mm.ServeExactWorker([]string{worker.Name})
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					b.StopTimer()
					mm.ServeExactPool(models.PoolKeyNone, nil)
					served := make([]*models.Mock, calls)
					for i, m := range own {
						served[i] = roundMock(mm, m.Name)
					}
					mm.ServeExactTest("test-a", served)
					ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
					app, agentEnd := net.Pipe()
					done := make(chan struct{})
					go func() {
						_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
						_ = agentEnd.Close()
						close(done)
					}()
					br := bufio.NewReader(app)
					b.StartTimer()
					for i := range own {
						if _, err := io.WriteString(app, "GET "+shape.path(i)+" HTTP/1.1\r\nHost: deps\r\n\r\n"); err != nil {
							b.Fatalf("GET %s: %v", shape.path(i), err)
						}
						resp, err := http.ReadResponse(br, nil)
						if err != nil || resp.StatusCode != http.StatusOK {
							b.Fatalf("GET %s was not answered: %v", shape.path(i), err)
						}
						if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, resp.ContentLength)); err != nil {
							b.Fatalf("GET %s: %v", shape.path(i), err)
						}
					}
					b.StopTimer()
					_ = app.Close()
					<-done
					cancel()
				}
			})
		}
	}
}
