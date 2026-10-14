package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// A stop signal waits while a recording's client holds a frame stream open,
// and goes on once every one is closed, or at once when told to stop now.
func TestWaitForRecordingClients(t *testing.T) {
	stop := make(chan struct{})
	if !WaitForRecordingClients(stop) {
		t.Fatal("waited with no client attached")
	}
	a, b := attachRecordingClient(), attachRecordingClient()
	if n := RecordingClients(); n != 2 {
		t.Fatalf("%d clients, want 2", n)
	}
	done := make(chan bool, 1)
	go func() { done <- WaitForRecordingClients(stop) }()
	a()
	a() // idempotent
	select {
	case <-done:
		t.Fatal("the wait ended with a stream still open")
	default:
	}
	b()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the wait says it was cut short")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end once every stream closed")
	}

	c := attachRecordingClient()
	defer c()
	go func() { done <- WaitForRecordingClients(stop) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("the wait says every stream closed")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stopNow did not end the wait")
	}
}

// Only a stream whose client drains it after the stop, and stops the agent
// itself, is held for. One without the header (keploy mock under compose,
// whose project stops the agent before the client closes its streams) is not:
// held for, the agent waited for a stream that closed only once the agent had
// stopped, until its container was killed. Only "true" asks for the hold.
func TestHoldForDrainOnlyForAClientThatDrains(t *testing.T) {
	before := RecordingClients()
	plain := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	detach := holdForDrain(plain)
	if n := RecordingClients(); n != before {
		t.Fatalf("a stream whose client does not drain after the stop is held for (%d clients, want %d)", n, before)
	}
	detach()
	other := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	other.Header.Set(models.DrainsAfterStopHeader, "false")
	detach = holdForDrain(other)
	if n := RecordingClients(); n != before {
		t.Fatalf("a stream whose header is not \"true\" is held for (%d clients, want %d)", n, before)
	}
	detach()
	draining := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	draining.Header.Set(models.DrainsAfterStopHeader, "true")
	detach = holdForDrain(draining)
	if n := RecordingClients(); n != before+1 {
		t.Fatalf("a stream its client drains after the stop is not held for (%d clients, want %d)", n, before+1)
	}
	detach()
	if n := RecordingClients(); n != before {
		t.Fatalf("%d clients after the stream closed, want %d", n, before)
	}
}

// streamsSvc hands each frame stream a channel the test closes.
type streamsSvc struct {
	agent.Service // nil: any other call panics loudly
	tc            chan *models.TestCase
	mocks         chan *models.Mock
	maps          chan models.TestMockMapping
}

func (s streamsSvc) StartIncomingProxy(context.Context, models.IncomingOptions) (chan *models.TestCase, error) {
	return s.tc, nil
}

func (s streamsSvc) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return s.mocks, nil
}

func (s streamsSvc) GetMapping(context.Context) (<-chan models.TestMockMapping, error) {
	return s.maps, nil
}

// Each frame stream's handler holds for its client while the stream is open,
// when the client drains it after the stop, and only then.
func TestStreamHandlersHoldOnlyForAClientThatDrains(t *testing.T) {
	for _, drains := range []bool{true, false} {
		svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		for _, s := range []struct {
			name    string
			handler http.HandlerFunc
			end     func()
		}{
			{"incoming", a.HandleIncoming, func() { close(svc.tc) }},
			{"outgoing", a.HandleOutgoing, func() { close(svc.mocks) }},
			{"mappings", a.HandleMappings, func() { close(svc.maps) }},
		} {
			before := RecordingClients()
			srv := httptest.NewServer(s.handler)
			req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
			if drains {
				req.Header.Set(models.DrainsAfterStopHeader, "true")
			}
			// Each handler flushes its headers once its stream is open, and
			// the hold is taken before that.
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if drains {
				want++
			}
			if n := RecordingClients(); n != want {
				t.Errorf("%s (drains=%v): %d clients held for while its stream is open, want %d", s.name, drains, n, want)
			}
			s.end()
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			srv.Close() // waits for the handler to return
			if n := RecordingClients(); n != before {
				t.Errorf("%s (drains=%v): %d clients held for once its stream ended, want %d", s.name, drains, n, before)
			}
		}
	}
}

// gracefulSvc records whether SetGracefulShutdown was told a draining client
// is attached.
type gracefulSvc struct {
	agent.Service // nil: any other call panics loudly
	drains        *bool
}

func (s gracefulSvc) SetGracefulShutdown(ctx context.Context) error {
	*s.drains = models.DrainsAfterStop(ctx)
	return nil
}

// A graceful shutdown says, to the agent, whether a recording client that
// drains after its stop holds a stream: that client stops the app it started
// first, and only then may a refusal from the app be taken for it going on
// purpose.
func TestHandleGracefulShutdownSaysWhetherADrainingClientHoldsAStream(t *testing.T) {
	for _, attached := range []bool{false, true} {
		var drains bool
		a := &Agent{logger: zap.NewNop(), svc: gracefulSvc{drains: &drains}}
		if attached {
			detach := attachRecordingClient()
			defer detach()
		}
		rec := httptest.NewRecorder()
		a.HandleGracefulShutdown(rec, httptest.NewRequest(http.MethodPost, "/agent/graceful-shutdown", nil))
		if rec.Code != http.StatusOK || drains != attached {
			t.Fatalf("attached=%v: status %d, told a draining client holds a stream: %v", attached, rec.Code, drains)
		}
	}
}
