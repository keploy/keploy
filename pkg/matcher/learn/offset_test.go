package learn

import (
	"encoding/base64"
	"errors"
	"net/http"
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

// unreadPayload is the payload RawOf gives of m where it cannot read m's
// text: its name, recorded instant, serve and call, nothing read.
func unreadPayload(m *models.Mock) Raw {
	r := RawOf(m, true)
	return Raw{mock: r.mock, at: r.at, served: r.served, call: r.call}
}

// An exchange F served leaf by leaf whose payload RawOf could not read, or
// whose adapter gave no payload at all (the zero Raw), is a place the offset
// rule and the word rule cannot search, never one they drop: an instant
// built on it by an offset and a word copied from its answer are each
// refused, naming it and why, never learned as noise. Its payload read from
// its mock is searched, and refuses nothing for want of a reading.
func TestAnUnreadLeafByLeafPayloadIsRefusedNamingIt(t *testing.T) {
	receive, delivered := offReceives()
	const p = "body.visible_after"
	why := map[string]string{
		"unread":     "keploy could not read mock-11, an exchange record's full round served the application leaf by leaf before the call, from its mock (learn.RawOf)",
		"none given": "keploy was given no payload of mock-11, an exchange record's full round served the application leaf by leaf before the call (learn.Earlier.Payload)",
	}
	for _, row := range []struct {
		name      string
		rec, live Shape
	}{
		{"an instant by an offset", strLeaf(p, offSent.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, offGot.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00"))},
		{"a word", strLeaf(p, "slot-7"), strLeaf(p, "slot-9")},
	} {
		for _, payload := range []string{"unread", "none given", "read"} {
			dc := NewDriftCache()
			e := Earlier{Mock: delivered.Name, At: delivered.Spec.ReqTimestampMock}
			switch payload {
			case "unread":
				e.Payload = unreadPayload(delivered)
			case "read":
				e.Payload = dc.RawOf(delivered, true)
			}
			ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, []Earlier{e}, []Raw{dc.RawOf(receive, false)}, nil, nil)[0]
			switch {
			case payload != "read" && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, why[payload])):
				t.Fatalf("%s, %s payload: %s (%s); want it refused, naming mock-11's payload as %s", row.name, payload, ln.Kind, ln.Why, payload)
			case payload == "read" && strings.Contains(ln.Why, "mock-11, an exchange record's full round served the application leaf by leaf"):
				t.Fatalf("%s, %s payload: %s (%s); want no unread place where the adapter gave a payload read from its mock", row.name, payload, ln.Kind, ln.Why)
			}
		}
	}
}

// payloadOf is the payload a producer gives of mock, an exchange it served
// leaf by leaf, recorded at at, in a row whose subject is the exchange's
// leaves: read from the mock (RawOf), holding nothing beyond them. A leafed
// exchange with no payload at all is refused, naming it
// (TestAnUnreadLeafByLeafPayloadIsRefusedNamingIt).
func payloadOf(mock string, at time.Time) Raw {
	return RawOf(&models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: at}}, true)
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
	call := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Noise: "body.slot", Type: String, Value: "2026-10-22T11:20Z"}, Live: "2026-10-22T11:21Z", Place: "body.slot"}}}}
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
			[]Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: at.Add(-time.Second).In(ist).Format("15:04:05")}, Live: "x", Place: "body.at"}}}}, nil, nil, nil)[0]
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
// it, as an answer to the same call: answers both runs gave it in the order
// the recording did (two users read in each, two health checks a minute
// apart, F serving each first as recorded) are no move, nor is an answer to
// another call F alone gave it beside one the recording alone did; a read of
// the clock the lead moved stays noise beside them, at the minute and at the
// second, as text and typed.
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
		{"two users both runs read", func(dc *DriftCache) []Raw {
			return []Raw{dc.RawOf(users1, true).ServedAs(1), dc.RawOf(users2, true).ServedAs(2)}
		}},
		{"two health checks both runs read", func(dc *DriftCache) []Raw {
			return []Raw{dc.RawOf(health2, true).ServedAs(4), dc.RawOf(health1, true).ServedAs(2)}
		}},
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

