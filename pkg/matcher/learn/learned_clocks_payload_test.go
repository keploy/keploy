package learn

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The learn step's own lines on a recording (its `clocks:` block,
// MockSpec.Clocks: each place it learned as a read of the clock, and a line
// that quotes what both runs read there) are not what the call carried, and
// a word of them is no word of the recording's payload.
//
// TestAppCRUD's PAT PUT writes lastUsedAt, a read of the application's clock
// in its call's window in both runs. A start-up recording made before it
// carried a `clocks:` line, which the learn step wrote in an earlier round,
// quoting the moment record's full round read at its own place, in the same
// second as the PUT's read; and the PAT lookup the test was served before
// the PUT holds the recorded second in its Date stamp. Read as payload, the
// line made the echo pair the two: a pick from what the application was
// given, a variable of the lookup's Date header, which an HTTP replay
// cannot follow, so the test was set aside. lastUsedAt is a read of the
// clock in its call's window, learned and checked as one.
//
// The same moment in the start-up recording's payload itself is still
// found, and the echo still takes the value for a pick from it: the rule
// is unchanged, only keploy's own lines are no longer read as payload.
func TestALearnedClockLineOnARecordingIsNoWordOfItsPayload(t *testing.T) {
	const p = "body.data.lastUsedAt"
	const lease = "body.docs.msgs.0.q.lease_until.$lt.$date.$numberLong"
	ms := time.Millisecond
	r0 := time.Date(2026, 11, 9, 14, 50, 41, 124_000_000, time.UTC)
	f0 := r0.Add(63*time.Second + 567*ms)
	if r0.Unix() == f0.Unix() {
		t.Fatal("precondition: the two runs read the clock in different seconds")
	}
	const stampMs = "2006-01-02T15:04:05.000Z"
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	num := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	at := r0.Add(ms)
	w := &Window{
		Rec: Span{From: r0.Add(-2 * ms), To: at, Began: r0.Add(-time.Second)},
		F:   Span{From: f0.Add(-2 * ms), To: f0.Add(ms), Began: f0.Add(-time.Second)},
	}
	// The PAT lookup, served as recorded before the PUT: its Date stamp
	// holds the recorded second.
	lookup := []Earlier{{Mock: "mock-213", At: r0.Add(-300 * ms), Payload: payloadOf("mock-213", r0.Add(-300*ms)), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: r0.Format(http.TimeFormat)}, Live: r0.Format(http.TimeFormat), Stamp: true},
	}}}
	line := fmt.Sprintf("mock-210 %s: recorded %q, round F %q: learned a clock read, checked in its call's span: a read of the application's clock its call's window holds in both runs.", lease, r0.Format(stampMs), f0.Format(stampMs))
	// startUp is the janitor's start-up find, made before the PUT: with the
	// learn step's line on its lease_until, or (inPayload) with the moment
	// F read in what it carried, and no such line.
	startUp := func(inPayload bool) *models.Mock {
		body := `{"find":"janitor_leases","filter":{"lease_until":{"$lt":{"$date":{"$numberLong":"` + strconv.FormatInt(r0.UnixMilli(), 10) + `"}}}}}`
		m := &models.Mock{Name: "mock-210", Kind: models.GENERIC, Spec: models.MockSpec{
			ReqTimestampMock: r0.Add(-700 * ms), ResTimestampMock: r0.Add(-699 * ms),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: body}}}},
			Clocks:          map[string]models.ClockOffset{lease: {Res: ms, Layout: "typed", Why: line}},
		}}
		if inPayload {
			m.Spec.Clocks = nil
			m.Spec.GenericRequests[0].Message[0].Data = strings.Replace(body, `"find"`, `"seen":"`+f0.Format(stampMs)+`","find"`, 1)
		}
		return m
	}
	for _, follow := range []bool{false, true} {
		for _, inPayload := range []bool{false, true} {
			dc := clockedDrifts()
			m := startUp(inPayload)
			raw := []Raw{dc.RawOf(m, true)}
			if !inPayload && strings.Contains(raw[0].rText, f0.Format(stampMs)) {
				t.Errorf("client test %v: the payload text holds the learn step's line: %q", follow, raw[0].rText)
			}
			var ln Learned
			if follow {
				ln = ClassifyFollowing(dc, []string{p}, num(secs(f0)), num(secs(r0)), at, lookup, raw, nil, w, nil)[0]
			} else {
				ln = ClassifyIn(dc, []string{p}, num(secs(f0)), num(secs(r0)), at, lookup, raw, nil, w)[0]
			}
			if inPayload {
				if ln.Kind != KindVariable || ln.From.Mock != "mock-213" || ln.From.Path != "header.date" || !ln.From.Echo || !strings.Contains(ln.Why, "in mock-210 at spec.genericrequests") {
					t.Errorf("client test %v, F's moment in the start-up recording's payload: %s from %+v (%s); want the echo's variable of mock-213's header.date, naming mock-210's payload", follow, ln.Kind, ln.From, ln.Why)
				}
				continue
			}
			if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() {
				t.Errorf("client test %v, F's moment only in the learn step's line: %s Read=%+v from %+v (%s); want a plain read of the clock", follow, ln.Kind, ln.Read, ln.From, ln.Why)
			}
		}
	}
}

