package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/hooks/conn"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
		{"incoming", func(svc streamsSvc) { svc.tc <- windowedNow("test-1") }},
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

// recordingOnTheAgent binds the agent's mock manager to a recording's mock and
// mapping streams, with its log observed, and returns them with the func that
// ends the mapping stream's request (its client read the marker and went).
// The manager records again when the test ends.
func recordingOnTheAgent(t *testing.T) (out chan *models.Mock, mappings chan models.TestMockMapping, endStream func(), logs *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	g := syncmgr.Get()
	g.SetLogger(zap.New(core))
	out = make(chan *models.Mock, 64)
	g.SetOutputChannel(out)
	g.SetFirstRequestSignaled()
	streamCtx, endStream := context.WithCancel(context.Background())
	mappings = make(chan models.TestMockMapping) // its client reads none past its marker
	g.SetMappingChannel(streamCtx, mappings)
	t.Cleanup(func() {
		endStream()
		g.SetMappingChannel(context.Background(), nil)
		syncmgr.Get().StartRecordSession()
		g.SetLogger(nil)
	})
	return out, mappings, endStream, logs
}

// tokenStream is a frame stream's request carrying its recording's token.
func tokenStream(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/agent/outgoing", nil)
	r.Header.Set(models.StreamEndTokenHeader, tok)
	return r
}

// callAt is a mock of a call the app made at at, one a test's window owns.
func callAt(at time.Time) *models.Mock {
	return &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// served captures one exchange the app served at at through the ingress's
// capture hook, and returns what it handed on, if anything.
func served(t *testing.T, at time.Time) []*models.TestCase {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://app:8080/q", strings.NewReader(`{"k":1}`))
	resp := &http.Response{StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}
	tcs := make(chan *models.TestCase, 1)
	conn.Capture(context.Background(), zap.NewNop(), tcs, req, resp, at, at.Add(5*time.Millisecond), models.IncomingOptions{}, false, true, 8080)
	close(tcs)
	var got []*models.TestCase
	for tc := range tcs {
		got = append(got, tc)
	}
	return got
}

// A sidecar's app serves on after its recording's stop. Once the recording's
// client has read all it was owed and asked the agent to end its streams, the
// agent captures nothing of it: no test case, no mock, no mapping, and no
// ERROR. Before, it captured on until it exited, and each mapping of a
// request served after the stop queued for the stream whose client had read
// its marker and gone, and was lost with one ERROR each ("mapping stream
// closed with mappings still queued"). A recording that starts after it
// captures from its start.
func TestTheAgentCapturesNothingOnceARecordingsClientEndsItsStreams(t *testing.T) {
	out, mappings, endStream, logs := recordingOnTheAgent(t)
	tok := rand.Text()
	_, closed := streamEnding(tokenStream(tok))
	endRecordingStreams(tok) // its client read all it was owed
	closed()
	endStream()

	at := time.Now()
	if d := syncmgr.Get().AddMock(callAt(at.Add(time.Millisecond))); !d.Dropped() {
		t.Fatal("a call the app made after its recording ended was captured")
	}
	if got := served(t, at); len(got) != 0 {
		t.Fatalf("a request the app served after its recording ended was made %d test case(s)", len(got))
	}
	syncmgr.Get().ResolveRange(at, at.Add(5*time.Millisecond), "test-after", true, true)
	time.Sleep(100 * time.Millisecond) // a drainer that would say its loss has had its time
	select {
	case m := <-mappings:
		t.Fatalf("a request served after the recording ended was mapped: %+v", m)
	default:
	}
	if n := len(out); n != 0 {
		t.Fatalf("%d mocks went to the ended recording's mock stream", n)
	}
	for _, e := range logs.All() {
		if e.Level >= zapcore.ErrorLevel {
			t.Fatalf("the app's traffic after its recording ended was said at %s: %q", e.Level, e.Message)
		}
	}

	next := rand.Text()
	_, closedNext := streamEnding(tokenStream(next)) // the next recording's stream attached
	defer closedNext()
	if d := syncmgr.Get().AddMock(callAt(time.Now())); d.Dropped() {
		t.Fatalf("the next recording's call was dropped: %q", d)
	}
	if got := served(t, time.Now()); len(got) != 1 {
		t.Fatalf("the next recording made %d test cases of a request it served, want 1", len(got))
	}
}

// A recording's client that stopped but never asked for the markers (its
// drain gave up, or it went away mid-stop) ends the capture as its last
// stream closes: no client resumes a stopped recording. One whose streams
// close with no stop asked (a client that died mid-recording, and comes
// back) leaves the capture on.
func TestARecordingsStreamsClosingAfterItsStopEndItsCapture(t *testing.T) {
	recordingOnTheAgent(t)
	tok := rand.Text()
	_, closedMocks := streamEnding(tokenStream(tok))
	_, closedMappings := streamEnding(tokenStream(tok))
	syncmgr.Get().BacklogBefore(time.Now()) // its stop asked what the agent holds
	closedMocks()
	if d := syncmgr.Get().AddMock(callAt(time.Now())); d.Dropped() {
		t.Fatalf("the capture ended while one of the recording's streams was still open: %q", d)
	}
	closedMappings()
	if d := syncmgr.Get().AddMock(callAt(time.Now())); !d.Dropped() {
		t.Fatal("the agent captured on after a stopped recording's last stream closed")
	}

	again := rand.Text()
	_, closedAgain := streamEnding(tokenStream(again)) // a recording no stop has asked of
	closedAgain()
	if d := syncmgr.Get().AddMock(callAt(time.Now())); d.Dropped() {
		t.Fatalf("a recording whose streams closed with no stop asked stopped its capture: %q", d)
	}
}

// One recording's end leaves the capture of the recording whose streams
// attached after it as it is.
func TestARecordingsEndLeavesTheNextOnesCapture(t *testing.T) {
	recordingOnTheAgent(t)
	old, current := rand.Text(), rand.Text()
	_, closedOld := streamEnding(tokenStream(old))
	defer closedOld()
	_, closedCurrent := streamEnding(tokenStream(current))
	defer closedCurrent()
	endRecordingStreams(old)
	if d := syncmgr.Get().AddMock(callAt(time.Now())); d.Dropped() {
		t.Fatalf("an older recording's end stopped the current one's capture: %q", d)
	}
}

// An agent stopping after its recording ended says once how much of the app's
// traffic it left uncaptured, as its HTTP server shuts down: a sidecar's
// proxyless agent stops without the proxy's teardown (CloseOutChan). When
// both run, it is said once.
func TestAnAgentSaysOnceWhatItLeftUncapturedAsItStops(t *testing.T) {
	_, _, endStream, logs := recordingOnTheAgent(t)
	tok := rand.Text()
	_, closed := streamEnding(tokenStream(tok))
	endRecordingStreams(tok)
	closed()
	endStream()
	served(t, time.Now()) // the app serves on: not captured

	holder, port, _ := grabPort(t)
	_ = holder.Close()
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		StartAgentServer(ctx, zap.NewNop(), port, false, http.NotFoundHandler())
	}()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent's server never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop() // the agent stops
	<-stopped
	said := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("no recording took the application's traffic").All()
	}
	for time.Now().Before(deadline) && len(said()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := said(); len(got) != 1 || got[0].ContextMap()["requests_not_captured"] != uint64(1) {
		t.Fatalf("as the agent stopped it said %v, want once: 1 request not captured", got)
	}
	syncmgr.Get().CloseOutChan() // the proxy's teardown, too
	if n := len(said()); n != 1 {
		t.Fatalf("with both teardowns it was said %d times, want once", n)
	}
}
