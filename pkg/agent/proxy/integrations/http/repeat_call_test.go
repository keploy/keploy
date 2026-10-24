package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// earlierDb is a mockMemDb whose reads show nothing, that remembers the
// recordings that answered earlier in the set (integrations.AnsweredFinder):
// answered, oldest first. asked are the keys each lookup asked by. byKeys
// makes a lookup see only the answers recorded under one of its keys
// (models.RequestKeys), as the agent's answered log does.
type earlierDb struct {
	*mockMemDb
	answered []integrations.AnsweredCall
	asked    *[][]string
	byKeys   bool
}

func (d earlierDb) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	if d.asked != nil {
		*d.asked = append(*d.asked, keys)
	}
	for i := len(d.answered) - 1; i >= 0; i-- {
		a := d.answered[i]
		if d.byKeys && !slices.ContainsFunc(models.RequestKeys(a.Mock), func(k string) bool { return slices.Contains(keys, k) }) {
			continue
		}
		if a.Mock.Kind == kind && match(a.Mock) {
			return a, true
		}
	}
	return integrations.AnsweredCall{}, false
}

// ownDb is earlierDb whose per-test read shows the open test's own
// recordings, none of them consumed.
type ownDb struct {
	earlierDb
	own []*models.Mock
}

func (d ownDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return d.own, nil }
func (d ownDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return d.own, nil }

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
// (models.RepeatNote): the gap at replay from the store's time and the
// call's, and the recording's time at record, with the switch on or off. A call no earlier answer is
// gets no note.
func TestAMissOnARepeatedCallNamesTheEarlierRecording(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	// own are the open test's own recordings its per-test read shows.
	var own []*models.Mock
	miss := func(t *testing.T, path string, opts models.OutgoingOptions, shown ...*models.Mock) *models.MockMismatchReport {
		t.Helper()
		earlier := sapCount("mock-7", host, rec)
		// The earlier call was answered an hour before this one at replay.
		db := ownDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{mocks: shown}, answered: []integrations.AnsweredCall{{
			Mock: earlier, AnsweredAt: time.Now().Add(-time.Hour),
		}}}, own: own}
		return missReport(t, db, addr, host, path, opts)
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
		// The gap at record, and so the likely cause, the replay measures
		// from the set's recordings, which the agent does not hold.
		if r.Repeat.RecordGapKnown || r.Repeat.ReplayGapFromStart || r.Repeat.ReplayGapSeconds < 3600 || !r.Repeat.RecordedAt.Equal(rec) {
			t.Fatalf("switch %q: the gaps are %+v, want an hour at replay, to the call, recorded at %v, and none at record", on, r.Repeat, rec)
		}
		if !strings.Contains(r.NextSteps, "identical to mock-7") || !strings.Contains(r.NextSteps, "elapsed time") || len(r.FieldDiffs) != 0 {
			t.Fatalf("switch %q: the next step is %q (diffs %v)", on, r.NextSteps, r.FieldDiffs)
		}
		if r := miss(t, "/sap/A_BusinessPartner/$count?$filter=other", row.opts); r.Repeat != nil {
			t.Fatalf("switch %q: a call no earlier answer is got a note: %+v", on, r.Repeat)
		}
	}

	// The open test's own recording of the endpoint, unconsumed, that the
	// call differs from in its query: the call may be that recording
	// changed, so the miss names it as its closest, with the query's diff,
	// and gets no note, with the switch on or off. An own recording of
	// another endpoint leaves the note.
	for _, on := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, on)
		own = []*models.Mock{onHost("mock-own", "/sap/A_BusinessPartner/$count?$filter=y", host)}
		r := miss(t, "/sap/A_BusinessPartner/$count?$filter=x", models.OutgoingOptions{})
		if r.Repeat != nil || r.ClosestMock != "mock-own" || len(r.FieldDiffs) == 0 || strings.Contains(r.NextSteps, "identical to") {
			t.Fatalf("switch %q: a call that differs from the test's own recording got closest %q, diffs %v, note %+v, next step %q; want mock-own with its diff and no note", on, r.ClosestMock, r.FieldDiffs, r.Repeat, r.NextSteps)
		}
		own = []*models.Mock{onHost("mock-own", "/sap/A_Customer?$filter=y", host)}
		if r := miss(t, "/sap/A_BusinessPartner/$count?$filter=x", models.OutgoingOptions{}); r.Repeat == nil || r.Repeat.Mock != "mock-7" {
			t.Fatalf("switch %q: the test's own recording of another endpoint took the note away: %+v", on, r)
		}
		own = nil
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

