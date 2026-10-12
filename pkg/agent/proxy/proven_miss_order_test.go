package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// httpMiss sends one unrecorded GET through the HTTP integration in test mode,
// with the proxy's miss reporter as the proxy installs it, and returns once
// the app has read the 502. done yields what the integration returned.
func httpMiss(t *testing.T, p *Proxy) (done <-chan error) {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	now := time.Now()
	mm.SetMocksWithWindow(nil, nil, now.Add(-time.Hour), now.Add(time.Hour))
	app, pipeEnd := net.Pipe()
	agentEnd := &writeSeen{Conn: pipeEnd}
	// The order itself, whatever the scheduler does: when the miss is
	// reported, nothing may have been written to the app yet.
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
		if provenswitch.On() && agentEnd.wrote.Load() {
			t.Errorf("the 502 reached the app before its miss was reported")
		}
		p.sendMockNotFoundError(err)
	})
	t.Cleanup(func() { _ = app.Close() })
	returned := make(chan error, 1)
	go func() {
		returned <- httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	if _, err := app.Write([]byte("GET /orders/42 HTTP/1.1\r\nHost: deps.example\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("the app got %d, want 502", resp.StatusCode)
	}
	return returned
}

// writeSeen notes that something was written to the app.
type writeSeen struct {
	net.Conn
	wrote atomic.Bool
}

func (c *writeSeen) Write(b []byte) (int, error) {
	c.wrote.Store(true)
	return c.Conn.Write(b)
}

// Under the proven switch an HTTP miss is queued for the test that made it
// before the app reads its 502: a test that ends as soon as it has the 502
// has its miss, and the next test is never charged with it. The integration
// then returns nothing, so the miss is not reported twice.
func TestAnHTTPMissIsQueuedBeforeTheAppSeesIts502(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	ctx := context.Background()

	p.ContinueTestErrorCapture() // test N begins
	returned := httpMiss(t, p)
	gotN, err := p.GetMockErrors(ctx) // test N ends: its harness read the 502
	if err != nil {
		t.Fatal(err)
	}
	if len(gotN) != 1 {
		t.Fatalf("test N's misses at its end: %d, want its own one", len(gotN))
	}
	if err := <-returned; err != nil {
		t.Fatalf("the integration returned %v after reporting the miss; it would be reported twice", err)
	}

	p.ContinueTestErrorCapture() // test N+1 begins
	gotN1, err := p.GetMockErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotN1) != 0 {
		t.Fatalf("test N+1 was charged with %d miss(es) it did not make", len(gotN1))
	}
}

// With the switch off the integration returns the miss, as it always has,
// and the proxy reports it from there.
func TestAnHTTPMissIsReturnedWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	p := startedDrainProxy(t, 100)
	p.ContinueTestErrorCapture()
	returned := httpMiss(t, p)
	if err := <-returned; !errors.Is(err, httpint.ErrMockNotMatched) {
		t.Fatalf("the integration returned %v, want the miss", err)
	}
}
