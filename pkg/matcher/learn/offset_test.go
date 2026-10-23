package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The SQS sample's delivering receive (mock-4, its message sent at sent) and
// the recording F delivered in its place (mock-11, its message sent two
// minutes later), each answer's Date header the same instant outside its
// span, so no stamp tells them apart: the consumer's insert (r1Insert) writes
// visible_after, the message's SentTimestamp and five minutes.
var (
	offSent = time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	offGot  = offSent.Add(2*time.Minute + 311*time.Millisecond)
)

// offReceives are mock-4 and mock-11 (offSent, offGot), as payloads.
func offReceives() (*models.Mock, *models.Mock) {
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	receive, delivered := r1Receive(attrs(offSent)), r1Receive(attrs(offGot))
	delivered.Name = "mock-11"
	for _, m := range []*models.Mock{receive, delivered} {
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	return receive, delivered
}

// strLeaf and typedLeaf are a drifted leaf at p, a text or an instant its
// protocol types (at res).
func strLeaf(p, v string) Shape {
	return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
}

func typedLeaf(p string, at time.Time, res time.Duration) Shape {
	return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, res)}}
}

// A value built on what the application was given by an offset is no read of
// the clock, however the lead takes its move: the consumer's visible_after,
// the delivered message's SentTimestamp and five minutes, moved by two
// minutes when F delivered another recording, which at the minute the lead
// takes for a read. It is refused, naming the receive the recording gave the
// application, the one F gave it in its place, and the offset, in every form
// it is written in: text in UTC, a DATETIME of no zone, an instant its
// protocol types at the minute and at the second; and a value five minutes
// before the message's time (which no bound of its call refuses) too. The
// receives as payloads served earlier, or the one F delivered as an exchange
// it served leaf by leaf (its payload: Earlier.Payload), which a payload of
// it among the recordings before the call does not hide.
func TestAnInstantBuiltOnAServedOneByAnOffsetIsRefused(t *testing.T) {
	receive, delivered := offReceives()
	const p = "body.visible_after"
	for _, row := range []struct {
		name      string
		rec, live Shape
		rel       string
	}{
		{"text, the minute", strLeaf(p, offSent.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, offGot.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00")), "5m0s after"},
		{"a DATETIME of no zone, the minute", strLeaf(p, offSent.Add(5*time.Minute).Format("2006-01-02 15:04")), strLeaf(p, offGot.Add(5*time.Minute).Format("2006-01-02 15:04")), "5m0s after"},
		{"typed, the minute", typedLeaf(p, offSent.Add(5*time.Minute), time.Minute), typedLeaf(p, offGot.Add(5*time.Minute), time.Minute), "5m0s after"},
		{"typed, the second", typedLeaf(p, offSent.Add(5*time.Minute), time.Second), typedLeaf(p, offGot.Add(5*time.Minute), time.Second), "5m0s after"},
		{"text, the minute, five minutes before", strLeaf(p, offSent.Add(-5*time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, offGot.Add(-5*time.Minute).Format("2006-01-02T15:04Z07:00")), "5m0s before"},
		{"typed, the minute, five minutes before", typedLeaf(p, offSent.Add(-5*time.Minute), time.Minute), typedLeaf(p, offGot.Add(-5*time.Minute), time.Minute), "5m0s before"},
		{"a time of day, the minute", strLeaf(p, offSent.Add(5*time.Minute).Format("15:04")), strLeaf(p, offGot.Add(5*time.Minute).Format("15:04")), "5m0s after"},
	} {
		for _, served := range []string{"payloads", "delivered leaf by leaf", "delivered leaf by leaf, recorded before the call too"} {
			dc := NewDriftCache()
			raw := []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, true)}
			var earlier []Earlier
			switch served {
			case "delivered leaf by leaf":
				raw = raw[:1]
				earlier = []Earlier{{Mock: delivered.Name, At: delivered.Spec.ReqTimestampMock, Payload: RawOf(delivered, true)}}
			case "delivered leaf by leaf, recorded before the call too":
				// One recording, read twice: F gave it, as its exchange
				// says, whatever its payload among raw says.
				raw = []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, false)}
				earlier = []Earlier{{Mock: delivered.Name, At: delivered.Spec.ReqTimestampMock, Payload: RawOf(delivered, true)}}
			}
			for i := 0; i < 3; i++ {
				ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, earlier, raw, nil, nil)[0]
				if ln.Kind != KindUnaccounted || ln.From.Mock != "" ||
					!strings.Contains(ln.Why, "stands "+row.rel+" the moment mock-4 at spec.resp.body holds as the recording gave it") ||
					!strings.Contains(ln.Why, row.rel+" the moment mock-11 at spec.resp.body holds, which record's full round gave the application in its place") {
					t.Fatalf("%s, %s: %s from %+v (%s); want it refused, built on the receives %s", row.name, served, ln.Kind, ln.From, ln.Why, row.rel)
				}
			}
		}
	}
}

