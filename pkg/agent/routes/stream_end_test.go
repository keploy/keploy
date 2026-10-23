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

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
		a := &Agent{logger: zap.NewNop(), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{}, tok)
		defer done()
		// Each in a stretch of its own, before every span another test
		// recorded (newStretch): a test case with no window is left out by
		// every span (WasMockOrphanedFor).
		at := newStretch()
		for i, n := range []string{"test-1", "test-2"} {
			req := at.Add(time.Duration(i) * time.Millisecond)
			svc.tc <- &models.TestCase{Name: n, HTTPReq: models.HTTPReq{Timestamp: req},
				HTTPResp: models.HTTPResp{Timestamp: req.Add(time.Microsecond)}}
		}
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
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("the stream did not end after its marker: %v", err)
		}
	})
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