// missReport is the mismatch report of a GET of path to host (addr) the
// HTTP parser answers from db, with a request id no recording carries.
func missReport(t *testing.T, db integrations.MockMemDb, addr, host, path string, opts models.OutgoingOptions) *models.MockMismatchReport {
	t.Helper()
	return missReportOf(t, db, addr, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\nX-Request-Id: live-9\r\n\r\n", opts)
}

// missReportOf is the mismatch report of raw, a whole HTTP/1.1 request to
// addr, that the HTTP parser answers from db.
func missReportOf(t *testing.T, db integrations.MockMemDb, addr, raw string, opts models.OutgoingOptions) *models.MockMismatchReport {
	t.Helper()
	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
	app, prox := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, opts)
	}()
	if _, err := io.WriteString(app, raw); err != nil {
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

// gateDb is earlierDb under exact serving: staged are other tests'
// recordings no read shows (integrations.StagedFinder), and consumed the open
// test's recordings that already answered a call of it
// (integrations.ExactConsumedReader).
type gateDb struct {
	earlierDb
	staged   []*models.Mock
	consumed []*models.Mock
}

func (d gateDb) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			return m.Name
		}
	}
	return ""
}

func (d gateDb) ConsumedTestMocks() []*models.Mock { return d.consumed }

// Under exact serving a call that is another test's recording no read shows
// stays named as that recording made elsewhere (not owned): it is the reason
// the call missed, and its closest. When an earlier test's identical
// recording already answered, the note joins it beside that reason, never
// instead of it: the next step names both, and says judging moves the
// recording to the worker scope as any call made elsewhere, the repeat being
// information about why the application asked again. With no earlier
// answer the miss is the plain not-owned one. A call the test recorded fewer
// times than it made stays one call more than recorded (extra_call), the
// test's own recording its closest, with no note.
func TestARepeatNoteJoinsANotOwnedMissAndLeavesAnExtraCall(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	t.Setenv(provenswitch.Env, "1")
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	earlier := earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{
		Mock: sapCount("mock-7", host, rec), AnsweredAt: time.Now().Add(-time.Hour),
	}}}
	if r := missReport(t, gateDb{earlierDb: earlier}, addr, host, path, models.OutgoingOptions{}); r.Repeat == nil || r.Repeat.Mock != "mock-7" || r.NotOwned || r.MissKind == models.MissExtraCall {
		t.Fatalf("with nothing else to explain it the miss is %+v (repeat %+v), want the note on mock-7", r, r.Repeat)
	}

	elsewhere := onHost("mock-w", path, host)
	elsewhere.Spec.HTTPReq.Header["X-Request-Id"] = "live-9"
	r := missReport(t, gateDb{earlierDb: earlier, staged: []*models.Mock{elsewhere}}, addr, host, path, models.OutgoingOptions{})
	if !r.NotOwned || r.ClosestMock != "mock-w" || len(r.FieldDiffs) != 0 || r.Repeat == nil || r.Repeat.Mock != "mock-7" {
		t.Fatalf("a call made elsewhere that repeats an earlier answer is closest %q, not owned %v, diffs %v, note %+v; want mock-w made elsewhere with the note on mock-7 beside it", r.ClosestMock, r.NotOwned, r.FieldDiffs, r.Repeat)
	}
	for _, want := range []string{"This request is mock-w, a recording keploy made between tests or in another test's window", "identical to mock-7", "it moves mock-w to the test set's worker scope", "its confirming round still compares this test's response"} {
		if !strings.Contains(r.NextSteps, want) {
			t.Fatalf("the next step of a call made elsewhere that repeats an earlier answer lacks %q: %s", want, r.NextSteps)
		}
	}
	if strings.Contains(r.NextSteps, "does not move") {
		t.Fatalf("the next step says judging keeps mock-w where it is: %s", r.NextSteps)
	}

	// In record's full round a learn step may name a recording the request
	// changed from; a call made elsewhere is named as that recording all the
	// same, with the note beside it, which the learn reason no longer
	// explains.
	changed := onHost("mock-c", path+"&$top=1", host)
	learning := earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{changed}}, answered: earlier.answered}
	r = missReport(t, gateDb{earlierDb: learning, staged: []*models.Mock{elsewhere}}, addr, host, path, models.OutgoingOptions{Learn: true, LearnPool: true})
	if !r.NotOwned || r.ClosestMock != "mock-w" || r.Repeat == nil || r.Repeat.Mock != "mock-7" || !strings.Contains(r.NextSteps, "This request is mock-w") {
		t.Fatalf("in the learn round a call made elsewhere is closest %q, not owned %v, note %+v, next step %q; want mock-w with the note on mock-7", r.ClosestMock, r.NotOwned, r.Repeat, r.NextSteps)
	}

	alone := earlierDb{mockMemDb: &mockMemDb{}}
	r = missReport(t, gateDb{earlierDb: alone, staged: []*models.Mock{elsewhere}}, addr, host, path, models.OutgoingOptions{})
	if !r.NotOwned || r.ClosestMock != "mock-w" || r.Repeat != nil || r.NextSteps != notOwnedNextSteps("mock-w") {
		t.Fatalf("a call made elsewhere with no earlier answer is closest %q, not owned %v, note %+v, next step %q; want the plain not-owned miss", r.ClosestMock, r.NotOwned, r.Repeat, r.NextSteps)
	}

	own := onHost("mock-own", path, host)
	r = missReport(t, gateDb{earlierDb: earlier, consumed: []*models.Mock{own}}, addr, host, path, models.OutgoingOptions{})
	if r.MissKind != models.MissExtraCall || r.Recorded != 1 || r.Repeat != nil || strings.Contains(r.NextSteps, "identical to mock-7") {
		t.Fatalf("one call more than recorded is kind %q (recorded %d), note %+v, next step %q; want extra_call, recorded once, and no note", r.MissKind, r.Recorded, r.Repeat, r.NextSteps)
	}
}

