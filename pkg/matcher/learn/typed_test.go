package learn

import (
	"strconv"
	"strings"
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
	dc := NewDriftCache()
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
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, nil, raw...); ln.Kind != KindNoise {
		t.Errorf("typed, in its window: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	lead := qnRec.Add(FullRoundLead + 202*time.Millisecond)
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(lead), nil, nil, nil, raw...); ln.Kind != KindNoise {
		t.Errorf("typed, moved by the lead: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	for name, rs := range map[string][]*models.Mock{"with mock-15": raw, "without mock-15": noMonitor} {
		ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), nil, nil, nil, rs...)
		if ln.Kind != KindUnaccounted || ln.From.Mock != "" || !strings.HasPrefix(ln.Why, "nothing shows it is a read of the application's clock") || strings.Contains(ln.Why, "mock-") {
			t.Errorf("typed, no provenance, %s: %s (from %q, %s); want refused for its provenance, naming no recording", name, ln.Kind, ln.From.Mock, ln.Why)
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
	if ln := classifyTyped(str(qnRec), str(qnLive), nil, nil, nil, noMonitor...); ln.Kind != KindNoise {
		t.Errorf("a date-like string, no provenance, without mock-15: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	if ln := classifyTyped(str(qnRec), str(qnLive), qnWindow(), nil, nil, raw...); ln.Kind != KindNoise {
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
				if ln.Kind != KindNoise {
					t.Errorf("%s, %s: %s (from %q, %s); want noise", row.name, wname, ln.Kind, ln.From.Mock, ln.Why)
				}
			default:
				if ln.Kind != KindUnaccounted || ln.From.Mock != "" {
					t.Errorf("%s, %s: %s (from %q, %s); want refused for its provenance", row.name, wname, ln.Kind, ln.From.Mock, ln.Why)
				}
			}
		}
	}
	// A payload recorded after the insert is no earlier place.
	late := answer(`{"Attributes":{"SentTimestamp":"1792726810740"}}`)
	late.Spec.ReqTimestampMock = qnInsert.Add(time.Second)
	if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, nil, late); ln.Kind != KindNoise {
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
		dc := NewDriftCache()
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

// A typed instant came from a leaf of an exchange F served that drifted,
// whatever its provenance: one that was, as recorded, that instant (in any
// layout and form: an equal instant is followed), or that held the moment it
// was recorded with and the moment F saw, both ways, neither on its other
// side (rule 1). Whole, of the place, only where its text is the instant's.
// A leaf F served as recorded did not drift, and holds nothing of it; one
// that held another recorded moment, or the recorded moment only as
// recorded, is no source.
func TestATypedInstantThatDriftedWithAServedLeafIsAVariable(t *testing.T) {
	sent := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-1", At: qnInsert.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "header.x-sent-at", Noise: "header.x-sent-at", Type: String, Value: rec}, Live: live},
		}}}
	}
	for _, row := range []struct {
		name      string
		rec, live string
		source    bool
		whole     bool
	}{
		{"its epoch milliseconds", "1792726810740", "1792726870942", true, false},
		{"its text", "2026-10-23T03:40:10.740Z", "2026-10-23T03:41:10.942Z", true, true},
		{"Go's text of it", "2026-10-23T03:40:10.74Z", "2026-10-23T03:41:10.942Z", true, false},
		{"it percent-encoded", "2026-10-23T03%3A40%3A10.740Z", "2026-10-23T03%3A41%3A10.942Z", true, false},
		{"Go's text of it, F another moment", "2026-10-23T03:40:10.74Z", "2026-10-23T03:41:10.000Z", true, false},
		{"it percent-encoded, F another text", "2026-10-23T03%3A40%3A10.740Z", "none", true, false},
		{"its moments inside a longer text", "sent 1792726810740 by sqs", "sent 1792726870942 by sqs", true, false},
		{"its moments percent-encoded inside a longer text", "at=2026-10-23T03%3A40%3A10.740Z&x=1", "at=2026-10-23T03%3A41%3A10.942Z&x=1", true, false},
		{"the recorded moment only as recorded", "sent 1792726810740 by sqs", "sent 1792726870000 by sqs", false, false},
		{"F's moment beside the recorded one in F", "at 1792726810740", "at 1792726870942 was 1792726810740", false, false},
		{"another recorded moment", "1792726810741", "1792726870942", false, false},
	} {
		for wname, w := range map[string]*Window{"in its window": qnWindow(), "no window": nil} {
			ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), w, sent(row.rec, row.live), nil)
			switch {
			case row.source:
				if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != "header.x-sent-at" || ln.From.Whole != row.whole || ln.From.Echo {
					t.Errorf("%s, %s: %s (from %+v, %s); want a variable of mock-1's header.x-sent-at, whole %v", row.name, wname, ln.Kind, ln.From, ln.Why, row.whole)
				}
			case w != nil:
				if ln.Kind != KindNoise {
					t.Errorf("%s, %s: %s (from %+v, %s); want noise", row.name, wname, ln.Kind, ln.From, ln.Why)
				}
			default:
				if ln.Kind != KindUnaccounted || ln.From.Mock != "" {
					t.Errorf("%s, %s: %s (from %+v, %s); want refused for its provenance", row.name, wname, ln.Kind, ln.From, ln.Why)
				}
			}
		}
	}
	// A leaf F served as recorded did not drift: nothing came from it, its
	// recorded text whole included.
	for _, undrifted := range []string{"1792726810740", "2026-10-23T03:40:10.740Z"} {
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), sent(undrifted, undrifted), nil); ln.Kind != KindNoise {
			t.Errorf("a leaf F served as recorded (%s): %s (from %+v, %s); want noise", undrifted, ln.Kind, ln.From, ln.Why)
		}
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
		dc := NewDriftCache()
		const p = "body.q"
		r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: rec}}}
		l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: live}}}
		return ClassifyIn(dc, []string{p}, l, r, qnInsert, earlier, nil, resp, w)[0]
	}
	typedSeen := func(rec, live time.Time) []Earlier {
		return []Earlier{{Mock: "mock-1", At: qnInsert.Add(-time.Second), Own: true, Seen: []Seen{
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
		if ln := classifyTyped(qnTyped(qnRec), qnTyped(qnLive), qnWindow(), nil, resp[:1]); ln.Kind != KindNoise {
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
			dc := NewDriftCache()
			return ClassifyIn(dc, []string{qnPath}, Shape{Leaves: map[string]Leaf{qnPath: l}}, Shape{Leaves: map[string]Leaf{qnPath: r}}, at, nil, []Raw{dc.RawOf(holder, false)}, nil, w)[0]
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.live), w); ln.Kind != KindNoise {
			t.Errorf("%s, typed, in its window: %s (from %q, %s); want noise", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.rec.Add(FullRoundLead+202*time.Millisecond)), nil); ln.Kind != KindNoise {
			t.Errorf("%s, typed, moved by the lead: %s (from %q, %s); want noise", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		if ln := classify(qnTyped(row.rec), qnTyped(row.live), nil); ln.Kind != KindUnaccounted || ln.From.Mock != "" || ln.Why != instantNotClock {
			t.Errorf("%s, typed, no provenance: %s (from %q, %s); want refused for its provenance, naming no recording", row.name, ln.Kind, ln.From.Mock, ln.Why)
		}
		str := func(at time.Time) Leaf {
			return Leaf{Path: qnPath, Noise: "body.docs.msgs.0.processed_at", Type: String, Value: at.Format(time.RFC3339Nano)}
		}
		if ln := classify(str(row.rec), str(row.live), nil); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in "+holder.Name+" at "+row.field+":") {
			t.Errorf("%s, a date-like string, no provenance: %s (from %q, %s); want refused for %s, as the lane was", row.name, ln.Kind, ln.From.Mock, ln.Why, holder.Name)
		}
	}
}
