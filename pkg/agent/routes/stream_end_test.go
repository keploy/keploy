package routes

import (
	"bytes"
	"crypto/rand"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// openHeldStream opens one of the agent's frame streams the way a recording's
// client does (its body, the header that says it drains the stream after its
// stop, and its recording's token), and returns its response.
func openHeldStream(t *testing.T, h http.HandlerFunc, body any, tok string) (*http.Response, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(models.DrainsAfterStopHeader, "true")
	req.Header.Set(models.StreamEndTokenHeader, tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, func() { resp.Body.Close(); srv.Close() }
}

// Asked to end its streams (models.StreamEndPath), the agent puts on each
// what it still has, then the stream's marker, and ends it: its client's
// drain then ends on the marker, every frame read, not on a quiet bound that
// a frame still on its way could outlast.
func TestFrameStreamsEndWithTheirMarkerOnRequest(t *testing.T) {
	t.Run("mocks", func(t *testing.T) {
		svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock, 4), maps: make(chan models.TestMockMapping)}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		resp, done := openHeldStream(t, a.HandleOutgoing, models.OutgoingReq{}, tok)
		defer done()
		for _, n := range []string{"m-1", "m-2", "m-3"} {
			svc.mocks <- &models.Mock{Name: n, Kind: models.HTTP}
		}
		endRecordingStreams(tok)
		dec := gob.NewDecoder(resp.Body)
		var got []string
		for {
			var m models.Mock
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("the stream ended without its marker, after %v: %v", got, err)
			}
			if m.Kind == models.StreamEndKind {
				break
			}
			got = append(got, m.Name)
		}
		if len(got) != 3 {
			t.Fatalf("before its marker the stream carried %v, want the three mocks queued", got)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("the stream did not end after its marker: %v", err)
		}
	})
	t.Run("mappings", func(t *testing.T) {
		svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping, 4)}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		resp, done := openHeldStream(t, a.HandleMappings, nil, tok)
		defer done()
		for _, n := range []string{"test-1", "test-2"} {
			svc.maps <- models.TestMockMapping{TestName: n, MockIDs: []string{"mock-" + n}}
		}
		endRecordingStreams(tok)
		dec := json.NewDecoder(resp.Body)
		var got []string
		for {
			var m models.TestMockMapping
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("the stream ended without its marker, after %v: %v", got, err)
			}
			if m.StreamEnd {
				break
			}
			got = append(got, m.TestName)
		}
		if len(got) != 2 {
			t.Fatalf("before its marker the stream carried %v, want the two mappings queued", got)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("the stream did not end after its marker: %v", err)
		}
	})
	t.Run("test cases", func(t *testing.T) {
		syncmgr.Get().SetWatermark(nil) // no hold: test cases go at once
		svc := streamsSvc{tc: make(chan *models.TestCase, 4), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
		core, logs := observer.New(zapcore.DebugLevel)
		// Counted after the marker went out and before the session's
		// summary, which the handler defers, reads them: the handler logs
		// the marker's flush on its own goroutine, and this hook runs there.
		// The summary must say what the marker said (the counts are read
		// once), not these too.
		var markerSeen int
		core = zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
			if e.Message == streamEndMarkerSent {
				markerSeen++
				syncmgr.Get().NoteNotHandedOn()
				syncmgr.Get().NoteUnnamedCallerConn()
				syncmgr.Get().NoteTLSRelayedConn()
			}
			return nil
		})
		a := &Agent{logger: zap.New(core), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		// A request a pause cost before this session, and one the ingress
		// read during a pause before it: not its own.
		syncmgr.Get().NoteNotRecordedUnderPressure("10.0.0.9:5009 -> 127.0.0.1:8080")
		// A test case an earlier session's ingress could not hand on, and a
		// connection of an earlier session whose caller the agent could not
		// name.
		syncmgr.Get().NoteNotHandedOn()
		syncmgr.Get().NoteUnnamedCallerConn()
		// And a TLS connection of an earlier session the ingress relayed to
		// the app unread.
		syncmgr.Get().NoteTLSRelayedConn()
		endPause := memoryguard.SetRecordingPausedForTest(true)
		memoryguard.IngressRequest()
		endPause()
		resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{}, tok)
		defer done()
		for _, n := range []string{"test-1", "test-2"} {
			svc.tc <- &models.TestCase{Name: n}
		}
		// Requests a pause of the memory guard cost this session, three on
		// two connections, noted once its stream is open: what the session
		// counts from.
		for _, c := range []string{"10.0.0.1:5000 -> 127.0.0.1:8080", "10.0.0.1:5000 -> 127.0.0.1:8080", "10.0.0.2:5001 -> 127.0.0.1:8080"} {
			syncmgr.Get().NoteNotRecordedUnderPressure(c)
		}
		// Two test cases this session's ingress could not hand on: it
		// stopped first.
		syncmgr.Get().NoteNotHandedOn()
		syncmgr.Get().NoteNotHandedOn()
		// Two connections to the app this session whose callers the agent
		// could not name in its PID namespace.
		syncmgr.Get().NoteUnnamedCallerConn()
		syncmgr.Get().NoteUnnamedCallerConn()
		// Two TLS connections to the app this session, relayed to the app's
		// TLS unread.
		syncmgr.Get().NoteTLSRelayedConn()
		syncmgr.Get().NoteTLSRelayedConn()
		// The ingress requests of this session: one before its pause, two
		// during it, one after.
		memoryguard.IngressRequest()
		endPause = memoryguard.SetRecordingPausedForTest(true)
		memoryguard.IngressRequest()
		memoryguard.IngressRequest()
		endPause()
		memoryguard.IngressRequest()
		endRecordingStreams(tok)
		_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		mr := multipart.NewReader(resp.Body, params["boundary"])
		var got []string
		var sum models.StreamEndSummary
		for {
			part, err := mr.NextPart()
			if err != nil {
				t.Fatalf("the stream ended without its marker, after %v: %v", got, err)
			}
			if part.FormName() == models.StreamEndPart {
				// The marker says what the session sent and lost: its client
				// says it when the agent's own log may not reach the user.
				if err := json.NewDecoder(part).Decode(&sum); err != nil {
					t.Fatalf("the marker carries no summary of the session: %v", err)
				}
				break
			}
			if part.FormName() == "metadata" {
				var tc models.TestCase
				if err := json.NewDecoder(part).Decode(&tc); err != nil {
					t.Fatal(err)
				}
				got = append(got, tc.Name)
			}
		}
		if len(got) != 2 {
			t.Fatalf("before its marker the stream carried %v, want the two test cases queued", got)
		}
		if sum.TestCasesSent != 2 || sum.TestCasesSuppressed != 0 || sum.MassSuppressed {
			t.Fatalf("the marker's summary says %+v, want the two test cases sent and none suppressed", sum)
		}
		if want := (models.PressureSkipSummary{Requests: 3, Connections: 2}); sum.NotRecordedUnderPressure != want {
			t.Fatalf("the marker says a pause cost %+v, want %+v", sum.NotRecordedUnderPressure, want)
		}
		if sum.NotHandedOn != 2 {
			t.Fatalf("the marker says %d test cases were not handed on, want this session's 2", sum.NotHandedOn)
		}
		if sum.UnnamedCallerConns != 2 {
			t.Fatalf("the marker says %d connections came from unnamed callers, want this session's 2", sum.UnnamedCallerConns)
		}
		if sum.TLSRelayedConns != 2 {
			t.Fatalf("the marker says %d TLS connections were relayed unread, want this session's 2", sum.TLSRelayedConns)
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("the stream did not end after its marker: %v", err)
		}
		// The agent's own summary, which a lane reads, says the session's
		// counts; its warning names the connections the requests were lost
		// on, most first.
		fields := func(msg string) map[string]any {
			t.Helper()
			got := logs.FilterMessage(msg).All()
			if len(got) != 1 {
				t.Fatalf("the agent logged %q %d times, want once", msg, len(got))
			}
			return got[0].ContextMap()
		}
		done() // the handler has returned, its summary said
		if markerSeen != 1 {
			t.Fatalf("the agent logged the marker's flush %d times, want once", markerSeen)
		}
		sumLine := fields("agent: recording complete")
		for name, want := range map[string]uint64{
			"tcs_not_recorded_under_pressure":  3,
			"requests_during_pauses":           2,
			"requests_before_first_pause":      1,
			"tcs_not_handed_on":                2,
			"connections_from_unnamed_callers": 2,
			"connections_relayed_tls":          2,
		} {
			if got := sumLine[name]; got != want {
				t.Errorf("the summary says %s=%v, want %d", name, got, want)
			}
		}
		warn := fields("agent: some requests were not recorded: the agent was short of memory when they came, and the memory guard had paused recording; each connection's requests after the pause are recorded")
		if got := fmt.Sprint(warn["requests"], " ", warn["requests_during_pauses"], " ", warn["connections"]); got != "3 2 2" {
			t.Errorf("the warning says requests, requests_during_pauses, connections = %s, want 3 2 2", got)
		}
		if got := fields(notHandedOnSaid)["test_cases"]; got != uint64(2) {
			t.Errorf("the warning of test cases not handed on says %v, want 2", got)
		}
		if got := fields(unnamedCallerConnsSaid)["connections"]; got != uint64(2) {
			t.Errorf("the warning of connections from unnamed callers says %v, want 2", got)
		}
		if f := fields(tlsRelayedConnsSaid); f["connections"] != uint64(2) || f["next_step"] != tlsRelayedNextStep {
			t.Errorf("the warning of TLS connections relayed unread says %v, want 2 connections and what to do so their calls are recorded", f)
		}
		if got, want := fmt.Sprint(warn["connections_most"]), "[10.0.0.1:5000 -> 127.0.0.1:8080 (2) 10.0.0.2:5001 -> 127.0.0.1:8080 (1)]"; got != want {
			t.Errorf("the warning names the connections %s, want %s", got, want)
		}
	})
}