// At the day too: a due date two days before the delivered message's sent
// day, F delivering a message sent across midnight, moves a day as a read of
// the clock across midnight does, and no earlier place holds either day. It
// is refused, naming the receive the recording gave the application and the
// offset, as text and as an instant its protocol types.
func TestADateBuiltOnAServedOneByAnOffsetIsRefused(t *testing.T) {
	sent := time.Date(2026, 10, 21, 23, 58, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	receive, delivered := r1Receive(attrs(sent)), r1Receive(attrs(got))
	delivered.Name = "mock-11"
	for _, m := range []*models.Mock{receive, delivered} {
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	const p = "body.due"
	day := 24 * time.Hour
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"text, the day", strLeaf(p, sent.Add(-2*day).Format("2006-01-02")), strLeaf(p, got.Add(-2*day).Format("2006-01-02"))},
		{"typed, the day", typedLeaf(p, sent.Add(-2*day), day), typedLeaf(p, got.Add(-2*day), day)},
	} {
		dc := NewDriftCache()
		ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, true)}, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands 48h0m0s before the moment mock-4 at spec.resp.body holds as the recording gave it") {
			t.Errorf("%s: %s from %+v (%s); want it refused, naming the receive and the offset", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// A read of the clock beside moved places that its move does not match is
// noise: the insert's processed_at at the minute of its call, moved by the
// lead, beside the receives F moved by two minutes; at the millisecond,
// beside them too.
func TestAReadOfTheClockBesideAMovedPlaceItDoesNotMatchIsNoise(t *testing.T) {
	receive, delivered := offReceives()
	const p = "body.processed_at"
	now := r1Insert.Add(-40 * time.Millisecond)
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"text, the minute", strLeaf(p, now.Format("2006-01-02T15:04Z07:00")), strLeaf(p, now.Add(FullRoundLead).Format("2006-01-02T15:04Z07:00"))},
		{"typed, the minute", typedLeaf(p, now, time.Minute), typedLeaf(p, now.Add(FullRoundLead), time.Minute)},
		{"typed, the millisecond", typedLeaf(p, now, time.Millisecond), typedLeaf(p, now.Add(FullRoundLead+37*time.Millisecond), time.Millisecond)},
	} {
		dc := NewDriftCache()
		if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, true)}, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%s: %s from %+v (%s); want noise", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// A move is a place the recording gave the application against another