// Two recordings both runs gave the application, which F gave in another
// order than the recording did, are a move: a consumer that prefetches takes
// two messages before it processes the first, and builds visible_after on
// the first it got, its SentTimestamp and a minute, to the minute. Both
// messages were sent before either receive, and both receives came before
// the insert in both runs: the recording's insert is built on A, F's on B.
// It is refused, naming both receives and the offset, as text, as a
// DATETIME of no zone and typed at the minute, as text at the hour, through the
// payloads F served and leaf by leaf, whatever the gap between the two
// messages; so is one whose order F served them in keploy cannot tell. F
// serving them in the recorded order leaves nothing in the other's place,
// and the move is the lead's, as of the health checks above.
func TestRecordingsBothRunsGaveInAnotherOrderAreAMove(t *testing.T) {
	receive := func(name string, sent, at time.Time) *models.Mock {
		m := r1Receive(`{"SentTimestamp": "` + strconv.FormatInt(sent.UnixMilli(), 10) + `"}`)
		m.Name = name
		// Answered on a clock no value here stands near.
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 03:02:51 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(200*time.Millisecond)
		return m
	}
	const p = "body.visible_after"
	type row struct {
		name      string
		rec, live Shape
	}
	minuteRows := func(sentA, sentB time.Time) []row {
		at := func(t time.Time) time.Time { return t.Add(time.Minute) }
		return []row{
			{"text, the minute", strLeaf(p, at(sentA).Format("2006-01-02T15:04Z07:00")), strLeaf(p, at(sentB).Format("2006-01-02T15:04Z07:00"))},
			{"a DATETIME of no zone, the minute", strLeaf(p, at(sentA).Format("2006-01-02 15:04")), strLeaf(p, at(sentB).Format("2006-01-02 15:04"))},
			{"typed, the minute", typedLeaf(p, at(sentA), time.Minute), typedLeaf(p, at(sentB), time.Minute)},
		}
	}
	// At the hour (ISO 8601's reduced precision), the value two hours after
	// a message sent an hour after the other.
	hourRows := func(sentA, sentB time.Time) []row {
		at := func(t time.Time) string {
			return t.Add(2 * time.Hour).Truncate(time.Hour).Format("2006-01-02T15Z07:00")
		}
		return []row{{"text, the hour", strLeaf(p, at(sentA)), strLeaf(p, at(sentB))}}
	}
	for _, sent := range []struct {
		a    time.Time
		gap  time.Duration
		rows func(sentA, sentB time.Time) []row
	}{
		{time.Date(2026, 10, 22, 11, 10, 16, 473e6, time.UTC), 2*time.Minute + 311*time.Millisecond, minuteRows},
		{time.Date(2026, 10, 22, 11, 10, 16, 473e6, time.UTC), time.Minute + 5*time.Second, minuteRows},
		{time.Date(2026, 10, 22, 8, 40, 16, 473e6, time.UTC), time.Hour + 3*time.Second, hourRows},
	} {
		sentA, gap := sent.a, sent.gap
		sentB := sentA.Add(gap)
		a := receive("mock-4", sentA, time.Date(2026, 10, 22, 11, 14, 13, 778e6, time.UTC))
		b := receive("mock-11", sentB, time.Date(2026, 10, 22, 11, 14, 15, 101e6, time.UTC))
		for _, row := range sent.rows(sentA, sentB) {
			for _, served := range []struct {
				name    string
				refused bool
				of      func(dc *DriftCache) ([]Raw, []Earlier)
			}{
				{"payloads F served in the other order", true, func(dc *DriftCache) ([]Raw, []Earlier) {
					return []Raw{dc.RawOf(a, true).ServedAs(2), dc.RawOf(b, true).ServedAs(1)}, nil
				}},
				{"payloads F served in an order keploy cannot tell", true, func(dc *DriftCache) ([]Raw, []Earlier) {
					return []Raw{dc.RawOf(a, true), dc.RawOf(b, true)}, nil
				}},
				{"leaf by leaf in the other order", true, func(*DriftCache) ([]Raw, []Earlier) {
					return nil, []Earlier{
						{Mock: b.Name, At: b.Spec.ReqTimestampMock, Own: true, Payload: RawOf(b, true).ServedAs(1)},
						{Mock: a.Name, At: a.Spec.ReqTimestampMock, Own: true, Payload: RawOf(a, true).ServedAs(2)},
					}
				}},
				{"one leaf by leaf, one payload, in the other order", true, func(dc *DriftCache) ([]Raw, []Earlier) {
					return []Raw{dc.RawOf(a, true).ServedAs(3)}, []Earlier{{Mock: b.Name, At: b.Spec.ReqTimestampMock, Own: true, Payload: RawOf(b, true).ServedAs(1)}}
				}},
				{"payloads F served in the recorded order", false, func(dc *DriftCache) ([]Raw, []Earlier) {
					return []Raw{dc.RawOf(a, true).ServedAs(1), dc.RawOf(b, true).ServedAs(2)}, nil
				}},
				{"in the recorded order, A read twice, F's order on its exchange", false, func(dc *DriftCache) ([]Raw, []Earlier) {
					return []Raw{dc.RawOf(a, false), dc.RawOf(b, true).ServedAs(2)}, []Earlier{{Mock: a.Name, At: a.Spec.ReqTimestampMock, Own: true, Payload: RawOf(a, true).ServedAs(1)}}
				}},
			} {
				dc := NewDriftCache()
				raw, earlier := served.of(dc)
				ln := ClassifyIn(dc, []string{p}, row.live, row.rec, r1Insert, earlier, raw, nil, nil)[0]
				if row.name == "text, the hour" {
					// Read to the hour, the instant is a pick from what
					// the application was given (the echo, before any
					// offset), compared at replay in any order, or built on
					// it by an offset: never noise.
					// Leaf by leaf, the echo reads the exchanges' leaves,
					// and the offset refuses it.
					bound := ln.Kind == KindVariable && ln.From.Echo && (ln.From.Mock == b.Name || ln.From.Mock == a.Name)
					refused := ln.Kind == KindUnaccounted && strings.Contains(ln.Why, "to the hour, stands 2h0m0s after the moment mock-4 at spec.resp.body holds as the recording gave it")
					if !bound && !refused {
						t.Errorf("gap %v, %s, %s: %s from %+v (%s); want a variable of a receive, or refused, built on the receives by two hours", gap, row.name, served.name, ln.Kind, ln.From, ln.Why)
					}
					continue
				}
				if !served.refused {
					if ln.Kind != KindNoise {
						t.Errorf("gap %v, %s, %s: %s (%s); want noise, the lead's move", gap, row.name, served.name, ln.Kind, ln.Why)
					}
					continue
				}
				if ln.Kind != KindUnaccounted || ln.From.Mock != "" ||
					!strings.Contains(ln.Why, "the moment mock-4 at spec.resp.body holds as the recording gave it") ||
					!strings.Contains(ln.Why, "the moment mock-11 at spec.resp.body holds, which record's full round gave the application in its place") {
					t.Errorf("gap %v, %s, %s: %s from %+v (%s); want it refused, built on mock-4 and mock-11 by an offset", gap, row.name, served.name, ln.Kind, ln.From, ln.Why)
				}
			}
		}
	}
	// One receive, B, served and recorded after the call: F alone gave it,
	// and the value is refused as built on it.
	sentA := time.Date(2026, 10, 22, 11, 10, 16, 473e6, time.UTC)
	sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
	a := receive("mock-4", sentA, time.Date(2026, 10, 22, 11, 14, 13, 778e6, time.UTC))
	b := receive("mock-11", sentB, time.Date(2026, 10, 22, 11, 16, 20, 0, time.UTC))
	dc := NewDriftCache()
	if ln := ClassifyIn(dc, []string{p}, strLeaf(p, sentB.Add(time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, sentA.Add(time.Minute).Format("2006-01-02T15:04Z07:00")), r1Insert, nil, []Raw{dc.RawOf(a, false), dc.RawOf(b, true).ServedAs(1)}, nil, nil)[0]; ln.Kind != KindUnaccounted {
		t.Errorf("B alone served, after the call: %s (%s); want it refused", ln.Kind, ln.Why)
	}
	// One of the two recorded at the call's own instant, F serving them in
	// the order they were recorded in: the recording may never have given
	// that one before the call (the equal instant read fail-closed), so the
	// recorded order is no order both runs gave the application, and a
	// value on A's moment as recorded and B's as F gave is refused, built on
	// both by an offset, as in the other order.
	for _, c := range []struct {
		name         string
		a, b         *models.Mock
		firstA, gave string
	}{
		{"B at the call's own instant, F serving A then B", a, receive("mock-11", sentB, r1Insert), "", ", and"},
		{"A at the call's own instant, F serving B then A", receive("mock-4", sentA, r1Insert), receive("mock-11", sentB, time.Date(2026, 10, 22, 11, 14, 15, 101e6, time.UTC)), "b", " (recorded at the call's own instant, so keploy reads it as given to the application before the call), and"},
	} {
		dc := NewDriftCache()
		ra, rb := dc.RawOf(c.a, true).ServedAs(1), dc.RawOf(c.b, true).ServedAs(2)
		if c.firstA == "b" {
			ra, rb = dc.RawOf(c.a, true).ServedAs(2), dc.RawOf(c.b, true).ServedAs(1)
		}
		ln := ClassifyIn(dc, []string{p}, strLeaf(p, sentB.Add(time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, sentA.Add(time.Minute).Format("2006-01-02T15:04Z07:00")), r1Insert, nil, []Raw{ra, rb}, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "the moment mock-4 at spec.resp.body holds as the recording gave it"+c.gave) ||
			!strings.Contains(ln.Why, "the moment mock-11 at spec.resp.body holds, which record's full round gave the application in its place") {
			t.Errorf("FALSE GREEN? in the recorded order, %s: %s (%s); want it refused, built on mock-4 and mock-11 by an offset", c.name, ln.Kind, ln.Why)
		}
	}
}

// inRecordedOrder: one recording is in its own order; two are in the
// recording's order only where F's order of them is known and the same.
func TestInRecordedOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)
	at := func(mock string, d time.Duration, nth int) offsetPlace {
		return offsetPlace{mock: mock, at: t0.Add(d), first: nth, last: nth}
	}
	span := func(mock string, d time.Duration, first, last int) offsetPlace {
		return offsetPlace{mock: mock, at: t0.Add(d), first: first, last: last}
	}
	for _, c := range []struct {
		name string
		a, b offsetPlace
		want bool
	}{
		{"one recording", at("mock-1", 0, 0), at("mock-1", 0, 0), true},
		{"as recorded", at("mock-1", 0, 1), at("mock-2", time.Second, 2), true},
		{"as recorded, b first", at("mock-2", time.Second, 5), at("mock-1", 0, 3), true},
		{"the other order", at("mock-1", 0, 2), at("mock-2", time.Second, 1), false},
		{"the other order, b first", at("mock-2", time.Second, 1), at("mock-1", 0, 2), false},
		{"a's order unknown", at("mock-1", 0, 0), at("mock-2", time.Second, 2), false},
		{"b's order unknown", at("mock-1", 0, 1), at("mock-2", time.Second, 0), false},
		{"recorded at one instant", at("mock-1", 0, 1), at("mock-2", 0, 2), false},
		{"recorded at one instant, served the other way", at("mock-1", 0, 2), at("mock-2", 0, 1), false},
		{"served as one", at("mock-1", 0, 1), at("mock-2", time.Second, 1), false},
		// A pool's recording F served at the start-up and again after the
		// other: F gave the application each before the other.
		{"the first served again after the second", span("mock-1", 0, 1, 8), at("mock-2", time.Second, 7), false},
		{"the second served before the first, and again", span("mock-2", time.Second, 2, 9), at("mock-1", 0, 5), false},
		{"the second served before the first's last serve", at("mock-2", time.Second, 4), span("mock-1", 0, 1, 4), false},
		{"each served wholly before the other", span("mock-1", 0, 1, 3), span("mock-2", time.Second, 4, 9), true},
		{"each served wholly before the other, b first", span("mock-2", time.Second, 4, 9), span("mock-1", 0, 1, 3), true},
		{"wholly in the other order", span("mock-1", 0, 4, 9), span("mock-2", time.Second, 1, 3), false},
	} {
		if got := inRecordedOrder(c.a, c.b); got != c.want {
			t.Errorf("%s: %v; want %v", c.name, got, c.want)
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

// A recording of another recorded test that F gave the application before
// the call (Raw.OfAnotherTest) is a place of the offset rule and the word
// rule, and of no other: a date built on what F gave there by an offset is
// refused, naming it, and so is a value its recording holds, which binds no
// variable of this test's call (only a test's own calls carry its
// variables), as a place of the test would: it is refused, naming it, never
// noise.
func TestAnotherTestsRecordingIsAPlaceOfTheOffsetAndWordRulesAlone(t *testing.T) {
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
	dc := NewDriftCache()
	raw := []Raw{dc.RawOf(receive, true).ServedAs(2).OfAnotherTest(), dc.RawOf(delivered, true).ServedAs(1).OfAnotherTest()}
	ln := ClassifyIn(dc, []string{p}, strLeaf(p, got.Add(-2*day).Format("2006-01-02")), strLeaf(p, sent.Add(-2*day).Format("2006-01-02")), r1Insert, nil, raw, nil, nil)[0]
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands 48h0m0s before the moment mock-4 at spec.resp.body holds as the recording gave it") {
		t.Errorf("an offset from another test's recordings: %s (%s); want it refused, naming the receive and the offset", ln.Kind, ln.Why)
	}
	const q = "body.user"
	row := Raw{read: true, mock: "pg-3", at: r1Insert.Add(-2 * time.Second), text: `{"query":"INSERT INTO users VALUES ($1)","params":["u-12"]}`}
	for _, another := range []bool{false, true} {
		x := row
		if another {
			x = x.OfAnotherTest()
		}
		ln := ClassifyIn(NewDriftCache(), []string{q}, strLeaf(q, "u-77"), strLeaf(q, "u-12"), r1Insert, nil, []Raw{x}, nil, nil)[0]
		if want := map[bool]Kind{false: KindVariable, true: KindUnaccounted}[another]; ln.Kind != want || another && !strings.Contains(ln.Why, "appears in pg-3, a recording record's full round gave the application before the call that is another test's") {
			t.Errorf("another test's %v: %s from %+v (%s); want %s", another, ln.Kind, ln.From, ln.Why, want)
		}
	}
}

// receiveOf is the receive r1Receive records, of the message id, recorded at
// at.
func receiveOf(name, id string, at time.Time) *models.Mock {
	m := r1Receive("")
	m.Name = name
	m.Spec.HTTPResp.Body = strings.Replace(m.Spec.HTTPResp.Body, "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323", id, 1)
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Second)
	return m
}

// A value copied word for word from the answer of a recording F gave the
// application in another test (the queue's: F gave the consumer another
// test's message, whose id it writes) is refused, naming that recording,
// never noise: a word the value holds now, found in any such answer; a word
// it was recorded with, found in one recorded before the call, which the
// recording gave the application too. The answer is read, never the request
// the application sent in that test, and never a recording made after the
// call for what the value was recorded with.
func TestAWordOfAnotherTestsAnswerIsRefusedNamingIt(t *testing.T) {
	const p = "body.message_id"
	recID, liveID := "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323", "0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"
	before, after := r1Insert.Add(-time.Minute), r1Insert.Add(time.Minute)
	sentOnly := receiveOf("mock-20", "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb", after)
	sentOnly.Spec.HTTPReq.Body = strings.Replace(sentOnly.Spec.HTTPReq.Body, `"WaitTimeSeconds":4`, `"WaitTimeSeconds":4,"After":"`+liveID+`"`, 1)
	// An answer whose Date header, its own clock stamp, alone holds the
	// word "17" of the value's "slot-17" (rules 2 and 3).
	stamped := receiveOf("mock-21", "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb", after)
	stamped.Spec.HTTPResp.Header["Date"] = after.Format(http.TimeFormat)
	if !strings.Contains(stamped.Spec.HTTPResp.Header["Date"], ":17 ") {
		t.Fatalf("the stamp %q holds no word 17", stamped.Spec.HTTPResp.Header["Date"])
	}
	for _, row := range []struct {
		name      string
		m         *models.Mock
		rec, live string
		want      string // "" noise
	}{
		{"a word of the value in another test's answer's own clock stamp alone", stamped, "slot-03", "slot-17", ""},
		{"the live id in another test's answer, recorded after the call", receiveOf("mock-12", liveID, after), recID, liveID, "a word of what record's full round saw in it (its characters 1 to 8) appears in mock-12, a recording record's full round gave the application before the call that was recorded after the call"},
		{"the recorded id in another test's answer recorded before the call", receiveOf("mock-13", recID, before), recID, liveID, "a word of its recorded value (its characters 1 to 8) appears in mock-13, a recording record's full round gave the application before the call that is another test's"},
		{"the recorded id in another test's answer recorded after the call", receiveOf("mock-14", recID, after), recID, liveID, ""},
		{"the live id in another test's request alone", sentOnly, recID, liveID, ""},
	} {
		for _, viaSet := range []bool{false, true} {
			dc := NewDriftCache()
			x := dc.RawOf(row.m, true).ServedAs(3)
			var ln Learned
			if viaSet {
				reads := 0
				ln = ClassifyGiven(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
					reads++
					return []GivenPlaces{PlacesOf(x)}, nil
				})[0]
				if reads != 1 {
					t.Errorf("%s, F gave it earlier in the set: the set read %d times; want once", row.name, reads)
				}
			} else {
				ln = ClassifyIn(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, []Raw{x.OfAnotherTest()}, nil, nil)[0]
			}
			if row.want == "" {
				if ln.Kind != KindNoise {
					t.Errorf("%s (set %v): %s (%s); want noise", row.name, viaSet, ln.Kind, ln.Why)
				}
				continue
			}
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.want) || !strings.Contains(ln.Why, "otherwise it flowed from "+row.m.Name) {
				t.Errorf("%s (set %v): %s (%s); want it refused: %s", row.name, viaSet, ln.Kind, ln.Why, row.want)
			}
		}
	}
}

