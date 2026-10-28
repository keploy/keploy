package learn

import (
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// InstantLeaf writes an instant in UTC, to its resolution, in the coarsest
// form that writes it (RFC 3339 with the fraction digits its resolution
// needs, its time to the minute, its date), and InstantOf reads back what it
// wrote, and nothing else: a String that reads as a date is no instant (only
// a decoder types a leaf one), nor is a text no form writes. An instant of a
// year no form writes is still an Instant, which InstantOf does not read.
func TestAnInstantLeafIsItsInstant(t *testing.T) {
	ms := time.UnixMilli(1792726810740)
	ist := time.FixedZone("IST", 5*3600+1800)
	for _, row := range []struct {
		name string
		at   time.Time
		res  time.Duration
		text string
		back time.Duration
	}{
		{"a BSON datetime", ms, time.Millisecond, "2026-10-23T03:40:10.740Z", time.Millisecond},
		{"a BSON timestamp's second", ms, time.Second, "2026-10-23T03:40:10Z", time.Second},
		{"a microsecond", ms.Add(5 * time.Microsecond), time.Microsecond, "2026-10-23T03:40:10.740005Z", time.Microsecond},
		{"a nanosecond", ms.Add(7), time.Nanosecond, "2026-10-23T03:40:10.740000007Z", time.Nanosecond},
		{"no resolution: to the nanosecond", ms.Add(7), 0, "2026-10-23T03:40:10.740000007Z", time.Nanosecond},
		{"a hundredth: truncated to it, written to the millisecond", ms.Add(9 * time.Millisecond), 10 * time.Millisecond, "2026-10-23T03:40:10.740Z", time.Millisecond},
		{"a minute", ms, time.Minute, "2026-10-23T03:40Z", time.Minute},
		{"an hour: truncated to it, written to the minute", ms, time.Hour, "2026-10-23T03:00Z", time.Minute},
		{"a date", ms, 24 * time.Hour, "2026-10-23", 24 * time.Hour},
		{"a zoned time: in UTC", ms.In(ist), time.Millisecond, "2026-10-23T03:40:10.740Z", time.Millisecond},
		{"year 0", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Second, "0000-01-01T00:00:00Z", time.Second},
	} {
		l := InstantLeaf("body.at", "body.at", row.at, row.res)
		if l.Type != Instant || l.Value != row.text || l.Path != "body.at" || l.Noise != "body.at" {
			t.Errorf("%s: InstantLeaf = %+v; want an Instant %q", row.name, l, row.text)
			continue
		}
		want := row.at
		if row.res > 0 {
			want = want.Truncate(row.res)
		}
		got, res, ok := InstantOf(l)
		if !ok || res != row.back || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%s: InstantOf = %v, %v, %v; want %v at %v", row.name, got, res, ok, want.UTC(), row.back)
		}
	}
	for _, at := range []time.Time{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(58000, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)} {
		l := InstantLeaf("body.at", "body.at", at, time.Millisecond)
		if _, _, ok := InstantOf(l); l.Type != Instant || ok {
			t.Errorf("year %d: %+v (read: %v); want an Instant InstantOf does not read", at.Year(), l, ok)
		}
	}
	for _, l := range []Leaf{
		{Type: String, Value: "2026-10-23T03:40:10.740Z"},
		{Type: Instant, Value: "2026-10-23T03:40:10.74Z"},
		{Type: Instant, Value: "2026-10-23T03:40:10.740+00:00"},
		{Type: Instant, Value: "2026-10-23 03:40:10.740Z"},
		{Type: Instant, Value: "1792726810740"},
		{Type: Instant, Value: "2026-10-23T03:40:10.Z"},
		{Type: Instant, Value: "2026-13-23T03:40:10Z"},
		{Type: Instant, Value: "2026-10-23T03:40:10.7Z"},
	} {
		if at, _, ok := InstantOf(l); ok {
			t.Errorf("InstantOf(%+v) = %v; want none", l, at)
		}
	}
}

// The queue consumer's lane, leg (b), query build, run 7 (keploy e3d67a7e4):
// the recordings made before the boot insert (mock-8), as keploy wrote them.
// The delivering receive (mock-4), the handshake on the insert's new
// connection (mock-5, its localTime 3 ms before processed_at), the
// duplicate read (mock-6), post-url-1's upsert (mock-7) and the monitor's
// awaitable hello, interrupted as the app stopped (mock-15), whose reply's
// requestId is 40.
var (
	qnInsert = time.Date(2026, 10, 23, 3, 40, 10, 740913467, time.UTC)
	qnRec    = time.UnixMilli(1792726810740).UTC()
	qnLive   = time.UnixMilli(1792726870942).UTC()
)

func qnRecordings() []*models.Mock {
	hello := r2Hello("mock-5", 8, time.Date(2026, 10, 23, 3, 40, 10, 489040231, time.UTC), time.Date(2026, 10, 23, 3, 40, 10, 737656653, time.UTC), 1792726810737)
	read := r2Mongo("mock-6", time.Date(2026, 10, 23, 3, 40, 10, 454769765, time.UTC), time.Date(2026, 10, 23, 3, 40, 10, 739678118, time.UTC),
		[]string{`{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$match":{"event":"S12 message"}},{"$group":{"_id":{"$numberInt":"1"},"n":{"$sum":{"$numberInt":"1"}}}}],"cursor":{},"lsid":{"id":{"$binary":{"base64":"Yt9CVBS2Q5Czthm/2mCc1w==","subType":"04"}}},"$db":"keploy"} }`},
		`{ SectionSingle msg: {"cursor":{"firstBatch":[],"id":{"$numberLong":"0"},"ns":"keploy.events"},"ok":{"$numberDouble":"1.0"}} }`)
	upsert := r2Mongo("mock-7", time.Date(2026, 10, 23, 3, 40, 10, 737736823, time.UTC), time.Date(2026, 10, 23, 3, 40, 11, 416321932, time.UTC),
		[]string{`{ SectionSingle msg: {"update":"url-shortener","ordered":true,"writeConcern":{"w":"majority"},"lsid":{"id":{"$binary":{"base64":"UfxXozy7QZ2wuojZVVscZQ==","subType":"04"}}},"$db":"keploy"} }`,
			`{ SectionSingle identifier: documents , msgs: [ {"q":{"_id":"Lhr4BWAi"},"u":{"$set":{"_id":"Lhr4BWAi","created":{"$date":{"$numberLong":"1792726810486"}},"updated":{"$date":{"$numberLong":"1792726810486"}},"url":"https://google.com"}},"upsert":true} ] }`},
		`{ SectionSingle msg: {"n":{"$numberInt":"1"},"upserted":[{"index":{"$numberInt":"0"},"_id":"Lhr4BWAi"}],"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`)
	monitor := &models.Mock{Name: "mock-15", Kind: models.Mongo, ConnectionID: "0", Spec: models.MockSpec{
		Metadata: map[string]string{"type": "config", "connID": "0"},
		MongoRequests: []models.MongoRequest{{
			Header:  &models.MongoHeader{Length: 148, RequestID: 4, Opcode: wiremessage.OpMsg},
			Message: &models.MongoOpMessage{FlagBits: 65536, Sections: []string{`{ SectionSingle msg: {"hello":{"$numberInt":"1"},"helloOk":true,"topologyVersion":{"processId":{"$oid":"6adad70fb0f8b7545e1ef46d"},"counter":{"$numberLong":"0"}},"maxAwaitTimeMS":{"$numberLong":"10000"},"$db":"admin"} }`}},
		}},
		MongoResponses: []models.MongoResponse{{
			Header:  &models.MongoHeader{Length: 117, RequestID: 40, ResponseTo: 4, Opcode: wiremessage.OpMsg},
			Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":{"$numberDouble":"0.0"},"errmsg":"operation was interrupted","code":{"$numberInt":"279"},"codeName":"ClientDisconnect"} }`}},
		}},
		ReqTimestampMock: time.Date(2026, 10, 23, 3, 40, 7, 366530913, time.UTC),
		ResTimestampMock: time.Date(2026, 10, 23, 3, 40, 11, 788152063, time.UTC),
	}}
	return []*models.Mock{qnReceive(), hello, read, upsert, monitor}
}

func qnReceive() *models.Mock {
	return &models.Mock{Name: "mock-4", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"connID": "6", "operation": "POST", "type": "HTTP_CLIENT", "lifetime": "perTest"},
		HTTPReq: &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "/000000000000/localstack-queue/",
			Header: map[string]string{
				"Accept-Encoding":       "gzip",
				"Amz-Sdk-Invocation-Id": "75192eb5-2118-4d4a-9078-44304c17211e",
				"Amz-Sdk-Request":       "attempt=1; max=3",
				"Authorization":         "AWS4-HMAC-SHA256 Credential=dummy/20261023/us-east-1/sqs/aws4_request, SignedHeaders=amz-sdk-invocation-id;amz-sdk-request;content-length;content-type;host;x-amz-date, Signature=c211c323d2e89bda53d9f0d1d12018bda5771af12c2485cf3681a2aa64c39aed",
				"Content-Length":        "185",
				"Content-Type":          "application/x-www-form-urlencoded",
				"Host":                  "sqs.us-east-1.localhost.localstack.cloud:4566",
				"User-Agent":            "aws-sdk-go-v2/1.22.1 os/linux lang/go#1.27.0 md/GOOS#linux md/GOARCH#amd64 api/sqs#1.26.0",
				"X-Amz-Date":            "20261023T034007Z",
			},
			Body: "Action=ReceiveMessage&MaxNumberOfMessages=10&QueueUrl=http%3A%2F%2Fsqs.us-east-1.localhost.localstack.cloud%3A4566%2F000000000000%2Flocalstack-queue&Version=2012-11-05&WaitTimeSeconds=4"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{
			"Content-Length": "660",
			"Content-Type":   "text/xml",
			"Date":           "Fri, 23 Oct 2026 03:40:10 GMT",
			"Server":         "hypercorn-h11",
		}, Body: `<?xml version='1.0' encoding='utf-8'?>` + "\n" + `<ReceiveMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/"><ReceiveMessageResult><Message><MessageId>177cfea3-10ab-4cd4-87a1-9b27513d25dd</MessageId><ReceiptHandle>NzM1M2E5NDAtZTU4My00YmNjLWExNjMtMWZmOGQxMWU5Y2Y2IGFybjphd3M6c3FzOnVzLWVhc3QtMTowMDAwMDAwMDAwMDA6bG9jYWxzdGFjay1xdWV1ZSAxNzdjZmVhMy0xMGFiLTRjZDQtODdhMS05YjI3NTEzZDI1ZGQgMTc5MjcyNjgxMC40NDgyMTk1</ReceiptHandle><MD5OfBody>5d9bee94789228e620151756cc9c5e8e</MD5OfBody><Body>S12 message</Body></Message></ReceiveMessageResult><ResponseMetadata><RequestId>cb7f9776-c5e4-4f67-8f1f-69cbce7f5857</RequestId></ResponseMetadata></ReceiveMessageResponse>`},
		ReqTimestampMock: time.Date(2026, 10, 23, 3, 40, 7, 385568144, time.UTC),
		ResTimestampMock: time.Date(2026, 10, 23, 3, 40, 10, 453275884, time.UTC),
	}}
}

// qnWindow is the boot insert's call window in both runs (Window): from the
// duplicate read's request to the insert's, and F's around the moment it saw.
func qnWindow() *Window {
	return &Window{Rec: Span{From: time.Date(2026, 10, 23, 3, 40, 10, 454769765, time.UTC), To: qnInsert}, F: Span{From: qnLive.Add(-300 * time.Millisecond), To: qnLive.Add(time.Millisecond)}}
}

// classifyTyped classifies the boot insert's processed_at, recorded rec and
// live in F, as one leaf (r, l), at qnInsert, against raw (recorded before
// it) and the exchanges F served (earlier), with the call's recorded answer
// resp, in w.
func classifyTyped(r, l Leaf, w *Window, earlier []Earlier, resp []Leaf, raw ...*models.Mock) Learned {
	dc := clockedDrifts()
	var rs []Raw
	for _, m := range raw {
		rs = append(rs, dc.RawOf(m, false))
	}
	p := r.Path
	return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: l}}, Shape{Leaves: map[string]Leaf{p: r}}, qnInsert, earlier, rs, resp, w)[0]
}

const qnPath = "body.docs.msgs.0.processed_at.$date.$numberLong"

func qnTyped(at time.Time) Leaf {
	return InstantLeaf(qnPath, "body.docs.msgs.0.processed_at", at, time.Millisecond)
}

