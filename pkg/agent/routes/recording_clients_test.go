package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// and goes on once every one is closed.
func TestWaitForRecordingClients(t *testing.T) {
	WaitForRecordingClients() // no client attached: it does not wait
	a, b := attachRecordingClient(), attachRecordingClient()
	if n := RecordingClients(); n != 2 {
		t.Fatalf("%d clients, want 2", n)
	}
	done := make(chan struct{})
	go func() { WaitForRecordingClients(); close(done) }()
	a()
	a() // idempotent
	select {
	case <-done:
		t.Fatal("the wait ended with a stream still open")
	default:
	}
	b()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("the wait did not end once every stream closed")
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

// A client that dies while it holds its streams -- killed, crashed --
// drops them: each handler notices its request end, and the stop the agent
// holds for the client goes on. Nothing else ends that hold (no repeated
// signal does: utils.SetSignalHold), so a stream whose handler did not notice
// would keep the agent, its hooks and its ports, up for good. The streams are
// as the CLI opens them: the bodies it sends, the header, and frames the
// agent still has to hand over (its channels stay open).
func TestAGoneClientsStreamsEndTheHold(t *testing.T) {
	body := func(v any) func() io.Reader {
		return func() io.Reader {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return bytes.NewReader(b)
		}
	}
	svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	for _, s := range []struct {
		name    string
		handler http.HandlerFunc
		body    func() io.Reader
	}{
		{"incoming", a.HandleIncoming, body(models.IncomingReq{})},
		{"outgoing", a.HandleOutgoing, body(models.OutgoingReq{})},
		{"mappings", a.HandleMappings, func() io.Reader { return nil }},
	} {
		before := RecordingClients()
		srv := httptest.NewServer(s.handler)
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, s.body())
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(models.DrainsAfterStopHeader, "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if n := RecordingClients(); n != before+1 {
			t.Fatalf("%s: setup: %d clients held for, want %d", s.name, n, before+1)
		}
		cancel() // the client dies: its connection drops
		resp.Body.Close()
		deadline := time.Now().Add(30 * time.Second) // only turns a hang into a failure
		for RecordingClients() != before {
			if time.Now().After(deadline) {
				t.Fatalf("%s: a gone client's stream is still held for: the agent's stop would wait for it for good", s.name)
			}
			time.Sleep(5 * time.Millisecond)
		}
		srv.Close() // waits for the handler to return
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

// freshStopNotice gives the test an agent no stop has reached, with no notice
// client's stream having carried a frame, and puts the agent's own back after.
func freshStopNotice(t *testing.T) {
	t.Helper()
	saved, carried := stopNotice, noticeCarried.Load()
	stopNotice = newStopNotice()
	noticeCarried.Store(false)
	t.Cleanup(func() {
		stopNotice = saved
		noticeCarried.Store(carried)
	})
}

// A stream whose client drains it once the agent says a stop reached it
// (docker compose stops the agent) is counted apart from one whose client
// stops the agent itself: RecordingClients, which tells the ingress the app
// was stopped on purpose, is not changed by it.
func TestHoldForDrainCountsANoticeClientApart(t *testing.T) {
	before, noticed := RecordingClients(), noticeClients.count()
	r := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	r.Header.Set(models.DrainsOnAgentStopHeader, "true")
	detach := holdForDrain(r)
	if n, m := RecordingClients(), noticeClients.count(); n != before || m != noticed+1 {
		t.Fatalf("recording clients %d (want %d), notice clients %d (want %d)", n, before, m, noticed+1)
	}
	detach()
	if m := noticeClients.count(); m != noticed {
		t.Fatalf("%d notice clients once the stream closed, want %d", m, noticed)
	}
}

// A stop waits for a notice client only while this agent has something for it
// to save: a frame its stream carried, or one the agent still holds. One with
// nothing goes at once: under docker compose that is an agent whose app never
// ran, which keploy's retry of the project's `up` starts again.
func TestNoticeClientsHoldOnlyForSomethingToSave(t *testing.T) {
	freshStopNotice(t)
	none := func() bool { return false }
	held := func() bool { return true }
	if NoticeClientsHold(held) {
		t.Fatal("a stop waits with no notice client to wait for")
	}
	r := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	r.Header.Set(models.DrainsOnAgentStopHeader, "true")
	detach := holdForDrain(r)
	defer detach()
	if NoticeClientsHold(none) {
		t.Fatal("a stop waits for a client the agent has nothing for")
	}
	if !NoticeClientsHold(held) {
		t.Fatal("a stop does not wait for a client the agent still holds a frame for")
	}
	other := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	carriedFrame(other) // a stream with no notice header carries a frame
	if NoticeClientsHold(none) {
		t.Fatal("a frame on a stream whose client is not told of a stop counts")
	}
	carriedFrame(r)
	if !NoticeClientsHold(none) {
		t.Fatal("a stop does not wait for a client whose stream carried a frame")
	}
}

// A stop signal waits for a notice client only while this agent has something
// for it to save, and then tells it (AnnounceStop) and waits until its stream
// closes. With nothing to save it goes at once, unannounced: under docker
// compose that is an agent whose app never ran, which keploy's retry of the
// project's `up` starts again, and an announced stop would have the recording
// drain, and end, before the app it records started.
func TestHoldStopWaitsForANoticeClientOnlyWithSomethingToSave(t *testing.T) {
	freshStopNotice(t)
	announced := func() bool {
		select {
		case <-stopNotice.announced:
			return true
		default:
			return false
		}
	}
	nothing := func() bool { return false }
	r := httptest.NewRequest(http.MethodPost, "/agent/incoming", nil)
	r.Header.Set(models.DrainsOnAgentStopHeader, "true")
	detach := holdForDrain(r)
	defer detach()

	held := make(chan struct{})
	go func() { holdStop(zap.NewNop(), nothing)(); close(held) }()
	select {
	case <-held:
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("a stop with nothing to save waited for a notice client")
	}
	if announced() {
		t.Fatal("a stop with nothing to save told the notice client of it")
	}

	carriedFrame(r)
	held = make(chan struct{})
	go func() { holdStop(zap.NewNop(), nothing)(); close(held) }()
	select {
	case <-stopNotice.announced:
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("a stop with something to save did not tell the notice client of it")
	}
	select {
	case <-held:
		t.Fatal("the stop went with the notice client's stream still open")
	default:
	}
	detach()
	select {
	case <-held:
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("the stop did not go once the notice client's stream closed")
	}
}

// Each frame stream notes, for a notice client, that it carried a frame.
func TestStreamHandlersNoteAFrameTheyCarried(t *testing.T) {
	for _, s := range []struct {
		name string
		send func(streamsSvc)
	}{
		{"incoming", func(svc streamsSvc) { svc.tc <- &models.TestCase{Name: "test-1", Kind: models.HTTP} }},
		{"outgoing", func(svc streamsSvc) { svc.mocks <- &models.Mock{Name: "mock-1", Kind: models.HTTP} }},
	} {
		t.Run(s.name, func(t *testing.T) {
			freshStopNotice(t)
			svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
			a := &Agent{logger: zap.NewNop(), svc: svc}
			handler := map[string]http.HandlerFunc{"incoming": a.HandleIncoming, "outgoing": a.HandleOutgoing}[s.name]
			srv := httptest.NewServer(handler)
			defer srv.Close()
			body := map[string]any{"incoming": models.IncomingReq{}, "outgoing": models.OutgoingReq{}}[s.name]
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(b))
			req.Header.Set(models.DrainsOnAgentStopHeader, "true")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if noticeCarried.Load() {
				t.Fatal("a stream that carried nothing yet is noted as having carried a frame")
			}
			s.send(svc)
			deadline := time.Now().Add(30 * time.Second) // only turns a hang into a failure
			for !noticeCarried.Load() {
				if time.Now().After(deadline) {
					t.Fatal("a frame the stream carried is not noted")
				}
				time.Sleep(5 * time.Millisecond)
			}
			close(svc.tc)
			close(svc.mocks)
		})
	}
}

// A notice client's wait on the agent's word of a stop ends, 204, once a stop
// has reached the agent and it waits for the client (AnnounceStop), and at
// once for one that asks after. A client that goes first ends its wait
// unanswered.
func TestHandleStopNoticeAnswersOnceTheStopIsAnnounced(t *testing.T) {
	freshStopNotice(t)
	a := &Agent{logger: zap.NewNop()}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleStopNotice))
	defer srv.Close()

	gone, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(gone, http.MethodGet, srv.URL, nil)
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			err = errors.New("answered " + resp.Status)
		}
		errc <- err
	}()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("a client that went before any stop: %v, want its own cancel", err)
	}

	answered := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case code := <-answered:
		t.Fatalf("answered %d before any stop", code)
	case <-time.After(100 * time.Millisecond): // a wait still open, not a bound on the answer
	}
	AnnounceStop()
	AnnounceStop() // once is all a stop takes
	if code := <-answered; code != http.StatusNoContent {
		t.Fatalf("the wait ended %d, want 204", code)
	}
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a wait made after the stop answered %d, want 204 at once", resp.StatusCode)
	}
}