// Where the call's own places say a stamp is a place the value was built on
// (stampsEchoed: the value's minute moved as the minute of a Date header F
// served at record to that of another F served), stamps are searched as any
// text, another test's answer's too: a word the value holds now that
// another test's answer's Date header alone holds is refused, naming it,
// as any word of that answer is; without the echo it is noise (the first
// row of TestAWordOfAnotherTestsAnswerIsRefusedNamingIt).
func TestAnotherTestsAnswerStampIsSearchedWhereTheValueEchoesAStamp(t *testing.T) {
	const p = "body.slot"
	date := func(v string) Seen {
		return Seen{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: v}, Live: v, Stamp: true}
	}
	own := []Earlier{
		{Mock: "mock-1", At: r1Insert.Add(-3 * time.Second), Payload: payloadOf("mock-1", r1Insert.Add(-3*time.Second)), Own: true, Seen: []Seen{date("Wed, 21 Oct 2026 08:14:44 GMT")}},
		{Mock: "mock-2", At: r1Insert.Add(-2 * time.Second), Payload: payloadOf("mock-2", r1Insert.Add(-2*time.Second)), Own: true, Seen: []Seen{date("Wed, 21 Oct 2026 08:15:44 GMT")}},
	}
	// The value's instant moves by the lead, so its minute and second are
	// clock words the word rules skip; only "03" -> "54" changed besides.
	rec := "slot-03 " + r1Insert.Format(time.RFC3339)
	live := "slot-54 " + r1Insert.Add(FullRoundLead).Format(time.RFC3339)
	for _, stampHolds := range []bool{false, true} {
		// Another test's answer, its Date header its own clock stamp (its
		// span holds it): 10:54:17 holds the word 54, 10:53:17 does not.
		at := r1Insert.Add(-21 * time.Minute)
		if stampHolds {
			at = r1Insert.Add(-20 * time.Minute)
		}
		other := receiveOf("mock-21", "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb", at)
		other.Spec.HTTPResp.Header["Date"] = at.Format(http.TimeFormat)
		for _, echoed := range []bool{false, true} {
			var earlier []Earlier
			if echoed {
				earlier = own
			}
			dc := NewDriftCache()
			x := dc.RawOf(other, true).ServedAs(3).OfAnotherTest()
			ln := ClassifyIn(dc, []string{p}, strLeaf(p, live), strLeaf(p, rec), r1Insert, earlier, []Raw{x}, nil, nil)[0]
			if stampHolds && echoed {
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "a word of what record's full round saw in it (its characters 6 to 7) appears in mock-21, a recording record's full round gave the application before the call that is another test's") {
					t.Errorf("another test's stamp holds 54, the value echoes a stamp: %s (%s); want it refused, naming mock-21", ln.Kind, ln.Why)
				}
				continue
			}
			if ln.Kind != KindNoise {
				t.Errorf("another test's stamp holds 54 %v, the value echoes a stamp %v: %s (%s); want noise", stampHolds, echoed, ln.Kind, ln.Why)
			}
		}
	}
}

// A recording of a kind that holds its answer apart from its request that
// holds no answer (a generic parser's frames the application sent, that
// nothing answered) gave the application nothing (answerApart): the offset
// rule reads no place of it and the word rule no word, so a value that
// changed to a word the application sent there in another test is noise,
// while the same text in its answer is a place of both, and refused.
func TestARecordingThatHoldsNoAnswerGaveTheApplicationNothing(t *testing.T) {
	const p = "body.user"
	frame := func(o models.OriginType, data string) []models.Payload {
		return []models.Payload{{Origin: o, Message: []models.OutputBinary{{Type: "utf-8", Data: data}}}}
	}
	for _, answered := range []bool{false, true} {
		m := &models.Mock{Name: "gen-1", Kind: models.GENERIC, Spec: models.MockSpec{
			GenericRequests:  frame(models.FromClient, "SET user u-77 at 2026-10-22T11:13:16Z"),
			ReqTimestampMock: r1Insert.Add(-time.Minute), ResTimestampMock: r1Insert.Add(-time.Minute + time.Millisecond)}}
		if answered {
			m.Spec.GenericResponses = frame(models.FromServer, "VALUE user u-77 at 2026-10-22T11:13:16Z")
		}
		dc := NewDriftCache()
		x := dc.RawOf(m, true).ServedAs(2)
		places := dc.keyedPlaces(x)
		if len(places) > 0 != answered {
			t.Errorf("answered %v: %d places of the offset rule (%+v)", answered, len(places), places)
		}
		for _, kp := range places {
			if fieldRoot(kp.k.path) != "ResponseBin" {
				t.Errorf("answered %v: a place of the offset rule in what the application sent: %s", answered, kp.k.path)
			}
		}
		ln := ClassifyIn(dc, []string{p}, strLeaf(p, "u-77"), strLeaf(p, "u-12"), r1Insert, nil, []Raw{x.OfAnotherTest()}, nil, nil)[0]
		switch {
		case !answered && ln.Kind != KindNoise:
			t.Errorf("a word the application sent in another test: %s (%s); want noise", ln.Kind, ln.Why)
		case answered && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "appears in gen-1, a recording record's full round gave the application before the call that is another test's")):
			t.Errorf("a word F answered in another test: %s (%s); want it refused, naming gen-1", ln.Kind, ln.Why)
		}
	}
}

// The recordings F gave the application earlier in the set (GivenEarlier)
// are read only when a changed instant reaches the offset rule or a word of
// what changed the word rule, once for the call, and as places of those
// rules alone: a word of a value in one is refused, naming it, never bound;
// one that cannot be read refuses every changed instant or word that reaches
// either rule, naming it, never noise.
func TestClassifyGivenReadsTheSetOnlyForTheOffsetAndWordRules(t *testing.T) {
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
	row := Raw{read: true, mock: "pg-3", at: r1Insert.Add(-2 * time.Second), text: `{"query":"INSERT INTO users VALUES ($1)","params":["u-12"]}`}
	reads := 0
	set := func(raw []Raw, err error) GivenEarlier {
		return func() ([]GivenPlaces, error) {
			reads++
			var out []GivenPlaces
			for _, x := range raw {
				out = append(out, PlacesOf(x))
			}
			return out, err
		}
	}
	const p, q = "body.due", "body.user"
	day := 24 * time.Hour
	due := func(at time.Time) Shape { return strLeaf(p, at.Add(-2*day).Format("2006-01-02")) }
	both := func(dc *DriftCache) []Raw {
		return []Raw{dc.RawOf(receive, true).ServedAs(2), dc.RawOf(delivered, true).ServedAs(1), row.ServedAs(3)}
	}
	// A value a place of the call's own test accounts for: the set is not
	// read, and it is a variable of that place.
	dc := NewDriftCache()
	if ln := ClassifyGiven(dc, []string{q}, strLeaf(q, "u-77"), strLeaf(q, "u-12"), r1Insert, nil, []Raw{row}, nil, nil, set(both(dc), nil))[0]; ln.Kind != KindVariable || reads != 0 {
		t.Errorf("a place of the test's own: %s (%s), the set read %d times; want a variable, the set unread", ln.Kind, ln.Why, reads)
	}
	// A value a place of the call's own test refuses (a word of it there):
	// the set is not read.
	dc = NewDriftCache()
	own := Raw{read: true, mock: "pg-5", at: r1Insert.Add(-time.Second), text: `{"params":["77"]}`}
	if ln := ClassifyGiven(dc, []string{q}, strLeaf(q, "u-77"), strLeaf(q, "u-12"), r1Insert, nil, []Raw{own}, nil, nil, set(both(dc), nil))[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "pg-5") || reads != 0 {
		t.Errorf("refused by a place of the test's own: %s (%s), the set read %d times; want it refused, naming pg-5, the set unread", ln.Kind, ln.Why, reads)
	}
	// A value of no changed instant whose word F gave earlier in the set:
	// refused, naming it, never bound; the set read once for the call.
	dc = NewDriftCache()
	if ln := ClassifyGiven(dc, []string{q, "body.user2"}, Shape{Leaves: map[string]Leaf{q: strLeaf(q, "u-77").Leaves[q], "body.user2": strLeaf(q, "u-77").Leaves[q]}}, Shape{Leaves: map[string]Leaf{q: strLeaf(q, "u-12").Leaves[q], "body.user2": strLeaf(q, "u-12").Leaves[q]}}, r1Insert, nil, nil, nil, nil, set(both(dc), nil)); ln[0].Kind != KindUnaccounted || !strings.Contains(ln[0].Why, "appears in pg-3, a recording record's full round gave the application before the call that is another test's") || ln[1].Kind != KindUnaccounted || reads != 1 {
		t.Errorf("a word F gave earlier in the set: %s (%s), the set read %d times; want it refused, naming pg-3, the set read once", ln[0].Kind, ln[0].Why, reads)
	}
	// A changed instant built on what F gave earlier in the set by an
	// offset: refused, the set read once for the call.
	reads = 0
	dc = NewDriftCache()
	ln := ClassifyGiven(dc, []string{p, "body.due2"}, Shape{Leaves: map[string]Leaf{p: due(got).Leaves[p], "body.due2": due(got).Leaves[p]}}, Shape{Leaves: map[string]Leaf{p: due(sent).Leaves[p], "body.due2": due(sent).Leaves[p]}}, r1Insert, nil, nil, nil, nil, set(both(dc), nil))
	if ln[0].Kind != KindUnaccounted || !strings.Contains(ln[0].Why, "stands 48h0m0s before the moment mock-4 at spec.resp.body holds as the recording gave it") || reads != 1 {
		t.Errorf("an offset from the set: %s (%s), the set read %d times; want it refused, naming the receive, the set read once", ln[0].Kind, ln[0].Why, reads)
	}
	// What cannot be read refuses the changed instant, naming it.
	for name, c := range map[string]struct {
		raw  []Raw
		err  error
		want string
	}{
		"an answer not read back": {nil, errors.New(`read back the recorded answer of "mock-9": gone`), `keploy cannot read back a recording record's full round gave the application earlier in the test set, so it cannot tell whether what changed came from it or was built on it: read back the recorded answer of "mock-9"`},
		"a recording not read":    {[]Raw{{mock: "mock-9"}}, nil, "keploy could not read mock-9"},
	} {
		for _, leaf := range []string{p, q} {
			live, rec := due(got), due(sent)
			if leaf == q {
				live, rec = strLeaf(q, "u-77"), strLeaf(q, "u-12")
			}
			ln := ClassifyGiven(NewDriftCache(), []string{leaf}, live, rec, r1Insert, nil, nil, nil, nil, set(c.raw, c.err))[0]
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, c.want) {
				t.Errorf("%s, at %s: %s (%s); want it refused, naming mock-9", name, leaf, ln.Kind, ln.Why)
			}
		}
	}
}