// recording F gave it there: two receives the recording gave the
// application, a minute apart, of which F served neither, are no move, and a
// read of the clock at its call's minute that the lead moved by that minute
// is noise beside them; so are two F served that were both recorded after
// the call, which the recording gave the application neither of. Where F
// served the later one in the test, its move matches the read's: refused, a
// false red that fails closed, naming both.
func TestAMoveIsWhatTheFullRoundGaveInPlaceOfTheRecordings(t *testing.T) {
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	first, second := r1Receive(attrs(r1Insert.Add(-97*time.Second))), r1Receive(attrs(r1Insert.Add(-37*time.Second)))
	second.Name = "mock-5"
	for i, m := range []*models.Mock{first, second} {
		m.Spec.ReqTimestampMock = r1Insert.Add(-time.Duration(100-60*i) * time.Second)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Second)
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	const p = "body.processed_at"
	now := r1Insert.Add(-40 * time.Millisecond)
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"text, the minute", strLeaf(p, now.Format("2006-01-02T15:04Z07:00")), strLeaf(p, now.Add(FullRoundLead).Format("2006-01-02T15:04Z07:00"))},
		{"typed, the minute", typedLeaf(p, now, time.Minute), typedLeaf(p, now.Add(FullRoundLead), time.Minute)},
	} {
		dc := NewDriftCache()
		if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, []Raw{dc.RawOf(first, false), dc.RawOf(second, false)}, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%s, F serving neither: %s from %+v (%s); want noise", row.name, ln.Kind, ln.From, ln.Why)
		}
		later1, later2 := *first, *second
		later1.Spec.ReqTimestampMock, later2.Spec.ReqTimestampMock = r1Insert.Add(time.Second), r1Insert.Add(61*time.Second)
		dc = NewDriftCache()
		if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, []Raw{dc.RawOf(&later1, true), dc.RawOf(&later2, true)}, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%s, F serving two recorded after the call: %s from %+v (%s); want noise", row.name, ln.Kind, ln.From, ln.Why)
		}
		dc = NewDriftCache()
		if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, []Raw{dc.RawOf(first, false), dc.RawOf(second, true)}, nil, nil)[0]; ln.Kind != KindUnaccounted ||
			!strings.Contains(ln.Why, "the moment mock-4 at spec.resp.body holds as the recording gave it") || !strings.Contains(ln.Why, "the moment mock-5 at spec.resp.body holds, which record's full round gave the application in its place") {
			t.Errorf("%s, F serving the later: %s from %+v (%s); want it refused, naming both receives", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// Only a place of what the application was given moves: the instants of the
// recordings' requests (the application's own: an AWS signature's
// X-Amz-Date) and an answer's own clock stamp (rule 3: no value came from
// one) are none, however much two recordings of a call apart they stand. A
// read of the clock moved by the lead stays noise beside two receives, the
// first the recording gave the application and the next F gave it in the
// test, whose request dates and Date headers stand the lead apart: read as
// places, either would make the read a value built on them by an offset.
func TestOnlyAnAnswersPlaceMovesForTheOffsetRule(t *testing.T) {
	first := r1Receive("")
	next := r1Receive("")
	next.Name = "mock-5"
	shift := 61 * time.Second
	next.Spec.ReqTimestampMock = first.Spec.ReqTimestampMock.Add(shift)
	next.Spec.ResTimestampMock = first.Spec.ResTimestampMock.Add(shift)
	next.Spec.HTTPReq.Header["X-Amz-Date"] = "20261022T111514Z"
	next.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:15:17 GMT"
	const p = "body.processed_at"
	call := next.Spec.ResTimestampMock.Add(time.Second)
	now := call.Add(-40 * time.Millisecond)
	dc := NewDriftCache()
	raw := []Raw{dc.RawOf(first, false), dc.RawOf(next, true)}
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"text, the second", strLeaf(p, now.Format(time.RFC3339)), strLeaf(p, now.Add(FullRoundLead).Format(time.RFC3339))},
		{"typed, the second", typedLeaf(p, now, time.Second), typedLeaf(p, now.Add(FullRoundLead), time.Second)},
	} {
		if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, call, nil, raw, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%s: %s from %+v (%s); want noise", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// A part of the test case's request keploy test sent with another value
// than recorded (a template value) is a moved place too: a value built on it
// by an offset is refused, naming the part.
func TestAValueBuiltOnAMovedPartOfTheRequestIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.until"
	req := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		jsonType,
		{Name: "body", Recorded: `{"since":"2026-10-22T11:09:00Z"}`, Sent: `{"since":"2026-10-22T11:10:00Z"}`},
	}})}
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"text, the minute", strLeaf(p, "2026-10-22T11:14Z"), strLeaf(p, "2026-10-22T11:15Z")},
		{"typed, the minute", typedLeaf(p, at.Truncate(time.Minute), time.Minute), typedLeaf(p, at.Truncate(time.Minute).Add(time.Minute), time.Minute)},
	} {
		ln := ClassifyIn(NewDriftCache(), []string{p}, row.live, row.rec, at, req, nil, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands 5m0s after the moment test case test-1 at body.since holds as the recording gave it") {
			t.Errorf("%s: %s from %+v (%s); want it refused, built on the request's since", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// A read of the clock is no later than the call that carries it: the lead is
// no read's provenance for an instant recorded after its call, nor for one
// F saw later than its call's read moved by the lead, whatever the lead takes
// at its resolution. An instant its protocol types is refused so, at the day,
// the hour, the minute and the second; one at its call's minute, or that a
// read just before midnight moved into the next day, is still noise. A text
// whose words an earlier place holds is searched there where it is no read:
// one after its call is refused, one at its call's minute is noise.
func TestAReadOfTheClockIsNoLaterThanItsCall(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.due"
	day := 24 * time.Hour
	one := func(r, l Leaf, at time.Time, earlier []Earlier) Learned {
		return ClassifyIn(NewDriftCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: l}}, Shape{Leaves: map[string]Leaf{p: r}}, at, earlier, nil, nil, nil)[0]
	}
	typed := func(at time.Time, res time.Duration) Leaf { return InstantLeaf(p, p, at, res) }
	for _, row := range []struct {
		name  string
		r, l  Leaf
		at    time.Time
		noise bool
	}{
		{"a day a month on", typed(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), day), typed(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC), day), at, false},
		{"the call's day, F's the next", typed(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), day), typed(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), day), at, false},
		{"the call's day before midnight, F's the next", typed(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), day), typed(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), day), time.Date(2026, 10, 22, 23, 59, 31, 0, time.UTC), true},
		{"an hour after the call", typed(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), time.Minute), typed(time.Date(2026, 10, 22, 12, 1, 0, 0, time.UTC), time.Minute), at, false},
		{"a minute after the call's", typed(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC), time.Minute), typed(time.Date(2026, 10, 22, 11, 16, 0, 0, time.UTC), time.Minute), at, false},
		{"the call's minute, F's two on", typed(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC), time.Minute), typed(time.Date(2026, 10, 22, 11, 16, 0, 0, time.UTC), time.Minute), at, false},
		{"the call's minute, F's the next", typed(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC), time.Minute), typed(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC), time.Minute), at, true},
		{"a second a month on", typed(time.Date(2026, 11, 21, 17, 30, 0, 0, time.UTC), time.Second), typed(time.Date(2026, 11, 21, 17, 31, 1, 0, time.UTC), time.Second), at, false},
		{"the second before the call", typed(at.Add(-time.Second), time.Second), typed(at.Add(-time.Second+FullRoundLead), time.Second), at, true},
		{"a millisecond read half a second after the call", typed(at.Add(500*time.Millisecond), time.Millisecond), typed(at.Add(500*time.Millisecond+FullRoundLead), time.Millisecond), at, false},
		{"a millisecond read just before the call", typed(at.Add(-time.Millisecond), time.Millisecond), typed(at.Add(-time.Millisecond+FullRoundLead), time.Millisecond), at, true},
	} {
		ln := one(row.r, row.l, row.at, nil)
		// Each refused one moved by the lead: its reason says it is past its
		// call, never that it did not move by the lead.
		if row.noise && ln.Kind != KindNoise || !row.noise && (ln.Kind != KindUnaccounted || ln.Why != instantAfterCall && ln.Why != instantPastCall) {
			t.Errorf("%s: %s (%s); want noise %v, or refused as past its call", row.name, ln.Kind, ln.Why, row.noise)
		}
	}
	// Text: the slot an earlier call sent, which drifted with it.
	call := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Noise: "body.slot", Type: String, Value: "2026-10-22T11:20Z"}, Live: "2026-10-22T11:21Z", Place: "body.slot"}}}}
	if ln := one(Leaf{Path: p, Noise: p, Type: String, Value: "book 2026-10-22T11:20Z"}, Leaf{Path: p, Noise: p, Type: String, Value: "book 2026-10-22T11:21Z"}, at, call); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in mock-1 at body.slot") {
		t.Errorf("text six minutes after its call: %s (%s); want it refused, its word at mock-1's body.slot", ln.Kind, ln.Why)
	}
	if ln := one(Leaf{Path: p, Noise: p, Type: String, Value: "book 2026-10-22T11:20Z"}, Leaf{Path: p, Noise: p, Type: String, Value: "book 2026-10-22T11:21Z"}, time.Date(2026, 10, 22, 11, 20, 17, 0, time.UTC), call); ln.Kind != KindNoise {
		t.Errorf("text at its call's minute: %s (%s); want noise, two reads of the clock", ln.Kind, ln.Why)
	}
	// A wall clock of no zone is a read in the application's zone where it
	// is one there: an application in a zone ahead of UTC writes its read
	// later than its call in UTC. In UTC alone, or in a zone the application
	// is not in, it is no read before its call.
	ist := time.FixedZone("IST", 5*3600+1800)
	for _, z := range []struct {
		name  string
		zone  *time.Location
		noise bool
	}{{"the application's zone IST", ist, true}, {"UTC alone", nil, false}, {"the application's zone UTC-5", time.FixedZone("EST", -5*3600), false}} {
		dc := NewDriftCache()
		dc.zone = z.zone
		ln := ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "book " + at.Add(-time.Second+FullRoundLead).In(ist).Format("2006-01-02 15:04:05")}}}, Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "book " + at.Add(-time.Second).In(ist).Format("2006-01-02 15:04:05")}}}, at,
			[]Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: at.Add(-time.Second).In(ist).Format("15:04:05")}, Live: "x", Place: "body.at"}}}}, nil, nil, nil)[0]
		if (ln.Kind == KindNoise) != z.noise {
			t.Errorf("an IST wall clock of no zone read just before its call, %s: %s (%s); want noise %v", z.name, ln.Kind, ln.Why, z.noise)
		}
	}
}