// An echo's reason names where the moment record's full round saw stands
// (Place.liveAt) beside where the recorded moment does, and says no more
// where no earlier place was named for it: never a dangling ", " with no
// place after it.
func TestAnEchosReasonNamesWhereTheFullRoundsMomentStandsOnlyWhereKnown(t *testing.T) {
	const head = "the moment it was recorded with stands in mock-31 at spec.resp.body, and the moment record's full round saw in it stands earlier in the test too"
	if got := echoFound("mock-31 at spec.resp.body", ""); got != head {
		t.Errorf("no place for F's moment: %q; want %q", got, head)
	}
	if got, want := echoFound("mock-31 at spec.resp.body", "in mock-11 at spec.resp.body"), head+", in mock-11 at spec.resp.body"; got != want {
		t.Errorf("F's moment in mock-11: %q; want %q", got, want)
	}
}

// The same line on another, older test's recording: a plain read of the
// clock beside an older test's answer whose Date stamp holds the moment
// only at its second, and an older test's recording the learn step already
// wrote a `clocks:` line on, quoting both runs' reads of its own nanosecond
// clock in the same seconds. The line is no word of that recording's
// payload, and the call's own window, which holds the read in both runs,
// makes it a read of the clock, in both classifiers. Read as payload, the
// line made `keploy mock`'s client tests take the value for a pick from
// the older test's Date stamp: an echo, a variable an HTTP replay cannot
// follow.
//
// api-server's TestDashboardAppOperations writes updatedAt into an audit
// insert in the same second as TestAppCRUD's PAT lookup and
// TestAppScopedReadEndpoints' audit inserts, whose nanosecond reads the
// learn step had learned.
func TestAPlainReadBesideAnotherTestsLearnedClockLineIsARead(t *testing.T) {
	const p = "body.docs.msgs.0.app.updatedAt.$numberLong"
	const nanos = "body.docs.msgs.0.auditedAtNanos.$numberLong"
	ms := time.Millisecond
	r0 := time.Date(2026, 11, 9, 7, 2, 32, 690_000_000, time.UTC)
	f0 := r0.Add(67*time.Second + 20*ms)
	if r0.Unix() == f0.Unix() {
		t.Fatal("precondition: the two runs read the clock in different seconds")
	}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	num := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	at := r0.Add(ms)
	w := &Window{
		Rec: Span{From: r0.Add(-2 * ms), To: at, Began: r0.Add(-time.Second)},
		F:   Span{From: f0.Add(-2 * ms), To: f0.Add(ms), Began: f0.Add(-time.Second)},
	}
	// lookup is the older test's PAT lookup, answered before the call with
	// a Date stamp of the run's second.
	lookup := func(date time.Time) *models.Mock {
		return &models.Mock{Name: "mock-213", Kind: models.HTTP, Spec: models.MockSpec{
			ReqTimestampMock: r0.Add(-260 * ms), ResTimestampMock: r0.Add(-259 * ms),
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://localhost:8200/v1/secret/data/keploy/personal-access-tokens/_lookup/f16e", Header: map[string]string{"Accept": "application/json"}},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": date.Format(http.TimeFormat)}, Body: `{"data":{"data":{"_id":"e2e-go-key"}}}`},
		}}
	}
	recNs, liveNs := r0.Add(-70*ms).UnixNano(), f0.Add(-90*ms).UnixNano()
	// insert is the older test's audit insert, made before the call, with
	// the learn step's line on its nanosecond read.
	insert := &models.Mock{Name: "mock-607", Kind: models.GENERIC, Spec: models.MockSpec{
		ReqTimestampMock: r0.Add(-70 * ms), ResTimestampMock: r0.Add(-69 * ms),
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: `{"insert":"app_config_audits","documents":[{"eventType":"SaveSettings"}]}`}}}},
		GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: `{"n":1,"ok":1}`}}}},
		Clocks: map[string]models.ClockOffset{nanos: {Res: time.Nanosecond, Layout: "epoch-ns", Why: fmt.Sprintf(
			`".::TestAppScopedReadEndpoints" mock-607 %s word 0: recorded "%d", round F "%d": learned a clock read, checked in its call's span`, nanos, recNs, liveNs)}},
	}}
	for _, follow := range []bool{false, true} {
		dc := clockedDrifts()
		raw := []Raw{
			dc.RawOf(lookup(r0), true).ServedLive(RawOf(lookup(f0), true)).ServedInAnOlderTest(),
			dc.RawOf(insert, true).ServedAsRecorded().ServedInAnOlderTest(),
		}
		if strings.Contains(raw[1].rText, strconv.FormatInt(liveNs, 10)) {
			t.Errorf("client test %v: the payload text holds the learn step's line: %q", follow, raw[1].rText)
		}
		var ln Learned
		if follow {
			ln = ClassifyFollowing(dc, []string{p}, num(secs(f0)), num(secs(r0)), at, nil, raw, nil, w, nil)[0]
		} else {
			ln = ClassifyIn(dc, []string{p}, num(secs(f0)), num(secs(r0)), at, nil, raw, nil, w)[0]
		}
		if !plainRead(ln) {
			t.Errorf("client test %v: %s Read=%+v from %+v (%s); want a plain read of the clock", follow, ln.Kind, ln.Read, ln.From, ln.Why)
		}
	}
}