// sentAtReceive is a receive whose answer holds the message's SentAt (sentAt,
// as its text), recorded at at.
func sentAtReceive(name string, at time.Time, sentAt ...string) *models.Mock {
	msgs := make([]string, 0, len(sentAt))
	for i, s := range sentAt {
		msgs = append(msgs, `{"MessageId":"k7f3a9c2`+strconv.Itoa(i)+`","SentAt":"`+s+`"}`)
	}
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://sqs.local/000000000000/q", Body: `{"MaxNumberOfMessages":10}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"Messages":[` + strings.Join(msgs, ",") + `]}`},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// A time the application copies word for word from what record's full round
// gave it before the call, at a place of another test's or recorded after the
// call (a recording F gave it in another test, Raw.OfAnotherTest, or earlier
// in the set: GivenEarlier, which holds one F served in this test before the
// call but that was recorded after it), is refused, naming that recording,
// never noise, though it moved as the lead moves a read of the clock: the
// echo reads the moment F served there, in every form, whenever its recording
// was made. A time the recording held when it was made there is a place of
// the value only where it was recorded before the call, which the recording
// gave the application too: a recording made after the call holds what the
// recorded value was built on nowhere, and its moment that only F's value is
// cut from (an instant naming both, the neighbour that names the recorded
// one recorded after the call) is a copy of neither. Each place, as a
// recording of another test's and as one of the set.
func TestATimeCopiedFromWhatFGaveElsewhereIsRefusedNamingIt(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	before, after := at.Add(-time.Minute), at.Add(3*time.Minute)
	const minute = "2006-01-02T15:04Z07:00"
	forms := []struct {
		name      string
		rec, live Shape
		copied    string
	}{
		{"a minute", strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute)), recT.Add(time.Minute).Format(minute)},
		{"a typed instant", typedLeaf(p, recT, time.Millisecond), typedLeaf(p, recT.Add(61*time.Second), time.Millisecond), recT.Add(61 * time.Second).Format(time.RFC3339Nano)},
	}
	for _, f := range forms {
		// Nothing gave the application the moment: a read of the clock the
		// lead moved, learned as noise.
		if ln := ClassifyIn(NewDriftCache(), []string{p}, f.live, f.rec, at, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Fatalf("%s given nowhere: %s (%s); want a read of the clock, noise", f.name, ln.Kind, ln.Why)
		}
		for _, row := range []struct {
			name string
			m    *models.Mock
			want string // "" noise
		}{
			{"copied, recorded after the call", sentAtReceive("mock-30", after, f.copied), "was recorded after the call"},
			{"copied, recorded before the call", sentAtReceive("mock-31", before, f.copied), "is another test's"},
			{"the recorded moment alone, recorded after the call", sentAtReceive("mock-32", after, recT.Format(time.RFC3339Nano)), ""},
			{"copied in the request the application sent alone", requestOnly("mock-35", after, f.copied), ""},
			{"copied, inside a base64 body", base64Receive("mock-36", after, f.copied), "was recorded after the call"},
		} {
			for _, viaSet := range []bool{false, true} {
				dc := NewDriftCache()
				x := dc.RawOf(row.m, true).ServedAs(2)
				var ln Learned
				if viaSet {
					ln = ClassifyGiven(dc, []string{p}, f.live, f.rec, at, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
						return []GivenPlaces{PlacesOf(x)}, nil
					})[0]
				} else {
					ln = ClassifyIn(dc, []string{p}, f.live, f.rec, at, nil, []Raw{x.OfAnotherTest()}, nil, nil)[0]
				}
				if row.want == "" {
					if ln.Kind != KindNoise {
						t.Errorf("%s, %s (set %v): %s (%s); want noise", f.name, row.name, viaSet, ln.Kind, ln.Why)
					}
					continue
				}
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "the moment record's full round saw in it stands in "+row.m.Name+" at spec.resp.body") || !strings.Contains(ln.Why, row.want) || !strings.Contains(ln.Why, "otherwise it flowed from "+row.m.Name) {
					t.Errorf("FALSE GREEN? %s, %s (set %v): %s (%s); want it refused, naming %s: %s", f.name, row.name, viaSet, ln.Kind, ln.Why, row.m.Name, row.want)
				}
			}
		}
	}
	// A moment of the answer names both the recorded minute (cut down) and
	// F's (rounded): alone a copy of neither where both runs were given it.
	// The neighbour naming the recorded minute rounded, as F's is, makes it a
	// copy where the recording gave the application it before the call.
	// Recorded after the call, the recorded application was never given it
	// before the call, so only what F served there counts, and F's minute is
	// a cut of it: a copy all the same.
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	both, neighbour := recT.Truncate(time.Minute).Add(45*time.Second).Format(time.RFC3339), recT.Truncate(time.Minute).Add(10*time.Second).Format(time.RFC3339)
	for _, row := range []struct {
		name string
		m    *models.Mock
		want string
	}{
		{"recorded before the call", sentAtReceive("mock-33", before, both, neighbour), "is another test's"},
		{"recorded after the call", sentAtReceive("mock-34", after, both, neighbour), "was recorded after the call"},
	} {
		for _, viaSet := range []bool{false, true} {
			dc := NewDriftCache()
			x := dc.RawOf(row.m, true).ServedAs(2)
			var ln Learned
			if viaSet {
				ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
					return []GivenPlaces{PlacesOf(x)}, nil
				})[0]
			} else {
				ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{x.OfAnotherTest()}, nil, nil)[0]
			}
			if row.want == "" {
				if ln.Kind != KindNoise {
					t.Errorf("an instant naming both, its neighbour %s (set %v): %s (%s); want noise", row.name, viaSet, ln.Kind, ln.Why)
				}
				continue
			}
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in "+row.m.Name+" at spec.resp.body") || !strings.Contains(ln.Why, row.want) {
				t.Errorf("an instant naming both, its neighbour %s (set %v): %s (%s); want it refused, naming %s", row.name, viaSet, ln.Kind, ln.Why, row.m.Name)
			}
		}
	}
}

// A recording F gave the application before the call that was recorded
// after the call was never given to the recorded application before the
// call, so only what F served there counts: an instant of it that names the
// moment F saw is a copy, refused naming it, even where the same instant
// names the recorded moment too (11:14:40 rounds to F's minute and cuts down
// to the recorded one); one that names the recorded moment alone shows no
// copy, noise. One recorded at the call's own instant (a fixture's whole
// second) is read both ways, the fail-closed side: what F served there, and
// what the recording may have given the application before the call; the
// verdicts are the same, the reasons saying it was recorded at the call's
// own instant, and the recorded moment alone is noise unless the recording
// names it by the rule by which the call's own place names F's. Beside the own place, the offset rule
// reads such a recording as the recording gave it (the equal instant read
// fail-closed) against the own place F gave, and refuses naming both and
// that it was recorded at the call's own instant.
func TestARecordingMadeAtOrAfterTheCallCountsOnlyWhatFServed(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	before, after := at.Add(-time.Minute), at.Add(3*time.Minute)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	for _, when := range []struct {
		name string
		at   time.Time
	}{{"recorded after the call", after}, {"recorded at the call's own instant", at}} {
		for _, tc := range []struct {
			name    string
			sent    time.Time
			withOwn bool
			want    string
		}{
			{"F's minute alone", base.Add(65 * time.Second), false, "stands in mock-44 at spec.resp.body, a recording record's full round gave the application before the call that was recorded at or after the call"},
			{"F's minute alone, beside the own place", base.Add(65 * time.Second), true, "mock-44"},
			{"rounds to F's minute, cuts down to the recorded one", base.Add(40 * time.Second), false, "stands in mock-44 at spec.resp.body, a recording record's full round gave the application before the call that was recorded at or after the call"},
			{"rounds to F's minute, cuts down to the recorded one, beside the own place", base.Add(40 * time.Second), true, "stands in mock-44 at spec.resp.body, a recording record's full round gave the application before the call that was recorded at or after the call"},
			{"the recorded minute alone", base.Add(10 * time.Second), false, ""},
			{"the recorded minute alone, beside the own place", base.Add(10 * time.Second), true, ""},
		} {
			// Recorded at the call's own instant, the neighbour is one the
			// recorded application may have been given before the call (the
			// fail-closed side), and the reason says so.
			want := strings.Replace(tc.want, "was recorded at or after the call", "was recorded after the call", 1)
			if when.at.Equal(at) {
				want = strings.Replace(tc.want, "was recorded at or after the call", "was "+atCallWords+", yet as no earlier place of the call's own test", 1)
			}
			if when.at.Equal(at) && tc.withOwn && tc.sent != base.Add(65*time.Second) {
				// The neighbour, which the recorded application may have
				// been given before the call, as the recording gave it, and
				// the own place F gave stand a whole minute apart or at one
				// minute: the offset rule refuses naming both.
				want = " the moment mock-44 at spec.resp.body holds as the recording gave it (recorded at the call's own instant, so keploy reads it as given to the application before the call), and record's full round saw it "
			}
			for _, viaSet := range []bool{false, true} {
				dc := NewDriftCache()
				nb := dc.RawOf(sentAtReceive("mock-44", when.at, tc.sent.Format(time.RFC3339)), true).ServedAs(1)
				var own []Raw
				if tc.withOwn {
					own = []Raw{dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3)}
				}
				var ln Learned
				if viaSet {
					ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, own, nil, nil, func() ([]GivenPlaces, error) {
						return []GivenPlaces{PlacesOf(nb)}, nil
					})[0]
				} else {
					ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, append(own, nb.OfAnotherTest()), nil, nil)[0]
				}
				if want == "" {
					if ln.Kind != KindNoise {
						t.Errorf("%s, %s (set %v): %s (%s); want noise, no copy shown", when.name, tc.name, viaSet, ln.Kind, ln.Why)
					}
					continue
				}
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, want) {
					t.Errorf("FALSE GREEN? %s, %s (set %v): %s (%s); want it refused: %s", when.name, tc.name, viaSet, ln.Kind, ln.Why, want)
				}
			}
		}
		// A day both moments fall in, served there, names both by one rule:
		// it is the source of no change (what changed is the application's
		// own minute), so the value is noise, never refused naming it.
		for _, viaSet := range []bool{false, true} {
			dc := NewDriftCache()
			nb := dc.RawOf(sentAtReceive("mock-45", when.at, base.Format("2006-01-02")), true).ServedAs(1)
			var ln Learned
			if viaSet {
				ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
					return []GivenPlaces{PlacesOf(nb)}, nil
				})[0]
			} else {
				ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{nb.OfAnotherTest()}, nil, nil)[0]
			}
			if ln.Kind != KindNoise {
				t.Errorf("%s, the day both moments fall in (set %v): %s (%s); want noise, the day no source of the change", when.name, viaSet, ln.Kind, ln.Why)
			}
		}
	}
}

