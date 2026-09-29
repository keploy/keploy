package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// incomingSvc hands out a test-case channel the test feeds.
type incomingSvc struct {
	agent.Service // nil: any other call panics loudly
	tc            chan *models.TestCase
}

func (s incomingSvc) StartIncomingProxy(context.Context, models.IncomingOptions) (chan *models.TestCase, error) {
	return s.tc, nil
}

// switchWatermark settles every test case once flipped.
type switchWatermark struct{ settled atomic.Bool }

func (w *switchWatermark) Settled(string, string, time.Time, time.Time) bool { return w.settled.Load() }

// streamedNames reads the test cases a HandleIncoming stream carried.
func streamedNames(t *testing.T, resp *http.Response) []string {
	t.Helper()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var got []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			return got
		}
		if strings.Contains(p.Header.Get("Content-Disposition"), `name="metadata"`) {
			var tc models.TestCase
			b, _ := io.ReadAll(p)
			if json.Unmarshal(b, &tc) == nil {
				got = append(got, tc.Name)
			}
		}
	}
}

// The stream says where the test cases it still holds start, and the
// recording's spans keep those exact past the cap: the spans before them are
// joined first. Here the newest gaps are the smallest; joined by gap size
// alone, a held test case in one was left out, though no span covers it.
func TestHandleIncoming_SpansOfHeldTestCasesStayExactPastTheCap(t *testing.T) {
	w := &switchWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })

	tcs := make(chan *models.TestCase, 4)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// After every other test's spans (10,000 s): the watermark only moves
	// forward, and a test case that comes after it moved past its start is
	// checked against what was joined before it.
	base := time.Unix(100_000, 0)
	// 6,000 spans 10 ms apart, then, from `held` on, 4,300 spans 1 ms
	// apart with 0.5 ms gaps: past the cap (8,192).
	var at []time.Time
	for i := 0; i < 6000; i++ {
		at = append(at, base.Add(time.Duration(i)*10*time.Millisecond))
	}
	held := at[len(at)-1].Add(10 * time.Millisecond)
	for i := 0; i < 4300; i++ {
		at = append(at, held.Add(time.Duration(i)*1500*time.Microsecond))
	}
	// A test case in a gap among the newest spans, held for its verdict.
	gap := held.Add(100*1500*time.Microsecond + 1100*time.Microsecond)
	tcs <- &models.TestCase{Name: "in-a-gap", HTTPReq: models.HTTPReq{Timestamp: gap}, HTTPResp: models.HTTPResp{Timestamp: gap.Add(200 * time.Microsecond)}}
	over := held.Add(200*1500*time.Microsecond + 500*time.Microsecond)
	tcs <- &models.TestCase{Name: "over-a-span", HTTPReq: models.HTTPReq{Timestamp: over}, HTTPResp: models.HTTPResp{Timestamp: over.Add(200 * time.Microsecond)}}
	time.Sleep(100 * time.Millisecond) // held, and said so
	for _, s := range at {
		syncmgr.Get().RecordOrphanWindow(s, s.Add(time.Millisecond))
	}
	w.settled.Store(true)
	close(tcs)

	got := streamedNames(t, resp)
	if len(got) != 1 || got[0] != "in-a-gap" {
		t.Fatalf("streamed %v, want only [in-a-gap]: past the cap, a held test case's spans were joined", got)
	}
}

// A capture that can tell when a test case's verdict is final has it held
// until then, and checked then: a connection that stopped being recorded over
// the test case can become known after it completed (its parser failed behind
// the traffic). Checked as it came, it was streamed without its mocks.
func TestHandleIncoming_HoldsATestCaseUntilItsVerdictIsFinal(t *testing.T) {
	w := &switchWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })

	tcs := make(chan *models.TestCase, 4)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Far in the past, so no other test's spans reach it.
	start := time.Unix(10_000, 0)
	tcs <- &models.TestCase{Name: "over-a-stop-found-late", HTTPReq: models.HTTPReq{Timestamp: start}, HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Millisecond)}}
	later := start.Add(time.Hour)
	tcs <- &models.TestCase{Name: "clean", HTTPReq: models.HTTPReq{Timestamp: later}, HTTPResp: models.HTTPResp{Timestamp: later.Add(time.Millisecond)}}
	time.Sleep(100 * time.Millisecond) // held, not streamed

	// The parser fails, behind the traffic: it stopped where it had got to,
	// before the first test case ended.
	syncmgr.Get().RecordOrphanWindow(start.Add(-time.Second), start.Add(time.Second))
	w.settled.Store(true)
	close(tcs)

	got := streamedNames(t, resp)
	if len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]: a test case over a stop found after it completed was saved without its mocks", got)
	}
}

// backlogWatermark says a backlog is pending until flipped.
type backlogWatermark struct {
	switchWatermark
	pending atomic.Bool
}

func (w *backlogWatermark) PendingBefore(time.Time) bool { return w.pending.Load() }

// A recording's stop asks the agent whether it may still hand over something
// captured before it; an agent whose capture cannot tell says so (501), and
// the stop goes by quiet alone.
func TestHandlePending(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	ask := func() (int, string) {
		rr := httptest.NewRecorder()
		a.HandlePending(rr, httptest.NewRequest(http.MethodGet, "/agent/record/pending?before=1000", nil))
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	syncmgr.Get().SetWatermark(nil)
	if code, _ := ask(); code != http.StatusNotImplemented {
		t.Fatalf("without a watermark: %d, want 501", code)
	}
	w := &backlogWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil); syncmgr.Get().NoteHeld(time.Time{}) })
	w.pending.Store(true)
	if code, body := ask(); code != http.StatusOK || body != `{"pending":true}` {
		t.Fatalf("capture behind: %d %s", code, body)
	}
	w.pending.Store(false)
	if code, body := ask(); code != http.StatusOK || body != `{"pending":false}` {
		t.Fatalf("capture through: %d %s", code, body)
	}
	// A test case held for its verdict that ended before the time asked
	// about is pending too.
	syncmgr.Get().NoteHeld(time.Unix(0, 999))
	if _, body := ask(); body != `{"pending":true}` {
		t.Fatalf("a held test case from before: %s", body)
	}
	syncmgr.Get().NoteHeld(time.Unix(0, 1001))
	if _, body := ask(); body != `{"pending":false}` {
		t.Fatalf("a held test case from after: %s", body)
	}
}