// readNoLaterThan bounds the lead by the call: at the minute, the hour and
// the day it leaves the moves a read before its call makes; a zero call
// bounds nothing; a wall clock of no zone is read in UTC and in the
// application's zone, no other.
func TestReadNoLaterThan(t *testing.T) {
	call := time.Date(2026, 10, 22, 11, 14, 17, 155e6, time.UTC)
	for _, row := range []struct {
		name   string
		rt, lt time.Time
		res    time.Duration
		call   time.Time
		zoned  bool
		zone   *time.Location
		want   bool
	}{
		{"the call's minute, F's the next", time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC), time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC), time.Minute, call, true, nil, true},
		{"the call's minute, F's two on", time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC), time.Date(2026, 10, 22, 11, 16, 0, 0, time.UTC), time.Minute, call, true, nil, false},
		{"the minute after the call's", time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC), time.Date(2026, 10, 22, 11, 16, 0, 0, time.UTC), time.Minute, call, true, nil, false},
		{"no call", time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC), time.Date(2026, 10, 22, 11, 16, 0, 0, time.UTC), time.Minute, time.Time{}, true, nil, true},
		{"the second before the call", time.Date(2026, 10, 22, 11, 14, 16, 0, time.UTC), time.Date(2026, 10, 22, 11, 15, 18, 0, time.UTC), time.Second, call, true, nil, true},
		{"the second before the call, F's past the lead", time.Date(2026, 10, 22, 11, 14, 16, 0, time.UTC), time.Date(2026, 10, 22, 11, 15, 19, 0, time.UTC), time.Second, call, true, nil, false},
		{"a wall clock of no zone, IST", time.Date(2026, 10, 22, 16, 44, 16, 0, time.UTC), time.Date(2026, 10, 22, 16, 45, 17, 0, time.UTC), time.Second, call, false, time.FixedZone("IST", 5*3600+1800), true},
		{"a wall clock of no zone, IST, read in UTC alone", time.Date(2026, 10, 22, 16, 44, 16, 0, time.UTC), time.Date(2026, 10, 22, 16, 45, 17, 0, time.UTC), time.Second, call, false, nil, false},
		{"a wall clock of no zone, IST, read in a zone behind it", time.Date(2026, 10, 22, 16, 44, 16, 0, time.UTC), time.Date(2026, 10, 22, 16, 45, 17, 0, time.UTC), time.Second, call, false, time.FixedZone("EET", 2*3600), false},
		{"a recorded read in the second after its call", time.Date(2026, 10, 22, 11, 14, 17, 600e6, time.UTC), time.Date(2026, 10, 22, 11, 15, 18, 0, time.UTC), time.Millisecond, call, true, nil, false},
		{"a recorded read in its call's second, before it", time.Date(2026, 10, 22, 11, 14, 17, 100e6, time.UTC), time.Date(2026, 10, 22, 11, 15, 18, 0, time.UTC), time.Millisecond, call, true, nil, true},
		{"the same, read as zoned", time.Date(2026, 10, 22, 16, 44, 16, 0, time.UTC), time.Date(2026, 10, 22, 16, 45, 17, 0, time.UTC), time.Second, call, true, time.FixedZone("IST", 5*3600+1800), false},
		{"a wall clock of no zone past its zone's", time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC), time.Date(2026, 10, 23, 2, 1, 1, 0, time.UTC), time.Second, call, false, time.FixedZone("IST", 5*3600+1800), false},
	} {
		if got := readNoLaterThan(row.rt, row.lt, row.res, row.call, row.zoned, row.zone); got != row.want {
			t.Errorf("%s: %v; want %v", row.name, got, row.want)
		}
	}
}