// A recording of the test that record's full round did not serve, made at
// the call's own instant (a fixture's whole second), may have been made
// before the call: it is an earlier place, read as one recorded a moment
// before the call is, so the id it answered with, which the value now holds,
// is refused as one found there, never noise. One recorded a moment after
// the call is no earlier place.
func TestAnUnservedRecordingAtTheCallsOwnInstantIsAnEarlierPlace(t *testing.T) {
	const p = "body.message_id"
	recID, liveID := "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323", "0b6e2f1c-5d7a-4e8b-9c3d-2a1f4e6b8c7d"
	at := r1Insert
	verdict := func(when time.Time) Learned {
		dc := NewDriftCache()
		x := dc.RawOf(receiveOf("mock-50", liveID, when), false)
		return ClassifyIn(dc, []string{p}, strLeaf(p, liveID), strLeaf(p, recID), at, nil, []Raw{x}, nil, nil)[0]
	}
	before, same, after := verdict(at.Add(-time.Second)), verdict(at), verdict(at.Add(time.Second))
	if before.Kind == KindNoise || !strings.Contains(before.Why, "mock-50") {
		t.Fatalf("recorded a moment before the call: %s (%s); want it refused naming mock-50", before.Kind, before.Why)
	}
	// The same verdict and reason, the reason saying the recording was made
	// at the call's own instant.
	if want := strings.Replace(before.Why, "mock-50 at spec.resp.body", "mock-50 at spec.resp.body"+atCallNote, 1); same.Kind != before.Kind || want == before.Why || same.Why != want {
		t.Errorf("FALSE GREEN? recorded at the call's own instant: %s (%s); want what one recorded a moment before gets: %s (%s)", same.Kind, same.Why, before.Kind, want)
	}
	if after.Kind != KindNoise {
		t.Errorf("recorded a moment after the call: %s (%s); want noise, no earlier place", after.Kind, after.Why)
	}
}

// A recording record's full round served before the call, made at the
// call's own instant, is read as one made a moment before the call is (the
// fail-closed side keploy reads the equal instant by everywhere), wherever
// the caller hands it: as an earlier place F served (a Raw), or as one F
// gave the application that no earlier place of the call's own test is
// (GivenEarlier). A value it holds whole is refused naming it either way,
// the reason saying it was recorded at the call's own instant: handed as an
// earlier place, it is read as one made after the call too, the stricter
// reading (TestAServedRecordingAtTheCallsOwnInstantIsReadBothWays). One
// recorded a moment after the call, which the caller hands only as what F
// gave, is no source of it.
func TestAServedRecordingAtTheCallsOwnInstantIsReadAsOneAMomentBefore(t *testing.T) {
	const p = "body.msg_id"
	const recID, liveID = "k1b2e4d70", "k5d0e8a33"
	at := r1Insert
	for _, viaSet := range []bool{false, true} {
		verdict := func(when time.Time) Learned {
			dc := NewDriftCache()
			x := dc.RawOf(receiveOf("mock-50", recID, when), true).ServedAs(1)
			if viaSet {
				return ClassifyGiven(dc, []string{p}, strLeaf(p, liveID), strLeaf(p, recID), at, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
					return []GivenPlaces{PlacesOf(x)}, nil
				})[0]
			}
			return ClassifyIn(dc, []string{p}, strLeaf(p, liveID), strLeaf(p, recID), at, nil, []Raw{x}, nil, nil)[0]
		}
		before, same, after := verdict(at.Add(-time.Second)), verdict(at), verdict(at.Add(time.Second))
		if before.Kind == KindNoise || !strings.Contains(before.Why, "mock-50") {
			t.Fatalf("set %v, recorded a moment before the call: %s (%s); want it refused naming mock-50", viaSet, before.Kind, before.Why)
		}
		if same.Kind == KindNoise || !strings.Contains(same.Why, "mock-50") || !strings.Contains(same.Why, atCallWords) {
			t.Errorf("FALSE GREEN? set %v, recorded at the call's own instant: %s (%s); want it refused naming mock-50 and the instant, as one recorded a moment before is: %s (%s)", viaSet, same.Kind, same.Why, before.Kind, before.Why)
		}
		// At least the verdict of one recorded a moment before: read as an
		// earlier place too, the value it holds whole is bound to it there,
		// and read as one made after the call, which F alone served, it is
		// refused; the stricter reading decides (classifyGiven).
		if same.Kind != before.Kind && same.Kind != KindUnaccounted {
			t.Errorf("set %v: recorded at the call's own instant %s, a moment before %s; want that verdict or a refusal", viaSet, same.Kind, before.Kind)
		}
		if !viaSet && same.Kind != KindUnaccounted {
			t.Errorf("FALSE GREEN? set %v, recorded at the call's own instant, handed as an earlier place F served: %s (%s); want it refused, as read as one made after the call", viaSet, same.Kind, same.Why)
		}
		// A Raw F served is an earlier place whenever it was recorded
		// (Raw.earlierThan); the caller hands one recorded after the call
		// only as what F gave (GivenEarlier).
		if viaSet && after.Kind != KindNoise {
			t.Errorf("set %v, recorded a moment after the call: %s (%s); want noise, no earlier place", viaSet, after.Kind, after.Why)
		}
	}
	// The echo: a minute F saw that such a recording holds, while no earlier
	// place holds the recorded one, is refused naming it, as where one made
	// a moment before the call holds it.
	const sp = "body.sent_at"
	const minute = "2006-01-02T15:04Z07:00"
	recT := at.Truncate(time.Second)
	base := recT.Truncate(time.Minute)
	rec, live := strLeaf(sp, recT.Format(minute)), strLeaf(sp, recT.Add(time.Minute).Format(minute))
	echo := func(when time.Time) Learned {
		dc := NewDriftCache()
		x := dc.RawOf(sentAtReceive("mock-51", when, base.Add(65*time.Second).Format(time.RFC3339)), true).ServedAs(1)
		return ClassifyIn(dc, []string{sp}, live, rec, at, nil, []Raw{x}, nil, nil)[0]
	}
	before, same := echo(at.Add(-time.Second)), echo(at)
	if before.Kind != KindUnaccounted || !strings.Contains(before.Why, "in mock-51 at spec.resp.body") {
		t.Fatalf("the echo, recorded a moment before the call: %s (%s); want it refused naming mock-51", before.Kind, before.Why)
	}
	if want := strings.Replace(before.Why, "in mock-51 at spec.resp.body", "in mock-51 at spec.resp.body"+atCallNote, 1); same.Kind != before.Kind || same.Why != want {
		t.Errorf("FALSE GREEN? the echo, recorded at the call's own instant: %s (%s); want %s (%s)", same.Kind, same.Why, before.Kind, want)
	}
}

// requestOnly is a receive whose request, which the application sent, holds
// sentAt, and whose answer holds no instant, recorded at at.
func requestOnly(name string, at time.Time, sentAt string) *models.Mock {
	m := sentAtReceive(name, at)
	m.Spec.HTTPReq.Body = `{"MaxNumberOfMessages":10,"After":"` + sentAt + `"}`
	return m
}

// base64Receive is a receive whose message body, base64, holds sentAt.
func base64Receive(name string, at time.Time, sentAt string) *models.Mock {
	m := sentAtReceive(name, at)
	m.Spec.HTTPResp.Body = `{"Messages":[{"MessageId":"k7f3a9c20","Body":"` + base64.StdEncoding.EncodeToString([]byte(`{"SentAt":"`+sentAt+`"}`)) + `"}]}`
	return m
}

