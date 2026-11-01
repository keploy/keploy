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
// (integrations.ExactConsumedReader). It marks every call in one staging
// that it never leaves, so it holds each call to its round as it is
// (integrations.RoundHolder); a store that marks a call's round but cannot
// hold it reads nothing for it, and names nothing made elsewhere.
type gateDb struct {
	earlierDb
	staged   []*models.Mock
	consumed []*models.Mock
}

func (d gateDb) MarkCallRound() integrations.CallRound { return integrations.NewCallRound(1, nil) }

func (d gateDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d gateDb) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	if !call.Read() {
		return "", false
	}
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			return m.Name, false
		}
	}
	return "", false
}

func (d gateDb) ConsumedTestMocks() []*models.Mock { return d.consumed }

// roundGateDb is gateDb read in one snapshot of the round
// (integrations.RoundReader), as the agent's MockManager is under exact
// serving: the store shows the pool view between tests, whose session tier
// is the recordings earlierDb holds, none of them the view's own, so a
// call's first pass is where the call was made (RoundSnapshot.CallRound).
type roundGateDb struct{ gateDb }

func (d roundGateDb) RoundSnapshot(integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s := integrations.RoundSnapshot{At: integrations.RoundAt{Epoch: 1, Gen: 1}, Phase: integrations.RoundBetween, Session: d.mocks}
	return s.WithParts(integrations.RoundParts{}), true
}

func (d roundGateDb) WithinRound(integrations.RoundAt) integrations.RoundScope {
	return roundGateScope{d}
}

