package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// earlierDb is a mockMemDb whose reads show nothing, that remembers the
// recordings that answered earlier in the set (integrations.AnsweredFinder):
// answered, oldest first. asked are the keys each lookup asked by.
type earlierDb struct {
	*mockMemDb
	answered []integrations.AnsweredCall
	asked    *[][]string
}

func (d earlierDb) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	if d.asked != nil {
		*d.asked = append(*d.asked, keys)
	}
	for i := len(d.answered) - 1; i >= 0; i-- {
		a := d.answered[i]
		if a.Mock.Kind == kind && match(a.Mock) {
			return a, true
		}
	}
	return integrations.AnsweredCall{}, false
}

// sapCount is a recording of the call a cache fills: a GET with a query.
func sapCount(name, host string, recordedAt time.Time) *models.Mock {
	m := onHost(name, "/sap/A_BusinessPartner/$count?$filter=x", host)
	m.Spec.HTTPReq.Header["X-Request-Id"] = "rec-1"
	m.Spec.ReqTimestampMock = recordedAt
	return m
}

// A request identical to an earlier answered recording, by method,
// destination, path, query and body, is found, whatever other header it
// carries (a request id); one that differs in the query, the path, the
// destination or the X-Amz-Target, an AWS call's action, is not. A store that
// keeps no answered history finds nothing.
func TestARepeatIsTheSameRequestHeadersAside(t *testing.T) {
	const host = "localhost:8099"
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	earlier := sapCount("mock-7", host, rec)
	db := earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{Mock: earlier, AnsweredAt: rec}}}
	n := newHTTP().newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	live := func(path string) *req {
		r := getTo("/sap/A_BusinessPartner/$count", host)
		r.url.RawQuery = path
		r.header.Set("X-Request-Id", "live-2")
		return r
	}
	if a, ok := repeatOfAnswered(ctx, live("$filter=x"), db, n); !ok || a.Mock != earlier {
		t.Fatalf("an identical request with another request id found %v (%v), want %s", a.Mock, ok, earlier.Name)
	}
	if a, ok := repeatOfAnswered(ctx, live("$filter=y"), db, n); ok {
		t.Fatalf("a request with another query was taken for a repeat of %s", a.Mock.Name)
	}
	other := getTo("/sap/A_BusinessPartner/$count", "otherhost:8099")
	other.url.RawQuery = "$filter=x"
	if a, ok := repeatOfAnswered(ctx, other, db, n); ok {
		t.Fatalf("a request to another destination was taken for a repeat of %s", a.Mock.Name)
	}
	if _, ok := repeatOfAnswered(ctx, live("$filter=x"), &mockMemDb{mocks: []*models.Mock{earlier}}, n); ok {
		t.Fatal("a store that keeps no answered history found a repeat")
	}

	// The X-Amz-Target names the action: two calls that differ only there
	// are two calls.
	sqs := onHost("mock-sqs", "/", host)
	sqs.Spec.HTTPReq.Header["X-Amz-Target"] = "AmazonSQS.SendMessage"
	amz := earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{Mock: sqs, AnsweredAt: rec}}}
	call := getTo("/", host)
	call.header.Set("X-Amz-Target", "AmazonSQS.ReceiveMessage")
	if a, ok := repeatOfAnswered(ctx, call, amz, n); ok {
		t.Fatalf("a call of another X-Amz-Target was taken for a repeat of %s", a.Mock.Name)
	}
	call.header.Set("X-Amz-Target", "AmazonSQS.SendMessage")
	if _, ok := repeatOfAnswered(ctx, call, amz, n); !ok {
		t.Fatal("a call of the same X-Amz-Target was not taken for a repeat")
	}
}

// sameRequest tolerates a difference in a header, never in the X-Amz-Target
// nor in any other field.
func TestSameRequestToleratesOnlyHeaders(t *testing.T) {
	f := func(path string) strictField { return field(models.MockFieldDiff{Path: path}) }
	rows := []struct {
		diffs []strictField
		same  bool
	}{
		{nil, true},
		{[]strictField{f("header.x-request-id"), f("header.Date")}, true},
		{[]strictField{f("header.X-Amz-Target")}, false},
		{[]strictField{f("header.x-request-id"), f("query.$filter")}, false},
		{[]strictField{f("body.id")}, false},
		{[]strictField{f("method")}, false},
		{[]strictField{f("host")}, false},
	}
	for _, r := range rows {
		if got := sameRequest(r.diffs); got != r.same {
			t.Errorf("sameRequest(%v) = %v, want %v", r.diffs, got, r.same)
		}
	}
}

