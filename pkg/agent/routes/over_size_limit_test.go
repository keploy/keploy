package routes

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/hooks/conn"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// endSummaryOf reads an incoming stream's parts to its end marker, and returns
// what the marker says the session lost.
func endSummaryOf(t *testing.T, contentType string, body io.Reader) models.StreamEndSummary {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended without its marker: %v", err)
		}
		if part.FormName() != models.StreamEndPart {
			continue
		}
		var sum models.StreamEndSummary
		if err := json.NewDecoder(part).Decode(&sum); err != nil {
			t.Fatalf("the marker does not decode: %v", err)
		}
		return sum
	}
}

// Each recording session says, in its summary, its end marker and a WARN, the
// requests the app served that its ingress did not record as test cases
// because each was over keploy's size limit for one: how many
// (tcs_not_recorded_over_size_limit), the calls with the most, and the limits.
// They were counted nowhere: a recording ended short of its traffic without a
// word, and a lane that held the recording to its requests could not tell
// these from requests lost by mistake. Only the session's own: one counted
// before its stream opened is not, and one counted after its marker went out
// is in neither the marker nor the summary, which say the same. The next
// session, which loses none, says none.
func TestEachRecordingSessionSaysItsRequestsOverTheSizeLimit(t *testing.T) {
	syncmgr.Get().SetWatermark(nil) // no hold: test cases go at once
	// Each session's end ends the recording on the default manager: the
	// tests after this one start with one recording.
	t.Cleanup(syncmgr.Get().StartRecordSession)
	syncmgr.Get().NoteOverSizeLimit("GET /{} [path tag #before]")
	openBefore := syncmgr.Get().OpenOverSizeTallies()
	for session, lost := range [][]string{
		{"POST /{} [path tag #a]", "GET /{}/{} [path tag #b]", "POST /{} [path tag #a]", "POST /{} [path tag #a]"},
		nil,
	} {
		svc := streamsSvc{tc: make(chan *models.TestCase, 1), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
		// Debug: the marker's flush is logged at Debug, and a hook runs
		// only on the entries its core takes.
		core, logs := observer.New(zapcore.DebugLevel)
		var markerSeen int
		core = zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
			if e.Message == streamEndMarkerSent {
				// After the marker, before the summary the handler
				// defers: the session's count is read once.
				markerSeen++
				syncmgr.Get().NoteOverSizeLimit("PUT /{} [path tag #after-marker]")
			}
			return nil
		})
		a := &Agent{logger: zap.New(core), svc: svc}
		tok := rand.Text() // its own each run: a token ended stays ended
		resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{}, tok)
		svc.tc <- windowedNow("test-1")
		for _, call := range lost {
			syncmgr.Get().NoteOverSizeLimit(call)
		}
		endRecordingStreams(tok)
		sum := endSummaryOf(t, resp.Header.Get("Content-Type"), resp.Body)
		done() // the handler has returned, its summary said
		if markerSeen != 1 {
			t.Fatalf("session %d's marker was flushed %d times, want once: the count after it is never made", session+1, markerSeen)
		}
		// The session's tally ends with it: one left open would count
		// every later request over the size limit, for good.
		if n := syncmgr.Get().OpenOverSizeTallies(); n != openBefore {
			t.Fatalf("after session %d, %d tallies of requests over the size limit are open, want the %d before it", session+1, n, openBefore)
		}

		var want models.OverSizeSkipSummary
		if len(lost) > 0 {
			want = models.OverSizeSkipSummary{Requests: 4,
				Calls:      []string{"POST /{} [path tag #a] (3)", "GET /{}/{} [path tag #b] (1)"},
				LimitBytes: conn.MaxTestCaseSize, BodyLimitBytes: conn.MaxTestCaseBodySize}
		}
		if !reflect.DeepEqual(sum.NotRecordedOverSizeLimit, want) {
			t.Fatalf("session %d's end marker says %+v of the requests over the size limit, want %+v", session+1, sum.NotRecordedOverSizeLimit, want)
		}
		line := logs.FilterMessage("agent: recording complete").All()
		if len(line) != 1 || line[0].ContextMap()["tcs_not_recorded_over_size_limit"] != want.Requests {
			t.Fatalf("session %d's summary is %v, want tcs_not_recorded_over_size_limit=%d", session+1, line, want.Requests)
		}
		warned := logs.FilterMessage(models.OverSizeLimitSaid).All()
		if len(lost) == 0 {
			if len(warned) != 0 {
				t.Fatalf("session %d, which lost no request to the size limit, warned %v", session+1, warned)
			}
			continue
		}
		if len(warned) != 1 || warned[0].Level != zapcore.WarnLevel {
			t.Fatalf("session %d warned of its requests over the size limit %d times, want once at WARN", session+1, len(warned))
		}
		f := warned[0].ContextMap()
		if f["requests"] != uint64(4) || f["limit_bytes"] != int64(conn.MaxTestCaseSize) || f["body_limit_bytes"] != int64(conn.MaxTestCaseBodySize) ||
			fmt.Sprint(f["calls_most"]) != fmt.Sprint(want.Calls) || f["next_step"] != models.OverSizeLimitNextStep {
			t.Fatalf("session %d's warning says %v, want its 4 requests, the calls with the most, the limits and what to do", session+1, f)
		}
	}
}
