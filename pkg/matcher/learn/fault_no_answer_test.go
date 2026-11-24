package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestNoAnswerFaultHoldsNoAnswer verifies that HTTP fault recordings where the
// upstream wrote zero response bytes (EOF, or Reset/Timeout before the head)
// clear the zero-valued HTTPResp struct in payloadSpec so holdsAnswer reports
// false and an unstated serve in another test does not poison later calls.
// Faults that wrote a head, partial body, or delayed answer stay fail-closed.
func TestNoAnswerFaultHoldsNoAnswer(t *testing.T) {
	faultMock := func(name string, f *models.Fault, body string) *models.Mock {
		return &models.Mock{
			Name: name,
			Kind: models.HTTP,
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Method: "POST",
					URL:    "http://localhost:9324/",
					Body:   `{"WaitTimeSeconds":20}`,
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 0,
					Header:     map[string]string{},
					Body:       body,
				},
				Fault:            f,
				ReqTimestampMock: r1Insert.Add(-time.Minute),
				ResTimestampMock: r1Insert.Add(-time.Minute),
			},
		}
	}

	cases := []struct {
		name      string
		fault     *models.Fault
		body      string
		hasAnswer bool
	}{
		{"timeout before head", &models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true}, "", false},
		{"eof before head", &models.Fault{Kind: models.FaultEOF}, "", false},
		{"reset before head", &models.Fault{Kind: models.FaultReset}, "", false},
		{"reset before head with non-empty body stays fail-closed", &models.Fault{Kind: models.FaultReset}, `{"secret":"v"}`, true},
		{"timeout before head with non-empty body stays fail-closed", &models.Fault{Kind: models.FaultTimeout}, `{"secret":"v"}`, true},
		{"timeout after head", &models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 3}, "abc", true},
		{"reset after head", &models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 3}, "abc", true},
		{"truncate", &models.Fault{Kind: models.FaultTruncate, BodyBytes: 10}, "0123456789", true},
		{"stopped_reading", &models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5}, "01234", true},
		{"delay serves full answer", &models.Fault{Kind: models.FaultDelay, Delay: time.Second}, `{"ok":true}`, true},
		{"unknown fault fails closed", &models.Fault{Kind: "custom"}, "", true},
		{"no fault", nil, `{"ok":true}`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dc := clockedDrifts()
			m := faultMock("mock-fault", tc.fault, tc.body)
			x := dc.RawOf(m, true).ServedAs(2)
			if got := holdsAnswer(x.payload().fields); got != tc.hasAnswer {
				t.Fatalf("holdsAnswer = %v, want %v", got, tc.hasAnswer)
			}
			g := dc.wordsOf(x.OfAnotherTest())
			if g.unstated != tc.hasAnswer {
				t.Fatalf("wordsOf.unstated = %v, want %v", g.unstated, tc.hasAnswer)
			}

			// Verify GivenPlaces / statedWords agrees when F served m unstated
			// in an earlier test.
			tr := NewTracker()
			tr.Begin()
			tr.ServedMock(m)
			tr.End()
			tr.Begin()
			gp, err := tr.Placed().Order.GivenPlaces(m)
			if err != nil {
				t.Fatalf("GivenPlaces: %v", err)
			}
			first, last := tr.Placed().Order.Of(m)
			gp = gp.ServedIn(first, last)
			stated := tr.Placed().Order.statedWords(m, gp.words, gp.answers)
			if stated.unstated != tc.hasAnswer {
				t.Fatalf("statedWords.unstated = %v, want %v", stated.unstated, tc.hasAnswer)
			}
		})
	}
}