// offsetOf finds an offset that puts each place's instant in the value's
// unit, and names the whole one of the value's resolution where one does;
// a time of day is read around the day.
func TestOffsetOf(t *testing.T) {
	const (
		s   = int64(time.Second)
		min = int64(time.Minute)
		day = int64(24 * time.Hour)
	)
	for _, row := range []struct {
		name              string
		rec, live, hr, hf int64
		rres, lres        time.Duration
		mod               int64
		want              time.Duration
		ok                bool
	}{
		{"five minutes on, cut to the minute", 19 * min, 21 * min, 14*min + 16473*s/1000, 16*min + 16784*s/1000, time.Minute, time.Minute, 0, 5 * time.Minute, true},
		{"five minutes on a whole minute", 14 * min, 15 * min, 9 * min, 10 * min, time.Minute, time.Minute, 0, 5 * time.Minute, true},
		{"five minutes before", 9 * min, 11 * min, 14*min + 16473*s/1000, 16*min + 16784*s/1000, time.Minute, time.Minute, 0, -5 * time.Minute, true},
		{"moved by the lead, the place by two minutes", 14 * min, 15 * min, 14*min + 16473*s/1000, 16*min + 16784*s/1000, time.Minute, time.Minute, 0, 0, false},
		{"seconds that no offset fits", 78 * s, 139 * s, 16 * s, 76 * s, time.Second, time.Second, 0, 0, false},
		{"around midnight", 2 * min, 4 * min, day - 3*min, day - min, time.Minute, time.Minute, day, 5 * time.Minute, true},
	} {
		got, ok := offsetOf(row.rec, row.live, row.hr, row.hf, row.rres, row.lres, row.mod)
		if ok != row.ok || ok && got != row.want {
			t.Errorf("%s: %v %v; want %v %v", row.name, got, ok, row.want, row.ok)
		}
	}
}