// Of the recordings F gave the application elsewhere, the echo names the one
// that holds the moment F saw, whichever of them comes first; and where that
// moment stands at a place of the call's own test whose instant names both
// moments (no copy alone), and another test's recording, made before the
// call, names the moment the value was recorded with by the rule that names
// F's, the value is refused naming both, never noise.
func TestTheEchoOfWhatFGaveElsewhereNamesItsPlace(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	before, after := at.Add(-time.Minute), at.Add(3*time.Minute)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	dc := NewDriftCache()
	other, copied := dc.RawOf(sentAtReceive("mock-40", after, recT.Add(-time.Hour).Format(time.RFC3339)), true).ServedAs(1), dc.RawOf(sentAtReceive("mock-41", after, recT.Add(time.Minute).Format(minute)), true).ServedAs(2)
	ln := ClassifyGiven(dc, []string{p}, live, rec, at, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
		return []GivenPlaces{PlacesOf(other), PlacesOf(copied)}, nil
	})[0]
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in mock-41 at spec.resp.body") || strings.Contains(ln.Why, "mock-40") {
		t.Errorf("two recordings of the set: %s (%s); want it refused naming mock-41 alone", ln.Kind, ln.Why)
	}
	// The call's own place (served in the test) holds 11:14:45, which names
	// the recorded minute cut down and F's rounded; another test's receive,
	// recorded before the call, holds 11:14:10, the recorded minute rounded.
	base := recT.Truncate(time.Minute)
	for _, viaSet := range []bool{false, true} {
		dc := NewDriftCache()
		own := dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3)
		nb := dc.RawOf(sentAtReceive("mock-43", before.Add(-time.Minute), base.Add(10*time.Second).Format(time.RFC3339)), true).ServedAs(1)
		if ln := ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{own}, nil, nil)[0]; ln.Kind != KindNoise {
			t.Fatalf("the own place alone: %s (%s); want no copy, noise", ln.Kind, ln.Why)
		}
		var ln Learned
		if viaSet {
			ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, []Raw{own}, nil, nil, func() ([]GivenPlaces, error) {
				return []GivenPlaces{PlacesOf(nb)}, nil
			})[0]
		} else {
			ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{own, nb.OfAnotherTest()}, nil, nil)[0]
		}
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "the moment record's full round saw in it stands in mock-42 at spec.resp.body, and the moment it was recorded with stands in mock-43 at spec.resp.body, a recording record's full round gave the application before the call that is another test's") {
			t.Errorf("the own place and another test's neighbour (set %v): %s (%s); want it refused naming both", viaSet, ln.Kind, ln.Why)
		}
	}
	// The same neighbour recorded after the call (the HTTP and the Mongo
	// learn steps read one F served in the test as this too): F gave it to
	// the application before the call, but the recording made the call
	// before it, so it is no place the recorded moment came from, and F's
	// moment is no cut of it. The own place's instant names both moments
	// (no copy alone), so nothing shows a copy: the value is noise, never
	// refused naming the later recording as where the recorded moment
	// stands.
	dc = NewDriftCache()
	own := dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3)
	late := dc.RawOf(sentAtReceive("mock-44", after, base.Add(10*time.Second).Format(time.RFC3339)), true).ServedAs(1)
	ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, []Raw{own}, nil, nil, func() ([]GivenPlaces, error) {
		return []GivenPlaces{PlacesOf(late)}, nil
	})[0]
	if ln.Kind != KindNoise {
		t.Errorf("the own place and a neighbour recorded after the call: %s (%s); want noise, the later recording no source of the recorded moment", ln.Kind, ln.Why)
	}
	// Both neighbours, the one recorded after the call read first: the
	// echo holds (the own place names F's minute rounded, the earlier
	// neighbour the recorded one), and the reason names the earlier
	// neighbour as where the recorded moment stands, never the later one,
	// which the recording made the call before.
	for _, viaSet := range []bool{false, true} {
		dc := NewDriftCache()
		own := dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3)
		late := dc.RawOf(sentAtReceive("mock-44", after, base.Add(-10*time.Second).Format(time.RFC3339)), true).ServedAs(1)
		nb := dc.RawOf(sentAtReceive("mock-43", before.Add(-time.Minute), base.Add(10*time.Second).Format(time.RFC3339)), true).ServedAs(2)
		var ln Learned
		if viaSet {
			ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, []Raw{own}, nil, nil, func() ([]GivenPlaces, error) {
				return []GivenPlaces{PlacesOf(late), PlacesOf(nb)}, nil
			})[0]
		} else {
			ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{own, late.OfAnotherTest(), nb.OfAnotherTest()}, nil, nil)[0]
		}
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "and the moment it was recorded with stands in mock-43 at spec.resp.body, a recording record's full round gave the application before the call that is another test's") || strings.Contains(ln.Why, "mock-44") {
			t.Errorf("the own place, a neighbour recorded after the call and one before it (set %v): %s (%s); want it refused naming mock-43, never mock-44", viaSet, ln.Kind, ln.Why)
		}
	}
}

// A place recorded at the call's own instant counts, for the offset rule, as
// one the recording gave the application before the call (the fail-closed
// reading every rule takes of the equal instant): a value built by an offset
// on what such a recording held, matched by what F gave in its place, is
// refused as one recorded a moment before the call is, never noise, and the
// reason says the recording was made at the call's own instant.
func TestAnOffsetOnARecordingAtTheCallsOwnInstantIsRefused(t *testing.T) {
	sent := time.Date(2026, 10, 21, 23, 58, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	const p = "body.due"
	day := 24 * time.Hour
	verdict := func(when time.Time) Learned {
		receive, delivered := r1Receive(attrs(sent)), r1Receive(attrs(got))
		delivered.Name = "mock-11"
		for _, m := range []*models.Mock{receive, delivered} {
			m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
		}
		for _, m := range []*models.Mock{receive, delivered} {
			m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = when, when
		}
		dc := NewDriftCache()
		raw := []Raw{dc.RawOf(receive, true).ServedAs(2).OfAnotherTest(), dc.RawOf(delivered, true).ServedAs(1).OfAnotherTest()}
		return ClassifyIn(dc, []string{p}, strLeaf(p, got.Add(-2*day).Format("2006-01-02")), strLeaf(p, sent.Add(-2*day).Format("2006-01-02")), r1Insert, nil, raw, nil, nil)[0]
	}
	before, same := verdict(r1Insert.Add(-time.Second)), verdict(r1Insert)
	if before.Kind != KindUnaccounted || !strings.Contains(before.Why, "stands 48h0m0s before the moment mock-4 at spec.resp.body holds as the recording gave it") {
		t.Fatalf("an offset on a recording made a moment before the call: %s (%s); want it refused naming the offset", before.Kind, before.Why)
	}
	const gave = " holds as the recording gave it"
	if want := strings.Replace(before.Why, gave, gave+" (recorded at the call's own instant, so keploy reads it as given to the application before the call)", 1); same.Kind != before.Kind || same.Why != want {
		t.Errorf("FALSE GREEN? an offset on a recording made at the call's own instant: %s (%s); want what one a moment before gets, saying when it was recorded: %s (%s)", same.Kind, same.Why, before.Kind, want)
	}
}

// A recording made at the call's own instant, of another call than the one
// whose answer the call's own place is (the offset rule pairs no places of
// two calls), holds the moment the value was recorded with by the rule by
// which the own place names F's (11:14:10 rounds to the recorded minute,
// 11:14:45 to F's): the recording may have given it to the application
// before the call, so the echo reads it as one the recording gave, and the
// value is refused naming both places, never only that no earlier place
// holds the recorded moment.
func TestARecordingOfAnotherCallAtTheCallsOwnInstantIsWhereTheRecordedMomentStands(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	for _, viaSet := range []bool{false, true} {
		dc := NewDriftCache()
		own := dc.RawOf(sentAtReceive("mock-42", at.Add(-time.Minute), base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3)
		other := sentAtReceive("mock-44", at, base.Add(10*time.Second).Format(time.RFC3339))
		other.Spec.HTTPReq.Method = "GET"
		nb := dc.RawOf(other, true).ServedAs(1)
		var ln Learned
		if viaSet {
			ln = ClassifyGiven(dc, []string{p}, live, rec, at, nil, []Raw{own}, nil, nil, func() ([]GivenPlaces, error) {
				return []GivenPlaces{PlacesOf(nb)}, nil
			})[0]
		} else {
			ln = ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{own, nb.OfAnotherTest()}, nil, nil)[0]
		}
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "the moment record's full round saw in it stands in mock-42 at spec.resp.body, and the moment it was recorded with stands in mock-44 at spec.resp.body, a recording record's full round gave the application before the call that was "+atCallWords) {
			t.Errorf("FALSE GREEN? the own place and another call's recording at the call's own instant (set %v): %s (%s); want it refused naming both", viaSet, ln.Kind, ln.Why)
		}
	}
}

// A recording record's full round gave the application before the call,
// made at the call's own instant, cannot be shown from the record to have
// been given to the recorded application before the call or after it, so it
// is read both ways and refused where either reading refuses: as an earlier
// place of the call's own test (one recorded a moment before the call), and
// as one made at or after the call, which only what F served there counts
// for. Handed as an earlier place, served in the test or before it, or
// unserved where the set shows F gave it, it gets the verdict of the
// stricter reading: 15:50:40 rounds to F's minute and cuts down to the
// recorded one, which an earlier place both runs were given shows no copy
// of, while what F alone was served there shows F's minute copied from it,
// so it is refused naming it and its instant, never noise. The same
// recording made a second before the call is read once, as an earlier place
// (controls), and the recorded minute alone stays noise where nothing names
// F's minute.
func TestAServedRecordingAtTheCallsOwnInstantIsReadBothWays(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	before := at.Add(-time.Minute)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	const (
		noise   = "noise"
		bound   = "variable"
		refused = "refused"
	)
	for _, tc := range []struct {
		name    string
		sent    time.Duration
		withOwn bool
		atCall  string // the verdict at the call's own instant
		before  string // the verdict a second before the call (control)
		why     string // what the refusal at the call's own instant says
	}{
		{"F's minute and more", 65 * time.Second, false, refused, refused, "mock-44 at spec.resp.body (" + atCallWords + ")"},
		{"F's minute and more, beside the own place", 65 * time.Second, true, refused, bound, "mock-44 at spec.resp.body holds, which record's full round gave the application in its place"},
		{"F's minute", 60 * time.Second, false, refused, refused, "mock-44 at spec.resp.body (" + atCallWords + ")"},
		{"rounds to F's minute, cuts down to the recorded one", 40 * time.Second, false, refused, noise, "stands in mock-44 at spec.resp.body, a recording record's full round gave the application before the call that was " + atCallWords + ", yet may as well have been made after the call, so keploy reads it both ways: as one made after the call, keploy follows no value of it but what record's full round served there, so it cannot tell"},
		{"rounds to F's minute, cuts down to the recorded one, beside the own place", 40 * time.Second, true, refused, refused, "mock-44 at spec.resp.body holds as the recording gave it (" + atCallWords + ")"},
		{"the recorded minute alone", 10 * time.Second, false, noise, noise, ""},
		{"the recorded minute alone, beside the own place", 10 * time.Second, true, refused, bound, "stands 1m0s after the moment mock-44 at spec.resp.body holds as the recording gave it (" + atCallWords + ")"},
	} {
		for _, hand := range []string{"served in the test", "served before the test", "unserved, the set showing F gave it"} {
			verdict := func(when time.Time) (string, string) {
				dc := NewDriftCache()
				m := sentAtReceive("mock-44", when, base.Add(tc.sent).Format(time.RFC3339))
				var nb Raw
				var given GivenEarlier
				switch hand {
				case "served in the test":
					nb = dc.RawOf(m, true).ServedAs(1)
				case "served before the test":
					nb = dc.RawOf(m, true)
				default:
					nb = dc.RawOf(m, false)
					given = func() ([]GivenPlaces, error) {
						return []GivenPlaces{PlacesOf(dc.RawOf(m, true).ServedAs(1))}, nil
					}
				}
				raw := []Raw{nb}
				if tc.withOwn {
					raw = append(raw, dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3))
				}
				ln := ClassifyGiven(dc, []string{p}, live, rec, at, nil, raw, nil, nil, given)[0]
				switch ln.Kind {
				case KindNoise:
					return noise, ln.Why
				case KindVariable:
					return bound, ln.Why
				case KindUnaccounted:
					return refused, ln.Why
				}
				return string(ln.Kind), ln.Why
			}
			if got, why := verdict(at.Add(-time.Second)); got != tc.before {
				t.Errorf("%s, %s, recorded a second before the call: %s (%s); want %s, read once as an earlier place", tc.name, hand, got, why, tc.before)
			}
			got, why := verdict(at)
			if got != tc.atCall || tc.why != "" && !strings.Contains(why, tc.why) {
				t.Errorf("FALSE GREEN? %s, %s, recorded at the call's own instant: %s (%s); want %s, the stricter of both readings: %q", tc.name, hand, got, why, tc.atCall, tc.why)
			}
			// Where the earlier place's reading binds the value (beside
			// the own place) and the late one refuses it, the value is
			// refused (fail closed), and the reason says where the first
			// reading would have bound it; elsewhere it names no binding.
			bindsAt := map[string]string{
				"F's minute and more, beside the own place":       "mock-42 at spec.resp.body",
				"the recorded minute alone, beside the own place": "mock-44 at spec.resp.body (" + atCallWords,
			}[tc.name]
			if says := strings.Contains(why, "; read as a recording made before the call, the value would be a variable bound at "+bindsAt) && strings.HasSuffix(why, ", yet keploy refuses a value either reading refuses"); says != (bindsAt != "") {
				t.Errorf("%s, %s, recorded at the call's own instant: %q; want it to name the earlier place's binding at %q: %v", tc.name, hand, why, bindsAt, bindsAt != "")
			}
		}
	}
	// The set cannot be read: an unserved recording at the call's own
	// instant may be one F gave, so it is read both ways, and the second
	// reading refuses with the set's error what reaches the set, where the
	// first binds the value to the own place without reading the set. A
	// value one reading binds and the other refuses is refused (fail
	// closed), and the reason says where the first would have bound it.
	dc := NewDriftCache()
	raw := []Raw{
		dc.RawOf(sentAtReceive("mock-44", at, base.Add(10*time.Second).Format(time.RFC3339)), false),
		dc.RawOf(sentAtReceive("mock-42", before, base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(3),
	}
	ln := ClassifyGiven(dc, []string{p}, live, rec, at, nil, raw, nil, nil, func() ([]GivenPlaces, error) {
		return nil, errors.New("disk gone")
	})[0]
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "disk gone") || !strings.Contains(ln.Why, "; read as a recording made before the call, the value would be a variable bound at mock-44 at ") || !strings.HasSuffix(ln.Why, ", yet keploy refuses a value either reading refuses") {
		t.Errorf("FALSE GREEN? the set unread, an unserved recording at the call's own instant beside the own place: %s (%s); want it refused with the set's error, saying the earlier place's reading binds it", ln.Kind, ln.Why)
	}
}