// The boot insert's processed_at, a BSON datetime its decoder hands the learn
// step whole, is compared whole: its minute 40 is no word, so the monitor's
// reply's requestId of 40 (mock-15), which refused the insert's pairing in
// run 7, holds nothing of it. In its call's window in both runs, or moved by
// the lead, it is a read of the clock: noise. Shown by neither (the boot
// insert at record and in F, 60.2 s apart, as run 7 had it), it is refused,
// saying that nothing shows a read of the clock and naming no recording, with
// mock-15 or without: an instant read whole is noise only by its provenance.
// The same text as a String (the date a string field holds) is read word by
// word as before: noise in its window; outside it refused, naming mock-15 at
// its requestId, and, without mock-15, noise (no earlier place holds a word
// of it).
func TestATypedInstantIsComparedWhole(t *testing.T) {
	raw := qnRecordings()
	noMonitor := raw[:len(raw)-1]
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, nil, raw...); !noiseOrRead(ln) {
		t.Errorf("typed, in its window: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	lead := qnRec.Add(FullRoundLead + 202*time.Millisecond)
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(lead), nil, nil, nil, raw...); !noiseOrRead(ln) {
		t.Errorf("typed, moved by the lead: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	for name, rs := range map[string][]*models.Mock{"with mock-15": raw, "without mock-15": noMonitor} {
		ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), nil, nil, nil, rs...)
		if !plainRead(ln) || ln.From.Mock != "" {
			t.Errorf("typed, no provenance, %s: %s (from %q, %s); want a plain read of the clock, with no call window, checked at replay, naming no recording", name, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
	// A window F's moment falls outside of shows no read either.
	w := qnWindow()
	w.F.To = qnLive.Add(-time.Millisecond)
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), w, nil, nil, raw...); ln.Kind != KindUnaccounted {
		t.Errorf("typed, read after its window in F: %s (%s); want refused", ln.Kind, ln.Why)
	}
	str := func(at time.Time) Leaf {
		return Leaf{Path: qnPath, Noise: "body.docs.msgs.0.processed_at", Type: String, Value: at.Format(time.RFC3339Nano)}
	}
	ln := classifyTyped(str(qnRec), str(qnLive), nil, nil, nil, raw...)
	if ln.Kind == KindNoise || ln.From.Mock != "mock-15" && !strings.Contains(ln.Why, "mock-15") {
		t.Errorf("a date-like string, no provenance: %s (from %q, %s); want refused for mock-15's requestId", ln.Kind, ln.From.Mock, ln.Why)
	}
	if ln := classifyTyped(str(qnRec), str(qnLive), nil, nil, nil, noMonitor...); ln.Kind != KindClockRead || ln.Read == nil {
		t.Errorf("a date-like string, no provenance, without mock-15: %s (from %q, %s); want a read of the clock: one instant whole, with no call window, compared at replay", ln.Kind, ln.From.Mock, ln.Why)
	}
	if ln := classifyTyped(str(qnRec), str(qnLive), qnWindow(), nil, nil, raw...); !noiseOrRead(ln) {
		t.Errorf("a date-like string, in its window: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
}

// A typed instant equal to one an earlier payload holds, exactly, in any
// layout and form, is a variable of that payload, read in its window or not:
// an SQS message's SentTimestamp (epoch milliseconds), an HTTP answer's ISO
// time in another zone, one percent-encoded. The handshake's localTime of
// that millisecond, its answer's own clock read (rule 2), is no such place:
// in the window it stays noise; the same reply recorded as a data-plane
// exchange is followed. An HTTP answer's Date header within the span keploy
// stamped the exchange in is its server's stamp (rule 3), no place a
// second-resolution instant (a BSON timestamp) came from; one outside its
// span is followed. A payload holding the second only, or its time of day
// only, holds another moment.
func TestATypedInstantEqualToAnEarlierOneIsAVariable(t *testing.T) {
	asked, answered := qnRec.Add(-300*time.Millisecond), qnRec.Add(time.Millisecond)
	answer := func(body string) *models.Mock {
		return &models.Mock{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/events/next"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: body},
			ReqTimestampMock: asked, ResTimestampMock: answered,
		}}
	}
	hello := r2Hello("mock-5", 8, qnRec.Add(-2*time.Millisecond), qnRec.Add(time.Millisecond), qnRec.UnixMilli())
	data := r2Hello("mock-5", 8, qnRec.Add(-2*time.Millisecond), qnRec.Add(time.Millisecond), qnRec.UnixMilli())
	delete(data.Spec.Metadata, "type")
	sec := qnRec.Truncate(time.Second)
	stamped := func(asked, answered time.Time) *models.Mock {
		m := answer(`{"ok":true}`)
		m.Spec.HTTPResp.Header["Date"] = "Fri, 23 Oct 2026 03:40:10 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = asked, answered
		return m
	}
	for _, row := range []struct {
		name  string
		m     *models.Mock
		res   time.Duration
		from  bool
		field string
	}{
		{"its epoch milliseconds", answer(`{"Attributes":{"SentTimestamp":"1792726810740"}}`), time.Millisecond, true, "spec.resp.body"},
		{"its moment in another zone", answer(`{"sent":"2026-10-23T09:10:10.740+05:30"}`), time.Millisecond, true, "spec.resp.body"},
		{"its moment percent-encoded", answer(`{"next":"/events?after=2026-10-23T03%3A40%3A10.740Z"}`), time.Millisecond, true, "spec.resp.body"},
		{"its second only", answer(`{"sent":"2026-10-23T03:40:10Z"}`), time.Millisecond, false, ""},
		{"its time of day only", answer(`{"slot":"03:40:10.740"}`), time.Millisecond, false, ""},
		{"a handshake's localTime of that millisecond", hello, time.Millisecond, false, ""},
		{"that reply as a data-plane exchange", data, time.Millisecond, true, "spec.responses[0].message.documents[0]"},
		{"a Date header within its span, of its second", stamped(sec.Add(-time.Second), sec.Add(time.Second)), time.Second, false, ""},
		{"a Date header outside its span, of its second", stamped(sec.Add(-6*time.Second), sec.Add(-5*time.Second)), time.Second, true, "spec.resp.header.Date"},
	} {
		leaf := func(at time.Time) Leaf { return InstantLeaf(qnPath, "body.docs.msgs.0.processed_at", at, row.res) }
		for wname, w := range map[string]*Window{"in its window": qnWindow(), "no window": nil} {
			ln := classifyTyped(leaf(qnRec), leaf(qnLive), w, nil, nil, row.m)
			switch {
			case row.from:
				if ln.Kind != KindVariable || ln.From.Mock != row.m.Name || ln.From.Field != row.field || ln.From.Echo || !strings.Contains(ln.Why, row.m.Name+" at "+row.field) || strings.Contains(ln.Why, "1792726810740") {
					t.Errorf("%s, %s: %s (from %+v, %s); want a variable of %s at %s, naming no value", row.name, wname, ln.Kind, ln.From, ln.Why, row.m.Name, row.field)
				}
			case w != nil:
				if !noiseOrRead(ln) {
					t.Errorf("%s, %s: %s (from %q, %s); want noise", row.name, wname, ln.Kind, ln.From.Mock, ln.Why)
				}
			default:
				if !plainRead(ln) || ln.From.Mock != "" {
					t.Errorf("%s, %s: %s (from %q, %s); want a plain read of the clock, with no call window, checked at replay", row.name, wname, ln.Kind, ln.From.Mock, ln.Why)
				}
			}
		}
	}
	// A payload recorded after the insert is no earlier place.
	late := answer(`{"Attributes":{"SentTimestamp":"1792726810740"}}`)
	late.Spec.ReqTimestampMock = qnInsert.Add(time.Second)
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, nil, late); !noiseOrRead(ln) {
		t.Errorf("a payload recorded after the insert: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
}

// A typed departure the app picks from a timetable it was served, by its
// clock, stays checked whole: the moment F saw is a departure the timetable
// lists (its time of day, in any form), so the echo reads it before the lead
// or the window excuses it, and the pick is a variable of the departure
// listing the moment it was recorded with, in the timetable served leaf by
// leaf or read as a payload. A pick only F's timetable lists is refused,
// naming where F's moment stands.
func TestATypedPickFromAServedTimetableStaysChecked(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 20, 0, time.UTC)
	rec, live := time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC), time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC)
	leaf := func(t time.Time) Leaf { return InstantLeaf("body.departure", "body.departure", t, time.Second) }
	classify := func(w *Window, earlier []Earlier, raw ...*models.Mock) Learned {
		dc := clockedDrifts()
		var rs []Raw
		for _, m := range raw {
			rs = append(rs, dc.RawOf(m, true))
		}
		p := "body.departure"
		return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(live)}}, Shape{Leaves: map[string]Leaf{p: leaf(rec)}}, at, earlier, rs, nil, w)[0]
	}
	w := &Window{Rec: Span{From: rec.Add(-time.Second), To: at}, F: Span{From: live.Add(-time.Second), To: live.Add(time.Second)}}
	timetable := func(slots ...string) *models.Mock {
		return &models.Mock{Name: "mock-2", Kind: models.Mongo, Spec: models.MockSpec{
			MongoResponses:   []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[{"at":"` + slots[0] + `"},{"at":"` + slots[1] + `"}]},"ok":1} }`}}}},
			ReqTimestampMock: at.Add(-time.Second), ResTimestampMock: at.Add(-time.Second),
		}}
	}
	for wname, w := range map[string]*Window{"in its window": w, "no window": nil} {
		for _, form := range [][3]string{{"11:13", "11:14", "11:15"}, {"11%3A13", "11%3A14", "11%3A15"}} {
			ln := classify(w, servedTimetable(at.Add(-2*time.Second), form[:]...))
			if ln.Kind != KindVariable || ln.From.Mock != "mock-0" || ln.From.Path != "body.departures[1]" || !ln.From.Echo || ln.From.Side != Response {
				t.Errorf("served leaf by leaf (%s), %s: %s (from %+v, %s); want a variable of mock-0's departure 11:14", form[1], wname, ln.Kind, ln.From, ln.Why)
			}
		}
		for _, slots := range [][]string{{"2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}, {"2026-10-22T11%3A14%3A00Z", "2026-10-22T11%3A15%3A00Z"}} {
			ln := classify(w, nil, timetable(slots...))
			if ln.Kind != KindVariable || ln.From.Mock != "mock-2" || !ln.From.Echo || !strings.Contains(ln.Why, "a pick from what the application was given") {
				t.Errorf("read as a payload (%s), %s: %s (from %+v, %s); want a variable of mock-2, by the echo", slots[0], wname, ln.Kind, ln.From, ln.Why)
			}
			// The field that holds it as it stands is named; one that holds
			// it only in another form (percent-encoded) is not.
			if plain := !strings.Contains(slots[0], "%"); plain != (ln.From.Field == "spec.responses[0].message.sections[0]") {
				t.Errorf("read as a payload (%s), %s: the field %q", slots[0], wname, ln.From.Field)
			}
		}
		ln := classify(w, servedTimetable(at.Add(-2*time.Second), "11:15", "11:16"))
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in mock-0 at body.departures[0]") {
			t.Errorf("a pick only F's timetable lists, %s: %s (%s); want refused, naming where F's moment stands", wname, ln.Kind, ln.Why)
		}
	}
}

// A typed instant came from a leaf of an exchange F served, whatever its
// provenance: one that was, as recorded, that instant (in any layout and
// form: an equal instant is followed), or that held the moment it was
// recorded with and the moment F saw, both ways, neither on its other side
// (rule 1); and, as a payload holding it is, one that holds the moment it was
// recorded with, exactly, as recorded, whatever F saw there, a leaf F saw as
// recorded too (instantLeafFrom). Whole, of the place, only where its text
// is the instant's. One that held another recorded moment is no source.
func TestATypedInstantThatDriftedWithAServedLeafIsAVariable(t *testing.T) {
	sent := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-1", At: qnInsert.Add(-time.Second), Payload: payloadOf("mock-1", qnInsert.Add(-time.Second)), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "header.x-sent-at", Noise: "header.x-sent-at", Type: String, Value: rec}, Live: live},
		}}}
	}
	// held: the leaf did not drift with the instant, and only holds the
	// moment it was recorded with as recorded (instantLeafFrom), which the
	// variable's evidence says; every other source drifted with it, or was
	// it as recorded (instantServedFrom), and needs none.
	for _, row := range []struct {
		name      string
		rec, live string
		source    bool
		whole     bool
		held      bool
	}{
		{"its epoch milliseconds", "1792726810740", "1792726870942", true, false, false},
		{"its text", "2026-10-23T03:40:10.740Z", "2026-10-23T03:41:10.942Z", true, true, false},
		{"Go's text of it", "2026-10-23T03:40:10.74Z", "2026-10-23T03:41:10.942Z", true, false, false},
		{"it percent-encoded", "2026-10-23T03%3A40%3A10.740Z", "2026-10-23T03%3A41%3A10.942Z", true, false, false},
		{"Go's text of it, F another moment", "2026-10-23T03:40:10.74Z", "2026-10-23T03:41:10.000Z", true, false, false},
		{"it percent-encoded, F another text", "2026-10-23T03%3A40%3A10.740Z", "none", true, false, false},
		{"its moments inside a longer text", "sent 1792726810740 by sqs", "sent 1792726870942 by sqs", true, false, false},
		{"its moments percent-encoded inside a longer text", "at=2026-10-23T03%3A40%3A10.740Z&x=1", "at=2026-10-23T03%3A41%3A10.942Z&x=1", true, false, false},
		{"the recorded moment only as recorded", "sent 1792726810740 by sqs", "sent 1792726870000 by sqs", true, false, true},
		{"F's moment beside the recorded one in F", "at 1792726810740", "at 1792726870942 was 1792726810740", true, false, true},
		{"another recorded moment", "1792726810741", "1792726870942", false, false, false},
	} {
		for wname, w := range map[string]*Window{"in its window": qnWindow(), "no window": nil} {
			ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), w, sent(row.rec, row.live), nil)
			switch {
			case row.source:
				if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != "header.x-sent-at" || ln.From.Whole != row.whole || ln.From.Echo {
					t.Errorf("%s, %s: %s (from %+v, %s); want a variable of mock-1's header.x-sent-at, whole %v", row.name, wname, ln.Kind, ln.From, ln.Why, row.whole)
				}
				if held := strings.Contains(ln.Why, "the moment it was recorded with stands in mock-1 at header.x-sent-at, as an instant of its own"); held != row.held || !held && ln.Why != "" {
					t.Errorf("%s, %s: evidence %q; want it held as recorded only %v", row.name, wname, ln.Why, row.held)
				}
			case w != nil:
				if !noiseOrRead(ln) {
					t.Errorf("%s, %s: %s (from %+v, %s); want noise", row.name, wname, ln.Kind, ln.From, ln.Why)
				}
			default:
				if !plainRead(ln) || ln.From.Mock != "" {
					t.Errorf("%s, %s: %s (from %+v, %s); want a plain read of the clock, with no call window, checked at replay", row.name, wname, ln.Kind, ln.From, ln.Why)
				}
			}
		}
	}
	// A leaf F saw as recorded did not drift, and still held the instant as
	// recorded: an equal instant is followed from there, as from a payload,
	// in its window too; whole only where its text is the instant's.
	for undrifted, whole := range map[string]bool{"1792726810740": false, "2026-10-23T03:40:10.740Z": true} {
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), sent(undrifted, undrifted), nil); ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != "header.x-sent-at" || ln.From.Whole != whole || ln.From.Echo || !strings.Contains(ln.Why, "the moment it was recorded with stands in mock-1 at header.x-sent-at, as an instant of its own") {
			t.Errorf("a leaf F saw as recorded (%s): %s (from %+v, %s); want a variable of mock-1's header.x-sent-at, whole %v", undrifted, ln.Kind, ln.From, ln.Why, whole)
		}
	}
	// Nor is an answer's own clock stamp, served as recorded, a place an
	// instant came from (rule 3).
	stampAt := []Earlier{{Mock: "mock-1", At: qnInsert.Add(-time.Second), Payload: payloadOf("mock-1", qnInsert.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: "2026-10-23T03:40:10.740Z"}, Live: "2026-10-23T03:40:10.740Z", Stamp: true},
	}}}
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), stampAt, nil); !noiseOrRead(ln) {
		t.Errorf("an answer's stamp served as recorded: %s (from %+v, %s); want noise", ln.Kind, ln.From, ln.Why)
	}
	// A leaf that held both moments, on one side or both, did not drift with
	// it (rule 1); F's moment stands there, so the echo finds the place
	// holding the recorded one.
	for name, e := range map[string][]Earlier{
		"both ways":             sent("1792726810740 1792726870942", "1792726870942 1792726810740"),
		"both only as recorded": sent("1792726810740 1792726870942", "1792726870942 x"),
	} {
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), nil, e, nil); ln.Kind != KindVariable || !ln.From.Echo || ln.From.Mock != "mock-1" {
			t.Errorf("a leaf holding both moments %s: %s (from %+v, %s); want a variable by the echo", name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// Read word by word, a value meets a leaf its protocol types as one instant
// by its moment, never by its words: an earlier exchange's instant that
// drifted with it, both ways, is its source (in its window too: an equal
// instant is followed); one whose minute is a word the value lost holds
// nothing of it; the call's own answer's instant that the value holds is an
// echo; one whose minute is a word of the value holds nothing of it.
func TestAValueReadWordByWordMeetsATypedInstantByItsMoment(t *testing.T) {
	classify := func(rec, live string, w *Window, earlier []Earlier, resp []Leaf) Learned {
		dc := clockedDrifts()
		const p = "body.q"
		r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: rec}}}
		l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: live}}}
		return ClassifyIn(dc, []string{p}, l, r, qnInsert, earlier, nil, resp, w)[0]
	}
	typedSeen := func(rec, live time.Time) []Earlier {
		return []Earlier{{Mock: "mock-1", At: qnInsert.Add(-time.Second), Payload: payloadOf("mock-1", qnInsert.Add(-time.Second)), Own: true, Seen: []Seen{
			{Side: Request, Leaf: qnTyped(rec), Live: qnTyped(live).Value},
		}}}
	}
	ln := classify("1792726810740", "1792726870942", qnWindow(), typedSeen(qnRec, qnLive), nil)
	if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Whole {
		t.Errorf("an epoch that drifted with an earlier typed instant, in its window: %s (from %+v, %s); want a variable of mock-1", ln.Kind, ln.From, ln.Why)
	}
	if ln := classify("batch-40", "batch-41", nil, typedSeen(qnRec, qnLive), nil); ln.Kind != KindNoise {
		t.Errorf("a word of an earlier typed instant's minute: %s (from %+v, %s); want noise", ln.Kind, ln.From, ln.Why)
	}
	answer := []Leaf{{Path: "body.doc.at", Type: Instant, Value: qnTyped(qnRec).Value}}
	for _, rec := range []string{"2026-10-23T03:40:10.740Z", "1792726810740", "after=2026-10-23T03%3A40%3A10.740Z"} {
		lead := qnRec.Add(FullRoundLead)
		live := strings.Replace(strings.Replace(rec, "1792726810740", strconv.FormatInt(lead.UnixMilli(), 10), 1), "03:40:10.740", lead.Format("15:04:05.000"), 1)
		live = strings.Replace(live, "03%3A40%3A10.740", strings.ReplaceAll(lead.Format("15:04:05.000"), ":", "%3A"), 1)
		ln := classify(rec, live, nil, nil, answer)
		if ln.Kind != KindEcho || len(ln.Echo) != 1 || ln.Echo[0].Path != "body.doc.at" {
			t.Errorf("a value (%s) its answer's typed instant holds: %s (echo %+v, %s); want an echo", rec, ln.Kind, ln.Echo, ln.Why)
		}
	}
	if ln := classify("batch-40", "batch-41", nil, nil, answer); ln.Kind != KindNoise {
		t.Errorf("a word of its answer's typed instant's minute: %s (%s); want noise", ln.Kind, ln.Why)
	}
}

// A typed instant the call's own recorded answer holds is answered there
// (an echo) where its provenance shows a read of the clock, and refused
// where none does.
func TestATypedInstantEchoedInItsOwnAnswer(t *testing.T) {
	for _, held := range []string{"1792726810740", "2026-10-23T03%3A40%3A10.740Z"} {
		resp := []Leaf{
			{Path: "body.n", Type: Number, Value: "1"},
			{Path: "body.doc.processed_at", Type: String, Value: held},
		}
		ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, resp)
		if ln.Kind != KindEcho || len(ln.Echo) != 1 || ln.Echo[0].Path != "body.doc.processed_at" {
			t.Errorf("%s, in its window: %s (echo %+v, %s); want an echo at body.doc.processed_at", held, ln.Kind, ln.Echo, ln.Why)
		}
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), nil, nil, resp); ln.Kind != KindUnaccounted || len(ln.Echo) != 0 {
			t.Errorf("%s, no provenance: %s (echo %+v, %s); want refused", held, ln.Kind, ln.Echo, ln.Why)
		}
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, resp[:1]); !noiseOrRead(ln) {
			t.Errorf("an answer that holds no instant: %s (%s); want noise", ln.Kind, ln.Why)
		}
	}
}

// A typed instant keploy cannot read on a side is refused, never noise,
// saying what it found and how to correct it; so is one before 2000 or after
// 2100 on either side, which no provenance shows is a read of a recording's
// clock, however it moved.
func TestATypedInstantKeployCannotReadIsRefused(t *testing.T) {
	bad := Leaf{Path: qnPath, Type: Instant, Value: "2026-10-23T03:40:10.74Z"}
	far := InstantLeaf(qnPath, "", time.Date(58000, 6, 1, 0, 0, 0, 0, time.UTC), time.Millisecond)
	for _, row := range []struct {
		name  string
		r, l  Leaf
		found string
	}{
		{"recorded unreadable", bad, qnTyped(qnLive), "cannot read that instant"},
		{"live unreadable", qnTyped(qnRec), bad, "cannot read that instant"},
		{"live of year 58000", qnTyped(qnRec), far, "cannot read that instant"},
		{"live a String", qnTyped(qnRec), Leaf{Path: qnPath, Type: String, Value: "2026-10-23T03:41:10.942Z"}, "as it was recorded, but not as record's full round saw it"},
		{"recorded a String", Leaf{Path: qnPath, Type: String, Value: "2026-10-23T03:40:10.740Z"}, qnTyped(qnLive), "as record's full round saw it, but not as it was recorded"},
	} {
		ln := classifyTyped(row.r, row.l, qnWindow(), nil, nil)
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.found) || !strings.Contains(ln.Why, "declare it noise in keploy.yml") {
			t.Errorf("%s: %s (%s); want refused, saying it %s and how to correct it", row.name, ln.Kind, ln.Why, row.found)
		}
	}
	old := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	ln := classifyTyped(qnTyped(old), qnTyped(old.Add(FullRoundLead)), &Window{Rec: Span{From: old.Add(-time.Second), To: old.Add(time.Second)}, F: Span{From: old.Add(FullRoundLead - time.Second), To: old.Add(FullRoundLead + time.Second)}}, nil, nil)
	if ln.Kind != KindUnaccounted {
		t.Errorf("an instant of 1990, moved by the lead and in its window: %s (%s); want refused", ln.Kind, ln.Why)
	}
	for name, rec := range map[string]time.Time{"into 2100": time.Date(2099, 12, 31, 23, 59, 30, 0, time.UTC), "out of 1999": time.Date(1999, 12, 31, 23, 59, 30, 0, time.UTC)} {
		if ln := classifyTyped(qnTyped(rec), qnTyped(rec.Add(FullRoundLead)), nil, nil, nil); ln.Kind != KindUnaccounted {
			t.Errorf("an instant that the lead moved %s: %s (%s); want refused", name, ln.Kind, ln.Why)
		}
		// Read in the second before its call, which a read is no later
		// than: refused for the range alone.
		if ln := ClassifyIn(clockedDrifts(), []string{qnPath}, Shape{Leaves: map[string]Leaf{qnPath: qnTyped(rec.Add(FullRoundLead))}}, Shape{Leaves: map[string]Leaf{qnPath: qnTyped(rec)}}, rec.Add(time.Second), nil, nil, nil, nil)[0]; ln.Kind != KindUnaccounted {
			t.Errorf("an instant that the lead moved %s, read before its call: %s (%s); want refused", name, ln.Kind, ln.Why)
		}
	}
}

// InstantClockRead is the provenance a typed instant is noise by: moved by
// the lead, compared to the second for finer instants and at its own
// resolution for coarser ones, or in its call's window in both runs, each
// instant at its own resolution; nothing else.
func TestInstantClockRead(t *testing.T) {
	rec := qnTyped(qnRec)
	minute := func(at time.Time) Leaf { return InstantLeaf(qnPath, "", at, time.Minute) }
	secondRec := InstantLeaf(qnPath, "", qnRec, time.Second)
	opensLate := &Window{Rec: Span{From: qnRec.Add(-time.Second), To: qnRec.Add(time.Second)}, F: Span{From: qnLive.Truncate(time.Second).Add(950 * time.Millisecond), To: qnLive.Add(2 * time.Second)}}
	for _, row := range []struct {
		name      string
		rec, live Leaf
		w         *Window
		want      bool
	}{
		{"moved by the lead", rec, qnTyped(qnRec.Add(FullRoundLead)), nil, true},
		{"moved by the lead and 0.7 s", rec, qnTyped(qnRec.Add(FullRoundLead + 700*time.Millisecond)), nil, true},
		{"moved by 60.2 s", rec, qnTyped(qnLive), nil, false},
		{"moved by 63 s", rec, qnTyped(qnRec.Add(63 * time.Second)), nil, false},
		{"moved by 60.2 s, in its window", rec, qnTyped(qnLive), qnWindow(), true},
		{"a minute moved by the lead", minute(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC)), minute(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC)), nil, true},
		{"a second, then a millisecond read before F's window opened", secondRec, InstantLeaf(qnPath, "", qnLive.Truncate(time.Second).Add(50*time.Millisecond), time.Millisecond), opensLate, false},
		{"a String", rec, Leaf{Type: String, Value: qnRec.Add(FullRoundLead).Format(time.RFC3339Nano)}, nil, false},
		{"a recorded String", Leaf{Type: String, Value: rec.Value}, qnTyped(qnRec.Add(FullRoundLead)), nil, false},
	} {
		if got := InstantClockRead(row.rec, row.live, row.w); got != row.want {
			t.Errorf("%s: %v; want %v", row.name, got, row.want)
		}
	}
}

// The other two boot inserts the queue consumer's lane refused for a
// two-digit coincidence, as unit rows on the classifier (the json build's
// runs 3 and 6, keploy e3d67a7e4): processed_at, a BSON datetime, read as
// text, had a minute F saw (34, 38) that stood as a word in a recording
// made before it, a Mongo reply's requestId (mock-10) and a version in the
// SQS SDK's User-Agent (mock-5). Read whole, the minute is no word: in its
// call's window, or moved by the lead, the instant is noise; shown by
// neither, it is refused for its provenance alone, naming no recording.
// Read word by word (a String), it is refused for the recording, as the lane
// was.
func TestATypedInstantMeetsNoTwoDigitCoincidence(t *testing.T) {
	reply := func(name string, requestID int32, asked time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.Mongo, ConnectionID: "0", Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config", "connID": "0"},
			MongoRequests: []models.MongoRequest{{
				Header:  &models.MongoHeader{Length: 148, RequestID: 4, Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{FlagBits: 65536, Sections: []string{`{ SectionSingle msg: {"hello":{"$numberInt":"1"},"helloOk":true,"$db":"admin"} }`}},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Length: 117, RequestID: requestID, ResponseTo: 4, Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":{"$numberDouble":"1.0"}} }`}},
			}},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Millisecond),
		}}
	}
	receive := func(name, userAgent string, asked time.Time) *models.Mock {
		m := qnReceive()
		m.Name = name
		m.Spec.HTTPReq.Header["User-Agent"] = userAgent
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = asked, asked.Add(time.Second)
		m.Spec.HTTPResp.Header["Date"] = asked.Add(time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT")
		return m
	}
	for _, row := range []struct {
		name      string
		rec, live time.Time
		holder    func(at time.Time) *models.Mock
		field     string
	}{
		{"a reply's requestId", time.UnixMilli(1792726422011).UTC(), time.UnixMilli(1792726485013).UTC(),
			func(at time.Time) *models.Mock { return reply("mock-10", 34, at.Add(-2*time.Second)) }, "spec.responses[0].header.requestId"},
		{"a version in a User-Agent", time.UnixMilli(1792726643396).UTC(), time.UnixMilli(1792726702045).UTC(),
			func(at time.Time) *models.Mock {
				return receive("mock-5", "aws-sdk-go-v2/1.36.3 ua/2.1 os/linux lang/go#1.27.0 md/GOOS#linux md/GOARCH#amd64 api/sqs#1.38.1 m/E", at.Add(-3*time.Second))
			}, "spec.req.header.User-Agent"},
	} {
		at := row.rec.Add(time.Millisecond)
		holder := row.holder(at)
		w := &Window{Rec: Span{From: row.rec.Add(-300 * time.Millisecond), To: at}, F: Span{From: row.live.Add(-300 * time.Millisecond), To: row.live.Add(time.Millisecond)}}
		classify := func(r, l Leaf, w *Window) Learned {
			dc := clockedDrifts()
			return ClassifyIn(dc, []string{qnPath}, Shape{Leaves: map[string]Leaf{qnPath: l}}, Shape{Leaves: map[string]Leaf{qnPath: r}}, at, nil, []Raw{dc.RawOf(holder, false)}, nil, w)[0]
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.live), w); !noiseOrRead(ln) {
			t.Errorf("%s, typed, in its window: %s (from %q, %s); want noise", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.rec.Add(FullRoundLead+202*time.Millisecond)), nil); !noiseOrRead(ln) {
			t.Errorf("%s, typed, moved by the lead: %s (from %q, %s); want noise", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.live), nil); !plainRead(ln) || ln.From.Mock != "" {
			t.Errorf("%s, typed, no provenance: %s (from %q, %s); want a plain read of the clock, with no call window, checked at replay, naming no recording", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		str := func(at time.Time) Leaf {
			return Leaf{Path: qnPath, Noise: "body.docs.msgs.0.processed_at", Type: String, Value: at.Format(time.RFC3339Nano)}
		}
		if ln := classify(str(row.rec), str(row.live), nil); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in "+holder.Name+" at "+row.field+":") {
			t.Errorf("%s, a date-like string, no provenance: %s (from %q, %s); want refused for %s, as the lane was", row.name, ln.Kind, ln.From.Mock, ln.Why, holder.Name)
		}
	}
}

// An instant its protocol types that moved by the lead is no read of the
// clock where its recorded moment is later than its call (a Mongo TTL,
// expiresAt = Date.now() + 1 h), and the reason says so, never that it did
// not move by the lead; one F saw past what a read before its call moves by
// the lead says that instead.
func TestATypedInstantLaterThanItsCallIsRefusedSayingSo(t *testing.T) {
	const p = "body.expiresAt"
	call := time.Date(2026, 10, 22, 11, 14, 17, 155e6, time.UTC)
	read := call.Add(-40 * time.Millisecond)
	for _, row := range []struct {
		name      string
		rec, live time.Time
		res       time.Duration
		why       string
	}{
		{"an hour after its call", read.Add(time.Hour), read.Add(time.Hour + FullRoundLead + 37*time.Millisecond), time.Millisecond, instantAfterCall},
		// The call's minute, F's two on: the lead takes two minutes'
		// move, a read before its call one. With no window it is a plain
		// read of the clock all the same ("" here), which every replay
		// checks in its call's span: how late record's full round read its
		// clock must not decide; in a window that does not hold it, it is
		// refused saying so.
		{"F's past the lead from its call", call.Truncate(time.Minute), call.Truncate(time.Minute).Add(2 * time.Minute), time.Minute, ""},
	} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.live, row.res)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.rec, row.res)}}, call, nil, nil, nil, nil)[0]
		if row.why == "" {
			if !plainRead(ln) {
				t.Errorf("%s, no window: %s Read=%+v (%s); want a plain read of the clock, checked at replay", row.name, ln.Kind, ln.Read, ln.Why)
			}
			// Its call's own answer echoes it: an echo is answered with
			// this run's value and checks no read, so it is refused, the
			// reason saying it moved by the lead past its call, never that
			// it did not move by the lead.
			answer := []Leaf{{Path: "body.doc.expiresAt", Type: Instant, Value: InstantLeaf(p, p, row.rec, row.res).Value}}
			if ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.live, row.res)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.rec, row.res)}}, call, nil, nil, answer, nil)[0]; ln.Kind != KindUnaccounted || len(ln.Echo) != 0 || ln.Why != instantPastCall {
				t.Errorf("%s, no window, its answer echoes it: %s (echo %+v, %s); want refused: %s", row.name, ln.Kind, ln.Echo, ln.Why, instantPastCall)
			}
			// Each run's window the two seconds before its call, F's the
			// lead on: neither holds the call's minute, and no offset later
			// than its call holds both values.
			w := &Window{Rec: Span{From: call.Add(-2 * time.Second), To: call}, F: Span{From: call.Add(FullRoundLead - 2*time.Second), To: call.Add(FullRoundLead)}}
			ln = ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.live, row.res)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, row.rec, row.res)}}, call, nil, nil, nil, w)[0]
			row.why = instantPastCall
		}
		if ln.Kind != KindUnaccounted || ln.Why != row.why {
			t.Errorf("%s: %s (%s); want it refused: %s", row.name, ln.Kind, ln.Why, row.why)
		}
	}
}

// classifyZoned is ClassifyIn of one drifted leaf at p (rec as recorded,
// live as F saw it) of a call recorded at call, by a cache whose
// application writes a wall clock of no zone in zone (nil: UTC alone), of
// keploy mock's client tests where clientTests is set, in window w.
func classifyZoned(zone *time.Location, clientTests bool, p string, rec, live Leaf, call time.Time, w *Window) Learned {
	dc := clockedDrifts()
	dc.zone, dc.clientTests = zone, clientTests
	return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, call, nil, nil, nil, w)[0]
}

// A wall clock of no zone its protocol types (a Postgres timestamp an
// application east of UTC binds its own clock as) that moved by the lead is
// a read of the clock where it is one in UTC or in the zone the application
// writes such a clock in, as a text's is (readNoLaterThan): one read in IST
// is noise, where its reading in UTC alone is later than its call and was
// refused. No other zone excuses it, the zone not known reads UTC alone, and
// an instant that names its moment is read in UTC alone, whatever zone the
// application writes a wall clock in.
func TestAWallClockOfNoZoneIsAClockReadInTheApplicationsZoneToo(t *testing.T) {
	const p = "args.0"
	ist, jst := time.FixedZone("IST", 5*3600+1800), time.FixedZone("JST", 9*3600)
	call := time.Date(2026, 10, 22, 11, 14, 17, 155e6, time.UTC)
	read := call.Add(-40 * time.Millisecond)
	live := read.Add(FullRoundLead + 37*time.Millisecond)
	wall := func(in *time.Location) func(time.Time) Leaf {
		return func(at time.Time) Leaf { return WallClockLeaf(p, p, at.In(in), time.Microsecond) }
	}
	// The instant its reading in IST names, were it written in UTC.
	named := func(at time.Time) Leaf {
		w := at.In(ist)
		return InstantLeaf(p, p, time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC), time.Microsecond)
	}
	for _, row := range []struct {
		name string
		zone *time.Location
		leaf func(time.Time) Leaf
		why  string
	}{
		{"read in the application's zone, east of UTC", ist, wall(ist), ""},
		{"read in UTC, the application's zone east of it", ist, wall(time.UTC), ""},
		{"read in UTC, the application's zone not known", nil, wall(time.UTC), ""},
		{"read in the application's zone, which is not known", nil, wall(ist), instantAfterCall},
		{"read in a zone that is not the application's", ist, wall(jst), instantAfterCall},
		{"an instant that names its moment, the application's zone east of UTC", ist, named, instantAfterCall},
	} {
		ln := classifyZoned(row.zone, false, p, row.leaf(read), row.leaf(live), call, nil)
		switch {
		case row.why == "" && !noiseOrRead(ln):
			t.Errorf("%s: %s (%s); want noise, a read of the clock", row.name, ln.Kind, ln.Why)
		case row.why != "" && (ln.Kind != KindUnaccounted || ln.Why != row.why):
			t.Errorf("%s: %s (%s); want it refused: %s", row.name, ln.Kind, ln.Why, row.why)
		}
	}
	// Recorded as a wall clock, F's an instant that names its moment, moved
	// by the lead: a change of form is no read of the clock, and the reason
	// names both forms, never that it did not move by the lead.
	formWhy := instantFormChangedWhy(namesWall, namesMoment)
	if !strings.Contains(formWhy, "recorded as a wall clock that names no zone and record's full round saw an instant that names its moment") {
		t.Errorf("the reason for a change of form: %s", formWhy)
	}
	if ln := classifyZoned(ist, false, p, wall(time.UTC)(read), InstantLeaf(p, p, live, time.Microsecond), call, nil); ln.Kind != KindUnaccounted || ln.Why != formWhy {
		t.Errorf("a wall clock, then an instant naming its moment: %s (%s); want it refused: %s", ln.Kind, ln.Why, formWhy)
	}
	if ln := classifyZoned(ist, true, p, TimeOfDayLeaf(p, p, time.Duration(clockOfDay(read)), time.Microsecond), wall(time.UTC)(live), call, nil); ln.Kind != KindUnaccounted || ln.Why != instantFormChangedWhy(namesTimeOfDay, namesWall) {
		t.Errorf("a time of day, then a wall clock, in a client test: %s (%s); want it refused: %s", ln.Kind, ln.Why, instantFormChangedWhy(namesTimeOfDay, namesWall))
	}
	// The call's minute in IST, F's two on: the lead takes two minutes'
	// move, a read before its call one. With no window, a plain read of the
	// clock all the same (noWindowRead), in the application's zone, where
	// its recorded reading is its call's minute: in UTC it is hours after
	// its call (nearestZone).
	minute := func(at time.Time) Leaf { return WallClockLeaf(p, p, at.In(ist), time.Minute) }
	if ln := classifyZoned(ist, false, p, minute(call), minute(call.Add(2*time.Minute)), call, nil); !zonedRead(ln, models.ZoneApp) {
		t.Errorf("a minute read in IST, F's past the lead from its call: %s Read=%+v (%s); want a plain read in the application's zone", ln.Kind, ln.Read, ln.Why)
	}
	// A day of no zone (a Postgres date), read in IST just before its
	// midnight, that the lead moved to the next day: in UTC F's day is hours
	// after the call; in IST it is the lead after it.
	call = time.Date(2026, 10, 22, 18, 29, 30, 155e6, time.UTC)
	read = call.Add(-40 * time.Millisecond)
	live = read.Add(FullRoundLead + 37*time.Millisecond)
	day := func(at time.Time) Leaf { return WallClockLeaf(p, p, at.In(ist), 24*time.Hour) }
	if r, l := day(read), day(live); r.Value != "2026-10-22" || l.Value != "2026-10-23" {
		t.Fatalf("the days as written: %q and %q; want 2026-10-22 and 2026-10-23", r.Value, l.Value)
	}
	if ln := classifyZoned(ist, false, p, day(read), day(live), call, nil); !noiseOrRead(ln) {
		t.Errorf("a day read in the application's zone that the lead moved: %s (%s); want noise", ln.Kind, ln.Why)
	}
	// The zone not known, the learn step reads it in UTC alone: a plain
	// read there, with no window (noWindowRead), which a replay checks in
	// UTC (models.ZoneUTC).
	if ln := classifyZoned(nil, false, p, day(read), day(live), call, nil); !zonedRead(ln, models.ZoneUTC) {
		t.Errorf("a day read in IST, the application's zone not known: %s Read=%+v (%s); want a plain read in UTC", ln.Kind, ln.Read, ln.Why)
	}
	// InstantLeaf's date is the same day of no zone.
	if got := InstantLeaf(p, p, time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), 24*time.Hour); got != day(read) {
		t.Errorf("InstantLeaf's date %+v; want WallClockLeaf's %+v", got, day(read))
	}
}

// A client test's window reads a wall clock of no zone its protocol types in
// UTC and in the application's zone, as Window.holdsRead reads a text's: a
// read in IST within the window on both runs is noise, and refused where the
// zone is not known.
func TestAClientTestsWindowReadsAWallClockOfNoZoneInTheApplicationsZone(t *testing.T) {
	const p = "args.0"
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 22, 11, 14, 17, 155e6, time.UTC)
	fCall := call.Add(3 * time.Minute)
	w := &Window{Rec: Span{From: call.Add(-time.Second), To: call}, F: Span{From: fCall.Add(-time.Second), To: fCall}}
	rec := WallClockLeaf(p, p, call.Add(-40*time.Millisecond).In(ist), time.Millisecond)
	live := WallClockLeaf(p, p, fCall.Add(-25*time.Millisecond).In(ist), time.Millisecond)
	if ln := classifyZoned(ist, true, p, rec, live, call, w); !noiseOrRead(ln) {
		t.Errorf("read in IST within the window: %s (%s); want noise", ln.Kind, ln.Why)
	}
	if ln := classifyZoned(nil, true, p, rec, live, call, w); ln.Kind != KindUnaccounted || ln.Why != clientTestWhy("", true) {
		t.Errorf("read in IST, the zone not known: %s (%s); want it refused: %s", ln.Kind, ln.Why, clientTestWhy("", true))
	}
	// Written in a zone that is neither UTC nor the application's (JST),
	// within the window on both runs: no read keploy can tell from a value
	// the test harness gave the application (its clock plus two hours).
	jst := time.FixedZone("JST", 9*3600)
	recJST := WallClockLeaf(p, p, call.Add(-40*time.Millisecond).In(jst), time.Millisecond)
	liveJST := WallClockLeaf(p, p, fCall.Add(-25*time.Millisecond).In(jst), time.Millisecond)
	if ln := classifyZoned(ist, true, p, recJST, liveJST, call, w); ln.Kind != KindUnaccounted || ln.Why != clientTestWhy("", true) {
		t.Errorf("read in JST within the window, the application's zone IST: %s (%s); want it refused: %s", ln.Kind, ln.Why, clientTestWhy("", true))
	}
	if !clientDriftsIn(ist).InstantClockRead(rec, live, w) || InstantClockRead(rec, live, w) {
		t.Errorf("InstantClockRead: the cache's reads the zone, the bare one UTC alone")
	}
}