// The HTTP parser's miss on a call identical to an earlier test's answered
// recording names that recording as the call's closest, with the note
// (models.RepeatNote): the gaps from the store's times and the call's, and
// the likely cause, with the switch on or off. A call no earlier answer is
// gets no note.
func TestAMissOnARepeatedCallNamesTheEarlierRecording(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	miss := func(t *testing.T, path string, opts models.OutgoingOptions, shown ...*models.Mock) *models.MockMismatchReport {
		t.Helper()
		earlier := sapCount("mock-7", host, rec)
		// The open test began 3 s after the recording at record time; the
		// earlier call was answered an hour before this one at replay.
		db := earlierDb{mockMemDb: &mockMemDb{mocks: shown}, answered: []integrations.AnsweredCall{{
			Mock: earlier, AnsweredAt: time.Now().Add(-time.Hour), TestRecordedStart: rec.Add(3 * time.Second),
		}}}
		var reported error
		ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
		app, prox := net.Pipe()
		defer app.Close()
		done := make(chan error, 1)
		go func() {
			done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, opts)
		}()
		if _, err := io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\nX-Request-Id: live-9\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			t.Fatalf("the app got no answer: %v", err)
		}
		_ = resp.Body.Close()
		_ = app.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the parser did not return after the miss")
		}
		var withReport interface {
			MismatchReport() *models.MockMismatchReport
		}
		if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
			t.Fatalf("the miss was reported as %v, with no mismatch report", reported)
		}
		return withReport.MismatchReport()
	}
	for _, row := range []struct {
		on   string
		opts models.OutgoingOptions
		// reason is judging's code the miss keeps: in record's full round
		// the learn step finds no recorded call of the test for it.
		reason string
	}{
		{"", models.OutgoingOptions{}, ""},
		{"1", models.OutgoingOptions{}, ""},
		{"1", models.OutgoingOptions{Learn: true}, models.MissUnmockedCall},
	} {
		on := row.on
		t.Setenv(provenswitch.Env, on)
		if row.opts.Learn {
			on += "+learn"
		}
		r := miss(t, "/sap/A_BusinessPartner/$count?$filter=x", row.opts)
		if r.Reason != row.reason {
			t.Fatalf("switch %q: the miss's reason is %q, want %q", on, r.Reason, row.reason)
		}
		if r.Repeat == nil || r.Repeat.Mock != "mock-7" || r.ClosestMock != "mock-7" {
			t.Fatalf("switch %q: the miss of a repeated call is %+v (repeat %+v), want it named as mock-7", on, r, r.Repeat)
		}
		if !r.Repeat.RecordGapKnown || r.Repeat.RecordGapSeconds != 3 || r.Repeat.ReplayGapSeconds < 3600 {
			t.Fatalf("switch %q: the gaps are %+v, want 3s at record and an hour at replay", on, r.Repeat)
		}
		if !strings.Contains(r.NextSteps, "identical to mock-7") || !strings.Contains(r.NextSteps, "elapsed time") || len(r.FieldDiffs) != 0 {
			t.Fatalf("switch %q: the next step is %q (diffs %v)", on, r.NextSteps, r.FieldDiffs)
		}
		if r := miss(t, "/sap/A_BusinessPartner/$count?$filter=other", row.opts); r.Repeat != nil {
			t.Fatalf("switch %q: a call no earlier answer is got a note: %+v", on, r.Repeat)
		}
	}

	// In record's full round a learn step that names a recorded call the
	// request changed from keeps its reason and closest: the note joins only
	// a call the test recorded nothing like (unmocked_call).
	t.Setenv(provenswitch.Env, "1")
	changed := onHost("mock-own", "/sap/A_BusinessPartner/$count?$filter=x&$top=1", host)
	r := miss(t, "/sap/A_BusinessPartner/$count?$filter=x", models.OutgoingOptions{Learn: true, LearnPool: true}, changed)
	if r.Reason == models.MissUnmockedCall || r.Reason == "" || r.Repeat != nil || r.ClosestMock == "mock-7" {
		t.Fatalf("a learn miss with reason %q got the note %+v (closest %q), want its own reason kept and no note", r.Reason, r.Repeat, r.ClosestMock)
	}
}