// perTestDb is earlierDb whose per-test read shows perTest, or fails with
// readErr; spent are the recordings it reports spent (integrations.
// SpentReader), and others those it tells are not the open test's own
// (integrations.TestOwner).
type perTestDb struct {
	earlierDb
	perTest []*models.Mock
	readErr error
	spent   map[*models.Mock]bool
	others  map[*models.Mock]bool
}

func (d perTestDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return d.perTest, d.readErr }
func (d perTestDb) Spent(m *models.Mock) bool                        { return d.spent[m] }
func (d perTestDb) OwnedByOpenTest(m *models.Mock) bool              { return !d.others[m] }

// Only the open test's own live recording under the call's keys keeps the
// note off (ownRecordingUnder): its diff explains the miss. Another test's
// recording the per-test read shows, and a stateful recording of an
// earlier test the store reports spent, are not the open test's to explain
// it, and leave the note; a per-test read that fails keeps it off, the side
// that hides nothing.
func TestOnlyTheOpenTestsOwnLiveRecordingKeepsTheNoteOff(t *testing.T) {
	const host, path = "localhost:8099", "/sap/A_BusinessPartner/$count"
	keys := RequestKeysOf("GET", path, "", false)
	mine := onHost("mock-own", path+"?$filter=y", host)
	other := onHost("mock-other", path+"?$filter=y", host)
	cursor := onHost("mock-cursor", path+"?$filter=y", host)
	base := earlierDb{mockMemDb: &mockMemDb{}}
	for _, c := range []struct {
		name string
		db   perTestDb
		want bool
	}{
		{"the open test's own recording", perTestDb{earlierDb: base, perTest: []*models.Mock{mine}}, true},
		{"a per-test read that fails", perTestDb{earlierDb: base, readErr: errors.New("read failed")}, true},
		{"another test's recording", perTestDb{earlierDb: base, perTest: []*models.Mock{other}, others: map[*models.Mock]bool{other: true}}, false},
		{"a spent stateful recording", perTestDb{earlierDb: base, perTest: []*models.Mock{cursor}, spent: map[*models.Mock]bool{cursor: true}}, false},
		{"none", perTestDb{earlierDb: base}, false},
	} {
		if got := ownRecordingUnder(c.db, keys); got != c.want {
			t.Errorf("%s: own recording under the call's keys %v, want %v", c.name, got, c.want)
		}
	}
}