// leafedOf is m as an adapter that serves an exchange leaf by leaf hands it
// (Earlier, as keploy's HTTP learn step's earlierOf does): each leaf of its
// JSON request and answer, as F served it, with its variable place, and its
// payload, served as the n-th serve of F's in the set.
func leafedOf(m *models.Mock, own bool, n int) Earlier {
	e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: own, Payload: RawOf(m, true).ServedAs(n)}
	for _, part := range []struct {
		side Side
		body string
	}{{Request, m.Spec.HTTPReq.Body}, {Response, m.Spec.HTTPResp.Body}} {
		v, ok := DecodeJSON([]byte(part.body))
		if !ok {
			continue
		}
		leaves := map[string]Leaf{}
		JSONLeaves("body", v, leaves)
		for _, l := range leaves {
			place, _ := VarPlace(l, true)
			e.Seen = append(e.Seen, Seen{Side: part.side, Leaf: l, Live: l.Value, Place: place})
		}
	}
	return e
}

// An exchange F served leaf by leaf in the test before the call (Earlier)
// that was recorded after the call (later in the call's own test's window,
// or in a later test's) gave the recorded application nothing before the
// call: it is read as a recording made after the call is, which only what F
// served there counts for (the offset rule, the word rule and the echo of
// what F gave, never a variable's search), and one recorded at the call's
// own instant is read both ways, refused where either reading refuses, as
// the same recording handed as a Raw is. A copy of what F served there
// (15:51:05 cut to F's minute, 15:51:00, the minute verbatim, 15:50:40
// rounded to it), beside the own place or not, is refused naming the
// recording it was copied from, never learned as noise because its leaves
// were read as an earlier place both runs were given. The recorded minute
// alone (15:50:10) names no moment of F's, so it gets the verdict the same
// recording handed as a Raw gets.
func TestAnExchangeServedLeafByLeafIsReadByWhenItWasRecorded(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	for _, f := range []struct {
		name string
		sent string
		copy bool
	}{
		{"15:51:05, cut to F's minute", base.Add(65 * time.Second).Format(time.RFC3339), true},
		{"15:51:00, F's minute", base.Add(60 * time.Second).Format(time.RFC3339), true},
		{"the minute 15:51 verbatim", base.Add(time.Minute).Format(minute), true},
		{"15:50:40, rounded to F's minute", base.Add(40 * time.Second).Format(time.RFC3339), true},
		{"15:50:10, the recorded minute alone", base.Add(10 * time.Second).Format(time.RFC3339), false},
	} {
		for _, rc := range []struct {
			name string
			at   time.Time
			own  bool
		}{
			{"recorded later in the call's own test's window", at.Add(30 * time.Second), true},
			{"recorded in a later test's window", at.Add(2 * time.Minute), false},
			{"recorded at the call's own instant", at, true},
		} {
			for _, withOwn := range []bool{false, true} {
				for _, nbFirst := range []bool{true, false} {
					name := f.name + "/" + rc.name
					if withOwn {
						name += "/beside the own place"
					}
					if !nbFirst {
						name += "/served after it"
					}
					nbN, ownN := 1, 2
					if !nbFirst {
						nbN, ownN = 2, 1
					}
					nb := sentAtReceive("mock-44", rc.at, f.sent)
					var ownRaw []Raw
					if withOwn {
						ownRaw = []Raw{RawOf(sentAtReceive("mock-42", at.Add(-time.Second), base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(ownN)}
					}
					// The same recording handed as a Raw, as the adapters
					// hand one: recorded after the call, a place of no
					// test's call; at its own instant, an earlier place.
					twin := RawOf(nb, true).ServedAs(nbN)
					if rc.at.After(at) {
						twin = twin.OfAnotherTest()
					}
					asRaw := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, nil, append([]Raw{twin}, ownRaw...), nil, nil, nil)[0]
					got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{leafedOf(nb, rc.own, nbN)}, ownRaw, nil, nil, nil)[0]
					if f.copy && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "mock-44")) {
						t.Errorf("FALSE GREEN? %s: %s (%s); want it refused naming mock-44, whose answer F served before the call", name, got.Kind, got.Why)
					}
					if got.Kind != asRaw.Kind {
						t.Errorf("%s: %s (%s); the same recording handed as a Raw is %s (%s)", name, got.Kind, got.Why, asRaw.Kind, asRaw.Why)
					}
				}
			}
		}
	}
	// What F served there counts, not only what the recording holds: an
	// exchange recorded after the call whose answer F served with another
	// value than recorded at a leaf (a template value: 15:51:05 where the
	// recording holds 15:50:10) gave the application F's value, so F's
	// minute cut from it is refused naming the exchange, while the same
	// leaf served as recorded is the recorded minute alone.
	for _, servedAs := range []string{base.Add(65 * time.Second).Format(time.RFC3339), base.Add(10 * time.Second).Format(time.RFC3339)} {
		nb := sentAtReceive("mock-44", at.Add(30*time.Second), base.Add(10*time.Second).Format(time.RFC3339))
		e := leafedOf(nb, true, 1)
		for i := range e.Seen {
			if e.Seen[i].Leaf.Path == "body.Messages[0].SentAt" {
				e.Seen[i].Live = servedAs
			}
		}
		got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{e}, nil, nil, nil, nil)[0]
		copied := servedAs != base.Add(10*time.Second).Format(time.RFC3339)
		if copied && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "mock-44 at spec.resp.body.Messages[0].SentAt")) {
			t.Errorf("FALSE GREEN? F served %s at a leaf of an exchange recorded after the call that holds 15:50:10: %s (%s); want it refused naming the leaf F served it at", servedAs, got.Kind, got.Why)
		}
		if !copied && got.Kind != KindNoise {
			t.Errorf("the leaf served as recorded, 15:50:10, names no moment of F's: %s (%s); want noise", got.Kind, got.Why)
		}
	}
	// An exchange recorded after the call that came with no payload is a
	// recording whose text cannot be told: every drift is refused, naming
	// it and why, never read as nothing.
	e := leafedOf(sentAtReceive("mock-44", at.Add(30*time.Second), base.Add(10*time.Second).Format(time.RFC3339)), true, 1)
	e.Payload = Raw{}
	if got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{e}, nil, nil, nil, nil)[0]; got.Kind != KindUnaccounted || !strings.Contains(got.Why, "was given no payload of mock-44") {
		t.Errorf("FALSE GREEN? an exchange recorded after the call with no payload: %s (%s); want it refused, naming it", got.Kind, got.Why)
	}
}