func (d roundGateDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (roundGateDb) ClaimPairingIn(*models.Mock, integrations.RoundAt) (claimed, moved bool, why string) {
	return false, false, "no pairing in this store"
}

func (roundGateDb) SpendByKindIn(*models.Mock, integrations.RoundAt) (spent, moved bool, why string) {
	return false, false, "no background spend in this store"
}

func (roundGateDb) TakenLog(uint64, int, int) []integrations.Take { return nil }

// roundGateScope is roundGateDb as a RoundScope whose round never moves.
type roundGateScope struct{ roundGateDb }

func (roundGateScope) Moved() string { return "" }

var _ integrations.RoundReader = roundGateDb{}

// restagedDb is earlierDb with a call the replay staged another set under
// while the call was decided. The store itself names the answered recording
// it holds (the other set's: recordings' names restart in each set). The
// store held to the call's round names none and says the call was
// restaged, as the agent's held store does once the staging moved
// (heldScope.FindAnsweredEarlier and FindStagedNotOwned).
type restagedDb struct{ earlierDb }

func (restagedDb) MarkCallRound() integrations.CallRound { return integrations.NewCallRound(1, nil) }

func (restagedDb) FindStagedNotOwned(models.Kind, integrations.CallRound, func(*models.Mock) bool) (string, bool) {
	return "", false
}

func (d restagedDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return restagedHeld{earlierDb{mockMemDb: d.mockMemDb}}, func() bool { return true }
}

// restagedHeld is restagedDb held to the call's round once the replay
// staged another set under the call.
type restagedHeld struct{ earlierDb }

func (restagedHeld) MarkCallRound() integrations.CallRound { return integrations.NewCallRound(1, nil) }

func (restagedHeld) FindStagedNotOwned(models.Kind, integrations.CallRound, func(*models.Mock) bool) (string, bool) {
	return "", true
}

// A miss's repeat note is asked of the store held to the call's round
// (integrations.HoldToRound), as every other diagnosis of the miss is: once
// the replay staged another set under the call, and that set's own mock-7
// answered, the call is named no repeat of mock-7, and its miss says it was
// restaged.
func TestARepeatNoteIsAskedOfTheStoreHeldToTheCallsRound(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	t.Setenv(provenswitch.Env, "1")
	other := sapCount("mock-7", host, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	db := restagedDb{earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{
		Mock: other, AnsweredAt: time.Now().Add(-time.Minute),
	}}}}
	if a, ok := db.FindAnsweredEarlier(models.HTTP, nil, func(*models.Mock) bool { return true }); !ok || a.Mock.Name != "mock-7" {
		t.Fatalf("precondition: the store itself names %v (%v); want the other set's mock-7", a.Mock, ok)
	}
	r := missReport(t, db, addr, host, "/sap/A_BusinessPartner/$count?$filter=x", models.OutgoingOptions{})
	if r.Repeat != nil || r.ClosestMock == "mock-7" || strings.Contains(r.NextSteps, "identical to") {
		t.Fatalf("a call the replay restaged under is named a repeat: %+v (closest %q, next step %q); want no note", r.Repeat, r.ClosestMock, r.NextSteps)
	}
	if !strings.Contains(r.NextSteps, restagedNote) {
		t.Fatalf("the restaged call's next step is %q; want it to say the replay staged a set again", r.NextSteps)
	}
}

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
	// explains. The learn step's first pass is the call's first read of the
	// round, and only a store that reads its round in one snapshot (the
	// agent's) says from it where the call was made.
	changed := onHost("mock-c", path+"&$top=1", host)
	learning := earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{changed}}, answered: earlier.answered}
	r = missReport(t, roundGateDb{gateDb{earlierDb: learning, staged: []*models.Mock{elsewhere}}}, addr, host, path, models.OutgoingOptions{Learn: true, LearnPool: true})
	if !r.NotOwned || r.ClosestMock != "mock-w" || r.Repeat == nil || r.Repeat.Mock != "mock-7" || !strings.Contains(r.NextSteps, "This request is mock-w") {
		t.Fatalf("in the learn round a call made elsewhere is closest %q, not owned %v, note %+v, next step %q; want mock-w with the note on mock-7", r.ClosestMock, r.NotOwned, r.Repeat, r.NextSteps)
	}
	// A store that reads no round in one snapshot gives the learn step's
	// first pass no round to ask a recording made elsewhere of: the miss
	// names none, keeps the learn step's reason and closest, and gets no
	// note.
	r = missReport(t, gateDb{earlierDb: learning, staged: []*models.Mock{elsewhere}}, addr, host, path, models.OutgoingOptions{Learn: true, LearnPool: true})
	if r.NotOwned || r.ClosestMock != "mock-c" || r.Reason != models.MissRequestChanged || r.Repeat != nil || strings.Contains(r.NextSteps, "mock-w") {
		t.Fatalf("in the learn round with no snapshot of it a call is closest %q, not owned %v, reason %q, note %+v, next step %q; want mock-c's learn miss, nothing made elsewhere and no note", r.ClosestMock, r.NotOwned, r.Reason, r.Repeat, r.NextSteps)
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

// A call one more than the test recorded names the test's own recording it
// repeats (ExtraOf): of the answered recordings it matches on its own, the
// first to answer, never one of another call that answered before it: the
// copy a failure names as made more often than recorded. Any other miss
// names none.
func TestAnExtraCallNamesTheRecordingItRepeats(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/cluster?name=default"
	t.Setenv(provenswitch.Env, "1")
	other := onHost("mock-1", "/cluster?name=other", host)
	first, second := onHost("mock-2", path, host), onHost("mock-3", path, host)
	r := missReport(t, gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{other, first, second}}, addr, host, path, models.OutgoingOptions{})
	if r.MissKind != models.MissExtraCall || r.Recorded != 2 || r.ExtraOf != "mock-2" {
		t.Fatalf("a third identical read of a call recorded twice is kind %q (recorded %d) of %q; want extra_call, recorded twice, of mock-2", r.MissKind, r.Recorded, r.ExtraOf)
	}
	r = missReport(t, gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{other}}, addr, host, path, models.OutgoingOptions{})
	if r.MissKind == models.MissExtraCall || r.ExtraOf != "" {
		t.Fatalf("a call the test never recorded is kind %q of %q; want no extra call and no recording it repeats", r.MissKind, r.ExtraOf)
	}
}