// Every recording session on an agent says, at WARN in its own summary, the
// TLS connections to the app its ingress relayed unread (none of their calls
// was recorded), however many sessions before it did: the ingress warns as
// they come only of the agent's first to each app port, so a later session's
// summary is the one place its own are said above Debug.
func TestEachRecordingSessionSaysItsOwnTLSConnectionsRelayedUnread(t *testing.T) {
	syncmgr.Get().SetWatermark(nil) // no hold: test cases go at once
	for session, relayed := range []int{1, 2} {
		svc := streamsSvc{tc: make(chan *models.TestCase, 1), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
		core, logs := observer.New(zapcore.InfoLevel)
		a := &Agent{logger: zap.New(core), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{}, tok)
		svc.tc <- &models.TestCase{Name: "test-1"}
		for i := 0; i < relayed; i++ {
			syncmgr.Get().NoteTLSRelayedConn()
		}
		endRecordingStreams(tok)
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("session %d: the stream did not end: %v", session+1, err)
		}
		done() // the handler has returned, its summary said
		said := logs.FilterMessage(tlsRelayedConnsSaid).All()
		if len(said) != 1 || said[0].Level != zapcore.WarnLevel {
			t.Fatalf("session %d said its TLS connections relayed unread %d times, want once at WARN", session+1, len(said))
		}
		if f := said[0].ContextMap(); f["connections"] != uint64(relayed) || f["next_step"] != tlsRelayedNextStep {
			t.Fatalf("session %d's warning says %v, want its own %d connections and what to do so their calls are recorded", session+1, f, relayed)
		}
		sumLine := logs.FilterMessage("agent: recording complete").All()
		if len(sumLine) != 1 || sumLine[0].ContextMap()["connections_relayed_tls"] != uint64(relayed) {
			t.Fatalf("session %d's summary is %v, want connections_relayed_tls=%d", session+1, sumLine, relayed)
		}
	}
}

