package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// servedCall is what serveReported saw of one call: the answer's status and
// body, and the reports the parser made of it, misses and answers alike.
type servedCall struct {
	status  int
	body    string
	reports []*models.MockMismatchReport
}

// answers is the reports of c that say it was answered (ReportAnswer).
func (c servedCall) answers() []*models.MockMismatchReport {
	var out []*models.MockMismatchReport
	for _, r := range c.reports {
		if r.Answer != "" {
			out = append(out, r)
		}
	}
	return out
}

// serveReported serves raw, a whole request, through decodeHTTP's replay on
// a connection of its own, with a reporter that keeps every report.
func serveReported(t *testing.T, h *HTTP, db *mockMemDb, raw string) servedCall {
	t.Helper()
	var mu sync.Mutex
	var reports []*models.MockMismatchReport
	app, proxy := tcpPair(t)
	done := runDecode(t, proxy, func(ctx context.Context) error {
		ctx = models.WithMockMismatchReporter(ctx, func(err error) {
			var withReport interface {
				MismatchReport() *models.MockMismatchReport
			}
			if errors.As(err, &withReport) {
				mu.Lock()
				reports = append(reports, withReport.MismatchReport())
				mu.Unlock()
			}
		})
		return h.decodeHTTP(ctx, []byte(raw), proxy, &models.ConditionalDstCfg{Addr: "127.0.0.1:1"}, db, models.OutgoingOptions{})
	})
	_ = app.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return once the app closed its connection")
	}
	mu.Lock()
	defer mu.Unlock()
	return servedCall{status: resp.StatusCode, body: string(body), reports: slices.Clone(reports)}
}

func getUser(id string) string {
	return "GET /users/" + id + " HTTP/1.1\r\nHost: users.local\r\n\r\n"
}