// What F served at a leaf of an exchange it served leaf by leaf (a template
// value: 11:10:11 where the recording holds 11:12:00) is a place F gave the
// application, which the offset rule reads where the exchange's payload holds
// the leaf, as another recording's payload holds the recording's: a value
// built on it by an offset (11:15:11, five minutes after it, where the
// recording's was five minutes after the receive it gave the application,
// mock-43's 11:09:10) is refused, naming the leaf F served it at and the
// offset, wherever the exchange was recorded (later in the call's own test's
// window, in a later test's, at the call's own instant, or before the call),
// and at any index of the answer's list. The leaf served as recorded names
// no moment of F's: noise. A leaf F served as recorded is no place of F's
// apart from its payload's, so two recordings both runs gave the
// application in the recording's order stay no move.
func TestAnOffsetOnWhatFServedAtAnExchangesLeafIsRefused(t *testing.T) {
	at := r1Insert
	const p = "body.visible_after"
	rec, live := strLeaf(p, "2026-10-22T11:14:10Z"), strLeaf(p, "2026-10-22T11:15:11Z")
	const recorded, template = "2026-10-22T11:12:00Z", "2026-10-22T11:10:11Z"
	for _, idx := range []int{0, 1} {
		sent := func(v string) []string {
			if idx == 0 {
				return []string{v}
			}
			return []string{"2026-10-22T11:00:00Z", v}
		}
		m43 := sentAtReceive("mock-43", at.Add(-2*time.Second), sent("2026-10-22T11:09:10Z")...)
		leaf := "body.Messages[" + strconv.Itoa(idx) + "].SentAt"
		for _, rc := range []struct {
			name string
			at   time.Time
			own  bool
		}{
			{"recorded later in the call's own test's window", at.Add(30 * time.Second), true},
			{"recorded in a later test's window", at.Add(2 * time.Minute), false},
			{"recorded at the call's own instant", at, true},
			{"recorded before the call", at.Add(-time.Second), true},
		} {
			for _, servedAs := range []string{template, recorded} {
				e := leafedOf(sentAtReceive("mock-44", rc.at, sent(recorded)...), rc.own, 1)
				for i := range e.Seen {
					if e.Seen[i].Leaf.Path == leaf {
						e.Seen[i].Live = servedAs
					}
				}
				name := rc.name + ", " + leaf + " served " + servedAs
				got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{e}, []Raw{RawOf(m43, false)}, nil, nil, nil)[0]
				if servedAs == recorded {
					if got.Kind != KindNoise {
						t.Errorf("%s: %s (%s); want noise: the leaf served as recorded names no moment of F's", name, got.Kind, got.Why)
					}
					continue
				}
				if got.Kind != KindUnaccounted ||
					!strings.Contains(got.Why, "stands 5m0s after the moment mock-43 at spec.resp.body holds as the recording gave it") ||
					!strings.Contains(got.Why, "5m0s after the moment mock-44 at spec.resp."+leaf+" holds, which record's full round gave the application in its place") {
					t.Errorf("FALSE GREEN? %s: %s (%s); want it refused, built on what F served at mock-44's leaf by an offset", name, got.Kind, got.Why)
				}
			}
		}
	}
	m43 := RawOf(sentAtReceive("mock-43", at.Add(-3*time.Second), "2026-10-22T11:09:10Z"), true).ServedAs(1)
	e := leafedOf(sentAtReceive("mock-44", at.Add(-time.Second), template), true, 2)
	if got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{e}, []Raw{m43}, nil, nil, nil)[0]; got.Kind != KindNoise {
		t.Errorf("two recordings both runs gave in the recording's order, the later one's leaf served as recorded: %s (%s); want noise", got.Kind, got.Why)
	}
	// What F served at the leaf is no place the recording gave the
	// application: the value 3m59s after it as recorded, and 3m59s after
	// what F served before the call at mock-43 (11:11:12) as F saw it, moved
	// by the lead and is built on no place both ways: noise.
	// Served first, or in an order keploy cannot tell (no pair of the two
	// is then let go as one both runs gave in the recording's order).
	m43 = RawOf(sentAtReceive("mock-43", at.Add(-3*time.Second), "2026-10-22T11:11:12Z"), true)
	for _, n43 := range []int{1, 0} {
		x43 := m43
		if n43 > 0 {
			x43 = m43.ServedAs(n43)
		}
		for _, when := range []time.Duration{30 * time.Second, -time.Second} {
			e = leafedOf(sentAtReceive("mock-44", at.Add(when), recorded), true, 2)
			for i := range e.Seen {
				if e.Seen[i].Leaf.Path == "body.Messages[0].SentAt" {
					e.Seen[i].Live = template
				}
			}
			if got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{e}, []Raw{x43}, nil, nil, nil)[0]; got.Kind != KindNoise {
				t.Errorf("mock-43 served as serve %d, mock-44 recorded %v from the call, its leaf served %s: %s (%s); want noise: what F served there is no place the recording gave the application", n43, when, template, got.Kind, got.Why)
			}
		}
	}
}

// An exchange F served leaf by leaf in the test before the call whose
// recorded time keploy cannot read (the zero time) cannot be shown, from the
// record, to have been given to the recorded application before the call or
// after it: it is read both ways, as one made at the call's own instant is,
// and refused where either reading refuses. A minute F's run rounded from
// what F served there (15:50:40 to 15:51, recorded 15:50) names both moments
// read as an earlier place, yet copies what F served read as one made after
// the call: refused, naming the exchange and saying keploy cannot read when
// it was recorded, beside the own place or not. What names the recorded
// minute alone (15:50:10) is noise. Each verdict is the one the same exchange
// recorded at the call's own instant gets.
func TestAnExchangeWithNoRecordedTimeIsReadBothWays(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	for _, f := range []struct {
		name string
		sent string
		copy bool
	}{
		{"15:50:40, rounded to F's minute", base.Add(40 * time.Second).Format(time.RFC3339), true},
		{"15:51:05, cut to F's minute", base.Add(65 * time.Second).Format(time.RFC3339), true},
		{"15:50:10, the recorded minute alone", base.Add(10 * time.Second).Format(time.RFC3339), false},
	} {
		for _, withOwn := range []bool{false, true} {
			name := f.name
			var ownRaw []Raw
			if withOwn {
				name += ", beside the own place"
				ownRaw = []Raw{RawOf(sentAtReceive("mock-42", at.Add(-time.Second), base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(2)}
			}
			nb := sentAtReceive("mock-44", time.Time{}, f.sent)
			got := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{leafedOf(nb, true, 1)}, ownRaw, nil, nil, nil)[0]
			twin := ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, []Earlier{leafedOf(sentAtReceive("mock-44", at, f.sent), true, 1)}, ownRaw, nil, nil, nil)[0]
			if got.Kind != twin.Kind {
				t.Errorf("%s: %s (%s); the same exchange recorded at the call's own instant is %s (%s)", name, got.Kind, got.Why, twin.Kind, twin.Why)
			}
			if !f.copy {
				if !withOwn && got.Kind != KindNoise {
					t.Errorf("%s: %s (%s); want noise", name, got.Kind, got.Why)
				}
				// Beside the own place, the offset rule reads it as one
				// the recording may have given before the call, and says
				// why.
				if withOwn && !strings.Contains(got.Why, "mock-44 at spec.resp.body holds as the recording gave it (recorded at a time keploy cannot read, so keploy reads it as given to the application before the call)") {
					t.Errorf("%s: %s (%s); want the reason to say keploy cannot read when mock-44 was recorded", name, got.Kind, got.Why)
				}
				continue
			}
			if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "mock-44") {
				t.Errorf("FALSE GREEN? %s: %s (%s); want it refused, naming mock-44", name, got.Kind, got.Why)
			}
			// Read as an earlier place, 15:50:40 names both moments: only
			// the reading as one made after the call refuses it, and says
			// why.
			if f.sent == base.Add(40*time.Second).Format(time.RFC3339) && !withOwn && !strings.Contains(got.Why, "recorded at a time keploy cannot read, so keploy reads it as given to the application before the call, yet may as well have been made after the call, so keploy reads it both ways") {
				t.Errorf("%s: %s; want the reason to say keploy cannot read when mock-44 was recorded, so it reads it both ways", name, got.Why)
			}
		}
	}
}

// A recording F served in the test before the call whose recorded time
// keploy cannot read (the zero time) is read both ways, as an exchange served
// leaf by leaf is (TestAnExchangeWithNoRecordedTimeIsReadBothWays): the
// record cannot show it was given to the recorded application before the
// call or after it, so it is read as one made at the call's own instant, and
// refused where either reading refuses. A minute F's run rounded from what F
// served there (15:50:40 to 15:51, recorded 15:50) names both moments read
// as an earlier place, yet copies what F served read as one made after the
// call: refused, naming the recording and saying keploy cannot read when it
// was recorded, beside the own place or not. What names the recorded minute
// alone (15:50:10) is noise. Each verdict is the one the same recording made
// at the call's own instant gets, served or not.
func TestARecordingWithNoRecordedTimeIsReadBothWays(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := strLeaf(p, recT.Format(minute)), strLeaf(p, recT.Add(time.Minute).Format(minute))
	base := recT.Truncate(time.Minute)
	for _, f := range []struct {
		name string
		sent string
		copy bool
	}{
		{"15:50:40, rounded to F's minute", base.Add(40 * time.Second).Format(time.RFC3339), true},
		{"15:51:05, cut to F's minute", base.Add(65 * time.Second).Format(time.RFC3339), true},
		{"15:50:10, the recorded minute alone", base.Add(10 * time.Second).Format(time.RFC3339), false},
	} {
		for _, mode := range []string{"served", "not served", "not served in the test, given earlier in the set"} {
			served := mode == "served"
			for _, withOwn := range []bool{false, true} {
				name := f.name + ", " + mode
				var ownRaw []Raw
				if withOwn {
					name += ", beside the own place"
					ownRaw = []Raw{RawOf(sentAtReceive("mock-42", at.Add(-time.Second), base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(2)}
				}
				nb := func(rat time.Time) []Raw {
					x := RawOf(sentAtReceive("mock-44", rat, f.sent), served)
					if served {
						x = x.ServedAs(1)
					}
					return append([]Raw{x}, ownRaw...)
				}
				read := func(rat time.Time) Learned {
					var given GivenEarlier
					if mode != "served" && mode != "not served" {
						given = func() ([]GivenPlaces, error) {
							return []GivenPlaces{PlacesOf(RawOf(sentAtReceive("mock-44", rat, f.sent), true).ServedAs(1))}, nil
						}
					}
					return ClassifyGiven(NewDriftCache(), []string{p}, live, rec, at, nil, nb(rat), nil, nil, given)[0]
				}
				got, twin := read(time.Time{}), read(at)
				if got.Kind != twin.Kind {
					t.Errorf("%s: %s (%s); the same recording made at the call's own instant is %s (%s)", name, got.Kind, got.Why, twin.Kind, twin.Why)
				}
				if !served {
					continue
				}
				if !f.copy {
					if !withOwn && got.Kind != KindNoise {
						t.Errorf("%s: %s (%s); want noise", name, got.Kind, got.Why)
					}
					continue
				}
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "mock-44") {
					t.Errorf("FALSE GREEN? %s: %s (%s); want it refused, naming mock-44", name, got.Kind, got.Why)
				}
				// Read as an earlier place, 15:50:40 names both moments:
				// only the reading as one made after the call refuses it,
				// and says why.
				if f.sent == base.Add(40*time.Second).Format(time.RFC3339) && !withOwn && !strings.Contains(got.Why, "recorded at a time keploy cannot read, so keploy reads it as given to the application before the call, yet may as well have been made after the call, so keploy reads it both ways") {
					t.Errorf("%s: %s; want the reason to say keploy cannot read when mock-44 was recorded, so it reads it both ways", name, got.Why)
				}
			}
		}
	}
}