// getAnswer is a recorded GET of url at at, answered with the JSON body.
func getAnswer(name, url, body string, at time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:          &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1, URL: url, Header: map[string]string{"Accept": "application/json"}},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: body},
		ReqTimestampMock: at, ResTimestampMock: at.Add(3 * time.Millisecond),
	}}
}

// A move is what F gave the application in place of what the recording gave
// it, as an answer to the same call: answers both runs gave it (two users
// read in each, two health checks a minute apart) are no move, nor is an
// answer to another call F alone gave it beside one the recording alone did;
// a read of the clock the lead moved stays noise beside them, at the minute
// and at the second, as text and typed.
func TestAnswersBothRunsGaveOrOfTwoCallsAreNoMove(t *testing.T) {
	const p = "body.seen_at"
	before := r1Insert.Add(-90 * time.Second)
	users1 := getAnswer("mock-1", "http://users:8080/users/1", `{"id":1,"created_at":"2026-10-22T09:00:00Z"}`, before)
	users2 := getAnswer("mock-2", "http://users:8080/users/2", `{"id":2,"created_at":"2026-10-22T09:00:01Z"}`, before.Add(time.Second))
	health1 := getAnswer("mock-3", "http://clock:8080/health", `{"server_time":"`+r1Insert.Add(-70*time.Second).Format(time.RFC3339Nano)+`"}`, r1Insert.Add(-70*time.Second))
	health2 := getAnswer("mock-4", "http://clock:8080/health", `{"server_time":"`+r1Insert.Add(-8800*time.Millisecond).Format(time.RFC3339Nano)+`"}`, r1Insert.Add(-8800*time.Millisecond))
	order := getAnswer("mock-5", "http://orders:8080/orders", `{"placed_at":"2026-10-22T09:00:01Z"}`, before.Add(2*time.Second))
	order.Spec.HTTPReq.Method = "POST"
	order.Spec.HTTPResp.Body = `{"created_at":"2026-10-22T09:00:01Z"}`
	now := r1Insert.Add(-40 * time.Millisecond)
	for _, set := range []struct {
		name string
		raw  func(dc *DriftCache) []Raw
	}{
		{"two users both runs read", func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(users1, true), dc.RawOf(users2, true)} }},
		{"two health checks both runs read", func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(health1, true), dc.RawOf(health2, true)} }},
		{"a user the recording alone read, an order F alone placed", func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(users1, false), dc.RawOf(order, true)} }},
	} {
		for _, row := range []struct {
			name      string
			rec, live Shape
		}{
			{"text, the minute", strLeaf(p, now.Format("2006-01-02T15:04Z07:00")), strLeaf(p, now.Add(FullRoundLead).Format("2006-01-02T15:04Z07:00"))},
			{"typed, the minute", typedLeaf(p, now, time.Minute), typedLeaf(p, now.Add(FullRoundLead), time.Minute)},
			{"text, the second", strLeaf(p, now.Format(time.RFC3339)), strLeaf(p, now.Add(FullRoundLead).Format(time.RFC3339))},
			{"typed, the second", typedLeaf(p, now, time.Second), typedLeaf(p, now.Add(FullRoundLead), time.Second)},
		} {
			dc := NewDriftCache()
			if ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, nil, set.raw(dc), nil, nil)[0]; ln.Kind != KindNoise {
				t.Errorf("%s, %s: %s from %+v (%s); want noise", set.name, row.name, ln.Kind, ln.From, ln.Why)
			}
		}
	}
}