// A time of day its protocol types (a Postgres time) names no day: it is a
// read of the clock where the lead moved it, read modulo a day (across
// midnight too), as a text's time of day is (moved), and refused where it
// moved otherwise, or in a client test, where the lead shows no read. Its
// hour 24 is the day's end, no 00:00; one outside a day, and a wire text
// with no designator (a timetz of no zone a parser keeps as its text), are
// no text InstantOf reads.
func TestATimeOfDayIsAClockReadWhereTheLeadMovedIt(t *testing.T) {
	const p = "args.0"
	call := time.Date(2026, 10, 22, 23, 59, 30, 155e6, time.UTC)
	read := call.Add(-40 * time.Millisecond)
	tod := func(at time.Time) Leaf { return TimeOfDayLeaf(p, p, time.Duration(clockOfDay(at)), time.Microsecond) }
	if r, l := tod(read), tod(read.Add(FullRoundLead)); r.Value != "T23:59:30.115000" || l.Value != "T00:00:31.660000" {
		t.Fatalf("the times as written: %q and %q", r.Value, l.Value)
	}
	if ln := classifyZoned(nil, false, p, tod(read), tod(read.Add(FullRoundLead+37*time.Millisecond)), call, nil); !todRead(ln, models.ZoneUTC) || ln.Read.Layout != models.LayoutTypedTimeOfDay {
		t.Errorf("moved by the lead across midnight: %s Read=%+v (%s); want a read of the clock modulo a day, typed, in UTC", ln.Kind, ln.Read, ln.Why)
	}
	if ln := classifyZoned(nil, false, p, tod(read), tod(read.Add(time.Hour)), call, nil); ln.Kind != KindUnaccounted || ln.Why != instantNotClock {
		t.Errorf("moved by an hour: %s (%s); want it refused: %s", ln.Kind, ln.Why, instantNotClock)
	}
	for _, w := range []*Window{nil, {Rec: Span{From: read.Add(-time.Second), To: call}, F: Span{From: read.Add(FullRoundLead), To: read.Add(FullRoundLead + time.Second)}}} {
		if ln := classifyZoned(nil, true, p, tod(read), tod(read.Add(FullRoundLead+37*time.Millisecond)), call, w); ln.Kind != KindUnaccounted || ln.Why != timeOfDayClientTestWhy {
			t.Errorf("a client test's, moved by the lead (window %v): %s (%s); want it refused: %s", w != nil, ln.Kind, ln.Why, timeOfDayClientTestWhy)
		}
	}
	// Its moment is a text's time of day's, which names no day.
	found := false
	for k, lay := range clockLayouts {
		if !lay.wraps {
			continue
		}
		if m, _, ok := instantKeyOf(k, "23:59:30.115"); ok {
			ti, _ := readTyped(TimeOfDayLeaf(p, p, time.Duration(clockOfDay(read)), time.Millisecond))
			if got := ti.moment(time.Millisecond); got.key != m.key || got.wall != m.wall {
				t.Errorf("a time of day's moment %+v; want a text's %+v", got.key, m.key)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no layout reads the text 23:59:30.115 as a time of day")
	}
	end, midnight := TimeOfDayLeaf(p, p, 24*time.Hour, time.Second), TimeOfDayLeaf(p, p, 0, time.Second)
	if end.Value != "T24:00:00" || midnight.Value != "T00:00:00" {
		t.Errorf("the day's end and its start: %q and %q", end.Value, midnight.Value)
	}
	if at, res, ok := InstantOf(end); !ok || res != time.Second || at.Sub(timeOfDayDay) != 24*time.Hour {
		t.Errorf("InstantOf(T24:00:00) = %v, %v, %v; want the day's end, to the second", at, res, ok)
	}
	// The day's end is the next day's start where a place is searched for
	// it (a served 00:00 echoes it).
	e, _ := readTyped(end)
	m, _ := readTyped(midnight)
	if got, want := e.moment(time.Second), m.moment(time.Second); got.key != want.key || got.wall != want.wall {
		t.Errorf("T24:00:00's moment %+v; want T00:00:00's %+v", got, want)
	}
	if got := TimeOfDayLeaf(p, p, 11*time.Hour+14*time.Minute+16*time.Second, time.Hour).Value; got != "T11:00" {
		t.Errorf("a time of day to the hour: %q; want T11:00", got)
	}
	for _, v := range []string{"T24:00:01", "T23:60:00", "T25:00", "T12:00:0x", "T1:00:00", "15:49:55", "12:00", "T12:00:60", "T12:00.00", "T12:00:00:000", "T12:00:00.0x0", "T12x00"} {
		if _, _, ok := InstantOf(Leaf{Type: Instant, Value: v}); ok {
			t.Errorf("InstantOf(%q) reads it; want no time of day", v)
		}
	}
	if out := TimeOfDayLeaf(p, p, 25*time.Hour, time.Second); out.Type != Instant || out.Value != (25*time.Hour).String() {
		t.Errorf("a time outside a day: %+v; want an Instant leaf of a text InstantOf does not read", out)
	} else if _, _, ok := InstantOf(out); ok {
		t.Errorf("a time outside a day, %q, is read", out.Value)
	}
}

// A wall clock of no zone or a time of day its protocol types that the
// application picked by its clock from a served timetable and wrote in its
// own zone (IST) is a variable of the timetable, as the same pick's text and
// the same pick as an instant naming its moment are, never a read of the
// clock: the echo reads it in the zone of each instant the timetable holds,
// as the clock rule reads it in the application's. By the lead or in its
// call's window, in a client test too, and whether the timetable is written
// in UTC or with the application's offset.
func TestAWallClockOrTimeOfDayPickedFromAServedTimetableStaysChecked(t *testing.T) {
	const p = "args.0"
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 22, 5, 44, 20, 0, time.UTC)
	timetable := func(slots ...string) *models.Mock {
		return &models.Mock{Name: "mock-2", Kind: models.Mongo, Spec: models.MockSpec{
			MongoResponses:   []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[{"at":"` + slots[0] + `"},{"at":"` + slots[1] + `"}]},"ok":1} }`}}}},
			ReqTimestampMock: call.Add(-time.Second), ResTimestampMock: call.Add(-time.Second),
		}}
	}
	classify := func(zone *time.Location, client bool, rec, live Leaf, w *Window, tt *models.Mock) Learned {
		dc := clockedDrifts()
		dc.zone, dc.clientTests = zone, client
		return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, call, nil, []Raw{dc.RawOf(tt, true)}, nil, w)[0]
	}
	want := func(name string, ln Learned) {
		t.Helper()
		if ln.Kind != KindVariable || ln.From.Mock != "mock-2" {
			t.Errorf("%s: %s (from %+v, %s); want a variable of mock-2's timetable", name, ln.Kind, ln.From, ln.Why)
		}
	}
	// The departure the clock picks: 05:44Z as recorded, 05:45Z as F saw it.
	recPick, livePick := time.Date(2026, 10, 22, 5, 44, 0, 0, time.UTC), time.Date(2026, 10, 22, 5, 45, 0, 0, time.UTC)
	w := &Window{Rec: Span{From: recPick.Add(-time.Second), To: call}, F: Span{From: livePick.Add(-time.Second), To: livePick.Add(time.Second)}}
	for tname, tt := range map[string]*models.Mock{
		"in UTC":             timetable("2026-10-22T05:44:00Z", "2026-10-22T05:45:00Z"),
		"at the app's +5:30": timetable("2026-10-22T11:14:00+05:30", "2026-10-22T11:15:00+05:30"),
	} {
		for _, res := range []time.Duration{time.Minute, time.Second, time.Microsecond} {
			for wname, win := range map[string]*Window{"in its window": w, "by the lead": nil} {
				for _, client := range []bool{false, true} {
					rec, live := WallClockLeaf(p, p, recPick.In(ist), res), WallClockLeaf(p, p, livePick.In(ist), res)
					want(fmt.Sprintf("a wall clock in IST from a timetable %s, to %v, %s, client test %v", tname, res, wname, client), classify(ist, client, rec, live, win, tt))
				}
			}
		}
	}
	// The next departure after the clock, written as a time of day in IST:
	// 11:15 as recorded, 11:16 as F saw it, the lead's move at the minute.
	recNext, liveNext := time.Date(2026, 10, 22, 5, 45, 0, 0, time.UTC), time.Date(2026, 10, 22, 5, 46, 0, 0, time.UTC)
	tod := func(at time.Time) Leaf {
		return TimeOfDayLeaf(p, p, time.Duration(clockOfDay(at.In(ist))), time.Minute)
	}
	for tname, tt := range map[string]*models.Mock{
		"in UTC":             timetable("2026-10-22T05:45:00Z", "2026-10-22T05:46:00Z"),
		"at the app's +5:30": timetable("2026-10-22T11:15:00+05:30", "2026-10-22T11:16:00+05:30"),
	} {
		for _, zone := range []*time.Location{nil, ist} {
			want(fmt.Sprintf("a time of day in IST from a timetable %s, the app's zone %v", tname, zone), classify(zone, false, tod(recNext), tod(liveNext), nil, tt))
		}
	}
}

// A wall clock of no zone or a time of day its protocol types that the
// application copied from the test case's own request (keploy test sends it
// with a template value, so F's moved by the lead) and wrote in its own zone
// is a variable of the request, as the same copy's text is: the request's
// instant in UTC and the copy in IST are one moment, not one built on the
// other by an offset of 5h30m.
func TestAWallClockOrTimeOfDayCopiedFromTheRequestInTheAppsZoneIsFollowed(t *testing.T) {
	const p = "args.0"
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 22, 5, 44, 17, 155e6, time.UTC)
	read := call.Add(-2 * time.Second)
	live := read.Add(FullRoundLead + 37*time.Millisecond)
	given := []Earlier{{Mock: "test-2", At: call.Add(-3 * time.Second), Given: true, Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.since", Noise: "body.since", Type: String, Value: read.Format("2006-01-02T15:04:05.000000Z")}, Live: live.Format("2006-01-02T15:04:05.000000Z")},
	}}}
	tod := func(at time.Time) Leaf {
		return TimeOfDayLeaf(p, p, time.Duration(clockOfDay(at.In(ist))), time.Microsecond)
	}
	for _, row := range []struct {
		name      string
		rec, live Leaf
	}{
		{"a wall clock", WallClockLeaf(p, p, read.In(ist), time.Microsecond), WallClockLeaf(p, p, live.In(ist), time.Microsecond)},
		{"a time of day", tod(read), tod(live)},
	} {
		dc := clockedDrifts()
		dc.zone = ist
		ln := ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: row.live}}, Shape{Leaves: map[string]Leaf{p: row.rec}}, call, given, nil, nil, nil)[0]
		if ln.Kind != KindVariable || ln.From.Mock != "test-2" || ln.From.Path != "body.since" {
			t.Errorf("%s in IST copied from the request's UTC instant: %s (from %+v, %s); want a variable of test-2's body.since", row.name, ln.Kind, ln.From, ln.Why)
		}
	}
}

// An instant that names its moment (a Mongo datetime) is read in UTC alone
// in its call's window too, whatever zone the application writes a wall
// clock in: one the test harness gave as the clock plus 5h30m, which read
// in IST lies within the window on both runs, is refused, in a client test
// and in a test case, and DriftCache.InstantClockRead says it is no read.
func TestAnInstantThatNamesItsMomentIsReadInUTCAloneInItsWindow(t *testing.T) {
	const p = "args.0"
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 22, 11, 14, 17, 155e6, time.UTC)
	fCall := call.Add(3 * time.Minute)
	w := &Window{Rec: Span{From: call.Add(-time.Second), To: call}, F: Span{From: fCall.Add(-time.Second), To: fCall}}
	off := 5*time.Hour + 30*time.Minute
	rec := InstantLeaf(p, p, call.Add(-40*time.Millisecond+off), time.Millisecond)
	live := InstantLeaf(p, p, fCall.Add(-25*time.Millisecond+off), time.Millisecond)
	for _, client := range []bool{true, false} {
		if ln := classifyZoned(ist, client, p, rec, live, call, w); ln.Kind != KindUnaccounted {
			t.Errorf("client test %v: the clock plus 5h30m as an instant naming its moment, the application's zone IST: %s (%s); want it refused", client, ln.Kind, ln.Why)
		}
	}
	if clientDriftsIn(ist).InstantClockRead(rec, live, w) {
		t.Errorf("DriftCache.InstantClockRead reads an instant that names its moment in the application's zone")
	}
}

// A time of day its protocol types that an earlier exchange F served holds
// (a Postgres time column's answer, T11:14:17.115000, then the lead's move)
// is a moment the learn step reads there, though the text reader reads no
// typed form (its designator T is glued to its digits): a copy of it at
// another precision, typed or as a text, and one built on it by an offset,
// is never learned as noise.
func TestATypedTimeOfDayAnEarlierExchangeHoldsIsReadThere(t *testing.T) {
	call := time.Date(2026, 10, 22, 11, 14, 20, 155e6, time.UTC)
	read := time.Date(2026, 10, 22, 11, 14, 17, 115e6, time.UTC)
	live := read.Add(FullRoundLead + 37*time.Millisecond)
	const sp, p = "rows.0.start_at", "args.0"
	tod := func(path string, at time.Time, res time.Duration) Leaf {
		return TimeOfDayLeaf(path, path, time.Duration(clockOfDay(at)), res)
	}
	text := func(at time.Time, layout string) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.Format(layout)}
	}
	for _, c := range []struct {
		name      string
		rec, live Leaf
	}{
		{"typed, to the microsecond", tod(p, read, time.Microsecond), tod(p, live, time.Microsecond)},
		{"typed, to the millisecond", tod(p, read, time.Millisecond), tod(p, live, time.Millisecond)},
		{"typed, to the second", tod(p, read, time.Second), tod(p, live, time.Second)},
		{"a text, to the second", text(read, "15:04:05"), text(live, "15:04:05")},
		{"a text, to the millisecond", text(read, "15:04:05.000"), text(live, "15:04:05.000")},
		{"typed, 30 minutes on", tod(p, read.Add(30*time.Minute), time.Microsecond), tod(p, live.Add(30*time.Minute), time.Microsecond)},
	} {
		for _, payload := range []bool{false, true} {
			e := Earlier{Mock: "pg-1", At: call.Add(-time.Second), Own: true, Seen: []Seen{{Side: Response, Leaf: tod(sp, read, time.Microsecond), Live: tod(sp, live, time.Microsecond).Value}}}
			if payload {
				e.Payload = payloadOf("pg-1", call.Add(-time.Second))
			}
			ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: c.live}}, Shape{Leaves: map[string]Leaf{p: c.rec}}, call, []Earlier{e}, nil, nil, nil)[0]
			if ln.Kind == KindNoise {
				t.Errorf("%s, the exchange's payload given %v: noise; want it followed or refused, never a read of the clock", c.name, payload)
			}
			if c.name == "typed, to the microsecond" && (ln.Kind != KindVariable || ln.From.Mock != "pg-1") {
				t.Errorf("an equal typed time of day, the payload given %v: %s (from %+v, %s); want a variable of pg-1", payload, ln.Kind, ln.From, ln.Why)
			}
		}
	}
}

// A typed time of day an earlier exchange holds is read there by each of the
// learn step's readers, and each gives its own outcome, naming the leaf: a
// drifted value that names the moment the leaf drifted with exactly, typed or
// a text, is a variable of it (instantServedFrom; a text's, instantSource);
// one cut coarser than that moment (a second, or a millisecond of a
// microsecond the millisecond does not hold whole), typed or a text, is the
// echo's variable there, with its evidence
// (instantEchoFrom, echoServed); one that names exactly what the leaf held as
// recorded and F served so is a variable of it, with its evidence
// (instantLeafFrom); and a move to what the leaf held as recorded, where F
// served it another (on either side), or to what F served there, where it
// was recorded as another, typed or a text, is refused as a pick naming it
// (heldAt), never noise.
func TestATypedTimeOfDayAnEarlierExchangeHoldsIsBoundOrRefusedNamingItsLeaf(t *testing.T) {
	call := time.Date(2026, 10, 22, 11, 14, 20, 155e6, time.UTC)
	read := time.Date(2026, 10, 22, 11, 14, 17, 115e6, time.UTC)
	live := read.Add(FullRoundLead + 37*time.Millisecond)
	const sp, p = "rows.0.start_at", "args.0"
	tod := func(path string, at time.Time, res time.Duration) Leaf {
		return TimeOfDayLeaf(path, path, time.Duration(clockOfDay(at)), res)
	}
	text := func(at time.Time, layout string) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.Format(layout)}
	}
	seen := func(side Side, rec, f time.Time) Earlier {
		return Earlier{Mock: "pg-1", At: call.Add(-time.Second), Own: true, Payload: payloadOf("pg-1", call.Add(-time.Second)), Seen: []Seen{{Side: side, Leaf: tod(sp, rec, time.Microsecond), Live: tod(sp, f, time.Microsecond).Value}}}
	}
	typed := func(res time.Duration) [2]Leaf { return [2]Leaf{tod(p, read, res), tod(p, live, res)} }
	texted := func(layout string) [2]Leaf { return [2]Leaf{text(read, layout), text(live, layout)} }
	fine := 123 * time.Microsecond
	for _, c := range []struct {
		name  string
		e     Earlier
		v     [2]Leaf
		want  string
		whole bool
	}{
		{"drifted with it, to the microsecond", seen(Response, read, live), typed(time.Microsecond), "served", true},
		{"drifted with it, to the millisecond", seen(Response, read, live), typed(time.Millisecond), "served", false},
		{"drifted with it, to the second", seen(Response, read, live), typed(time.Second), "echo", false},
		{"drifted with a microsecond the millisecond does not hold whole", seen(Response, read.Add(fine), live.Add(fine)), typed(time.Millisecond), "echo", false},
		{"a text drifted with it, to the second", seen(Response, read, live), texted("15:04:05"), "echo", false},
		{"a text drifted with it, to the millisecond", seen(Response, read, live), texted("15:04:05.000"), "served", false},
		{"held as recorded and served so, to the microsecond", seen(Response, read, read), typed(time.Microsecond), "held", true},
		{"held as recorded and served so, to the millisecond", seen(Response, read, read), typed(time.Millisecond), "held", false},
		{"F saw what an answer held as recorded", seen(Response, live, live.Add(time.Hour)), typed(time.Millisecond), "pick", false},
		{"F saw what a request held as recorded", seen(Request, live, live.Add(time.Hour)), typed(time.Millisecond), "pick", false},
		{"F saw what an answer served it, recorded as another", seen(Response, read.Add(-time.Hour), live), typed(time.Millisecond), "pick", false},
		{"a text F saw where an answer held it as recorded", seen(Response, live, live.Add(time.Hour)), texted("15:04:05.000"), "pick", false},
	} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: c.v[1]}}, Shape{Leaves: map[string]Leaf{p: c.v[0]}}, call, []Earlier{c.e}, nil, nil, nil)[0]
		at := ln.Kind == KindVariable && ln.From.Mock == "pg-1" && ln.From.Path == sp && ln.From.Whole == c.whole
		var ok bool
		switch c.want {
		case "served":
			ok = at && !ln.From.Echo && ln.Why == ""
		case "echo":
			ok = at && ln.From.Echo && strings.Contains(ln.Why, "the moment it was recorded with stands in pg-1 at "+sp) && (c.v[0].Type != Instant || ln.Why == instantEvidence(ln.From))
		case "held":
			ok = at && !ln.From.Echo && ln.Why == instantEvidence(ln.From)
		case "pick":
			ok = ln.Kind == KindUnaccounted && ln.Why == pickUnaccounted("in pg-1 at "+sp)
		}
		if !ok {
			t.Errorf("%s: %s (from %+v, %q); want the %s outcome at pg-1 %s, whole %v", c.name, ln.Kind, ln.From, ln.Why, c.want, sp, c.whole)
		}
	}
}

// A time of day written as a text and one its protocol types are one moment
// where they name it exactly, as two texts or two typed leaves are, so the
// typed rule reads a typed time of day where a text holds it (holdsExact,
// isInstant): a value that drifted, to the microsecond, with an earlier leaf
// of the other form, on either side of the exchange, is a variable of that
// leaf, never noise (two texts or two typed leaves of the same shape are
// bound there); so is a typed one whose recorded moment such a text leaf
// held as recorded, whatever F saw there, or a payload holds; and the call's
// own answer holding it, in either form, is its echo, and one holding
// another moment is none. A time of day and an instant keploy reads are two
// moments whatever their forms: one never names the other exactly (only the
// echo reads a time of day an answer's instant holds, as a pick by its
// clock).
func TestATimeOfDayInTheOtherFormIsTheSameMoment(t *testing.T) {
	call := time.Date(2026, 10, 22, 11, 14, 20, 155e6, time.UTC)
	read := time.Date(2026, 10, 22, 11, 14, 17, 115123e3, time.UTC)
	live := read.Add(FullRoundLead + 37*time.Millisecond)
	const sp, p, ap = "rows.0.start_at", "args.0", "rows.0.0"
	forms := map[string]func(path string, at time.Time) Leaf{
		"typed": func(path string, at time.Time) Leaf {
			return TimeOfDayLeaf(path, path, time.Duration(clockOfDay(at)), time.Microsecond)
		},
		"a text": func(path string, at time.Time) Leaf {
			return Leaf{Path: path, Noise: path, Type: String, Value: at.Format("15:04:05.000000")}
		},
		"an instant": func(path string, at time.Time) Leaf {
			return InstantLeaf(path, path, at, time.Microsecond)
		},
	}
	classify := func(v string, e []Earlier, raw []Raw, resp []Leaf) Learned {
		rec, lv := forms[v](p, read), forms[v](p, live)
		return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: lv}}, Shape{Leaves: map[string]Leaf{p: rec}}, call, e, raw, resp, nil)[0]
	}
	seen := func(side Side, form string, f time.Time) []Earlier {
		return []Earlier{{Mock: "pg-1", At: call.Add(-time.Second), Own: true, Payload: payloadOf("pg-1", call.Add(-time.Second)), Seen: []Seen{{Side: side, Leaf: forms[form](sp, read), Live: forms[form](sp, f).Value}}}}
	}
	ofLeaf := func(ln Learned) bool {
		return ln.Kind == KindVariable && ln.From.Mock == "pg-1" && ln.From.Path == sp
	}
	for _, side := range []Side{Response, Request} {
		for _, ef := range []string{"typed", "a text"} {
			for _, vf := range []string{"typed", "a text"} {
				if ln := classify(vf, seen(side, ef, live), nil, nil); !ofLeaf(ln) {
					t.Errorf("%s drifted with %s earlier on the %s side: %s (from %+v, %q); want a variable of pg-1 at %s", vf, ef, side, ln.Kind, ln.From, ln.Why, sp)
				}
			}
		}
		// Recorded as the moment, F saw another there: an equal moment is
		// followed whatever its provenance, as one drifted with it
		// (instantServedFrom), as a typed leaf's is (seenIs).
		for _, ef := range []string{"typed", "a text"} {
			if ln := classify("typed", seen(side, ef, live.Add(time.Hour)), nil, nil); !ofLeaf(ln) || ln.From.Echo || ln.Why != "" {
				t.Errorf("typed, %s earlier on the %s side recorded as its moment, F seeing another: %s (from %+v, %q); want a variable of pg-1 at %s, as one drifted with it", ef, side, ln.Kind, ln.From, ln.Why, sp)
			}
		}
		// Held as recorded and served so (instantLeafFrom).
		if ln := classify("typed", seen(side, "a text", read), nil, nil); !ofLeaf(ln) || ln.Why != instantEvidence(ln.From) {
			t.Errorf("typed, a text earlier on the %s side held as recorded and served so: %s (from %+v, %q); want a variable of pg-1 at %s, with its evidence", side, ln.Kind, ln.From, ln.Why, sp)
		}
		// An instant at the same time of day is another moment.
		for _, pair := range [][2]string{{"typed", "an instant"}, {"an instant", "typed"}, {"a text", "an instant"}, {"an instant", "a text"}} {
			if ln := classify(pair[1], seen(side, pair[0], live), nil, nil); ofLeaf(ln) && !ln.From.Echo {
				t.Errorf("%s drifted with %s earlier on the %s side: a variable of it, as the same moment; want them two moments", pair[1], pair[0], side)
			}
		}
	}
	// A payload holding the recorded moment as a text (instantRawFrom).
	raw := []Raw{{read: true, mock: "mock-db", at: call.Add(-time.Second), served: true, text: `{"rows":[["42","` + read.Format("15:04:05.000000") + `"]]}`}}
	if ln := classify("typed", nil, raw, nil); ln.Kind != KindVariable || ln.From.Mock != "mock-db" || ln.Why != instantEvidence(ln.From) {
		t.Errorf("typed, its recorded moment a text in a payload: %s (from %+v, %q); want a variable of mock-db, with its evidence", ln.Kind, ln.From, ln.Why)
	}
	// The call's own answer holding it (instantEchoes, Echoes), and holding
	// another moment: an instant at its time of day, a time of day a second
	// on.
	for _, vf := range []string{"typed", "a text"} {
		for _, af := range []string{"typed", "a text"} {
			if ln := classify(vf, nil, nil, []Leaf{forms[af](ap, read)}); ln.Kind != KindEcho || len(ln.Echo) != 1 || ln.Echo[0].Path != ap {
				t.Errorf("%s, the call's answer holding it as %s: %s (echo %v, %q); want its echo at %s", vf, af, ln.Kind, ln.Echo, ln.Why, ap)
			}
		}
		for _, a := range []Leaf{forms["an instant"](ap, read), forms["typed"](ap, read.Add(time.Second)), forms["a text"](ap, read.Add(time.Second))} {
			if ln := classify(vf, nil, nil, []Leaf{a}); ln.Kind == KindEcho {
				t.Errorf("%s, the call's answer holding another moment, %s: an echo of it; want none", vf, a.Value)
			}
		}
	}
}

// clockOffsetCall is a call recorded at call whose application set an expiry
// thirty minutes after a read of its clock 40 ms before it, the previous
// call 50 ms before; record's full round made it 61.537 s on (FullRoundLead
// less 8 ms), its read 45 ms before it, its previous call 60 ms before.
type clockOffsetCall struct {
	call, read, fcall, fread time.Time
	w                        *Window
}

func newClockOffsetCall() clockOffsetCall {
	call := time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC)
	fcall := call.Add(FullRoundLead - 8*time.Millisecond)
	return clockOffsetCall{
		call: call, read: call.Add(-40 * time.Millisecond),
		fcall: fcall, fread: fcall.Add(-45 * time.Millisecond),
		w: &Window{Rec: Span{From: call.Add(-50 * time.Millisecond), To: call}, F: Span{From: fcall.Add(-60 * time.Millisecond), To: fcall}},
	}
}

// classify is ClassifyIn of the expiry at p, recorded rec and seen live in
// F, each at resolution res, in window w, with the call's recorded answer
// resp.
func (c clockOffsetCall) classify(p string, rec, live time.Time, res time.Duration, w *Window, resp ...Leaf) Learned {
	return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, live, res)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, rec, res)}}, c.call, nil, nil, resp, w)[0]
}

// An instant its protocol types that the application builds on its clock by
// a constant offset (a Mongo expiresAt = now + 30 m) is, in its call's window
// in both runs, a read of the clock plus that offset: for a caller whose
// replay compares it (Window.ComparesOffsets), learned as one, the offset
// bounded by the two windows and read plainly ("body.expiresAt = clock +
// 30m"), never noise; for any other refused, the reason naming the offset
// and that a changed time to live would pass as noise; with no window, as
// it was. A minute's expiry reads the same, its offset bounded to about a
// minute.
func TestAnInstantAtTheClockPlusAnOffsetIsLearnedAsThatOffset(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	rec, live := c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms)

	ln := c.classify(p, rec, live, ms, c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || ln.Offset == nil {
		t.Fatalf("compared: %s (%s); want a clock offset", ln.Kind, ln.Why)
	}
	// The recording's window puts the offset from 29m59.96s up to 30m0.011s,
	// F's from 29m59.955s up to 30m0.016s: both, the first. The two values
	// share no granularity coarser than their encoding's millisecond.
	want := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms, Res: ms, Layout: models.LayoutTyped}
	if *ln.Offset != want {
		t.Errorf("compared: offset %+v, want %+v", *ln.Offset, want)
	}
	if !strings.HasPrefix(ln.Why, p+" = clock + 30m (from 29m59.96s to 30m0.011s): ") || ln.Why != clockOffsetEvidence(p, want) || !strings.Contains(ln.Why, ". A replay compares the offset: a time to live changed by a minute or more still fails; ") {
		t.Errorf("compared: why %q; want it to read %q", ln.Why, clockOffsetEvidence(p, want))
	}

	ln = c.classify(p, rec, live, ms, c.w)
	if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != clockOffsetUncomparedWhy(p, want) {
		t.Errorf("not compared: %s (%s); want refused: %s", ln.Kind, ln.Why, clockOffsetUncomparedWhy(p, want))
	}
	if !strings.Contains(ln.Why, "clock + 30m") || !strings.Contains(ln.Why, "a changed time to live would pass") {
		t.Errorf("not compared: %q names neither the offset nor why it is refused", ln.Why)
	}

	for _, w := range []*Window{nil, (*Window)(nil).ComparesOffsets(), (&Window{Rec: c.w.Rec}).ComparesOffsets(), (&Window{F: c.w.F}).ComparesOffsets()} {
		if ln := c.classify(p, rec, live, ms, w); ln.Kind != KindUnaccounted || ln.Why != instantAfterCall {
			t.Errorf("no window, or a run's span missing (%+v): %s (%s); want refused as before: %s", w, ln.Kind, ln.Why, instantAfterCall)
		}
	}

	// A wall clock of no zone stands from the clock by its zone's offset
	// as much as by a time to live: refused as before.
	wl := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: WallClockLeaf(p, p, live, ms)}}, Shape{Leaves: map[string]Leaf{p: WallClockLeaf(p, p, rec, ms)}}, c.call, nil, nil, nil, c.w.ComparesOffsets())[0]
	if wl.Kind != KindUnaccounted || wl.Offset != nil || wl.Why != instantAfterCall {
		t.Errorf("a wall clock of no zone: %s %+v (%s); want refused as before: %s", wl.Kind, wl.Offset, wl.Why, instantAfterCall)
	}

	// A minute's expiry: its offset is bounded to about a minute, no
	// narrower than the minute it reads in, so a replay could not tell
	// thirty-one minutes from it: refused, naming the range.
	min := time.Minute
	ln = c.classify(p, c.read.Add(30*time.Minute).Truncate(min), c.fread.Add(30*time.Minute).Truncate(min), min, c.w.ComparesOffsets())
	minute := models.ClockOffset{Min: 30*time.Minute - 17155*ms, Max: 31*time.Minute - 18632*ms, Res: min, Layout: models.LayoutTyped}
	if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != clockOffsetWideWhy(p, minute, min) || !strings.HasPrefix(ln.Why, p+" = clock plus an offset from 29m42.845s to 30m41.368s at a resolution of 1m (the one its encoding writes): ") || !strings.Contains(ln.Why, "not narrower than the minute the offset reads in") {
		t.Errorf("a minute's expiry: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, clockOffsetWideWhy(p, minute, min))
	}
	// An hour's expiry at a minute's resolution reads in hours: learned,
	// from 59m42.845s up to 1h0m41.368s, at the minute its encoding writes.
	// A replay at a minute's encoding holds an hour, never two or none; one
	// whose value is written at the millisecond is checked at the minute
	// (models.ClockOffset.CheckedAt), as standing for its whole minute.
	ln = c.classify(p, c.read.Add(time.Hour).Truncate(min), c.fread.Add(time.Hour).Truncate(min), min, c.w.ComparesOffsets())
	hour := models.ClockOffset{Min: time.Hour - 17155*ms, Max: time.Hour + 41368*ms, Res: min, Layout: models.LayoutTyped}
	if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != hour || ln.Offset.Unit() != time.Hour || !strings.HasPrefix(ln.Why, p+" = clock + 1h (") || !strings.Contains(ln.Why, "a time to live changed by an hour or more still fails") {
		t.Fatalf("an hour's expiry at a minute's resolution: %s %+v (%s); want the clock offset %+v", ln.Kind, ln.Offset, ln.Why, hour)
	}
	for step := time.Duration(0); step < min; step += 7 * time.Second {
		from := c.call.Add(3 * time.Hour).Truncate(min).Add(step)
		to, read := from.Add(50*ms), from.Add(20*ms)
		for _, row := range []struct {
			ttl  time.Duration
			want bool
		}{{time.Hour, true}, {2 * time.Hour, false}, {0, false}} {
			if got := ln.Offset.Holds(read.Add(row.ttl).Truncate(min), min, from, to); got != row.want {
				t.Errorf("%s: an hour at a minute's encoding, a replay's read %s into its minute plus %s: holds %v, want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], read.Sub(read.Truncate(min)), row.ttl, got, row.want)
			}
		}
		if !ln.Offset.Holds(read.Add(time.Hour).Truncate(ms), ms, from, to) || ln.Offset.Holds(read.Add(time.Hour+3*time.Minute).Truncate(ms), ms, from, to) {
			t.Errorf("an hour learned at a minute's encoding, a replay's value at the millisecond: want the read plus an hour held at the minute, and three minutes more not")
		}
	}
}

// B1 of the offset rule's review: an application idle three minutes before
// its call in both runs, its time to live thirty minutes, shows an offset
// from 29m59.96s up to 32m59.956s: thirty, thirty-one and thirty-two
// minutes alike. Learned, a replay would hold a time to live of thirty-two
// minutes; refused instead, the reason naming the range, never one offset.
func TestAWideCallWindowRefusesTheOffsetNamingItsRange(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	w := &Window{Rec: Span{From: c.call.Add(-3 * time.Minute), To: c.call}, F: Span{From: c.fcall.Add(-3 * time.Minute), To: c.fcall}}
	ln := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms), ms, w.ComparesOffsets())
	wide := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 33*time.Minute - 44*ms, Res: ms, Layout: models.LayoutTyped}
	if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != clockOffsetWideWhy(p, wide, ms) {
		t.Fatalf("FALSE GREEN? idle three minutes before the call: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, clockOffsetWideWhy(p, wide, ms))
	}
	if !strings.HasPrefix(ln.Why, p+" = clock plus an offset from 29m59.96s to 32m59.956s at a resolution of 1ms (the one its encoding writes): ") || strings.Contains(ln.Why, "31m") {
		t.Errorf("idle three minutes before the call: %q; want it to name the range, and no one offset", ln.Why)
	}
	// Not compared by its caller: refused the same way, the range named.
	if ln := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms), ms, w); ln.Kind != KindUnaccounted || ln.Why != clockOffsetWideWhy(p, wide, ms) {
		t.Errorf("idle three minutes, not compared: %s (%s); want refused: %s", ln.Kind, ln.Why, clockOffsetWideWhy(p, wide, ms))
	}
}

// S1 of the offset rule's review: F's own bounds decide too. F's value 60 ms
// or a minute below the recording's offset meets no offset of the
// recording's (refused, never the recording's offset); F's narrower window
// bounds the offset to its own range; and two ranges that only touch hold
// no offset (refused as before, never an empty offset).
func TestAClockOffsetIsBoundedByBothRunsWindows(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	rec := c.read.Add(30 * time.Minute).Truncate(ms)
	for _, lower := range []time.Duration{60 * ms, time.Minute} {
		if ln := c.classify(p, rec, c.fread.Add(30*time.Minute-lower).Truncate(ms), ms, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil {
			t.Errorf("FALSE GREEN? F's value %s below the recording's offset: %s %+v (%s); want refused", lower, ln.Kind, ln.Offset, ln.Why)
		}
	}
	// F's window 10 ms wide, its read 5 ms before its call: from 29m59.995s
	// up to 30m0.006s, inside the recording's. The two values (…17.115 and
	// …18.687, counted in milliseconds from the epoch) both happen to be
	// whole multiples of 3 ms, which is no step (offsetCut): learned at the
	// millisecond its encoding writes.
	narrow := &Window{Rec: c.w.Rec, F: Span{From: c.fcall.Add(-10 * ms), To: c.fcall}}
	ln := c.classify(p, rec, c.fcall.Add(30*time.Minute-5*ms), ms, narrow.ComparesOffsets())
	if want := (models.ClockOffset{Min: 30*time.Minute - 5*ms, Max: 30*time.Minute + 6*ms, Res: ms, Layout: models.LayoutTyped}); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want {
		t.Errorf("F's narrower window: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}
	// F's least offset is the recording's bound, 30m0.011s: they touch.
	touch := c.fcall.Add(rec.Add(ms).Sub(c.w.Rec.From))
	if ln := c.classify(p, rec, touch, ms, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != instantAfterCall {
		t.Errorf("ranges that touch: %s %+v (%s); want refused as before: %s", ln.Kind, ln.Offset, ln.Why, instantAfterCall)
	}
	// One resolution recorded, a coarser one seen (or the other way): the
	// coarser decides, a minute, which the offset is no narrower than.
	for _, row := range []struct {
		name       string
		rres, lres time.Duration
	}{{"a millisecond recorded, a minute seen", ms, time.Minute}, {"a minute recorded, a millisecond seen", time.Minute, ms}} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, c.fread.Add(30*time.Minute), row.lres)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, c.read.Add(30*time.Minute), row.rres)}}, c.call, nil, nil, nil, c.w.ComparesOffsets())[0]
		if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, "at a resolution of 1m (the one its encoding writes): ") {
			t.Errorf("%s: %s %+v (%s); want refused at a minute's resolution", row.name, ln.Kind, ln.Offset, ln.Why)
		}
	}
	// One form recorded, another seen: never an offset.
	for _, row := range []struct {
		name     string
		rec, see Leaf
		want     string
	}{
		{"a moment recorded, a wall clock seen", InstantLeaf(p, p, rec, ms), WallClockLeaf(p, p, c.fread.Add(30*time.Minute).Truncate(ms), ms), instantFormChangedWhy(namesMoment, namesWall)},
		{"a wall clock recorded, a moment seen", WallClockLeaf(p, p, rec, ms), InstantLeaf(p, p, c.fread.Add(30*time.Minute).Truncate(ms), ms), instantFormChangedWhy(namesWall, namesMoment)},
	} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: row.see}}, Shape{Leaves: map[string]Leaf{p: row.rec}}, c.call, nil, nil, nil, c.w.ComparesOffsets())[0]
		if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != row.want {
			t.Errorf("%s: %s %+v (%s); want refused: %s", row.name, ln.Kind, ln.Offset, ln.Why, row.want)
		}
	}
}

// clockOffsetOf shows no offset outside the years a recording's clock reads,
// on either side (the windows placed so the two runs' offsets meet), nor for
// a call with no window; offsetBounds none for a span missing a bound or out
// of order, or a resolution of none.
func TestAClockOffsetNeedsClockYearsAWindowAndAResolution(t *testing.T) {
	ms := time.Millisecond
	at := func(t time.Time) typedInstant { return typedInstant{at: t, res: ms, names: namesMoment} }
	span := func(to time.Time) Span { return Span{From: to.Add(-50 * ms), To: to} }
	late, early := clockTo.Add(time.Second), clockTo.Add(-time.Second)
	earlier := early.Add(-2 * time.Second)
	in := &Window{Rec: span(earlier.Add(-30 * time.Minute)), F: span(early.Add(-30 * time.Minute))}
	if _, ok := clockOffsetOf(at(earlier), at(early), in); !ok {
		t.Fatalf("both in the clock's years: no offset; want one")
	}
	for _, row := range []struct {
		name string
		r, l time.Time
		w    *Window
	}{
		{"recorded past the clock's years", late, early, &Window{Rec: span(late.Add(-30 * time.Minute)), F: span(early.Add(-30 * time.Minute))}},
		{"seen past the clock's years", early, late, &Window{Rec: span(early.Add(-30 * time.Minute)), F: span(late.Add(-30 * time.Minute))}},
		{"no window", early, early, nil},
	} {
		if off, ok := clockOffsetOf(at(row.r), at(row.l), row.w); ok {
			t.Errorf("%s: offset %s; want none", row.name, off)
		}
	}
	v := clockFrom.Add(time.Hour)
	to := v.Add(-30 * time.Minute)
	for _, row := range []struct {
		name string
		res  time.Duration
		s    Span
	}{
		{"no start", ms, Span{To: to}},
		{"no end", ms, Span{From: to}},
		{"an end before its start", ms, Span{From: to, To: to.Add(-ms / 2)}},
		{"no resolution", 0, Span{From: to.Add(-50 * ms), To: to}},
	} {
		if lo, hi, ok := offsetBounds(v, row.res, row.s); ok {
			t.Errorf("offsetBounds, %s: %s to %s; want none", row.name, lo, hi)
		}
	}
	if lo, hi, ok := offsetBounds(v, ms, Span{From: to.Add(-50 * ms), To: to}); !ok || lo != 30*time.Minute || hi != 30*time.Minute+51*ms {
		t.Errorf("offsetBounds: %s to %s (%v); want 30m to 30m0.051s", lo, hi, ok)
	}
}

// S5 of the offset rule's review: in a client test (keploy mock's harness
// gives the application inputs keploy does not see) a value at the clock
// plus an offset keeps the client test's reason, never an offset's.
func TestAClientTestsClockOffsetKeepsItsReason(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	ln := classifyZoned(nil, true, p, InstantLeaf(p, p, c.read.Add(30*time.Minute).Truncate(ms), ms), InstantLeaf(p, p, c.fread.Add(30*time.Minute).Truncate(ms), ms), c.call, c.w.ComparesOffsets())
	if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != clientTestWhy("", true) {
		t.Errorf("a client test: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, clientTestWhy("", true))
	}
}

// S2 of the offset rule's review: the reason names the field as keploy.yml's
// noise writes it, not its encoding's path.
func TestAClockOffsetsReasonNamesTheField(t *testing.T) {
	c := newClockOffsetCall()
	ms := time.Millisecond
	const path, field = "body.cmd.documents.0.expiresAt.$date.$numberLong", "body.cmd.documents[].expiresAt"
	ln := ClassifyIn(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: InstantLeaf(path, field, c.fread.Add(30*time.Minute).Truncate(ms), ms)}}, Shape{Leaves: map[string]Leaf{path: InstantLeaf(path, field, c.read.Add(30*time.Minute).Truncate(ms), ms)}}, c.call, nil, nil, nil, c.w.ComparesOffsets())[0]
	if ln.Kind != KindClockOffset || !strings.HasPrefix(ln.Why, field+" = clock + 30m (") || strings.Contains(ln.Why, "$numberLong") {
		t.Errorf("a Mongo date: %s (%s); want it to name %s", ln.Kind, ln.Why, field)
	}
	if got := fieldName(Leaf{Path: path}); got != path {
		t.Errorf("a leaf with no noise name: %q, want its path %q", got, path)
	}
	for u, want := range map[time.Duration]string{24 * time.Hour: "day", time.Hour: "hour", time.Minute: "minute", time.Second: "second", ms: "millisecond", time.Microsecond: "microsecond", time.Nanosecond: "nanosecond"} {
		if got := unitWord(u); got != want {
			t.Errorf("unitWord(%s) = %q, want %q", u, got, want)
		}
	}
}

// S3 of the offset rule's review: a value the earlier place's reading binds
// (a variable, or an echo) and the late reading learns as the clock plus an
// offset is refused, the reason naming both; an offset both readings learn
// stays one.
func TestAValueReadAsAVariableAndAsAClockOffsetIsRefused(t *testing.T) {
	const p = "body.expiresAt"
	off := models.ClockOffset{Min: 30*time.Minute - 40*time.Millisecond, Max: 30*time.Minute + 11*time.Millisecond}
	leaf := Leaf{Path: p, Noise: p}
	late := Learned{Kind: KindClockOffset, Recorded: leaf, Offset: &off, Why: clockOffsetEvidence(p, off)}
	for _, asPlace := range []Learned{
		{Kind: KindVariable, Recorded: leaf, From: Place{Mock: "mock-7", Path: "spec.resp.body.expiresAt"}},
		{Kind: KindEcho, Recorded: leaf},
	} {
		got := stricterOf(asPlace, late)
		if got.Kind != KindUnaccounted || got.Offset != nil || !strings.Contains(got.Why, p+" = clock + 30m (") || !strings.Contains(got.Why, "would be "+boundAt(asPlace)) || !strings.HasSuffix(got.Why, "keploy refuses a value the two readings explain differently") {
			t.Errorf("FALSE GREEN? %s before the call, a clock offset after: %s %+v (%s); want refused naming both", asPlace.Kind, got.Kind, got.Offset, got.Why)
		}
	}
	if got := stricterOf(late, late); got.Kind != KindClockOffset || got.Offset == nil {
		t.Errorf("an offset both readings learn: %s %+v; want it kept", got.Kind, got.Offset)
	}
	// S1 of the review of 8e4346458: the earlier place's own refusal stands,
	// with its reason; a noise reading before the call is refused too, the
	// reason naming it, never a blank binding.
	refused := Learned{Kind: KindUnaccounted, Recorded: leaf, Why: "the earlier place's own refusal"}
	if got := stricterOf(refused, late); got.Kind != KindUnaccounted || got.Offset != nil || got.Why != refused.Why {
		t.Errorf("refused before the call, a clock offset after: %s %+v (%s); want the earlier place's refusal as is", got.Kind, got.Offset, got.Why)
	}
	noise := Learned{Kind: KindNoise, Recorded: leaf}
	if got := stricterOf(noise, late); got.Kind != KindUnaccounted || got.Offset != nil || !strings.Contains(got.Why, p+" = clock + 30m (") || !strings.Contains(got.Why, "would be noise, which no replay compares, and keploy refuses") || strings.Contains(got.Why, "would be ,") {
		t.Errorf("FALSE GREEN? noise before the call, a clock offset after: %s %+v (%s); want refused naming both", got.Kind, got.Offset, got.Why)
	}
	// A plain read of the clock before the call (KindClockRead) is named as
	// one, never as noise no replay compares.
	read := Learned{Kind: KindClockRead, Recorded: leaf, Read: &models.ClockOffset{Res: time.Millisecond}}
	if got := stricterOf(read, late); got.Kind != KindUnaccounted || got.Offset != nil || !strings.Contains(got.Why, "would be a plain read of the application's clock, at no offset, and keploy refuses") {
		t.Errorf("FALSE GREEN? a plain read before the call, a clock offset after: %s %+v (%s); want refused naming both", got.Kind, got.Offset, got.Why)
	}
}

// What the learn step learns as the clock plus an offset, a replay compares
// (models.ClockOffset.Holds): the value a replay sees, a read in its call's
// window plus thirty minutes, holds; plus thirty-one minutes or an hour (a
// changed time to live), not. An offset that changed between the recording
// and record's full round is no constant offset: refused.
func TestATimeToLiveChangedAfterLearningStillFails(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	ln := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms), ms, c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || ln.Offset == nil {
		t.Fatalf("learned: %s (%s); want a clock offset", ln.Kind, ln.Why)
	}
	// A replay's window, on its application's clock, an hour on.
	from := c.call.Add(time.Hour)
	to, read := from.Add(30*ms), from.Add(12*ms)
	for _, row := range []struct {
		ttl  time.Duration
		want bool
	}{{30 * time.Minute, true}, {31 * time.Minute, false}, {time.Hour, false}, {29 * time.Minute, false}} {
		if got := ln.Offset.Holds(read.Add(row.ttl).Truncate(ms), ms, from, to); got != row.want {
			t.Errorf("a replay's value at its read plus %s: holds %v, want %v", row.ttl, got, row.want)
		}
	}
	changed := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(31*time.Minute).Truncate(ms), ms, c.w.ComparesOffsets())
	if changed.Kind != KindUnaccounted || changed.Offset != nil {
		t.Errorf("thirty minutes recorded, thirty-one in record's full round: %s %+v (%s); want refused", changed.Kind, changed.Offset, changed.Why)
	}
}

// A clock read plus a constant offset, then cut, is a clock read
// (2026-10-28): an application that cuts its value coarser than its
// encoding writes (Go's Truncate(time.Second), stored as a millisecond
// $date) is learned at the cut its values show (offsetCut, Res), its
// offset bounded at it, and a replay checks its value at the cut, within
// one cut of the span plus the offset on either side, never refused for
// the cut. Thirty minutes cut to the second is learned whether the second
// turns inside its call's windows or not, in either order of the bounds;
// at a replay an hour on it holds at every read position in a second, and
// thirty-one, twenty-nine minutes and an hour never do. Two values that
// share half a second by chance share no step: learned at the millisecond.
// Forty-five or ninety seconds cut to the second reads in seconds:
// refused, since a cut to the unit makes a time to live changed by one
// look the same. Thirty minutes cut to the minute is refused as too wide
// (three minutes and its width are no narrower than the minute it reads
// in), so no replay holds thirty-one minutes; an hour cut to the minute is
// learned, at a millisecond's encoding and at a second's, and checked at
// the minute. A millisecond cut at a microsecond's encoding is learned at
// the millisecond, and two second values that share 2 s by chance at the
// second. A minute cut plus a time to live that is no whole number of
// minutes (its values a whole minute apart, neither a whole minute) is
// read at the minute: refused as too wide. Two uncut millisecond values a
// whole number of seconds apart by chance are learned at the second. A
// value a millisecond after its call reads in milliseconds: refused.
func TestAnOffsetCutCoarserThanItsEncodingIsCheckedAtTheCut(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms, sec := time.Millisecond, time.Second
	w := c.w.ComparesOffsets()

	// The recording's value 11:44:17, its window from 11:14:17.105 to
	// 11:14:17.155: from 29m59.845s up to 30m0.895s at the second; F's
	// 11:45:18, its window from 11:15:18.632 to 11:15:18.692: from
	// 29m59.308s up to 30m0.368s.
	out := models.ClockOffset{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: sec, Layout: models.LayoutTyped}
	if ln := c.classify(p, c.read.Truncate(sec).Add(30*time.Minute), c.fread.Truncate(sec).Add(30*time.Minute), ms, w); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != out || ln.Why != clockOffsetEvidence(p, out) {
		t.Errorf("FALSE RED: thirty minutes cut to the second, the second outside the windows: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, out)
	}
	// Windows the second turns in: the recording's from 11:14:16.980, its
	// read at 11:14:17.010; F's from 11:15:17.992, its read at 11:15:18.010.
	// From 29m59.845s up to 30m1.02s, and from 29m59.308s up to 30m1.008s.
	turn := (&Window{Rec: Span{From: c.call.Add(-175 * ms), To: c.call}, F: Span{From: c.fcall.Add(-700 * ms), To: c.fcall}}).ComparesOffsets()
	rec, live := c.call.Add(-145*ms).Truncate(sec).Add(30*time.Minute), c.fcall.Add(-682*ms).Truncate(sec).Add(30*time.Minute)
	want := models.ClockOffset{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 1008*ms, Res: sec, Layout: models.LayoutTyped}
	ln := c.classify(p, rec, live, ms, turn)
	if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || !strings.HasPrefix(ln.Why, p+" = clock + 30m (from 29m59.845s to 30m1.008s): ") {
		t.Errorf("FALSE RED: thirty minutes cut to the second, the second inside both windows: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}
	// The same windows the other way round, the recording's bound the
	// lesser: the same offset.
	mirror := clockOffsetCall{call: c.call.Add(537 * ms), fcall: c.fcall.Add(-537 * ms)}
	mirror.w = (&Window{Rec: Span{From: mirror.call.Add(-700 * ms), To: mirror.call}, F: Span{From: mirror.fcall.Add(-175 * ms), To: mirror.fcall}}).ComparesOffsets()
	if ln := mirror.classify(p, mirror.call.Add(-682*ms).Truncate(sec).Add(30*time.Minute), mirror.fcall.Add(-145*ms).Truncate(sec).Add(30*time.Minute), ms, mirror.w); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want {
		t.Errorf("FALSE RED: thirty minutes cut to the second, the recording's bound the lesser: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}
	// A replay an hour on, its window 50 ms wide, its read 20 ms into it,
	// the window started at each 10 ms of a second, its value cut to the
	// second too: thirty minutes holds at every one (no verdict turns on
	// when the replay ran), cut down or rounded; the others never do.
	base := c.call.Add(time.Hour).Truncate(sec)
	for step := time.Duration(0); step < sec; step += 10 * ms {
		from := base.Add(step)
		to, read := from.Add(50*ms), from.Add(20*ms)
		for _, row := range []struct {
			at   time.Time
			want bool
		}{
			{read.Add(30 * time.Minute).Truncate(sec), true}, {read.Add(30 * time.Minute).Round(sec), true},
			{read.Add(31 * time.Minute).Truncate(sec), false}, {read.Add(29 * time.Minute).Truncate(sec), false}, {read.Add(time.Hour).Truncate(sec), false},
		} {
			if got, _ := CheckClockRead(p, want, InstantLeaf(p, p, row.at, ms), Span{From: from, To: to}, true); got != row.want {
				t.Errorf("%s: an offset at a second's cut, a replay's read %s into its second, its value %s after it: holds %v, want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], read.Sub(read.Truncate(sec)), row.at.Sub(read), got, row.want)
			}
		}
	}
	// A value moved by more than one cut past the widened span is refused,
	// the reason naming the cut it was checked at.
	from := base.Add(500 * ms)
	moved := from.Add(50 * ms).Add(want.Max).Add(sec + ms)
	if ok, why := CheckClockRead(p, want, InstantLeaf(p, p, moved, ms), Span{From: from, To: from.Add(50 * ms)}, true); ok || !strings.Contains(why, "made in this call's window: it changed (checked as standing for the whole 1s the learn step read it cut to)") {
		t.Errorf("FALSE GREEN? a value a second and a millisecond past the widened span: %v (%s); want refused naming the cut", ok, why)
	}

	// Two values that share half a second by chance (an application that
	// does not cut them: its reads fell on 17.5 s and 19.0 s, 61.5 s apart):
	// half a second is no step, so they show no cut: learned at the
	// millisecond, the bounds as before. A replay reads its value at its
	// own millisecond: the read plus thirty minutes holds, a value 400 ms
	// earlier does not.
	half := time.Date(2026, 10, 26, 11, 14, 17, 500e6, time.UTC)
	hrec, hlive := half.Add(30*time.Minute), half.Add(61500*ms+30*time.Minute)
	if g := sharedGrain(hrec, hlive); g != 500*ms {
		t.Fatalf("precondition: the two values share %s, want half a second", g)
	}
	for _, row := range []struct {
		name string
		w    *Window
		want models.ClockOffset
	}{
		{"the recording's bound the lesser", &Window{Rec: Span{From: half.Add(-20 * ms), To: half.Add(30 * ms)}, F: Span{From: half.Add(61500*ms - 50*ms), To: half.Add(61500*ms + 10*ms)}}, models.ClockOffset{Min: 30*time.Minute - 10*ms, Max: 30*time.Minute + 21*ms, Res: ms, Layout: models.LayoutTyped}},
		{"F's bound the lesser", &Window{Rec: Span{From: half.Add(-30 * ms), To: half.Add(20 * ms)}, F: Span{From: half.Add(61500*ms - 10*ms), To: half.Add(61500*ms + 20*ms)}}, models.ClockOffset{Min: 30*time.Minute - 20*ms, Max: 30*time.Minute + 11*ms, Res: ms, Layout: models.LayoutTyped}},
	} {
		hc := clockOffsetCall{call: row.w.Rec.To, fcall: row.w.F.To}
		ln := hc.classify(p, hrec, hlive, ms, row.w.ComparesOffsets())
		if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != row.want || ln.Why != clockOffsetEvidence(p, row.want) {
			t.Errorf("half a second shared by chance, %s: %s %+v (%s); want the offset %+v", row.name, ln.Kind, ln.Offset, ln.Why, row.want)
			continue
		}
		from := half.Add(time.Hour)
		to, read := from.Add(50*ms), from.Add(20*ms)
		if !ln.Offset.Holds(read.Add(30*time.Minute), ms, from, to) || ln.Offset.Holds(read.Add(30*time.Minute-400*ms), ms, from, to) {
			t.Errorf("half a second shared by chance, %s: a replay's read plus thirty minutes must hold and 400 ms earlier must not: %v, %v", row.name, ln.Offset.Holds(read.Add(30*time.Minute), ms, from, to), ln.Offset.Holds(read.Add(30*time.Minute-400*ms), ms, from, to))
		}
	}

	// Cut to the second, a time to live of forty-five or ninety seconds
	// reads in seconds: refused, naming it, where one offset shows; refused
	// where none does.
	for _, ttl := range []time.Duration{45 * sec, 90 * sec} {
		ln := c.classify(p, c.call.Add(-145*ms).Truncate(sec).Add(ttl), c.fcall.Add(-682*ms).Truncate(sec).Add(ttl), ms, turn)
		if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, "but the offset reads in seconds, and an application that cuts its value to the second makes a time to live changed by a second look the same, so keploy refuses an offset that reads in a second or finer") {
			t.Errorf("FALSE GREEN? %s cut to the second: %s %+v (%s); want refused: it reads in seconds", ttl, ln.Kind, ln.Offset, ln.Why)
		}
		if ln := c.classify(p, c.read.Truncate(sec).Add(ttl), c.fread.Truncate(sec).Add(ttl), ms, w); ln.Kind != KindUnaccounted || ln.Offset != nil {
			t.Errorf("FALSE GREEN? %s cut to the second, the second outside the windows: %s %+v (%s); want refused", ttl, ln.Kind, ln.Offset, ln.Why)
		}
	}
	// Uncut, too: a time to live of forty-five seconds at the millisecond.
	if ln := c.classify(p, c.read.Add(45*sec).Truncate(ms), c.fread.Add(45*sec).Truncate(ms), ms, w); ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.HasPrefix(ln.Why, p+" = clock + 45s (") || !strings.Contains(ln.Why, "reads in seconds") {
		t.Errorf("forty-five seconds at the millisecond: %s %+v (%s); want refused: it reads in seconds", ln.Kind, ln.Offset, ln.Why)
	}

	// Cut to the minute: refused, in the windows the second turns in too.
	for _, win := range []*Window{w, turn} {
		if ln := c.classify(p, c.read.Truncate(time.Minute).Add(30*time.Minute), c.fread.Truncate(time.Minute).Add(30*time.Minute), ms, win); ln.Kind != KindUnaccounted || ln.Offset != nil {
			t.Errorf("FALSE GREEN? thirty minutes cut to the minute, stored at the millisecond: %s %+v (%s); want refused", ln.Kind, ln.Offset, ln.Why)
		}
	}
	// Windows the minute turns in: the recording's call at 11:15:00.020,
	// its window from 11:14:59.900, its read at 11:15:00.005; F's call at
	// 11:16:00.070, from 11:15:59.950, its read at 11:16:00.010. Bounded at
	// the minute both values show, from 29m59.98s up to 31m0.05s: no
	// narrower than the minute it reads in, refused, the reason saying the
	// values' cut set the resolution, not their encoding.
	minute := clockOffsetCall{call: time.Date(2026, 10, 26, 11, 15, 0, 20e6, time.UTC), fcall: time.Date(2026, 10, 26, 11, 16, 0, 70e6, time.UTC)}
	minute.w = (&Window{Rec: Span{From: minute.call.Add(-120 * ms), To: minute.call}, F: Span{From: minute.fcall.Add(-120 * ms), To: minute.fcall}}).ComparesOffsets()
	ln = minute.classify(p, minute.call.Add(-15*ms).Truncate(time.Minute).Add(30*time.Minute), minute.fcall.Add(-60*ms).Truncate(time.Minute).Add(30*time.Minute), ms, minute.w)
	if want := p + " = clock plus an offset from 29m59.98s to 31m0.05s at a resolution of 1m (the cut both runs' values show, coarser than the 1ms their encoding writes): "; ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.HasPrefix(ln.Why, want) || !strings.Contains(ln.Why, "not narrower than the minute the offset reads in") {
		t.Errorf("FALSE GREEN? thirty minutes cut to the minute, the minute inside both windows: %s %+v (%s); want refused with the reason %q...", ln.Kind, ln.Offset, ln.Why, want)
	}

	// An hour cut to the minute, in the same windows: from 59m59.98s up to
	// 1h1m0.05s, one offset in hours, narrow enough with three minutes:
	// learned at a millisecond's encoding and at a second's, and checked at
	// the minute at every read position in a minute.
	hour := models.ClockOffset{Min: time.Hour - 20*ms, Max: time.Hour + time.Minute + 50*ms, Res: time.Minute, Layout: models.LayoutTyped}
	for _, enc := range []time.Duration{ms, sec} {
		ln = minute.classify(p, minute.call.Add(-15*ms).Truncate(time.Minute).Add(time.Hour), minute.fcall.Add(-60*ms).Truncate(time.Minute).Add(time.Hour), enc, minute.w)
		if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != hour {
			t.Errorf("FALSE RED: an hour cut to the minute, stored at %s: %s %+v (%s); want the offset %+v", enc, ln.Kind, ln.Offset, ln.Why, hour)
		}
	}
	for step := time.Duration(0); step < time.Minute; step += 250 * ms {
		from := minute.call.Add(3 * time.Hour).Truncate(time.Minute).Add(step)
		to, read := from.Add(50*ms), from.Add(20*ms)
		for _, row := range []struct {
			ttl  time.Duration
			want bool
		}{{time.Hour, true}, {2 * time.Hour, false}, {time.Hour + 3*time.Minute, false}, {time.Hour - 3*time.Minute, false}} {
			if got := hour.Holds(read.Add(row.ttl).Truncate(time.Minute), ms, from, to); got != row.want {
				t.Errorf("%s: an hour cut to the minute, a replay's read %s into its minute plus %s: holds %v, want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], read.Sub(read.Truncate(time.Minute)), row.ttl, got, row.want)
			}
		}
	}
	// A millisecond cut at a microsecond's encoding (a JavaScript Date in a
	// Postgres timestamp): learned at the millisecond.
	msCut := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms, Res: ms, Layout: models.LayoutTyped}
	if ln := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms), time.Microsecond, w); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != msCut {
		t.Errorf("FALSE RED: thirty minutes cut to the millisecond, stored at the microsecond: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, msCut)
	}
	// Two values at a second's encoding that share 2 s by chance: 2 s is no
	// step, so learned at the second. The recording's read at 11:14:58.100,
	// its window from 11:14:58.090 to 11:14:58.140; F's at 11:16:00.000,
	// from 11:15:59.990 to 11:16:00.040: from 29m59.86s up to 30m0.91s, and
	// from 29m59.96s up to 30m1.01s.
	two := clockOffsetCall{call: time.Date(2026, 10, 26, 11, 14, 58, 140e6, time.UTC), fcall: time.Date(2026, 10, 26, 11, 16, 0, 40e6, time.UTC)}
	two.w = (&Window{Rec: Span{From: two.call.Add(-50 * ms), To: two.call}, F: Span{From: two.fcall.Add(-50 * ms), To: two.fcall}}).ComparesOffsets()
	trec, tlive := two.call.Add(-40*ms).Add(30*time.Minute).Truncate(sec), two.fcall.Add(-40*ms).Add(30*time.Minute).Truncate(sec)
	if g := sharedGrain(trec, tlive); g != 2*sec {
		t.Fatalf("precondition: the two second values share %s, want 2s", g)
	}
	twoWant := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 910*ms, Res: sec, Layout: models.LayoutTyped}
	if ln := two.classify(p, trec, tlive, sec, two.w); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != twoWant {
		t.Errorf("2 s shared by chance at a second's encoding: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, twoWant)
	}

	// B1 of the offset rule's third review: a minute cut plus a time to
	// live that is no whole number of minutes, at a second's encoding (a
	// BSON timestamp), its windows about half a minute wide. Plus 30m30s
	// the two values share 30 s, plus 30m17s only the second, and both
	// stand a whole minute apart: one offset at the second holds both runs
	// (from 29m30.9s up to 30m0.5s, and from 29m58.9s up to 30m28.5s), and
	// one at the minute too, so keploy cannot tell at which cut a replay
	// must check them: refused, naming both (offsetReading).
	for _, row := range []struct {
		ttl  time.Duration
		read time.Time
		want models.ClockOffset
	}{
		{30*time.Minute + 30*sec, time.Date(2026, 10, 26, 11, 14, 58, 500e6, time.UTC), models.ClockOffset{Min: 30*time.Minute - 29100*ms, Max: 30*time.Minute + 500*ms, Res: sec, Layout: models.LayoutTyped}},
		{30*time.Minute + 17*sec, time.Date(2026, 10, 26, 11, 14, 17, 500e6, time.UTC), models.ClockOffset{Min: 30*time.Minute - 1100*ms, Max: 30*time.Minute + 28500*ms, Res: sec, Layout: models.LayoutTyped}},
	} {
		fread := row.read.Add(61 * sec)
		mc := clockOffsetCall{call: row.read.Add(600 * ms), fcall: fread.Add(600 * ms)}
		mc.w = (&Window{Rec: Span{From: row.read.Add(-28 * sec), To: mc.call}, F: Span{From: fread.Add(-29 * sec), To: mc.fcall}}).ComparesOffsets()
		ln := mc.classify(p, row.read.Truncate(time.Minute).Add(row.ttl), fread.Truncate(time.Minute).Add(row.ttl), sec, mc.w)
		if want := cutAmbiguousWhy(p, row.want, time.Minute, time.Minute); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != want || !strings.Contains(ln.Why, "but its two values stand 1m apart, a whole number of 1m: an application that cuts its value to 1m") {
			t.Errorf("FALSE GREEN? a minute cut plus %s at a second's encoding: %s %+v (%s); want refused: %s", row.ttl, ln.Kind, ln.Offset, ln.Why, want)
		}
	}
	// Two millisecond values the application does not cut that stand a
	// whole number of seconds apart by chance (62 s, a slow call's): one
	// offset at the millisecond holds both, and one at the second could:
	// refused, naming both.
	apart := clockOffsetCall{call: c.call, fcall: c.call.Add(62005 * ms)}
	apart.w = (&Window{Rec: Span{From: apart.call.Add(-50 * ms), To: apart.call}, F: Span{From: apart.fcall.Add(-60 * ms), To: apart.fcall}}).ComparesOffsets()
	apartAt := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms, Res: ms, Layout: models.LayoutTyped}
	if ln := apart.classify(p, apart.call.Add(-40*ms+30*time.Minute), apart.fcall.Add(-45*ms+30*time.Minute), ms, apart.w); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != cutAmbiguousWhy(p, apartAt, 62*sec, sec) {
		t.Errorf("two millisecond values 62 s apart: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, cutAmbiguousWhy(p, apartAt, 62*sec, sec))
	}

	// Thirty minutes cut to the second, each window 57 s wide: from
	// 29m59.7s up to 30m57.155s, 57.455 s wide, which with one second is
	// narrower than the minute it reads in, but with the three a replay's
	// check spans (one cut each side and the cut's start) is not: refused as
	// too wide, naming the range.
	long := clockOffsetCall{call: time.Date(2026, 10, 26, 11, 14, 17, 300e6, time.UTC)}
	long.fcall = long.call.Add(FullRoundLead)
	long.w = (&Window{Rec: Span{From: long.call.Add(-57 * sec), To: long.call}, F: Span{From: long.fcall.Add(-57 * sec), To: long.fcall}}).ComparesOffsets()
	longWant := models.ClockOffset{Min: 30*time.Minute - 300*ms, Max: 30*time.Minute + 57155*ms, Res: sec, Layout: models.LayoutTyped}
	if ln := long.classify(p, long.call.Truncate(sec).Add(30*time.Minute), long.fcall.Truncate(sec).Add(30*time.Minute), ms, long.w); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != clockOffsetWideWhy(p, longWant, ms) {
		t.Errorf("FALSE GREEN? thirty minutes cut to the second, windows 57 s wide: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, clockOffsetWideWhy(p, longWant, ms))
	}

	// N2: a value a millisecond after its call in both runs: from 1 ms up
	// to 52 ms, which reads in milliseconds.
	ln = c.classify(p, c.call.Add(ms), c.fcall.Add(ms), ms, w)
	if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.HasPrefix(ln.Why, p+" = clock plus an offset from 1ms to 52ms: ") || !strings.Contains(ln.Why, "reads in milliseconds") {
		t.Errorf("a value a millisecond after its call: %s %+v (%s); want refused: it reads in milliseconds", ln.Kind, ln.Offset, ln.Why)
	}
}

// The acceptance of 2026-10-28: eui's expiresAt, "2026-11-26T16:59:10.000Z",
// the call's clock plus thirty days with its milliseconds zeroed, stored at
// the millisecond. Its test called at 16:59:10.1, its window 300 ms wide;
// record's full round called at 17:01:26.3 (in one minute) or 17:02:03.3
// (across one). Both are learned as clock + 30d at the second, and every
// replay a day on, its read anywhere in its second, holds the read plus
// thirty days cut down and the widened span's start (a second before the
// second that holds its window's start plus the offset); a millisecond
// past either edge of the widened span, the recorded value, one three
// seconds stale or four ahead of the read, and twenty-nine or thirty-one
// days never do. With record's full round at the lead, its read up to half
// a second either way, or two seconds early, against the recording's, it
// is learned at the second; with F's read 1.4 s early its value stands a
// whole minute after the recording's, which a minute's cut plus 30d and
// some seconds writes too: refused, naming both cuts.
func TestAnExpiryOfThirtyDaysWithItsMillisecondsZeroedIsKeptAndCheckedAtTheSecond(t *testing.T) {
	const p = "body.expiresAt"
	const month = 30 * 24 * time.Hour
	ms, sec := time.Millisecond, time.Second
	call := time.Date(2026, 10, 27, 16, 59, 10, 100e6, time.UTC)
	rec := call.Add(month).Truncate(sec)
	if got := rec.Format("2006-01-02T15:04:05.000Z07:00"); got != "2026-11-26T16:59:10.000Z" {
		t.Fatalf("precondition: the recorded value is %s, want eui's", got)
	}
	for _, f := range []struct {
		name  string
		fcall time.Time
	}{
		{"a full round in one minute", time.Date(2026, 10, 27, 17, 1, 26, 300e6, time.UTC)},
		{"a full round across a minute", time.Date(2026, 10, 27, 17, 2, 3, 300e6, time.UTC)},
	} {
		oc := clockOffsetCall{call: call, fcall: f.fcall}
		oc.w = (&Window{Rec: Span{From: call.Add(-300 * ms), To: call}, F: Span{From: f.fcall.Add(-300 * ms), To: f.fcall}}).ComparesOffsets()
		ln := oc.classify(p, rec, f.fcall.Add(month).Truncate(sec), ms, oc.w)
		want := models.ClockOffset{Min: month - 100*ms, Max: month + sec, Res: sec, Layout: models.LayoutTyped}
		if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || !strings.HasPrefix(ln.Why, p+" = clock + 30d (") {
			t.Errorf("FALSE RED: eui's expiresAt, %s: %s %+v (%s); want the offset %+v", f.name, ln.Kind, ln.Offset, ln.Why, want)
			continue
		}
		base := call.Add(26 * time.Hour).Truncate(sec)
		for step := time.Duration(0); step < sec; step += 10 * ms {
			from := base.Add(step)
			to, read := from.Add(300*ms), from.Add(150*ms)
			lo, hi := from.Add(want.Min).Truncate(sec).Add(-sec), to.Add(want.Max).Add(sec)
			if ok, _ := CheckClockRead(p, want, InstantLeaf(p, p, lo, ms), Span{From: from, To: to}, true); !ok {
				t.Errorf("FALSE RED: eui's expiresAt (%s), the widened span's start: refused", f.name)
			}
			if ok, _ := CheckClockRead(p, want, InstantLeaf(p, p, lo.Add(-ms), ms), Span{From: from, To: to}, true); ok {
				t.Errorf("FALSE GREEN: eui's expiresAt (%s), a millisecond before the widened span: passed", f.name)
			}
			if ok, _ := CheckClockRead(p, want, InstantLeaf(p, p, hi.Add(ms), ms), Span{From: from, To: to}, true); ok {
				t.Errorf("FALSE GREEN: eui's expiresAt (%s), a millisecond after the widened span: passed", f.name)
			}
			for _, row := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"the read plus thirty days, cut", read.Add(month).Truncate(sec), true},
				{"the recorded value", rec, false},
				{"three seconds stale", read.Add(month).Truncate(sec).Add(-3 * sec), false},
				{"four seconds ahead", read.Add(month).Truncate(sec).Add(4 * sec), false},
				{"twenty-nine days", read.Add(month - 24*time.Hour).Truncate(sec), false},
				{"thirty-one days", read.Add(month + 24*time.Hour).Truncate(sec), false},
			} {
				if got, why := CheckClockRead(p, *ln.Offset, InstantLeaf(p, p, row.at, ms), Span{From: from, To: to}, true); got != row.want {
					t.Errorf("%s: eui's expiresAt (%s), %s, a replay's read %s into its second: holds %v (%s), want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], f.name, row.name, read.Sub(read.Truncate(sec)), got, why, row.want)
				}
			}
		}
	}
	for _, drift := range []time.Duration{-2 * sec, -500 * ms, 0, 500 * ms, -1400 * ms} {
		fcall := call.Add(FullRoundLead + drift)
		oc := clockOffsetCall{call: call, fcall: fcall}
		oc.w = (&Window{Rec: Span{From: call.Add(-300 * ms), To: call}, F: Span{From: fcall.Add(-300 * ms), To: fcall}}).ComparesOffsets()
		ln := oc.classify(p, rec, fcall.Add(month).Truncate(sec), ms, oc.w)
		if drift == -1400*ms {
			if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, "but its two values stand 1m apart, a whole number of 1m") {
				t.Errorf("eui's expiresAt, F's read 1.4 s early, its value a whole minute on: %s %+v (%s); want refused, naming both cuts", ln.Kind, ln.Offset, ln.Why)
			}
			continue
		}
		if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.Res != sec || ln.Offset.Min > month || ln.Offset.Max <= month {
			t.Errorf("FALSE RED: eui's expiresAt, F's read %s off the lead: %s %+v (%s); want clock + 30d at the second", drift, ln.Kind, ln.Offset, ln.Why)
		}
	}
}

// B1 and S2 of the review of the cut's first fix: two values cut to an hour
// (its boundaries on a zone's half hour, which the epoch reads as thirty
// minutes) show no offset (offsetCut: they stand a whole hour apart), even
// where windows over an hour wide put one offset in both runs' bounds:
// thirty minutes would read the hour cut finer than it is, and a time to
// live changed by an hour could hold. A value that did not move (its text
// drifted, not its moment) shows none either. Two values cut to the minute
// that share two minutes, more than FullRoundLead, show one, at the minute.
func TestACutCoarserThanTheLeadShowsNoOffset(t *testing.T) {
	ms := time.Millisecond
	// UTC+5:30: the local hour turns at :30 UTC. The recording reads at
	// 12:29:59, its value 11:30 + 2h; F reads FullRoundLead later, past the turn,
	// its value 12:30 + 2h. Each window opens 70 minutes before its read.
	readR := time.Date(2026, 10, 26, 12, 29, 59, 0, time.UTC)
	readF := readR.Add(FullRoundLead)
	cutR, cutF := time.Date(2026, 10, 26, 11, 30, 0, 0, time.UTC), time.Date(2026, 10, 26, 12, 30, 0, 0, time.UTC)
	r := typedInstant{at: cutR.Add(2 * time.Hour), res: ms, names: namesMoment}
	l := typedInstant{at: cutF.Add(2 * time.Hour), res: ms, names: namesMoment}
	w := &Window{Rec: Span{From: readR.Add(-70 * time.Minute), To: readR.Add(50 * ms)}, F: Span{From: readF.Add(-70 * time.Minute), To: readF.Add(50 * ms)}}
	if g := sharedGrain(r.at, l.at); g != 30*time.Minute {
		t.Fatalf("precondition: the two values share %s, want thirty minutes", g)
	}
	if o, ok := clockOffsetOf(r, l, w); ok {
		t.Errorf("FALSE GREEN? an hour cut on a zone's half hour: %+v (%s); want no offset", o, o)
	}
	if o, ok := clockOffsetOf(r, r, w); ok {
		t.Errorf("a value that did not move: %+v; want no offset", o)
	}
	// N1 of the offset rule's third review: Truncate(time.Hour) plus ninety
	// minutes, in UTC. The recording reads at 12:59:59, its value 13:30; F
	// reads FullRoundLead later, past the hour, its value 14:30. The two share
	// thirty minutes, and windows 70 minutes wide put one range of offsets in
	// both: no offset.
	readR = time.Date(2026, 10, 26, 12, 59, 59, 0, time.UTC)
	readF = readR.Add(FullRoundLead)
	r.at, l.at = readR.Truncate(time.Hour).Add(90*time.Minute), readF.Truncate(time.Hour).Add(90*time.Minute)
	w = &Window{Rec: Span{From: readR.Add(-70 * time.Minute), To: readR.Add(50 * ms)}, F: Span{From: readF.Add(-70 * time.Minute), To: readF.Add(50 * ms)}}
	if g := sharedGrain(r.at, l.at); g != 30*time.Minute {
		t.Fatalf("precondition: an hour cut plus ninety minutes shares %s, want thirty minutes", g)
	}
	if o, ok := clockOffsetOf(r, l, w); ok {
		t.Errorf("FALSE GREEN? an hour cut plus ninety minutes: %+v (%s); want no offset", o, o)
	}
	// The recording reads at 12:02:58.5, F FullRoundLead later, past two
	// minutes' turns: 14:02 and 14:04, two hours on, share two minutes.
	readR = time.Date(2026, 10, 26, 12, 2, 58, 500e6, time.UTC)
	readF = readR.Add(FullRoundLead)
	r.at, l.at = readR.Truncate(time.Minute).Add(2*time.Hour), readF.Truncate(time.Minute).Add(2*time.Hour)
	w = &Window{Rec: Span{From: readR.Add(-50 * ms), To: readR.Add(10 * ms)}, F: Span{From: readF.Add(-50 * ms), To: readF.Add(10 * ms)}}
	if g := sharedGrain(r.at, l.at); g != 2*time.Minute {
		t.Fatalf("precondition: the two minute values share %s, want two minutes", g)
	}
	if o, ok := clockOffsetOf(r, l, w); !ok || o.Res != time.Minute || o.Min >= 2*time.Hour || o.Max <= 2*time.Hour {
		t.Errorf("FALSE RED: two hours cut to the minute, its values sharing two minutes: %+v, %v; want two hours at the minute", o, ok)
	}
}

// N2 of the review of the cut's first fix: a reason says what set the
// resolution it names.
func TestAReasonSaysWhatSetTheResolution(t *testing.T) {
	ms := time.Millisecond
	if got, want := resolutionOf(time.Second, ms), "1s (the cut both runs' values show, coarser than the 1ms their encoding writes)"; got != want {
		t.Errorf("a shared second: %q, want %q", got, want)
	}
	if got, want := resolutionOf(time.Minute, ms), "1m (the cut both runs' values show, coarser than the 1ms their encoding writes)"; got != want {
		t.Errorf("a shared minute: %q, want %q", got, want)
	}
	if got, want := resolutionOf(ms, ms), "1ms (the one its encoding writes)"; got != want {
		t.Errorf("the encoding's: %q, want %q", got, want)
	}
	if aUnit(time.Hour) != "an hour" || aUnit(time.Minute) != "a minute" {
		t.Errorf("articles: %q, %q", aUnit(time.Hour), aUnit(time.Minute))
	}
}

// An offset's cut (offsetReading) is the one both values show; where their
// gap is a whole multiple of a coarser step, that step where the values'
// own cut puts no offset in both runs, and refused, naming both, where it
// does. Uncut millisecond values a fast call's lead apart: the
// millisecond; two whole seconds, a fast call's: the second; a minute's
// start plus 30m30s, narrow windows: the minute (the second holds no one
// offset); 10 ms values at a microsecond's encoding: 10 ms; at a
// millisecond's: the millisecond (a decimal step counts only at 1,000
// times the encoding: such a writer is checked at its encoding, the flake
// the PR text states); an hour apart on the half hour: refused naming the
// hour; a value that did not move: none, no reason.
func TestAnOffsetsCutIsTheOneItsValuesShowOrItsGapsWhereTheirsHoldsNone(t *testing.T) {
	us, ms, sec, min := time.Microsecond, time.Millisecond, time.Second, time.Minute
	read := time.Date(2026, 10, 26, 11, 14, 17, 123e6, time.UTC)
	fread := read.Add(FullRoundLead)
	narrow := &Window{Rec: Span{From: read.Add(-20 * ms), To: read.Add(20 * ms)}, F: Span{From: fread.Add(-20 * ms), To: fread.Add(20 * ms)}}
	in := func(at time.Time, enc time.Duration) typedInstant {
		return typedInstant{at: at, res: enc, names: namesMoment}
	}
	for _, row := range []struct {
		name string
		enc  time.Duration
		a, b time.Time
		w    *Window
		want time.Duration
		why  string
	}{
		{"uncut, a fast call's lead apart", ms, read.Add(time.Hour), fread.Add(time.Hour), narrow, ms, ""},
		{"two whole seconds", ms, read.Add(time.Hour).Truncate(sec), fread.Add(time.Hour).Truncate(sec), narrow, sec, ""},
		{"a minute's start plus 30m30s", sec, read.Truncate(min).Add(30*min + 30*sec), fread.Truncate(min).Add(30*min + 30*sec), narrow, min, ""},
		{"10 ms at a microsecond's encoding", us, read.Add(time.Hour).Truncate(10 * ms), fread.Add(time.Hour).Truncate(10 * ms), narrow, 10 * ms, ""},
		{"10 ms at a millisecond's encoding", ms, read.Add(time.Hour).Truncate(10 * ms), fread.Add(time.Hour).Truncate(10 * ms), narrow, ms, ""},
		{"an hour cut on the half hour, plus two hours", ms, time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC), time.Date(2026, 10, 26, 14, 30, 0, 0, time.UTC), &Window{Rec: Span{From: time.Date(2026, 10, 26, 12, 29, 58, 950e6, time.UTC), To: time.Date(2026, 10, 26, 12, 29, 59, 50e6, time.UTC)}, F: Span{From: time.Date(2026, 10, 26, 12, 31, 0, 495e6, time.UTC), To: time.Date(2026, 10, 26, 12, 31, 0, 595e6, time.UTC)}}, 0, "cut to 1h, but keploy checks a clock read plus an offset only at a cut of a minute or finer"},
		{"a value that did not move", ms, read.Add(time.Hour), read.Add(time.Hour), narrow, 0, ""},
	} {
		o, why, ok := offsetReading("p", in(row.a, row.enc), in(row.b, row.enc), row.w)
		if row.want != 0 && (!ok || why != "" || o.Res != row.want) || row.want == 0 && (ok || !strings.Contains(why, row.why) || (row.why == "") != (why == "")) {
			t.Errorf("%s: %+v %v (%s); want the cut %s, why %q", row.name, o, ok, why, row.want, row.why)
		}
	}
}

// sharedGrain is the greatest duration both values are whole multiples of,
// from the Unix epoch: a value's own encoding's (a millisecond), the second
// or the minute an application cut both to, the value itself for two equal
// values, and the same for values before the epoch.
func TestTheGranularityTwoValuesShare(t *testing.T) {
	at := time.Date(2026, 10, 26, 11, 44, 17, 115e6, time.UTC)
	for _, row := range []struct {
		name string
		a, b time.Time
		want time.Duration
	}{
		{"two milliseconds", at, at.Add(FullRoundLead + 1532*time.Millisecond), time.Millisecond},
		{"cut to the second", at.Truncate(time.Second), at.Add(FullRoundLead).Truncate(time.Second), time.Second},
		{"cut to the minute", at.Truncate(time.Minute), at.Add(FullRoundLead).Truncate(time.Minute), time.Minute},
		{"equal", at, at, time.Duration(at.UnixNano())},
		{"before the epoch", time.Unix(-3, 0), time.Unix(-5, 0), time.Second},
		{"either side of the epoch", time.Unix(5, 0), time.Unix(-3, 0), time.Second},
	} {
		if got := sharedGrain(row.a, row.b); got != row.want {
			t.Errorf("%s: %s, want %s", row.name, got, row.want)
		}
	}
}

// A read of the clock in its call's window (no offset) stays noise for a
// caller that compares offsets; a clock-plus-offset instant the call's
// recorded answer holds is refused naming the offset (keploy would serve
// the answer's recorded moment), and so is one of a test whose own request
// keploy could not read, as noise there is.
func TestAClockOffsetLeavesAPlainReadAloneAndRefusesOneTheAnswerHolds(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	if ln := c.classify(p, c.read.Truncate(ms), c.fread.Truncate(ms), ms, c.w.ComparesOffsets()); !noiseOrRead(ln) || ln.Offset != nil {
		t.Errorf("a read in its window: %s %+v (%s); want noise", ln.Kind, ln.Offset, ln.Why)
	}
	// A read made 2 ms before its window in both runs (before the previous
	// call: a boot that ran long) moved with the clock, but by how long ago
	// it was made, not by an offset: refused as before, never an offset.
	before := func(s Span) time.Time { return s.From.Add(-2 * ms) }
	if ln := c.classify(p, before(c.w.Rec), before(c.w.F).Add(FullRoundLead), ms, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != instantNotClock {
		t.Errorf("a read before its window: %s %+v (%s); want refused as before: %s", ln.Kind, ln.Offset, ln.Why, instantNotClock)
	}
	// Later than its call when recorded, but not in record's full round:
	// no constant offset, refused as before.
	if ln := c.classify(p, c.call.Add(ms), c.fcall.Add(-10*ms), ms, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != instantAfterCall {
		t.Errorf("later than its call when recorded alone: %s %+v (%s); want refused as before: %s", ln.Kind, ln.Offset, ln.Why, instantAfterCall)
	}
	rec := c.read.Add(30 * time.Minute).Truncate(ms)
	ln := c.classify(p, rec, c.fread.Add(30*time.Minute).Truncate(ms), ms, c.w.ComparesOffsets(), InstantLeaf("reply.expiresAt", "reply.expiresAt", rec, ms))
	if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.HasPrefix(ln.Why, p+" = clock + 30m") || !strings.Contains(ln.Why, "recorded answer holds") {
		t.Errorf("held by the recorded answer: %s %+v (%s); want refused naming the offset and the answer", ln.Kind, ln.Offset, ln.Why)
	}
	live := InstantLeaf(p, p, c.fread.Add(30*time.Minute).Truncate(ms), ms)
	ln = ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, rec, ms)}}, c.call, []Earlier{GivenRequest(nil)}, nil, nil, c.w.ComparesOffsets())[0]
	if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != unreadRequestWhy(unsentRequest) {
		t.Errorf("the test case's request unread: %s %+v (%s); want refused: %s", ln.Kind, ln.Offset, ln.Why, unreadRequestWhy(unsentRequest))
	}
}

// An instant its protocol types (a Mongo $date) in a call with no window (a
// test's own call, read by the lead alone) that the lead did not move, as
// when record's full round read its clock late on a loaded box, is a plain
// read of the application's clock all the same, read whole, which every
// replay checks in its call's own span: whether the lead moved it must not
// decide what is learned (noWindowRead). The confirming round's check passes
// a read of its own clock and refuses one that is not (the recorded value
// again, an instant the application built on what the test gave it), naming
// the field. One recorded later than its call and one echoed in its own
// answer (answered with this run's value, never checked) stay refused, and
// so does one in a client test. A wall clock of no zone is a plain read too,
// in the zone of its recorded reading nearest its call (here UTC).
func TestATypedInstantWithNoCallWindowIsAClockReadCheckedAtReplay(t *testing.T) {
	rec := time.Date(2026, 10, 27, 9, 0, 0, 123e6, time.UTC)
	call := rec.Add(time.Millisecond)
	const p = "body.u.processed_at"
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{p: l}} }
	ms := func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Millisecond) }
	classify := func(dc *DriftCache, r, l Leaf, at time.Time, resp []Leaf) Learned {
		return ClassifyIn(dc, []string{p}, shape(l), shape(r), at, nil, nil, resp, nil)[0]
	}
	late := rec.Add(FullRoundLead + 3400*time.Millisecond)
	for _, row := range []struct {
		what string
		l    time.Time
	}{{"read late", late}, {"read early", rec.Add(FullRoundLead - 400*time.Millisecond)}, {"moved by the lead", rec.Add(FullRoundLead)}} {
		if ln := classify(clockedDrifts(), ms(rec), ms(row.l), call, nil); !plainRead(ln) {
			t.Errorf("a $date with no window, %s: %s Read=%+v (%s); want a plain read of the clock, checked at replay", row.what, ln.Kind, ln.Read, ln.Why)
		}
	}
	o, ok := classify(clockedDrifts(), ms(rec), ms(late), call, nil).Clock()
	if !ok {
		t.Fatal("a $date with no window learned no read of the clock")
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond)}
	if ok, why := CheckClockRead(p, o, ms(now.Add(5*time.Millisecond)), span, true); !ok {
		t.Errorf("the confirming round's read of its own clock: refused (%s); want it passed", why)
	}
	// It carries the protocol's type as its layout: a text of the same
	// instant at replay is another form, refused.
	if o.Layout != models.LayoutTyped {
		t.Errorf("a typed read's layout = %q; want %q", o.Layout, models.LayoutTyped)
	}
	if ok, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: now.Add(5 * time.Millisecond).Format("2006-01-02T15:04:05.000Z")}, span, true); ok || !strings.Contains(why, "in another form") {
		t.Errorf("a typed read, its instant as a text at replay: %v (%s); want refused, naming the form", ok, why)
	}
	if ok, why := CheckClockRead(p, o, ms(rec), span, true); ok || !strings.HasPrefix(why, p+" was learned as clock") || !strings.Contains(why, "it changed") {
		t.Errorf("the confirming round's recorded value again: ok=%v (%s); want it refused, naming %s", ok, why, p)
	}
	if ln := classify(clockedDrifts(), ms(rec), ms(late), rec.Add(-time.Hour), nil); ln.Kind != KindUnaccounted || ln.Why != instantNotClock {
		t.Errorf("a $date recorded an hour after its call: %s (%s); want refused: %s", ln.Kind, ln.Why, instantNotClock)
	}
	answer := []Leaf{{Path: "body.doc.processed_at", Type: Instant, Value: ms(rec).Value}}
	if ln := classify(clockedDrifts(), ms(rec), ms(late), call, answer); ln.Kind != KindUnaccounted || len(ln.Echo) != 0 || ln.Why != instantNotClock {
		t.Errorf("a $date its own answer echoes: %s (echo %+v, %s); want refused: %s", ln.Kind, ln.Echo, ln.Why, instantNotClock)
	}
	// A wall clock of no zone with no window: a plain read of the clock
	// too, in UTC where the learn step reads such a clock in UTC alone.
	wall := func(at time.Time) Leaf { return WallClockLeaf(p, p, at, time.Millisecond) }
	if ln := classify(clockedDrifts(), wall(rec), wall(late), call, nil); !zonedRead(ln, models.ZoneUTC) {
		t.Errorf("a wall clock of no zone with no window: %s Read=%+v (%s); want a plain read in UTC", ln.Kind, ln.Read, ln.Why)
	}
	client := clockedDrifts()
	client.clientTests = true
	if ln := classify(client, ms(rec), ms(late), call, nil); ln.Kind != KindUnaccounted {
		t.Errorf("a $date with no window in a client test: %s Read=%+v (%s); want refused", ln.Kind, ln.Read, ln.Why)
	}
}

// A plain read of the clock is learned at its encoding's resolution (a
// $date's millisecond), never at the granularity its two values happen to
// share (readRes): two millisecond values both whole multiples of 492 ms by
// chance, and the same recording with a value of record's full round that
// shares only a millisecond, learn the same resolution, so a replay's
// window is the same whichever value the full round read, and a stamp
// 400 ms before its call's span is refused under each. A text one and a
// time of day the same. A value the application cut to the second (both
// whole seconds) is learned at the second's cut, whatever more the two
// share (7 s, or 2 s).
func TestAPlainClockReadsResolutionIsItsEncodingsNotAChanceGrain(t *testing.T) {
	const p = "body.u.processed_at"
	ms := time.Millisecond
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	rec := time.UnixMilli(base.UnixMilli()/492*492 + 492).UTC()
	chance, fine := rec.Add(126*492*ms), rec.Add(126*492*ms+ms)
	if g, f := sharedGrain(rec, chance), sharedGrain(rec, fine); g < 492*ms || f >= 492*ms {
		t.Fatalf("the values share %v and %v; want at least 492ms and less", g, f)
	}
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{p: l}} }
	typed := func(at time.Time) Leaf { return InstantLeaf(p, p, at, ms) }
	learn := func(r, l time.Time) models.ClockOffset {
		t.Helper()
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(typed(l)), shape(typed(r)), r.Add(ms), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok {
			t.Fatalf("a $date with no window, %v then %v: %s Read=%+v (%s); want a plain read of the clock", r, l, ln.Kind, ln.Read, ln.Why)
		}
		return o
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * ms)}
	for _, row := range []struct {
		what string
		l    time.Time
	}{{"sharing 492ms by chance", chance}, {"sharing a millisecond", fine}} {
		o := learn(rec, row.l)
		if o.Res != ms {
			t.Errorf("%s: Res = %v; want the 1ms a $date writes", row.what, o.Res)
		}
		if ok, why := CheckClockRead(p, o, typed(now.Add(-400*ms)), span, true); ok || !strings.Contains(why, "it changed") {
			t.Errorf("%s, a stamp 400ms before the call's span: %v (%s); want refused", row.what, ok, why)
		}
		if ok, why := CheckClockRead(p, o, typed(now.Add(5*ms)), span, true); !ok {
			t.Errorf("%s, a read in the call's span: refused (%s); want it passed", row.what, why)
		}
	}
	const lay = "2006-01-02T15:04:05.000Z"
	for _, l := range []time.Time{chance, fine} {
		if r := textClockRead(rec.Format(lay), l.Format(lay)); r == nil || r.Res != ms {
			t.Errorf("a text read, %v then %v: %+v; want a Res of 1ms", rec, l, r)
		}
	}
	rtod := 9*time.Hour + 960*ms
	for _, ltod := range []time.Duration{rtod + 129*480*ms, rtod + 129*480*ms + ms} {
		if r := clockedDrifts().todRead(rtod, ltod, ms, ms, models.LayoutTypedTimeOfDay, models.LayoutTypedTimeOfDay, nil, rec); r.Res != ms {
			t.Errorf("a time of day, %v then %v (sharing %v): Res = %v; want 1ms", rtod, ltod, todGrain(rtod, ltod), r.Res)
		}
	}
	if g := todGrain(rtod, rtod+129*480*ms); g < 480*ms {
		t.Fatalf("the times of day share %v; want at least 480ms", g)
	}
	// Above a day no unit is above the encoding, so two dates that share
	// two days by chance are no cut: read at the day.
	if got := readRes(24*time.Hour, 48*time.Hour); got != 24*time.Hour {
		t.Errorf("readRes of a day's encoding sharing two days = %v; want the day", got)
	}
	// Cut to the second: 7 s shared, or 2 s, learned alike at the second's
	// cut (readRes), never at the 7 s the two share by chance.
	sec := time.Unix(base.Unix()/7*7+7, 0).UTC()
	for _, l := range []time.Time{sec.Add(63 * time.Second), sec.Add(62 * time.Second)} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(typed(l)), shape(typed(sec)), sec.Add(ms), nil, nil, nil, nil)[0]
		if ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Res != time.Second {
			t.Errorf("cut to the second, %v then %v (sharing %v): %s Read=%+v (%s); want learned at the 1s cut", sec, l, sharedGrain(sec, l), ln.Kind, ln.Read, ln.Why)
		}
	}
}

// A text's fraction digits are no evidence of the resolution its writer
// writes: a writer that drops trailing zeros (Go's RFC3339Nano, Java's
// Instant) writes ".495123" where its read fell on a whole microsecond. So a
// text read is learned at the finer of its two values' digits (textReadRes),
// never coarser because one of them happened to end in zeros, and a
// nine-digit replay value passes. A replay's value with fewer digits passes
// where it is the same instant at its own precision (the writer dropped its
// zeros, 2026-10-27). Two values that both end in zeros at a step of a
// millisecond or finer are read at it, and a replay reads the cut by its
// own value: one that shows none is checked at its own resolution, one that
// shows a cut at that cut. A value the application cuts to the second is
// learned at the cut (founder decision, 2026-10-27): written with nine
// digits on both sides, with three and six, as a time of day, or as a
// $date; and an epoch in seconds with a fraction is read exactly, never
// through a float.
func TestATextClockReadsFractionDigitsAreNoEvidenceOfItsResolution(t *testing.T) {
	const p = "body.at"
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	learn := func(rec, live string) models.ClockOffset {
		t.Helper()
		ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: str(live)}}, Shape{Leaves: map[string]Leaf{p: str(rec)}}, call, nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok {
			t.Fatalf("%s then %s: %s Read=%+v (%s); want a plain read of the clock", rec, live, ln.Kind, ln.Read, ln.Why)
		}
		return o
	}
	// cutAt asserts the learn step learns rec then live at cut, the coarse
	// cut both show above the finer of their encodings, and returns it.
	cutAt := func(what, rec, live string, cut time.Duration) models.ClockOffset {
		t.Helper()
		o := learn(rec, live)
		if o.Res != cut {
			t.Errorf("%s, %s then %s: Res = %v; want learned at the %v cut", what, rec, live, o.Res, cut)
		}
		return o
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond)}
	check := func(what string, o models.ClockOffset, v string, want bool, whyHas string) {
		t.Helper()
		ok, why := CheckClockRead(p, o, str(v), span, true)
		if ok != want || !want && !strings.Contains(why, whyHas) {
			t.Errorf("%s, replay %s: %v (%s); want %v %q", what, v, ok, why, want, whyHas)
		}
	}
	six, nine := "2026-10-27T03:30:00.495123Z", "2026-10-27T03:31:02.032456789Z"
	for _, pr := range [][2]string{{six, nine}, {nine, six}} {
		o := learn(pr[0], pr[1])
		if o.Res != time.Nanosecond {
			t.Errorf("%s then %s: Res = %v; want the 1ns of the value with nine digits", pr[0], pr[1], o.Res)
		}
		what := "one value ending in 000ns"
		check(what, o, "2026-10-28T07:30:00.005123456Z", true, "")
		check(what, o, "2026-10-28T07:30:00.00512345Z", true, "")
		check(what+", fewer digits", o, "2026-10-28T07:30:00.005123Z", true, "")
		check(what+", before the span", o, "2026-10-28T07:29:59.995123456Z", false, "it changed")
	}
	// Both values happened to end in zeros: learned at the microsecond
	// their digits show. A replay reads the cut by its own value: a
	// nine-digit value that is no whole microsecond shows none and is
	// checked at its own nanosecond, one that is one is checked at the
	// microsecond; neither is refused as a cut, and one before the span is
	// refused as changed.
	both := learn(six, "2026-10-27T03:31:02.032456Z")
	if both.Res != time.Microsecond {
		t.Errorf("both values with six digits: Res = %v; want 1µs", both.Res)
	}
	check("both values ending in 000ns", both, "2026-10-28T07:30:00.005123Z", true, "")
	check("both values ending in 000ns, nine digits", both, "2026-10-28T07:30:00.005123456Z", true, "")
	check("both values ending in 000ns, nine digits ending in 000ns", both, "2026-10-28T07:30:00.005123000Z", true, "")
	check("both values ending in 000ns, nine digits, a nanosecond before the span (one unit stale)", both, "2026-10-28T07:29:59.999999999Z", true, "")
	check("both values ending in 000ns, nine digits, 3ns before the span", both, "2026-10-28T07:29:59.999999997Z", false, "it changed")
	// Cut to the second, nine digits on both sides: the cut (the coarsest
	// step both values are whole multiples of) is learned, and a replay
	// value cut so passes wherever in its second its read fell, and a
	// second stale, and is refused two seconds stale, naming the cut.
	o := cutAt("cut to the second with nine digits", "2026-10-27T03:30:00.000000000Z", "2026-10-27T03:31:02.000000000Z", time.Second)
	check("cut to the second with nine digits", o, "2026-10-28T07:30:00.000000000Z", true, "")
	check("cut to the second with nine digits, a second stale", o, "2026-10-28T07:29:59.000000000Z", true, "")
	check("cut to the second with nine digits, two seconds stale", o, "2026-10-28T07:29:58.000000000Z", false, "it changed (read as standing for the whole 1s it is cut to)")
	// Cut to the second, written with three digits, then six: the same,
	// above the finer of the two encodings.
	cutAt("cut to the second with three digits, then six", "2026-10-27T03:30:00.000Z", "2026-10-27T03:31:02.000000Z", time.Second)
	// Cut to the minute, written to the second: learned at the minute.
	cutAt("cut to the minute, written to the second", "2026-10-27T03:30:00Z", "2026-10-27T03:32:00Z", time.Minute)
	// A replay reads a Res coarser than its value's encoding by the cut its
	// own value shows, no coarser than Res: at a Res of an hour and a
	// second's encoding, a value on a whole minute is checked at the minute
	// (passed within a minute of the span, refused two minutes stale), one
	// on a whole hour at the hour, and one that is no whole minute shows no
	// cut above its second and is checked at it.
	hour := models.ClockOffset{Res: time.Hour, Layout: "iso-second"}
	late := Span{From: now.Add(time.Second + 5*time.Millisecond), To: now.Add(time.Second + 25*time.Millisecond), Zone: time.UTC, Own: true}
	for _, c := range []struct {
		v, why string
		ok     bool
	}{
		{"2026-10-28T07:30:00Z", "", true},
		{"2026-10-28T07:29:00Z", "", true},
		{"2026-10-28T07:28:00Z", "it changed (read as standing for the whole 1m it is cut to)", false},
		{"2026-10-28T07:00:00Z", "", true},
		{"2026-10-28T06:00:00Z", "", true},
		{"2026-10-28T05:00:00Z", "it changed (read as standing for the whole 1h it is cut to)", false},
	} {
		if ok, why := CheckClockRead(p, hour, str(c.v), late, true); ok != c.ok || !strings.HasSuffix(why, c.why) {
			t.Errorf("a Res of 1h, replay %s: %v (%s); want %v %q", c.v, ok, why, c.ok, c.why)
		}
	}
	if ok, why := CheckClockRead(p, hour, str("2026-10-28T07:30:01Z"), late, true); !ok {
		t.Errorf("a Res of 1h, a replay value no whole minute: refused (%s); want it checked at its second, and passed", why)
	}
	// An epoch in seconds cut to the millisecond, with six digits: read
	// exactly, both values are whole milliseconds, a cut, which a replay
	// checks at the cut: a value cut so passes in the millisecond its read
	// fell in and in the one before, and not in the one before that.
	if r := textClockRead("1793158200.123000", "1793158262.456000"); r == nil || r.Res != time.Millisecond {
		t.Errorf("an epoch in seconds cut to the millisecond: %+v; want a Res of the 1ms cut", r)
	} else {
		check("an epoch in seconds cut to the millisecond, checked at the cut", *r, "1793172600.005000", true, "")
		check("an epoch in seconds cut to the millisecond, the one before the span", *r, "1793172599.999000", true, "")
		check("an epoch in seconds cut to the millisecond, two before the span", *r, "1793172599.998000", false, "it changed")
	}
	// A time of day and an epoch in seconds read their fraction the same.
	if r := clockedDrifts().todRead(3*time.Hour+30*time.Minute+495123*time.Microsecond, 3*time.Hour+31*time.Minute+2032456789, time.Microsecond, time.Nanosecond, "time-second", "time-second", nil, call); r.Res != time.Nanosecond {
		t.Errorf("a time of day with six digits, then nine: Res = %v; want 1ns", r.Res)
	}
	if r := clockedDrifts().todRead(3*time.Hour+30*time.Minute+2032456789, 3*time.Hour+31*time.Minute+495123*time.Microsecond, time.Nanosecond, time.Microsecond, "time-second", "time-second", nil, call); r.Res != time.Nanosecond {
		t.Errorf("a time of day with nine digits, then six: Res = %v; want 1ns", r.Res)
	}
	// A time of day cut to the second, written with three digits and with
	// six, in either order: learned at its cut of a second; a replay value
	// cut so is checked at the cut in either form (passed in the span's
	// second and a second stale, refused two seconds stale, naming the cut);
	// one that is no whole second is checked at its own millisecond.
	cutAt("a time of day cut to the second, three digits then six", "03:30:00.000", "03:31:02.000000", time.Second)
	cutAt("a time of day cut to the second, six digits then three", "03:30:00.000000", "03:31:02.000", time.Second)
	tods := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: time.UTC, Own: true}
	for _, pr := range [][2]time.Duration{{time.Millisecond, time.Microsecond}, {time.Microsecond, time.Millisecond}} {
		r := clockedDrifts().todRead(3*time.Hour+30*time.Minute, 3*time.Hour+31*time.Minute+2*time.Second, pr[0], pr[1], "time-second", "time-second", nil, call)
		if r.Res != time.Second {
			t.Errorf("a time of day cut to the second, written at %v then %v: Res = %v; want the 1s cut", pr[0], pr[1], r.Res)
			continue
		}
		for _, v := range []string{"07:30:00.000", "07:30:00.000000"} {
			if ok, why := CheckClockRead(p, *r, str(v), tods, true); !ok {
				t.Errorf("a time of day cut to the second, written at %v then %v, replay %s: refused (%s); want checked at the cut, and passed", pr[0], pr[1], v, why)
			}
		}
		for _, v := range []string{"07:29:59.000", "07:29:59.000000"} {
			if ok, why := CheckClockRead(p, *r, str(v), tods, true); !ok {
				t.Errorf("a time of day cut to the second, written at %v then %v, replay %s a second stale: refused (%s); want passed", pr[0], pr[1], v, why)
			}
		}
		for _, v := range []string{"07:29:58.000", "07:29:58.000000"} {
			if ok, why := CheckClockRead(p, *r, str(v), tods, true); ok || !strings.HasSuffix(why, "it changed (read as standing for the whole 1s it is cut to)") {
				t.Errorf("a time of day cut to the second, written at %v then %v, replay %s two seconds stale: %v (%s); want refused naming the cut", pr[0], pr[1], v, ok, why)
			}
		}
		if ok, why := CheckClockRead(p, *r, str("07:30:00.005"), tods, true); !ok {
			t.Errorf("a time of day cut to the second, written at %v then %v, replay 07:30:00.005: refused (%s); want it checked at its millisecond, and passed", pr[0], pr[1], why)
		}
	}
	// An epoch in seconds with nine digits on both sides is read exactly, to
	// the nanosecond: no cut, and a nine-digit replay value in the span
	// passes, and so does one a nanosecond before it; one 3 ns before it is
	// refused.
	if r := textClockRead("1793158200.495123456", "1793158262.032456789"); r == nil || r.Res != time.Nanosecond {
		t.Errorf("an epoch in seconds with nine digits: %+v; want a Res of 1ns", r)
	} else {
		check("an epoch in seconds with nine digits", *r, "1793172600.005123456", true, "")
		check("an epoch in seconds with nine digits, a nanosecond before the span", *r, "1793172599.999999999", true, "")
		check("an epoch in seconds with nine digits, 3ns before the span", *r, "1793172599.999999997", false, "it changed")
	}
	if r := textClockRead("1793158200.495", "1793158262.032456"); r == nil || r.Res != time.Microsecond {
		t.Errorf("an epoch in seconds with three digits, then six: %+v; want a Res of 1µs", r)
	}
	// One fraction digit, the fewest the layout reads: a tenth of a second,
	// read exactly (the span's own tenth passes, and so does the next one,
	// which starts after the span ends, one tenth ahead; the one after it is
	// refused).
	if r := textClockRead("1793158200.5", "1793158262.3"); r == nil || r.Res != 100*time.Millisecond {
		t.Errorf("an epoch in seconds with one digit: %+v; want a Res of 100ms", r)
	} else {
		check("an epoch in seconds with one digit", *r, "1793172600.0", true, "")
		check("an epoch in seconds with one digit, after the span", *r, "1793172600.1", true, "")
		check("an epoch in seconds with one digit, two tenths after the span", *r, "1793172600.2", false, "it changed")
	}
	// A layout that fixes its resolution reads no fraction: a cut epoch in
	// nanoseconds is learned at its cut of a millisecond. A replay value
	// that is no whole number of the cut shows none: it is checked at its
	// own resolution and passes in the span; one cut to the millisecond is
	// checked at it. A $date cut to the second is learned at the cut.
	ns := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	sec := time.Date(2026, 10, 26, 9, 0, 0, 123000000, time.UTC)
	if r := textClockRead(ns(sec), ns(sec.Add(FullRoundLead+7*time.Millisecond))); r == nil || r.Res != time.Millisecond {
		t.Fatalf("a cut epoch in nanoseconds: %+v; want a Res of the 1ms cut", r)
	} else {
		check("a cut epoch in nanoseconds, a replay value no whole microsecond", *r, ns(now.Add(5*time.Millisecond+7)), true, "")
		check("a cut epoch in nanoseconds, a replay value cut so", *r, ns(now.Add(5*time.Millisecond)), true, "")
	}
	const tp = "body.at.$date.$numberLong"
	typed := func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at, time.Millisecond) }
	whole := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	ln := ClassifyIn(clockedDrifts(), []string{tp}, Shape{Leaves: map[string]Leaf{tp: typed(whole.Add(61 * time.Second))}}, Shape{Leaves: map[string]Leaf{tp: typed(whole)}}, whole.Add(time.Millisecond), nil, nil, nil, nil)[0]
	if ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Res != time.Second {
		t.Errorf("a cut $date: %s Read=%+v (%s); want learned at the 1s cut", ln.Kind, ln.Read, ln.Why)
	}
}

// clockWriters are the ways applications write a read of their clock as a
// text or a number, each from the clock its runtime reads (the resolution
// it truncates a read to), as recorded, in record's full round and at a
// replay alike.
var clockWriters = []struct {
	name string
	// cut is the cut the application makes coarser than a millisecond
	// (to the second, to 10 ms), which the learn step learns at every
	// recording, and a replay checks a value at; 0 for none.
	cut time.Duration
	// chance: the two values of some recordings are whole microseconds by
	// chance, a cut a replay reads by its own value.
	chance bool
	// msClock: a clock of milliseconds written with six or nine fixed
	// digits, whose two values both fall on a whole 10 ms by chance in about
	// one recording in a hundred where F's read moved against the
	// recording's as the row moves it (up to 200 ms; none of a fast call:
	// TestAFastCallsTwoValuesNeverFallOnACutByChance): learned at that cut.
	msClock bool
	// limit: a cut of 10 or 100 ms written at a resolution less than 1,000
	// times finer (milliseconds), which the learn step cannot tell from a
	// writer of milliseconds: a stated limit, whose verdict may turn on
	// where its read fell. The row asserts only that no moved value passes.
	limit bool
	w     func(t time.Time) string
}{
	{name: "Go RFC3339Nano, a clock of nanoseconds", w: func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }},
	{name: "Java Instant.toString, a clock of microseconds", w: func(t time.Time) string { return javaInstant(t.Truncate(time.Microsecond)) }},
	{name: "Java Instant.toString, a clock of nanoseconds", w: javaInstant},
	{name: "Python json.dumps(time.time())", chance: true, w: pythonFloat},
	{name: ".NET trimmed ticks (FFFFFFF)", chance: true, w: func(t time.Time) string { return t.Truncate(100).UTC().Format("2006-01-02T15:04:05.9999999Z07:00") }},
	{name: `.NET DateTime.ToString("o"), seven digits`, chance: true, w: func(t time.Time) string { return t.Truncate(100).UTC().Format("2006-01-02T15:04:05.0000000Z07:00") }},
	{name: "Jackson's numeric Instant, JDK 9-14 (a clock of microseconds)", w: func(t time.Time) string { return jacksonInstant(t.Truncate(time.Microsecond)) }},
	{name: "Jackson's numeric Instant, JDK 8 (a clock of milliseconds)", msClock: true, w: func(t time.Time) string { return jacksonInstant(t.Truncate(time.Millisecond)) }},
	{name: "nine digits, a clock of microseconds", w: func(t time.Time) string {
		return t.Truncate(time.Microsecond).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}},
	{name: "nine digits, a clock of milliseconds", msClock: true, w: func(t time.Time) string {
		return t.Truncate(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}},
	{name: "JavaScript Date.now()/1000", w: func(t time.Time) string {
		return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
	}},
	{name: "JavaScript toISOString", w: func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }},
	{name: "epoch seconds", w: func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }},
	{name: "RFC 3339 to the second", w: func(t time.Time) string { return t.UTC().Format(time.RFC3339) }},
	{name: "nine digits, cut to the second", cut: time.Second, w: func(t time.Time) string {
		return t.Truncate(time.Second).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}},
	{name: "three digits, cut to the second", cut: time.Second, w: func(t time.Time) string { return t.Truncate(time.Second).UTC().Format("2006-01-02T15:04:05.000Z07:00") }},
	{name: "Jackson's numeric Instant, cut to the second", cut: time.Second, w: func(t time.Time) string { return jacksonInstant(t.Truncate(time.Second)) }},
	{name: "Python datetime.now().replace(microsecond=0).timestamp()", cut: time.Second, w: func(t time.Time) string { return pythonFloat(t.Truncate(time.Second)) }},
	{name: "nine digits over a column of two fraction digits (cut to 10 ms)", cut: 10 * time.Millisecond, w: func(t time.Time) string {
		return t.Truncate(10 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}},
	{name: "Python isoformat over a column of two fraction digits (cut to 10 ms)", cut: 10 * time.Millisecond, w: func(t time.Time) string { return pythonIsoformat(t.Truncate(10 * time.Millisecond)) }},
	{name: "Python isoformat over a column of one fraction digit (cut to 100 ms)", cut: 100 * time.Millisecond, w: func(t time.Time) string { return pythonIsoformat(t.Truncate(100 * time.Millisecond)) }},
	{name: `.NET DateTime.ToString("o") over a column of two fraction digits (cut to 10 ms)`, cut: 10 * time.Millisecond, w: func(t time.Time) string {
		return t.Truncate(10 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.0000000Z07:00")
	}},
	{name: "six digits over a column of four fraction digits (cut to 100 µs)", w: func(t time.Time) string {
		return t.Truncate(100 * time.Microsecond).UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	}},
	{name: "three digits over a column of two fraction digits (cut to 10 ms)", limit: true, w: func(t time.Time) string {
		return t.Truncate(10 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}},
}

// pythonIsoformat is Python's datetime.isoformat of an aware UTC datetime:
// six fraction digits, none on a whole second.
func pythonIsoformat(t time.Time) string {
	t = t.Truncate(time.Microsecond).UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

// javaInstant is Java's Instant.toString: a fraction in groups of three
// digits, as few as the value needs.
func javaInstant(t time.Time) string {
	ns, base := t.Nanosecond(), t.UTC().Format("2006-01-02T15:04:05")
	switch {
	case ns == 0:
		return base + "Z"
	case ns%1e6 == 0:
		return base + fmt.Sprintf(".%03dZ", ns/1e6)
	case ns%1e3 == 0:
		return base + fmt.Sprintf(".%06dZ", ns/1e3)
	}
	return base + fmt.Sprintf(".%09dZ", ns)
}

// pythonFloat is an epoch in seconds as the shortest decimal that reads
// back as its float64 (Python's repr, JavaScript's Number toString), with
// ".0" on a whole second, as Python writes it.
func pythonFloat(t time.Time) string {
	s := strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', -1, 64)
	if t.Nanosecond() == 0 {
		s = strconv.FormatInt(t.Unix(), 10)
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// jacksonInstant is Jackson's numeric Instant (WRITE_DATES_AS_TIMESTAMPS
// with WRITE_DATE_TIMESTAMPS_AS_NANOSECONDS): seconds, and nine digits.
func jacksonInstant(t time.Time) string { return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()) }

// A replay's verdict on a plain read of the clock never turns on where the
// replay's own read fell: for each writer, 1,000 recordings (their two
// values one lead apart, at times a seeded generator picks) are learned as
// record's learn step learns them (ClassifyIn, no call window), and each
// one learned is replayed five times with the same writer's value of a read
// in the replay's span. Every writer that does not cut its value coarser
// than a millisecond passes every replay: a Python float epoch and .NET's
// trimmed and fixed ticks, whose two values are both whole microseconds in
// a few recordings in a hundred (the row counts them), are checked at the
// replay's own value (a value that shows no cut at its own resolution, one
// that shows a cut of a microsecond at it); Jackson's nine digits from a
// clock of microseconds or milliseconds, which shows that cut in every
// value, at the cut. A value cut coarser than a millisecond (to the second,
// or to 10 or 100 ms written with six, seven or nine digits) is learned at
// every recording at no finer than its cut, and passes every replay
// (founder decision, 2026-10-27: checked at the cut), Python's isoformat
// over a column of tenths learned at the second where both learned values
// had no fraction among them. No recording of any writer is refused at
// learn, and no replay's verdict flips. A writer of milliseconds with nine
// fixed digits has both values on a whole 10 ms in about one recording in
// a hundred, learned at that cut. A value moved a second either way, or
// 50 ms, beyond the cut it was learned at (a writer's cut, or a coarser one
// both learned values fell on by chance: a column of 10 ms whose two
// values were whole tenths), is refused at every replay, but where
// its own form is coarse enough to reach the span (a writer that drops
// trailing zeros wrote a whole second or tenth: the exception of
// 2026-10-27, it stands for its whole second or tenth). A cut of 10 ms
// written in milliseconds (a stated limit) passes no moved value either.
func TestAPlainClockReadsVerdictIsTheSameAtEveryReplayWhateverItsWriter(t *testing.T) {
	const recordings, replays = 1000, 5
	p := "body.at"
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: str(v)}} }
	base := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	for _, wr := range clockWriters {
		rng := rand.New(rand.NewSource(37))
		var passed, refused, coarse, movedPassed, byChance int
		var ex []string
		for i := 0; i < recordings; i++ {
			t0 := base.Add(time.Duration(rng.Int63n(int64(24 * time.Hour))))
			t1 := t0.Add(FullRoundLead + time.Duration(rng.Int63n(int64(200*time.Millisecond))))
			rec, live := wr.w(t0), wr.w(t1)
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape(rec), t0.Add(time.Millisecond), nil, nil, nil, nil)[0]
			r := textClockRead(rec, live)
			if r == nil {
				t.Fatalf("%s: %s then %s read as no clock read", wr.name, rec, live)
			}
			o, ok := ln.Clock()
			if !plainRead(ln) || !ok || o.Res != r.Res {
				t.Fatalf("%s: %s then %s: %s Read=%+v (%s); want a plain read of the clock at %v, never refused at learn", wr.name, rec, live, ln.Kind, ln.Read, ln.Why, r.Res)
			}
			// Learned at no finer than the writer's cut, and, above the
			// finer of the two encodings, only at a cut both values show.
			rt, rres, _ := readClockText(rec)
			lt, lres, _ := readClockText(live)
			enc := min(rres, lres)
			if o.Res < wr.cut || o.Res > enc && (rt.UnixNano()%int64(o.Res) != 0 || lt.UnixNano()%int64(o.Res) != 0) {
				t.Fatalf("%s: %s then %s learned at %v; want a cut both values show, of at least %v", wr.name, rec, live, o.Res, wr.cut)
			}
			if wr.cut == 0 && o.Res > enc && o.Res > time.Millisecond {
				coarse++
			}
			if o.Res >= time.Microsecond {
				byChance++
			}
			for j := 0; j < replays; j++ {
				now := base.Add(48*time.Hour + time.Duration(rng.Int63n(int64(24*time.Hour))))
				span := Span{From: now, To: now.Add(20 * time.Millisecond)}
				v := wr.w(now.Add(time.Duration(rng.Int63n(int64(20 * time.Millisecond)))))
				ok, why := CheckClockRead(p, o, str(v), span, true)
				if ClockReadHolds(o, str(v), span) != ok {
					t.Fatalf("%s: ClockReadHolds and CheckClockRead disagree on %s (Res %v), replay %s", wr.name, rec, o.Res, v)
				}
				if ok {
					passed++
				} else {
					refused++
					if len(ex) < 3 {
						ex = append(ex, fmt.Sprintf("%s then %s (Res %v), replay %s: %s", rec, live, o.Res, v, why))
					}
				}
				// Moved by more than two of the cut it was learned at (a
				// writer's cut, or a coarser one both learned values fell
				// on): a value passes only within one of the resolution it
				// is checked at of the span, either side, which is no
				// coarser than the coarser of o.Res and its own form's (a
				// writer that drops trailing zeros writes .7 for .700).
				for _, d := range []time.Duration{-2*o.Res - time.Second, -2*o.Res - 50*time.Millisecond, 2*o.Res + 50*time.Millisecond + 20*time.Millisecond, 2*o.Res + time.Second + 20*time.Millisecond} {
					mv := wr.w(now.Add(d + time.Duration(rng.Int63n(int64(20*time.Millisecond)))))
					at, res, _ := readClockText(mv)
					r := max(res, o.Res)
					if ok, _ := CheckClockRead(p, o, str(mv), span, true); ok && (at.Before(span.From.Truncate(r).Add(-r)) || at.After(span.To.Add(r))) {
						movedPassed++
						if len(ex) < 3 {
							ex = append(ex, fmt.Sprintf("%s then %s (Res %v), a value moved %v, %s, passed", rec, live, o.Res, d, mv))
						}
					}
				}
			}
		}
		switch {
		case movedPassed != 0:
			t.Errorf("%s: %d moved values passed; want none: %q", wr.name, movedPassed, ex)
		case wr.limit:
		case refused != 0 || passed != recordings*replays:
			t.Errorf("%s: %d of %d replays refused; want none: %q", wr.name, refused, passed+refused, ex)
		case wr.msClock && (coarse == 0 || coarse > recordings/25):
			t.Errorf("%s: %d of %d recordings learned at a cut coarser than a millisecond; want about one in a hundred (both values on a whole 10 ms)", wr.name, coarse, recordings)
		case wr.cut == 0 && !wr.msClock && coarse != 0:
			t.Errorf("%s: %d of %d recordings learned at a cut coarser than a millisecond; want none", wr.name, coarse, recordings)
		case wr.chance && byChance == 0:
			t.Errorf("%s: no recording's two values fell on a unit by chance; the row drives no such read", wr.name)
		}
	}
}

// Two values of a writer that does not cut its value can both fall on a
// step above their encoding by chance, where F's read moved against the
// recording's by about half a second or more (FullRoundLead): a writer of
// seconds on a whole minute, of minutes on a whole hour, of milliseconds on
// a whole second. The learn step learns each such recording at the cut
// both values show, and a replay checks a value at the cut its own value
// shows (founder decision, 2026-10-27), so every replay of it passes, its
// value on the step or not, and one a cut and a unit stale is refused; a
// recording whose values show no such cut is learned at its encoding, and
// every replay of it passes the same: no verdict turns on a replay's own
// read. A time of day its protocol types, cut to the second, is learned at
// the cut the same.
func TestAChanceCutIsOneVerdictPerRecordingNeverPerReplay(t *testing.T) {
	const p = "body.at"
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{p: l}} }
	t0 := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	for _, f := range []struct {
		name     string
		w        func(time.Time) string
		enc, cut time.Duration
	}{
		{"epoch seconds", func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }, time.Second, time.Minute},
		{"RFC 3339 to the second", func(t time.Time) string { return t.UTC().Format(time.RFC3339) }, time.Second, time.Minute},
		{"an HTTP date", func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }, time.Second, time.Minute},
		{"a time of day to the second", func(t time.Time) string { return t.UTC().Format("15:04:05") }, time.Second, time.Minute},
		{"an ISO date and time to the minute", func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z07:00") }, time.Minute, time.Hour},
		{"epoch milliseconds", func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }, time.Millisecond, time.Second},
		{"JavaScript toISOString", func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }, time.Millisecond, time.Second},
	} {
		at := func(units int64) time.Time { return t0.Add(time.Duration(units) * f.enc) }
		per := int64(f.cut / f.enc)
		learn := func(r, l time.Time) Learned {
			return ClassifyIn(clockedDrifts(), []string{p}, shape(str(f.w(l))), shape(str(f.w(r))), r.Add(f.enc), nil, nil, nil, nil)[0]
		}
		// Both on the cut: learned at it. One off it: learned at the
		// encoding. Either way every replay passes, on the step or off it,
		// and a value a cut and a unit before its span is refused.
		for _, c := range []struct {
			what string
			ln   Learned
			res  time.Duration
		}{
			{"both values on a whole " + f.cut.String(), learn(at(30*per), at(32*per)), f.cut},
			{"one value off a whole " + f.cut.String(), learn(at(30*per), at(31*per+2)), f.enc},
		} {
			o, ok := c.ln.Clock()
			if !plainRead(c.ln) || !ok || o.Res != c.res {
				t.Errorf("%s, %s: %s Read=%+v (%s); want a plain read at %v", f.name, c.what, c.ln.Kind, c.ln.Read, c.ln.Why, c.res)
				continue
			}
			for k := int64(0); k < 3*per && k < 5000; k++ {
				v := now.Add(time.Duration(k) * f.enc)
				span := Span{From: v.Add(-f.enc / 2), To: v.Add(f.enc / 2), Zone: time.UTC, Own: true}
				if ok, why := CheckClockRead(p, o, str(f.w(v)), span, true); !ok {
					t.Errorf("%s, %s, replay %s: refused (%s); want every replay passed", f.name, c.what, f.w(v), why)
					break
				}
				if ok, _ := CheckClockRead(p, o, str(f.w(v.Add(-2*c.res-f.enc))), span, true); ok {
					t.Errorf("%s, %s, replay %s, two cuts and a unit stale: passed; want refused", f.name, c.what, f.w(v.Add(-2*c.res-f.enc)))
					break
				}
			}
		}
	}
	// A time of day its protocol types (a Postgres time of microseconds),
	// cut to the second in both runs: learned at the cut; one that is no
	// whole second at its microsecond.
	const tp = "args.0"
	tod := func(at time.Time) Leaf { return TimeOfDayLeaf(tp, tp, time.Duration(clockOfDay(at)), time.Microsecond) }
	read := time.Date(2026, 10, 22, 11, 59, 30, 0, time.UTC)
	call := read.Add(115 * time.Millisecond)
	if ln := classifyZoned(nil, false, tp, tod(read), tod(read.Add(FullRoundLead).Truncate(time.Second)), call, nil); !todRead(ln, "") || ln.Read.Res != time.Second {
		t.Errorf("a typed time of day cut to the second: %s Read=%+v (%s); want a read modulo a day at the 1s cut", ln.Kind, ln.Read, ln.Why)
	}
	if ln := classifyZoned(nil, false, tp, tod(read.Add(37123*time.Microsecond)), tod(read.Add(FullRoundLead+37123*time.Microsecond)), call, nil); !todRead(ln, "") || ln.Read.Res != time.Microsecond {
		t.Errorf("a typed time of day of microseconds: %s Read=%+v (%s); want a read modulo a day at 1µs", ln.Kind, ln.Read, ln.Why)
	}
}

// A read of the clock the application cuts coarser than a millisecond is
// learned at its cut and checked at it (founder decision, 2026-10-27): a
// replay value of it stands for its whole cut, so it passes where its cut
// meets the call's span (the span widened by one cut before it, the value
// floored at the cut) and is refused otherwise. For a cut of 10 ms (nine
// digits), 100 ms (six digits), a second (a $date), a minute (RFC 3339 to
// the second) and a day (a midnight to the second), with a span starting a
// unit past a whole cut: the cut the span starts in passes and the one
// before, one cut and a unit before the span, is refused; the cut the span
// ends in passes and the next is refused. Over 2,000 replays whose reads
// fall anywhere in spans anywhere in a day, every value read in its span
// passes (no verdict turns on where the read fell) and every value read
// more than one cut before its span, or after it, is refused.
func TestACoarseCutIsCheckedAtTheCut(t *testing.T) {
	const tp = "body.at.$date.$numberLong"
	text := func(lay string, c time.Duration) func(time.Time) Leaf {
		return func(at time.Time) Leaf {
			return Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: at.Truncate(c).UTC().Format(lay)}
		}
	}
	for _, f := range []struct {
		name     string
		path     string
		cut, enc time.Duration
		w        func(time.Time) Leaf
		// u writes a value in w's form, cut to its encoding alone.
		u func(time.Time) Leaf
	}{
		{"nine digits cut to 10ms", "body.at", 10 * time.Millisecond, time.Nanosecond, text("2006-01-02T15:04:05.000000000Z07:00", 10*time.Millisecond), text("2006-01-02T15:04:05.000000000Z07:00", time.Nanosecond)},
		{"six digits cut to 100ms", "body.at", 100 * time.Millisecond, time.Microsecond, text("2006-01-02T15:04:05.000000Z07:00", 100*time.Millisecond), text("2006-01-02T15:04:05.000000Z07:00", time.Microsecond)},
		{"a $date cut to the second", tp, time.Second, time.Millisecond, func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at.Truncate(time.Second), time.Millisecond) }, func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at, time.Millisecond) }},
		{"RFC 3339 cut to the minute", "body.at", time.Minute, time.Second, text(time.RFC3339, time.Minute), text(time.RFC3339, time.Second)},
		{"a midnight to the second (a date)", "body.at", 24 * time.Hour, time.Second, text(time.RFC3339, 24*time.Hour), text(time.RFC3339, time.Second)},
	} {
		shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{f.path: l}} }
		rec := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).Add(f.cut * 7)
		live := rec.Add(3 * f.cut)
		ln := ClassifyIn(clockedDrifts(), []string{f.path}, shape(f.w(live)), shape(f.w(rec)), rec.Add(f.enc), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok || o.Res != f.cut {
			t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock learned at its %v cut", f.name, ln.Kind, ln.Read, ln.Why, f.cut)
			continue
		}
		check := func(what string, at time.Time, s Span, want bool) {
			t.Helper()
			ok, why := CheckClockRead(f.path, o, f.w(at), s, true)
			if ok != want || ClockReadHolds(o, f.w(at), s) != ok || !ok && !strings.Contains(why, "it changed") {
				t.Errorf("%s, %s (%s, span %s to %s): %v (%s); want %v", f.name, what, f.w(at).Value, s.From.Format(time.RFC3339Nano), s.To.Format(time.RFC3339Nano), ok, why, want)
			}
		}
		k := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).Add(5 * f.cut)
		span := Span{From: k.Add(f.enc), To: k.Add(f.cut + f.cut/2), Zone: time.UTC, Own: true}
		check("the cut the span starts in", k, span, true)
		check("one cut stale", k.Add(-f.cut), span, true)
		check("two cuts stale", k.Add(-2*f.cut), span, false)
		check("the cut the span ends in", k.Add(f.cut), span, true)
		check("one cut past the cut the span ends in (a writer that rounds)", k.Add(2*f.cut), span, true)
		check("two cuts past it", k.Add(3*f.cut), span, false)
		unit := func(what string, at time.Time, want bool) {
			t.Helper()
			l := f.u(at)
			if ok, why := CheckClockRead(f.path, o, l, span, true); ok != want {
				t.Errorf("%s, %s (%s): %v (%s); want %v", f.name, what, l.Value, ok, why, want)
			}
		}
		// A value one unit of its encoding past the widened span shows no
		// cut, so it is checked at its own resolution, and fails.
		unit("one cut and a unit stale", k.Add(-f.cut-f.enc), false)
		unit("one unit past the widened span", span.To.Add(f.cut).Truncate(f.enc).Add(f.enc), false)
		rng := rand.New(rand.NewSource(41))
		for i := 0; i < 2000; i++ {
			from := k.Add(time.Duration(rng.Int63n(int64(24 * time.Hour))))
			s := Span{From: from, To: from.Add(time.Duration(rng.Int63n(int64(f.cut)))), Zone: time.UTC, Own: true}
			read := s.From.Add(time.Duration(rng.Int63n(int64(s.To.Sub(s.From)) + 1)))
			check("a read in its span", read, s, true)
			check("a read in its span, rounded up", read.Truncate(f.cut).Add(f.cut), s, true)
			check("a read one cut before its span", s.From.Add(-f.cut), s, true)
			check("a read more than one cut before the cut its span starts in", s.From.Truncate(f.cut).Add(-f.cut-time.Duration(1+rng.Int63n(int64(f.cut)))), s, false)
			check("a value more than one cut after its span", s.To.Add(f.cut).Truncate(f.cut).Add(f.cut+time.Duration(rng.Int63n(int64(f.cut)))), s, false)
		}
	}
}

// A cut is read in UTC, in the offset a value writes, and in the
// application's zone, at learn and at replay, so an hour or a day the
// application cuts in a zone whose offset is no whole number of it is
// learned and checked as that hour or day (founder decision, 2026-10-27,
// and the coordinator's ruling of 23:44): a text that writes +05:30 or
// -03:30 cut to its hour, one that writes +02:00 cut to its day, a typed
// $date cut to the IST hour, a "Z" text cut to the IST day, and a $date
// cut to Berlin's day across the 25 hours of 2026-10-25, when its clocks go
// back (a day of the zone's calendar, never 24 hours). Read in UTC alone,
// each shows a finer cut (a minute, an hour) and a replay flipped by when
// in the hour or the day it ran. Writers that round rather than cut
// (JavaScript's Math.round(Date.now()/1000) as a JSON number, a value
// rounded to the second written with three fraction digits, a time of day
// rounded so, read on the day after the one the span ends on where it is
// ahead of the span's end) pass, one cut ahead of the span. Over 2,000 replays each, every value a read in its
// span writes passes, and one two cuts stale or two cuts past the span is
// refused: no verdict turns on when the replay ran.
func TestACutIsReadInTheZoneTheApplicationCutsIn(t *testing.T) {
	const tp = "body.at.$date.$numberLong"
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		ist = time.FixedZone("IST", 5*3600+1800)
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zone data: %v", err)
	}
	plus2, nf := time.FixedZone("", 2*3600), time.FixedZone("", -(3*3600+1800))
	hour := func(loc *time.Location) func(time.Time) time.Time {
		return func(t time.Time) time.Time {
			l := t.In(loc)
			return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
		}
	}
	day := func(loc *time.Location) func(time.Time) time.Time {
		return func(t time.Time) time.Time {
			l := t.In(loc)
			return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
		}
	}
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no zone data for %s: %v", name, err)
		}
		return loc
	}
	// first is the first instant at or before t whose wall clock in loc
	// shows the same key (its date, or its date and hour), found by a walk
	// back a minute at a time: the start of a day or an hour whose clocks
	// skipped it.
	first := func(loc *time.Location, key func(time.Time) string) func(time.Time) time.Time {
		return func(t time.Time) time.Time {
			t = t.Truncate(time.Minute)
			for k := key(t); key(t.Add(-time.Minute)) == k; {
				t = t.Add(-time.Minute)
			}
			return t
		}
	}
	dayStart := func(loc *time.Location) func(time.Time) time.Time {
		return first(loc, func(t time.Time) string { return t.In(loc).Format("2006-01-02") })
	}
	hourStart := func(loc *time.Location) func(time.Time) time.Time {
		return first(loc, func(t time.Time) string { return t.In(loc).Format("2006-01-02T15") })
	}
	round := func(c time.Duration) func(time.Time) time.Time {
		return func(t time.Time) time.Time { return t.Round(c) }
	}
	text := func(loc *time.Location, lay string) func(time.Time) Leaf {
		return func(at time.Time) Leaf {
			return Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: at.In(loc).Format(lay)}
		}
	}
	typed := func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at, time.Millisecond) }
	for _, f := range []struct {
		name, path string
		app        *time.Location
		cut        time.Duration
		days       int
		// cutOf is the value the application writes for a read at t,
		// leaf the leaf that writes it.
		cutOf func(time.Time) time.Time
		leaf  func(time.Time) Leaf
	}{
		{"a +05:30 text cut to its hour", "body.at", nil, time.Hour, 0, hour(ist), text(ist, time.RFC3339)},
		{"a -03:30 text cut to its hour", "body.at", nil, time.Hour, 0, hour(nf), text(nf, time.RFC3339)},
		{"a +02:00 text cut to its day", "body.at", nil, 24 * time.Hour, 1, day(plus2), text(plus2, time.RFC3339)},
		{"a typed $date cut to the IST hour", tp, ist, time.Hour, 0, hour(ist), typed},
		{"a Z text cut to the IST day", "body.at", ist, 24 * time.Hour, 1, day(ist), text(time.UTC, time.RFC3339)},
		{"a typed $date cut to Berlin's day", tp, berlin, 24 * time.Hour, 1, day(berlin), typed},
		// Days whose clocks skip midnight (summer time starts at 00:00):
		// the day starts at 01:00, as Go, Java and JavaScript cut it.
		{"a typed $date cut to Cairo's day", tp, zone("Africa/Cairo"), 24 * time.Hour, 1, dayStart(zone("Africa/Cairo")), typed},
		{"a typed $date cut to Santiago's day", tp, zone("America/Santiago"), 24 * time.Hour, 1, dayStart(zone("America/Santiago")), typed},
		{"a typed $date cut to Havana's day", tp, zone("America/Havana"), 24 * time.Hour, 1, dayStart(zone("America/Havana")), typed},
		// Lord Howe's clocks skip 02:00-02:30 when summer time starts: that
		// hour starts at 02:30.
		{"a typed $date cut to Lord Howe's hour", tp, zone("Australia/Lord_Howe"), time.Hour, 0, hourStart(zone("Australia/Lord_Howe")), typed},
		{"Math.round(Date.now()/1000)", "body.iat", nil, time.Second, 0, round(time.Second), func(at time.Time) Leaf {
			return Leaf{Path: "body.iat", Noise: "body.iat", Type: Number, Value: strconv.FormatInt(at.Unix(), 10)}
		}},
		{"three digits rounded to the second", "body.at", nil, time.Second, 0, round(time.Second), text(time.UTC, "2006-01-02T15:04:05.000Z07:00")},
		{"a time of day rounded to the second", "body.at", nil, time.Second, 0, round(time.Second), text(time.UTC, "15:04:05")},
	} {
		// shift is t moved by n cuts. Of an hour or a day, the start of the
		// n-th one after (before) t's, as the application cuts it: days and
		// hours of the zone's calendar, which may last 23 or 25 hours, or
		// start after the hour its clocks skip.
		shift := func(t time.Time, n int) time.Time {
			if f.cut < time.Hour {
				return t.Add(time.Duration(n) * f.cut)
			}
			step := f.cut + 1
			if f.days > 0 {
				step = 30 * time.Hour
			}
			for ; n < 0; n++ {
				t = f.cutOf(f.cutOf(t).Add(-1))
			}
			for ; n > 0; n-- {
				t = f.cutOf(f.cutOf(t).Add(step))
			}
			return t
		}
		shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{f.path: l}} }
		rec := time.Date(2026, 10, 20, 9, 47, 7, 600e6, time.UTC)
		live := shift(rec, 3)
		dc := clockedDrifts()
		dc.zone = f.app
		ln := ClassifyIn(dc, []string{f.path}, shape(f.leaf(f.cutOf(live))), shape(f.leaf(f.cutOf(rec))), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok || o.Res != f.cut || o.Zone != "" && !models.LayoutTimeOfDay(o.Layout) {
			t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock learned at its %v cut", f.name, ln.Kind, ln.Read, ln.Why, f.cut)
			continue
		}
		bad := 0
		check := func(what string, at time.Time, s Span, want bool) {
			t.Helper()
			if ok, why := CheckClockRead(f.path, o, f.leaf(at), s, true); ok != want && bad < 5 {
				bad++
				t.Errorf("%s, %s (%s, span %s to %s): %v (%s); want %v", f.name, what, f.leaf(at).Value, s.From.Format(time.RFC3339Nano), s.To.Format(time.RFC3339Nano), ok, why, want)
			}
		}
		rng := rand.New(rand.NewSource(42))
		base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
		switch f.name {
		case "a typed $date cut to Cairo's day":
			base = time.Date(2026, 4, 19, 0, 0, 0, 0, time.UTC)
		case "a typed $date cut to Santiago's day":
			base = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
		case "a typed $date cut to Havana's day":
			base = time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
		case "a typed $date cut to Lord Howe's hour":
			base = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		}
		for i := 0; i < 2000; i++ {
			from := base.Add(time.Duration(rng.Int63n(int64(min(10*24*time.Hour, 2000*f.cut)))))
			s := Span{From: from, To: from.Add(time.Duration(rng.Int63n(int64(min(f.cut, 10*time.Minute))))), Zone: f.app, Own: true}
			read := s.From.Add(time.Duration(rng.Int63n(int64(s.To.Sub(s.From)) + 1)))
			check("a read in its span", f.cutOf(read), s, true)
			check("one cut stale", shift(f.cutOf(read), -1), s, true)
			// The start of the cut that holds the span's start, which a
			// writer that rounds may write one cut later.
			start := f.cutOf(s.From)
			if start.After(s.From) {
				start = shift(start, -1)
			}
			check("two cuts stale", shift(start, -2), s, false)
			// An hour's window is one hour of instants past the span (the
			// ruling); where a skipped half hour shortened a wall hour, the
			// second wall hour after the span's may still lie in it.
			past := shift(f.cutOf(s.To), 2)
			for f.days == 0 && !past.After(s.To.Add(f.cut)) {
				past = shift(past, 1)
			}
			check("two cuts past the span", past, s, false)
		}
		if f.name == "a typed $date cut to Lord Howe's hour" {
			s := Span{From: time.Date(2026, 10, 4, 2, 50, 0, 0, f.app), Zone: f.app, Own: true}
			s.To = s.From.Add(time.Minute)
			check("the hour that starts at 02:30", f.cutOf(s.From), s, true)
		}
		if f.days > 0 && f.app == berlin && f.name == "a typed $date cut to Berlin's day" {
			late := time.Date(2026, 10, 25, 23, 40, 0, 0, berlin)
			s := Span{From: late, To: late.Add(time.Minute), Zone: berlin, Own: true}
			check("the day the clocks go back, read late in it", f.cutOf(late), s, true)
			check("the day before it", shift(f.cutOf(late), -1), s, true)
			check("two days before it", shift(f.cutOf(late), -2), s, false)
		}
	}
}

// A wall clock of no zone writes its own zone's clock: its cut is read on
// its digits (and where the application's zone skipped the start of their
// hour or day), never on the instant they name read in another zone. Digits
// of 18:30 in IST are a whole day in UTC: learned at the minute they show,
// and a replay value of 05:30 (00:00 in UTC) is checked at its minute, so
// one fourteen hours stale is refused, not passed as a day's cut.
func TestAWallClocksCutIsReadOnItsDigits(t *testing.T) {
	const p = "body.at"
	ist := time.FixedZone("IST", 5*3600+1800)
	wall := func(digits string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: digits} }
	call := time.Date(2026, 10, 20, 13, 0, 0, 5e6, time.UTC)
	ln := classifyZoned(ist, false, p, wall("2026-10-20 18:30:00"), wall("2026-10-21 18:30:00"), call, nil)
	if o, ok := ln.Clock(); !plainRead(ln) || !ok || o.Res != time.Minute {
		t.Errorf("digits of 18:30 a day apart, the application in IST: %s Read=%+v (%s); want a plain read learned at the minute its digits show", ln.Kind, ln.Read, ln.Why)
	}
	o := models.ClockOffset{Res: 24 * time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space"}
	now := time.Date(2026, 10, 27, 20, 0, 0, 0, ist)
	s := Span{From: now, To: now.Add(time.Minute), Zone: ist, Own: true}
	if ok, why := CheckClockRead(p, o, wall("2026-10-27 05:30:00"), s, true); ok {
		t.Errorf("a wall clock of 05:30 in IST (00:00 in UTC), read at 20:00: passed (%s); want checked at its minute and refused", why)
	}
	if ok, why := CheckClockRead(p, o, wall("2026-10-27 20:00:00"), s, true); !ok {
		t.Errorf("a wall clock of 20:00 in IST, read at 20:00: refused (%s); want passed", why)
	}
}

// fixedForm is a form of an instant that writes it at a fixed resolution
// (res), whatever its value.
type fixedForm struct {
	name string
	res  time.Duration
	w    func(time.Time) string
}

// fixedForms are the forms of an instant that write it at a fixed
// resolution, whatever its value: ISO 8601 at the hour, the minute, the
// second and one to nine fraction digits, and epochs in seconds,
// milliseconds, microseconds, nanoseconds and seconds with one to nine
// fraction digits; then a BSON datetime (a millisecond) and a typed
// microsecond (Postgres's timestamp), each with its encoding.
func fixedForms() (text []fixedForm, typed []time.Duration) {
	add := func(name string, res time.Duration, w func(time.Time) string) {
		text = append(text, fixedForm{name, res, w})
	}
	add("ISO 8601 to the hour", time.Hour, func(t time.Time) string { return t.UTC().Format("2006-01-02T15Z07:00") })
	add("ISO 8601 to the minute", time.Minute, func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z07:00") })
	add("ISO 8601 to the second", time.Second, func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z07:00") })
	for d := 1; d <= 9; d++ {
		layout := "2006-01-02T15:04:05." + strings.Repeat("0", d) + "Z07:00"
		res := time.Second / time.Duration(pow10(d))
		add(fmt.Sprintf("ISO 8601 with %d fixed fraction digits", d), res, func(t time.Time) string { return t.UTC().Format(layout) })
		add(fmt.Sprintf("epoch seconds with %d fixed fraction digits", d), res, func(t time.Time) string {
			return fmt.Sprintf("%d.%0*d", t.Unix(), d, t.Nanosecond()/int(res))
		})
	}
	add("epoch seconds", time.Second, func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) })
	add("epoch milliseconds", time.Millisecond, func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) })
	add("epoch microseconds", time.Microsecond, func(t time.Time) string { return strconv.FormatInt(t.UnixMicro(), 10) })
	add("epoch nanoseconds", time.Nanosecond, func(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) })
	return text, []time.Duration{time.Millisecond, time.Microsecond}
}

func pow10(d int) int64 {
	n := int64(1)
	for ; d > 0; d-- {
		n *= 10
	}
	return n
}

// learnedAtACoarseCut reports whether the learn step learns the plain read
// of the clock it reads from rec and live, written in form w (or typed at
// encoding enc, where w is nil), at a cut coarser than a millisecond both
// values show (readRes, textReadRes), above the finer of their encodings: a
// looser check at replay (a value may stand up to one cut stale), which
// FullRoundLead keeps a fast call's pair from by chance.
func learnedAtACoarseCut(w func(time.Time) string, enc time.Duration, rec, live time.Time) (bool, string) {
	coarse := func(res, enc time.Duration) bool { return res > enc && res > time.Millisecond }
	if w == nil {
		r, l := rec.Truncate(enc), live.Truncate(enc)
		if r.Equal(l) {
			return false, ""
		}
		return coarse(readRes(enc, sharedGrain(r, l)), enc), r.Format(time.RFC3339Nano) + " then " + l.Format(time.RFC3339Nano)
	}
	r, l := w(rec), w(live)
	if r == l {
		return false, ""
	}
	read := textClockRead(r, l)
	if read == nil {
		return false, r + " then " + l
	}
	_, rres, _ := readClockText(r)
	_, lres, _ := readClockText(l)
	return coarse(read.Res, min(rres, lres)), r + " then " + l
}

// leadClock is a clock an application reads, at its resolution: exact (the
// grid's own instants), or one that cuts each read to a microsecond or a
// millisecond before a form writes it.
type leadClock struct {
	name string
	res  time.Duration
}

func leadClocks() []leadClock {
	return []leadClock{{"an exact clock", time.Nanosecond}, {"a clock of microseconds", time.Microsecond}, {"a clock of milliseconds", time.Millisecond}}
}

// leadForm is a form (text, or typed at enc where w is nil) as one clock
// writes it: res is the coarser of the clock's resolution and the form's,
// the resolution its values carry.
type leadForm struct {
	name     string
	w        func(time.Time) string
	enc, res time.Duration
}

// leadForms are the forms a clock writes differently from an exact clock:
// every form for the exact clock, and those finer than the clock's
// resolution for the others. A form as fine as the clock or coarser writes
// the clock's value of a read as it writes the exact clock's, cut to the
// form's resolution, and the exact clock's rows take each read at both ends
// of that cut.
func leadForms(c leadClock) []leadForm {
	text, typed := fixedForms()
	var out []leadForm
	for _, f := range text {
		if c.res == time.Nanosecond || f.res < c.res {
			out = append(out, leadForm{f.name, f.w, f.res, max(f.res, c.res)})
		}
	}
	for _, enc := range typed {
		if c.res == time.Nanosecond || enc < c.res {
			out = append(out, leadForm{fmt.Sprintf("typed at %v", enc), nil, enc, max(enc, c.res)})
		}
	}
	return out
}

// fastBand is how far F's read may move against the recording's, either
// way, in a fast call whose values carry resolution r: 5 ms less r, since a
// cut of each read to r moves the two values apart by up to r; for values of
// 10 ms or coarser, whose nearest step is 355 ms or more off, 5 ms.
func fastBand(r time.Duration) time.Duration {
	if r >= 5*time.Millisecond {
		return 5 * time.Millisecond
	}
	return 5*time.Millisecond - r
}

// stepLearns reports whether the learn step, by the ruled steps, learns two
// values written at res (encoding enc) at a coarse cut: they differ and both
// fall on one step above a millisecond the learn step reads a cut at, 10 ms
// and 100 ms where the step is at least 1000 times the encoding, a second, a
// minute, an hour and a day above the encoding at any ratio.
func stepLearns(rec, live time.Time, enc, res time.Duration) bool {
	r, l := rec.Truncate(res), live.Truncate(res)
	if r.Equal(l) {
		return false
	}
	for _, s := range []time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second, time.Minute, time.Hour, 24 * time.Hour} {
		if s > enc && (s >= time.Second || s >= 1000*enc) && r.Truncate(s).Equal(r) && l.Truncate(s).Equal(l) {
			return true
		}
	}
	return false
}

// The lead record's full round runs at (FullRoundLead) is a whole number of
// no step the learn step reads a cut at above a millisecond, so two values
// of a writer that does not cut, read about as far into their calls in both
// runs (a fast call), never both fall on such a step, and the learn step
// never learns a fast call's pair at a cut by chance (a looser check). A fast call's full-round read
// lies within 5 ms less r of the recording's, r being the resolution its
// values carry, the clock's or the form's, the coarser (fastBand): 4 ms on a
// clock of milliseconds, 4.99 ms for five fraction digits, 4.999 ms for six
// or on a clock of microseconds. The first half takes recorded reads on a
// millisecond grid across two seconds about midnight, off the grid by a
// varying fraction of a millisecond, each at the first and the last instant
// of the cut its values carry, on an exact clock and on clocks of
// microseconds and milliseconds, in every form that writes a fixed
// resolution: no pair is learned at a cut with F's read moved up to 5 ms less r
// either way, and where a whole 10 ms is a step the learn step reads (an
// encoding of 10 µs or finer), a read on a whole day is learned at it a
// nanosecond further, so the band is exact. A lead that is a whole number
// of 10 ms (61.5 s) learns such pairs of a millisecond clock written with
// five to nine digits, in epoch micro- or nanoseconds or typed in
// microseconds at 10 ms in one recording in twelve with F's read within 0.3 ms of
// the recording's, and in one in 80 within 4 ms.
//
// The second half pins the bounds. From a recorded read on a whole day (so
// on every step at once), at the first and the last instant its value
// writes, F's read moves in half milliseconds out to 600 ms either way, and
// a pair is learned at a cut exactly where the ruled steps put both values on one
// (stepLearns): nothing short of 5 ms less r (a whole 10 ms), 45 ms back
// or 55 ms on less r (100 ms), 455 ms on or 545 ms back less r (a second:
// 355 ms on and 445 ms back for a writer of tenths), or 545 ms back for a
// writer of seconds (a minute). The table at its end names those bounds,
// to the half millisecond, for a form of each kind.
func TestAFastCallsTwoValuesNeverFallOnACutByChance(t *testing.T) {
	ms := time.Millisecond
	midnight := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, c := range leadClocks() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, f := range leadForms(c) {
				b := fastBand(f.res)
				moves := []time.Duration{-b, -b / 2, 0, b / 2, b}
				for i := -1000; i < 1000; i++ {
					at := midnight.Add(time.Duration(i)*ms + time.Duration((int64(i)+1000)*373_211)%ms)
					first := at.Truncate(f.res)
					for _, rec := range []time.Time{first, first.Add(f.res - time.Nanosecond)} {
						for _, j := range moves {
							if refused, pair := learnedAtACoarseCut(f.w, f.enc, rec.Truncate(c.res), rec.Add(FullRoundLead+j).Truncate(c.res)); refused {
								t.Fatalf("%s, %s, read at %s, F's read %v off the recording's: %s learned at a coarse cut; want no fast call's pair learned so within %v", c.name, f.name, rec.Format(time.RFC3339Nano), j, pair, b)
							}
						}
					}
				}
				if 1000*f.enc > 10*ms || f.res >= 5*ms {
					continue
				}
				for _, e := range []struct {
					rec time.Time
					j   time.Duration
				}{{midnight, -b - time.Nanosecond}, {midnight.Add(f.res - time.Nanosecond), b + time.Nanosecond}} {
					if refused, pair := learnedAtACoarseCut(f.w, f.enc, e.rec.Truncate(c.res), e.rec.Add(FullRoundLead+e.j).Truncate(c.res)); !refused {
						t.Errorf("%s, %s, read at %s, F's read %v off the recording's: %s not learned at a coarse cut; want the band of %v exact, a whole 10 ms reached a nanosecond past it", c.name, f.name, e.rec.Format(time.RFC3339Nano), e.j, pair, b)
					}
				}
			}
		})
	}
	t.Run("the bounds", func(t *testing.T) {
		// edge is the nearest move of F's read either way at which a pair
		// is learned at a coarse cut (0: none out to 600 ms), and whether
		// one is unmoved.
		type edge struct {
			on, back time.Duration
			unmoved  bool
		}
		got := map[string]edge{}
		var mu sync.Mutex
		swept := 0
		t.Run("swept", func(t *testing.T) {
			for _, c := range leadClocks() {
				for _, f := range leadForms(c) {
					swept++
					t.Run(c.name+", "+f.name, func(t *testing.T) {
						t.Parallel()
						var e edge
						for _, p := range []time.Duration{0, f.res - time.Nanosecond} {
							rec := midnight.Add(p)
							for k := -1200; k <= 1200; k++ {
								j := time.Duration(k) * ms / 2
								live := rec.Add(FullRoundLead + j)
								refused, pair := learnedAtACoarseCut(f.w, f.enc, rec.Truncate(c.res), live.Truncate(c.res))
								if want := stepLearns(rec.Truncate(c.res), live.Truncate(c.res), f.enc, f.res); refused != want {
									t.Fatalf("%s, %s, F's read %v off the recording's (%s): learned at a coarse cut %v; want %v, the ruled steps", c.name, f.name, j, pair, refused, want)
								}
								switch {
								case !refused:
								case j == 0:
									e.unmoved = true
								case j > 0 && (e.on == 0 || j < e.on):
									e.on = j
								case j < 0 && (e.back == 0 || j > e.back):
									e.back = j
								}
							}
						}
						mu.Lock()
						got[c.name+", "+f.name] = e
						mu.Unlock()
					})
				}
			}
		})
		for _, row := range []struct {
			form string
			want edge
		}{
			{"an exact clock, ISO 8601 with 9 fixed fraction digits", edge{on: 5 * ms, back: -5 * ms}},
			{"a clock of microseconds, ISO 8601 with 9 fixed fraction digits", edge{on: 5 * ms, back: -5 * ms}},
			{"a clock of milliseconds, ISO 8601 with 9 fixed fraction digits", edge{on: 4500 * time.Microsecond, back: -4500 * time.Microsecond}},
			{"a clock of milliseconds, epoch nanoseconds", edge{on: 4500 * time.Microsecond, back: -4500 * time.Microsecond}},
			{"a clock of milliseconds, typed at 1µs", edge{on: 4500 * time.Microsecond, back: -4500 * time.Microsecond}},
			{"an exact clock, ISO 8601 with 6 fixed fraction digits", edge{on: 5 * ms, back: -5 * ms}},
			{"an exact clock, ISO 8601 with 4 fixed fraction digits", edge{on: 55 * ms, back: -45 * ms}},
			{"a clock of milliseconds, ISO 8601 with 4 fixed fraction digits", edge{on: 54500 * time.Microsecond, back: -44500 * time.Microsecond}},
			{"an exact clock, ISO 8601 with 3 fixed fraction digits", edge{on: 454500 * time.Microsecond, back: -544500 * time.Microsecond}},
			{"an exact clock, typed at 1ms", edge{on: 454500 * time.Microsecond, back: -544500 * time.Microsecond}},
			{"an exact clock, ISO 8601 with 1 fixed fraction digits", edge{on: 355500 * time.Microsecond, back: -445500 * time.Microsecond}},
			{"an exact clock, ISO 8601 to the second", edge{back: -545500 * time.Microsecond}},
			{"an exact clock, epoch seconds", edge{back: -545500 * time.Microsecond}},
			{"an exact clock, ISO 8601 to the minute", edge{}},
		} {
			e, ok := got[row.form]
			if !ok && len(got) < swept {
				continue // a -run filter left this form unswept
			}
			if !ok {
				t.Errorf("%s: never swept; want a form and clock the sweep runs", row.form)
				continue
			}
			if e != row.want {
				t.Errorf("%s: the nearest moves of F's read learned at a coarse cut %v on and %v back, unmoved %v; want %v and %v, unmoved never", row.form, e.on, e.back, e.unmoved, row.want.on, row.want.back)
			}
		}
	})
}

// Python's isoformat over a database column of one or two fraction digits
// (a cut to 100 or 10 ms), whose two learned values both had no fraction
// (the recording's read in the first cut of its second, and F's read about
// 0.45 s on from the recording's against its call, so it fell there too),
// is learned at the second its two forms write. A replay checks each value
// at the cut it shows, no coarser than the second (founder decision,
// 2026-10-27): a value with a fraction at the 100 or 10 ms it falls on, a
// value on a whole second at the second, so every value read in its call's
// span passes, wherever in its second the read fell; a value with a
// fraction a cut stale, and a whole second's value a second stale (where
// the replay's window starts at or after the whole second that follows it),
// are refused as changed.
func TestAnIsoformatTenthsColumnLearnedOnWholeSecondsPassesEveryReplayInItsSpan(t *testing.T) {
	const p = "body.at"
	ms := time.Millisecond
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: str(v)}} }
	for _, col := range []struct {
		cut  time.Duration
		into time.Duration
	}{{100 * ms, 50 * ms}, {10 * ms, 5 * ms}} {
		w := func(t time.Time) string { return pythonIsoformat(t.Truncate(col.cut)) }
		read := time.Date(2026, 10, 27, 9, 14, 7, int(col.into), time.UTC)
		fread := read.Add(FullRoundLead + 450*ms)
		rec, live := w(read), w(fread)
		if rec != "2026-10-27T09:14:07+00:00" || live != "2026-10-27T09:15:09+00:00" {
			t.Fatalf("a column cut to %v, the two values as written: %q then %q; want both on a whole second", col.cut, rec, live)
		}
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape(rec), read.Add(ms), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok || o.Res != time.Second {
			t.Fatalf("a column cut to %v, %s then %s: %s Read=%+v (%s); want a plain read of the clock at the second its forms write", col.cut, rec, live, ln.Kind, ln.Read, ln.Why)
		}
		now := time.Date(2026, 10, 29, 16, 40, 2, 0, time.UTC)
		for _, row := range []struct {
			name   string
			at     time.Time
			v      string
			passes bool
			names  string
		}{
			{"the replay's read in the first cut of its second", now.Add(col.cut * 4 / 10), "", true, ""},
			{"the replay's read at .31 of its second", now.Add(310 * ms), "", true, ""},
			{"the replay's read at .57 of its second", now.Add(570 * ms), "", true, ""},
			{"the replay's read at .9 of its second", now.Add(900 * ms), "", true, ""},
			{"a value with a fraction a cut stale, the read at .31 of its second", now.Add(310 * ms), w(now.Add(305*ms - col.cut)), true, ""},
			{"a value with a fraction two cuts stale, the read at .31 of its second", now.Add(310 * ms), w(now.Add(305*ms - 2*col.cut)), false, "it changed (read as standing for the whole " + map[bool]string{true: "100ms", false: "10ms"}[col.cut == 100*ms] + " it is cut to)"},
			{"a whole second's value a second stale, the read 40 ms into its second", now.Add(40 * ms), "2026-10-29T16:40:01+00:00", true, ""},
			{"a whole second's value two seconds stale, the read 40 ms into its second", now.Add(40 * ms), "2026-10-29T16:40:00+00:00", false, "made in this call's window: it changed"},
		} {
			span := Span{From: row.at.Add(-5 * ms), To: row.at.Add(5 * ms)}
			v := row.v
			if v == "" {
				v = w(row.at)
			}
			got, why := CheckClockRead(p, o, str(v), span, true)
			if got != row.passes || ClockReadHolds(o, str(v), span) != got {
				t.Errorf("a column cut to %v, %s, %s: passes %v (%s); want %v", col.cut, row.name, v, got, why, row.passes)
			}
			if row.names != "" && !strings.Contains(why, row.names) {
				t.Errorf("a column cut to %v, %s, %s: refused (%s); want the reason to say %q", col.cut, row.name, v, why, row.names)
			}
		}
	}
}

// clientDriftsIn is an empty DriftCache of keploy mock's client tests in
// zone.
func clientDriftsIn(zone *time.Location) *DriftCache {
	dc := NewDriftCache()
	dc.zone, dc.clientTests = zone, true
	return dc
}