// A repeat that differs from its recording only in a header keploy.yml
// declares noise is still the same call, as the matcher judges it: the miss
// names the recording it repeats. A header the user did not declare noise
// makes it another call, which names none.
func TestAnExtraCallDifferingOnlyInANoisedHeaderNamesItsCopy(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/cluster?name=local-test"
	t.Setenv(provenswitch.Env, "1")
	rec := onHost("mock-77", path, host)
	rec.Spec.HTTPReq.Header["X-Session"] = "recorded-1"
	db := gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{rec}}
	raw := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nX-Session: live-2\r\n\r\n"
	noised := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"X-Session": {}}}}
	r := missReportOf(t, db, addr, raw, noised)
	if r.MissKind != models.MissExtraCall || r.Recorded != 1 || r.ExtraOf != "mock-77" {
		t.Fatalf("a second read differing only in a noised header is kind %q (recorded %d) of %q; want extra_call, recorded once, of mock-77", r.MissKind, r.Recorded, r.ExtraOf)
	}
	r = missReportOf(t, db, addr, raw, models.OutgoingOptions{})
	if r.MissKind == models.MissExtraCall || r.ExtraOf != "" {
		t.Fatalf("a read differing in a header no noise covers is kind %q of %q; want no extra call and no recording it repeats", r.MissKind, r.ExtraOf)
	}
}

// An extra call is diagnosed against the copy it repeats, never against the
// live pool's closest other recording: the call does not differ from its
// copy, so the miss names that copy as its closest with no difference, and
// its next step names that copy and both counts (made at least twice,
// recorded once) and says to fix the application's race or re-record,
// never "Request structure changed". A call the test never recorded keeps
// the pool's closest and its difference, with no copy and no counts.
func TestAnExtraCallNamesItsCopyNotThePoolsClosest(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/cluster?name=local-test"
	t.Setenv(provenswitch.Env, "1")
	near := onHost("mock-74", "/cluster?name=default", host)
	rec := onHost("mock-77", path, host)
	pool := earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{near}}}
	r := missReport(t, gateDb{earlierDb: pool, consumed: []*models.Mock{rec}}, addr, host, path, models.OutgoingOptions{})
	if r.MissKind != models.MissExtraCall || r.Recorded != 1 || r.ExtraOf != "mock-77" {
		t.Fatalf("a second identical read is kind %q (recorded %d) of %q; want extra_call, recorded once, of mock-77", r.MissKind, r.Recorded, r.ExtraOf)
	}
	if r.ClosestMock != "mock-77" || len(r.Differs) != 0 || len(r.FieldDiffs) != 0 || len(r.FieldReasons) != 0 || r.Diff != "" {
		t.Fatalf("the extra read's miss is closest %q, differs %v, diffs %v, reasons %+v, diff %q; want mock-77 with no difference named", r.ClosestMock, r.Differs, r.FieldDiffs, r.FieldReasons, r.Diff)
	}
	if !strings.Contains(r.ClosestMockReq, "name=local-test") {
		t.Fatalf("the extra read's closest request is %q; want mock-77's", r.ClosestMockReq)
	}
	if strings.Contains(r.NextSteps, "Request structure changed") || !strings.Contains(r.NextSteps, "identical to mock-77") || !strings.Contains(r.NextSteps, "made it at least 2 times") || !strings.Contains(r.NextSteps, "recorded it 1 time") || !strings.Contains(r.NextSteps, "fix the race") || strings.Contains(r.NextSteps, "repeat:") {
		t.Fatalf("the extra read's next step is %q; want it to name mock-77, both counts (made at least 2, recorded 1) and the race to fix, and no mark", r.NextSteps)
	}
	r = missReport(t, gateDb{earlierDb: pool, consumed: []*models.Mock{onHost("mock-1", "/cluster?name=other", host)}}, addr, host, path, models.OutgoingOptions{})
	if r.MissKind == models.MissExtraCall || r.ClosestMock != "mock-74" || len(r.Differs) == 0 || strings.Contains(r.NextSteps, "identical to") || strings.Contains(r.NextSteps, "made it at least") {
		t.Fatalf("a call the test never recorded is kind %q, closest %q, differs %v, next step %q; want no extra call, mock-74 with its difference and no copy or counts", r.MissKind, r.ClosestMock, r.Differs, r.NextSteps)
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
	// Nor why a field of the other recording was refused.
	other := &models.MockMismatchReport{ClosestMock: "mock-near", Differs: []string{"body.since"},
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: "body.since was learned as a read of the application's clock"}}}
	markRepeat(other, twin.answered[0])
	if other.ClosestMock != "mock-7" || other.FieldReasons != nil || other.Differs != nil {
		t.Fatalf("the repeat's miss is closest %q, differs %v, reasons %+v; want mock-7 with no reason of another recording's", other.ClosestMock, other.Differs, other.FieldReasons)
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
