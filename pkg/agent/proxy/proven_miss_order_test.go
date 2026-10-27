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
		if agentEnd.wrote.Load() {
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

// An HTTP miss is queued for the test that made it before the app reads its
// 502, with the proven switch on or off: a test that ends as soon as it has
// the 502 has its miss, and the next test is never charged with it. With the
// switch off the miss was queued only after the 502, from the miss the
// integration returned, and a test that ended first had it charged to the test
// after it (enterprise pipeline 11651: get-quote-1's GET /fx filed under
// get-rates-1, and get-quote-1's difference then learned as noise).
//
// Under the switch the integration then returns nothing. With it off it
// returns the miss marked reported, as the connection's end and its log
// always took it, and the proxy (mockFailed) does not queue it a second time.
func TestAnHTTPMissIsQueuedBeforeTheAppSeesIts502(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			v := ""
			if on {
				v = "1"
			}
			t.Setenv(provenswitch.Env, v)
			p := startedDrainProxy(t, 100)
			ctx := context.Background()

			p.ContinueTestErrorCapture() // test N begins
			returned := httpMiss(t, p)
			// Test N ends: its harness read the 502 and the app answered.
			// What the integration returns reaches the connection's handler
			// only now, after the read, as it did in pipeline 11651.
			gotN, err := p.GetMockErrors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p.ContinueTestErrorCapture() // test N+1 begins
			ret := <-returned
			if on {
				if ret != nil {
					t.Fatalf("the integration returned %v after reporting the miss; it would be reported twice", ret)
				}
			} else {
				if !errors.Is(ret, httpint.ErrMockNotMatched) {
					t.Fatalf("the integration returned %v, want the miss", ret)
				}
				// As the connection's handler takes it.
				_ = p.mockFailed(zap.NewNop(), ret)
			}
			gotN1, err := p.GetMockErrors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(gotN) != 1 || len(gotN1) != 0 {
				t.Fatalf("test N read %d miss(es) and test N+1 %d; want N's one miss read with N, and none with N+1", len(gotN), len(gotN1))
			}
			if !on && !models.IsMismatchReported(ret) {
				t.Errorf("the integration's miss was not marked reported; the handler would queue it again")
			}
		})
	}
}