// When the twin becomes a miss's closest, the miss names no difference: a
// call that first came closest to another recording, with that recording's
// diffs, is identical to the twin, and naming the twin with the other's
// diffs would say the call differs from it.
func TestARepeatNamesTheTwinWithNoOtherRecordingsDiffs(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	near := onHost("mock-near", "/sap/A_BusinessPartner/$count?$filter=y", host)
	alone := earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{near}}}
	if r := missReport(t, alone, addr, host, path, models.OutgoingOptions{}); r.ClosestMock != "mock-near" || len(r.FieldDiffs) == 0 {
		t.Fatalf("with no earlier answer the miss is closest %q with diffs %v; want mock-near with its query's diff", r.ClosestMock, r.FieldDiffs)
	}
	twin := earlierDb{mockMemDb: alone.mockMemDb, answered: []integrations.AnsweredCall{{Mock: sapCount("mock-7", host, rec), AnsweredAt: time.Now().Add(-time.Minute)}}}
	r := missReport(t, twin, addr, host, path, models.OutgoingOptions{})
	if r.ClosestMock != "mock-7" || r.Repeat == nil || len(r.FieldDiffs) != 0 || len(r.Differs) != 0 || r.Diff != "" {
		t.Fatalf("the repeat's miss is closest %q, note %+v, diffs %v, differs %v, diff %q; want mock-7 with no difference named", r.ClosestMock, r.Repeat, r.FieldDiffs, r.Differs, r.Diff)
	}
}

// An AWS JSON call names its action in its X-Amz-Target, which is part of
// its request keys: a repeat of one is found under the key that carries its
// target, and gets the note; a call with another target is another action,
// and gets none.
func TestARepeatedAWSCallIsFoundUnderItsTarget(t *testing.T) {
	const addr, host = "127.0.0.1:4566", "localhost:4566"
	const body = `{"QueueUrl":"http://localhost:4566/000000000000/orders"}`
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	recorded := onHost("mock-sqs", "/", host)
	recorded.Spec.HTTPReq.Method = "POST"
	recorded.Spec.HTTPReq.Header["X-Amz-Target"] = "AmazonSQS.GetQueueAttributes"
	recorded.Spec.HTTPReq.Header["Content-Type"] = "application/x-amz-json-1.0"
	recorded.Spec.HTTPReq.Body = body
	recorded.Spec.ReqTimestampMock = rec
	var asked [][]string
	db := earlierDb{mockMemDb: &mockMemDb{}, byKeys: true, asked: &asked, answered: []integrations.AnsweredCall{{Mock: recorded, AnsweredAt: time.Now().Add(-time.Minute)}}}
	call := func(target string) string {
		return "POST / HTTP/1.1\r\nHost: " + host + "\r\nContent-Type: application/x-amz-json-1.0\r\nX-Amz-Target: " + target + "\r\nX-Amz-Date: 20261024T100000Z\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	r := missReportOf(t, db, addr, call("AmazonSQS.GetQueueAttributes"), models.OutgoingOptions{})
	if r.Repeat == nil || r.Repeat.Mock != "mock-sqs" || r.ClosestMock != "mock-sqs" {
		t.Fatalf("a repeated SQS call's miss is closest %q, note %+v (asked by %q); want the note on mock-sqs", r.ClosestMock, r.Repeat, asked)
	}
	if want := RequestKeysOf("POST", "/", "AmazonSQS.GetQueueAttributes", false); len(asked) == 0 || !slices.Equal(asked[len(asked)-1], want) {
		t.Fatalf("the lookup asked by %q, want %q", asked, want)
	}
	if r := missReportOf(t, db, addr, call("AmazonSQS.ReceiveMessage"), models.OutgoingOptions{}); r.Repeat != nil {
		t.Fatalf("a call with another target got the note %+v", r.Repeat)
	}
}