// A list F gave the application with another number of elements than the
// recording did puts the instant a value is built on at another index: the
// recording's pending jobs held one, created 11:03:10, and F's another
// recording of the call with two, the last created 11:05:12. A retry five
// or fifteen minutes after the last job's creation, to the minute, moved by
// two minutes, which the lead takes for a read: it is refused, naming both
// answers and the offset, as text in UTC, as a DATETIME of no zone, as a
// time of day and as an instant its protocol types. A wall clock of no zone
// fifteen minutes on is the served instant in a zone a quarter of an hour
// ahead of UTC too: the echo follows it there, as a variable of the
// recording's answer, compared at replay, never noise.
func TestAnInstantBuiltOnAnotherIndexOfAServedListIsRefused(t *testing.T) {
	const p = "body.retry_at"
	call := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	recJob, fJob := time.Date(2026, 10, 22, 11, 3, 10, 0, time.UTC), time.Date(2026, 10, 22, 11, 5, 12, 0, time.UTC)
	rec := getAnswer("mock-1", "http://jobs:8080/jobs?status=pending", `{"jobs":[{"id":"j-1","created_at":"2026-10-22T11:03:10Z"}]}`, call.Add(-3*time.Minute))
	f := getAnswer("mock-2", "http://jobs:8080/jobs?status=pending", `{"jobs":[{"id":"j-0","created_at":"2026-10-22T10:50:00Z"},{"id":"j-2","created_at":"2026-10-22T11:05:12Z"}]}`, call.Add(-time.Minute))
	for _, off := range []time.Duration{5 * time.Minute, 15 * time.Minute} {
		for _, row := range []struct {
			name      string
			rec, live Shape
			noZone    bool
		}{
			{"text", strLeaf(p, recJob.Add(off).Format("2006-01-02T15:04Z07:00")), strLeaf(p, fJob.Add(off).Format("2006-01-02T15:04Z07:00")), false},
			{"a DATETIME of no zone", strLeaf(p, recJob.Add(off).Format("2006-01-02 15:04")), strLeaf(p, fJob.Add(off).Format("2006-01-02 15:04")), true},
			{"a time of day", strLeaf(p, "retry at "+recJob.Add(off).Format("15:04")), strLeaf(p, "retry at "+fJob.Add(off).Format("15:04")), true},
			{"typed", typedLeaf(p, recJob.Add(off).Truncate(time.Minute), time.Minute), typedLeaf(p, fJob.Add(off).Truncate(time.Minute), time.Minute), false},
		} {
			dc := NewDriftCache()
			ln := ClassifyIn(dc, []string{p}, row.live, row.rec, call, nil, []Raw{dc.RawOf(rec, false), dc.RawOf(f, true)}, nil, nil)[0]
			if row.noZone && off == 15*time.Minute {
				if ln.Kind != KindVariable || !ln.From.Echo || ln.From.Mock != "mock-1" {
					t.Errorf("%s, %s on: %s from %+v (%s); want an echo's variable of mock-1", row.name, off, ln.Kind, ln.From, ln.Why)
				}
				continue
			}
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, off.String()+" after the moment mock-1 at spec.resp.body holds as the recording gave it") || !strings.Contains(ln.Why, "the moment mock-2 at spec.resp.body holds, which record's full round gave the application in its place") {
				t.Errorf("%s, %s on: %s from %+v (%s); want it refused, built on both answers by %s", row.name, off, ln.Kind, ln.From, ln.Why, off)
			}
		}
	}
}