// A recording's request ends its own streams alone: another recording's
// stream (another token) is not ended by it.
func TestARecordingsRequestEndsItsOwnStreamsAlone(t *testing.T) {
	req := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/outgoing", nil)
		r.Header.Set(models.StreamEndTokenHeader, tok)
		return r
	}
	otherTok, thisTok := rand.Text(), rand.Text() // their own each run
	other, closedOther := streamEnding(req(otherTok))
	defer closedOther()
	this, closedThis := streamEnding(req(thisTok))
	defer closedThis()
	endRecordingStreams(thisTok)
	select {
	case <-this:
	default:
		t.Fatal("the recording's own stream was not ended by its request")
	}
	select {
	case <-other:
		t.Fatal("another recording's stream was ended by the request")
	default:
	}
	if none, _ := streamEnding(httptest.NewRequest(http.MethodPost, "/outgoing", nil)); none != nil {
		t.Fatal("a stream that names no recording waits on an end")
	}
}

// The answer to a stop says the agent ends its streams on request, which is
// how its client knows to.
func TestHandlePendingSaysTheAgentEndsItsStreams(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	syncmgr.Get().SetWatermark(&backlogWatermark{})
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })
	rr := httptest.NewRecorder()
	a.HandlePending(rr, httptest.NewRequest(http.MethodGet, "/agent/record/pending?before=1000", nil))
	var got PendingResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || !got.EndMarkers {
		t.Fatalf("%d %q: the answer does not say the agent ends its streams", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	a.HandleEndStreams(rr, httptest.NewRequest(http.MethodPost, models.StreamEndPath+"?token=rec-1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("the end request answered %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	a.HandleEndStreams(rr, httptest.NewRequest(http.MethodPost, models.StreamEndPath, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("an end request that names no recording answered %d, want 400", rr.Code)
	}
}

// A stream its client reopens once its recording's streams were ended (a
// reconnect after the request) ends at once, with its marker.
func TestAStreamReopenedAfterItsEndEndsAtOnce(t *testing.T) {
	svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	tok := rand.Text() // its own each run
	endRecordingStreams(tok)
	resp, done := openHeldStream(t, a.HandleMappings, nil, tok)
	defer done()
	var m models.TestMockMapping
	decoded := make(chan error, 1)
	go func() { decoded <- json.NewDecoder(resp.Body).Decode(&m) }()
	select {
	case err := <-decoded:
		if err != nil || !m.StreamEnd {
			t.Fatalf("a stream reopened after its end carried %+v (%v), want its marker", m, err)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a stream reopened after its end did not end")
	}
}

// What the agent keeps of the recordings whose streams it ends is bounded: a
// recording whose streams all closed without asking (its client died) is
// forgotten, and of those ended it keeps the latest endedKept, not one for
// each recording a long-lived agent served.
func TestTheAgentKeepsABoundedRecordOfRecordingsStreams(t *testing.T) {
	req := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/outgoing", nil)
		r.Header.Set(models.StreamEndTokenHeader, tok)
		return r
	}
	open := func(tok string) bool {
		e := &recordingStreamsEnd
		e.mu.Lock()
		defer e.mu.Unlock()
		_, ok := e.open[tok]
		return ok
	}
	died := rand.Text() // its own each run
	_, closedA := streamEnding(req(died))
	_, closedB := streamEnding(req(died))
	closedA()
	closedA() // a second call is the first's
	if !open(died) {
		t.Fatal("a recording with a stream still open was forgotten")
	}
	closedB()
	if open(died) {
		t.Fatal("a recording whose streams all closed without asking is still kept")
	}

	run := rand.Text() // its own each run
	for i := 0; i < endedKept+10; i++ {
		endRecordingStreams(fmt.Sprintf("%s-%d", run, i))
	}
	e := &recordingStreamsEnd
	e.mu.Lock()
	n, order := len(e.ended), len(e.endedOrder)
	e.mu.Unlock()
	if n > endedKept || order > endedKept {
		t.Fatalf("the agent keeps %d ended recordings (%d in order), want at most %d", n, order, endedKept)
	}
	latest, _ := streamEnding(req(fmt.Sprintf("%s-%d", run, endedKept+9)))
	if first, closedFirst := streamEnding(req(fmt.Sprintf("%s-0", run))); first != nil {
		select {
		case <-first:
			t.Fatal("the oldest recording ended past the bound is still remembered")
		default:
		}
		closedFirst()
	}
	select {
	case <-latest:
	default:
		t.Fatal("a stream reopened by the latest recording ended is not ended at once")
	}
}