// TestUnstatedNoAnswerFaultDoesNotBlockLaterTestsExpiryRescue reproduces the
// exact sequence where an earlier test served an unstated FaultTimeout (zero
// response bytes, e.g. an SQS ReceiveMessage long-poll timeout) and a later
// test's body.data.expiresAt is offset-refused on pass 1 and re-read via
// sinceTestBegin on pass 2: the zero-answer fault must not block pass 2,
// whereas an unstated FaultTruncate (which wrote response bytes) stays
// fail-closed.
func TestUnstatedNoAnswerFaultDoesNotBlockLaterTestsExpiryRescue(t *testing.T) {
	recBegin := time.Date(2026, 11, 24, 4, 32, 26, 0, time.UTC)
	recName := recBegin.Add(248 * time.Millisecond)
	recStamp := recBegin.Add(274 * time.Millisecond)
	recPrev := recBegin.Add(279 * time.Millisecond)
	recPut := recBegin.Add(280 * time.Millisecond)

	liveBegin := recBegin.Add(4*time.Minute + 19*time.Second)
	liveName := liveBegin.Add(1500 * time.Millisecond)
	liveStamp := liveBegin.Add(1806 * time.Millisecond)
	livePrev := liveBegin.Add(1904 * time.Millisecond)
	livePut := liveBegin.Add(2116 * time.Millisecond)

	const ttl = 24 * time.Hour
	recExp := strconv.FormatInt(recStamp.Add(ttl).Unix(), 10)
	liveExp := strconv.FormatInt(liveStamp.Add(ttl).Unix(), 10)

	for _, tc := range []struct {
		name       string
		fault      *models.Fault
		respBody   string
		wantResCue bool
	}{
		{"unstated FaultTimeout lets expiresAt rescue", &models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true}, "", true},
		{"unstated FaultTruncate stays fail-closed", &models.Fault{Kind: models.FaultTruncate, BodyBytes: 10}, `{"Message":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc := NewDriftCache()
			dc.clientTests = true
			dc.zone = time.UTC

			faultMock := &models.Mock{
				Name: "mock-3773",
				Kind: models.HTTP,
				Spec: models.MockSpec{
					HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:9324/", Body: `{"WaitTimeSeconds":20}`},
					HTTPResp:         &models.HTTPResp{StatusCode: 0, Header: map[string]string{}, Body: tc.respBody},
					Fault:            tc.fault,
					ReqTimestampMock: recBegin.Add(-2 * time.Second),
					ResTimestampMock: recBegin.Add(-2 * time.Second),
				},
			}
			faultRaw := dc.RawOf(faultMock, true).ServedAs(2).OfAnotherTest()

			clientGiven := Earlier{
				Mock:  "test case post-query-96",
				Given: true,
				At:    recName,
				Seen: []Seen{{
					Side: Request,
					Leaf: Leaf{Path: "body.variables.input.name", Type: String, Value: "e2e-ci-oidc-ttl-" + strconv.FormatInt(recName.UnixNano(), 10)},
					Live: "e2e-ci-oidc-ttl-" + strconv.FormatInt(liveName.UnixNano(), 10),
				}},
			}

			w := &Window{
				Rec: Span{From: recPrev, To: recPut, Began: recBegin},
				F:   Span{From: livePrev, To: livePut, Began: liveBegin},
			}

			recShape := Shape{Leaves: map[string]Leaf{"body.data.expiresAt": {Path: "body.data.expiresAt", Type: Number, Value: recExp}}}
			liveShape := Shape{Leaves: map[string]Leaf{"body.data.expiresAt": {Path: "body.data.expiresAt", Type: Number, Value: liveExp}}}

			got := ClassifyArrived(dc, []string{"body.data.expiresAt"}, liveShape, recShape, recPut, livePut,
				[]Earlier{clientGiven}, []Raw{faultRaw}, nil, w, nil)
			if len(got) != 1 {
				t.Fatalf("got %d learned, want 1", len(got))
			}
			ln := got[0]
			if tc.wantResCue {
				if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.Since != models.SinceTestBegin {
					t.Fatalf("got %v (offset=%+v, why=%q); want KindClockOffset with SinceTestBegin", ln.Kind, ln.Offset, ln.Why)
				}
			} else {
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "24h0m0s after") {
					t.Fatalf("got %v (why=%q); want KindUnaccounted keeping offset refusal", ln.Kind, ln.Why)
				}
			}
		})
	}
}