func searchCall(body string) string {
	return "POST /search HTTP/1.1\r\nHost: users.local\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// The quickstart's shape: the recording has GET /users/1 and GET /users/2,
// and a test calls GET /users/3. With the proven switch off the call is
// served user 1's recording, as #4289 made the default for a path id the
// recording lacks, and the parser reports it as answered with another call's
// recording (models.AnswerOtherID), naming the recording, its call and the
// segment that differs, never as the call's own answer. A call the
// recording has is answered with no such report. Under the switch the call
// is not served user 1's recording, and nothing reports it answered so.
func TestACallOfAnotherIDIsReportedAsAnsweredWithAnotherCallsRecording(t *testing.T) {
	recordings := func(t *testing.T, h *HTTP) *mockMemDb {
		return &mockMemDb{mocks: []*models.Mock{
			recordedAs(t, h, "mock-user-1", 1, getUser("1"), jsonAnswer(200, `{"id":1,"name":"Ada Lovelace"}`), nil),
			recordedAs(t, h, "mock-user-2", 2, getUser("2"), jsonAnswer(200, `{"id":2,"name":"Alan Turing"}`), nil),
		}, updateUnFilteredReturn: true}
	}
	t.Run("off", func(t *testing.T) {
		h := newHTTP()
		got := serveReported(t, h, recordings(t, h), getUser("3"))
		if got.status != 200 || !strings.Contains(got.body, "Ada Lovelace") {
			t.Fatalf("GET /users/3 got %d %q, want user 1's recording served (the default since #4289)", got.status, got.body)
		}
		a := got.answers()
		if len(a) != 1 {
			t.Fatalf("GET /users/3 made %d answer reports (%d reports), want 1", len(a), len(got.reports))
		}
		r := a[0]
		if r.Answer != models.AnswerOtherID || r.ClosestMock != "mock-user-1" {
			t.Errorf("the report says answer %q with %q, want %q with mock-user-1", r.Answer, r.ClosestMock, models.AnswerOtherID)
		}
		if !slices.Equal(r.Differs, []string{"url.path.1"}) {
			t.Errorf("the report says the call differs at %q, want [url.path.1]", r.Differs)
		}
		if r.ActualSummary != "GET /users/3" || !strings.Contains(r.Diff, "GET /users/3 was answered with the recording of GET /users/1") {
			t.Errorf("the report names the call %q and says %q", r.ActualSummary, r.Diff)
		}
		if r.NextSteps == "" || r.CountsAgainstTest() {
			t.Errorf("the report has next steps %q and counts against its test: %v", r.NextSteps, r.CountsAgainstTest())
		}

		h = newHTTP()
		own := serveReported(t, h, recordings(t, h), getUser("1"))
		if own.status != 200 || !strings.Contains(own.body, "Ada Lovelace") || len(own.reports) != 0 {
			t.Fatalf("GET /users/1 got %d %q with %d reports, want its own recording and none", own.status, own.body, len(own.reports))
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		h := newHTTP()
		got := serveReported(t, h, recordings(t, h), getUser("3"))
		if strings.Contains(got.body, "Ada Lovelace") {
			t.Fatalf("GET /users/3 was served user 1's recording under the switch: %d %q", got.status, got.body)
		}
		for _, r := range got.answers() {
			t.Errorf("GET /users/3 was reported answered (%q with %q) under the switch", r.Answer, r.ClosestMock)
		}
	})
}

// With the switch off a call whose body none of the recordings of its
// method and path has is served the closest of them by the fuzzy fallback,
// and reported as answered with another call's recording
// (models.AnswerClosestBody). A call that is one of them byte for byte, and
// a call that only one recording of its body's keys answers, are answered
// with no such report.
func TestTheClosestOfSeveralBodiesIsReportedAsAnsweredWithAnotherCallsRecording(t *testing.T) {
	recordings := func(t *testing.T, h *HTTP, bodies ...string) *mockMemDb {
		db := &mockMemDb{updateUnFilteredReturn: true}
		for i, b := range bodies {
			db.mocks = append(db.mocks, recordedAs(t, h, "mock-search-"+strconv.Itoa(i+1), i+1, searchCall(b), jsonAnswer(200, `{"hit":`+strconv.Itoa(i+1)+`}`), nil))
		}
		return db
	}
	h := newHTTP()
	got := serveReported(t, h, recordings(t, h, `{"q":"ada","page":1}`, `{"q":"alan","page":2}`), searchCall(`{"q":"ada","page":3}`))
	if got.status != 200 {
		t.Fatalf("the call got %d %q, want the closest recording served", got.status, got.body)
	}
	a := got.answers()
	if len(a) != 1 || a[0].Answer != models.AnswerClosestBody || !slices.Equal(a[0].Differs, []string{"body"}) {
		t.Fatalf("the call's answer reports are %+v, want one %q differing at body", a, models.AnswerClosestBody)
	}
	if !strings.HasPrefix(a[0].ClosestMock, "mock-search-") || !strings.Contains(a[0].Diff, "POST /search was answered with the recording of POST /search") {
		t.Errorf("the report names %q and says %q", a[0].ClosestMock, a[0].Diff)
	}

	h = newHTTP()
	exact := serveReported(t, h, recordings(t, h, `{"q":"ada","page":1}`, `{"q":"alan","page":2}`), searchCall(`{"q":"alan","page":2}`))
	if exact.status != 200 || !strings.Contains(exact.body, `"hit":2`) || len(exact.answers()) != 0 {
		t.Fatalf("the recorded call got %d %q with %d answer reports, want its own recording and none", exact.status, exact.body, len(exact.answers()))
	}

	h = newHTTP()
	one := serveReported(t, h, recordings(t, h, `{"q":"ada","page":1}`), searchCall(`{"q":"ada","page":3}`))
	if one.status != 200 || len(one.answers()) != 0 {
		t.Fatalf("the call one recording's body keys answer got %d %q with %d answer reports, want it served with none", one.status, one.body, len(one.answers()))
	}
}

// idSegmentsDiffering names each segment that differs, by the learn step's
// place names, and none of the segments that are the same.
func TestIDSegmentsDiffering(t *testing.T) {
	for _, tc := range []struct {
		rec, live string
		want      []string
	}{
		{"/users/1", "/users/3", []string{"url.path.1"}},
		{"/orgs/7/users/1", "/orgs/8/users/2", []string{"url.path.1", "url.path.3"}},
		{"/users/1", "/users/1", nil},
	} {
		if got := idSegmentsDiffering(tc.rec, tc.live); !slices.Equal(got, tc.want) {
			t.Errorf("idSegmentsDiffering(%q, %q) = %q, want %q", tc.rec, tc.live, got, tc.want)
		}
	}
}
