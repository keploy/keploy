package learn

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// The SQS sample's delivering ReceiveMessage of S12's E2 leg (b), run 1, as
// keploy recorded it (mock-4): its request's X-Amz-Date, its answer's Date
// header and a ReceiptHandle whose text ends in the message's epoch second
// with a fraction. attrs, when not "", is added to the message as its
// Attributes (a SentTimestamp an application may store).
func r1Receive(attrs string) *models.Mock {
	msg := `"MessageId": "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323", "MD5OfBody": "5d9bee94789228e620151756cc9c5e8e", "Body": "S12 message", "ReceiptHandle": "ODZiYTc1NDYtOGZlOC00YjdkLWIyYWEtZDA3ZjJmZDIzNTUzIGFybjphd3M6c3FzOnVzLWVhc3QtMTowMDAwMDAwMDAwMDA6bG9jYWxzdGFjay1xdWV1ZSBjZTNiYjNhOC04YmJlLTRmZDgtYjI1ZC0xZWZjNDg5ZjczMjMgMTc5MjY2NzY1Ni40NzMyMTAz"`
	if attrs != "" {
		msg += `, "Attributes": ` + attrs
	}
	return &models.Mock{Name: "mock-4", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"connID": "6", "operation": "POST", "type": "HTTP_CLIENT"},
		HTTPReq: &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "/000000000000/localstack-queue/",
			Header: map[string]string{
				"Amz-Sdk-Invocation-Id": "9e15e496-80d7-460c-bcfa-043fd37ebab3",
				"Authorization":         "AWS4-HMAC-SHA256 Credential=dummy/20261022/us-east-1/sqs/aws4_request, SignedHeaders=amz-sdk-invocation-id;amz-sdk-request;content-length;content-type;host;x-amz-date;x-amz-target;x-amzn-query-mode, Signature=ac369e82a17cef543ab9844e796222a3233cfd6d792543e85f41a18b04f31afc",
				"X-Amz-Date":            "20261022T111413Z",
				"X-Amz-Target":          "AmazonSQS.ReceiveMessage",
			},
			Body: `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/localstack-queue","WaitTimeSeconds":4}`},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{
			"Content-Type":     "application/x-amz-json-1.0",
			"Date":             "Thu, 22 Oct 2026 11:14:16 GMT",
			"X-Amzn-Requestid": "ec39f47f-d674-4f0a-99d6-15655f71954a",
		}, Body: `{"Messages": [{` + msg + `}]}`},
		ReqTimestampMock: time.Date(2026, 10, 22, 11, 14, 13, 778007762, time.UTC),
		ResTimestampMock: time.Date(2026, 10, 22, 11, 14, 16, 476120737, time.UTC),
	}}
}

// The consumer's insert (mock-8) at record.
var r1Insert = time.Date(2026, 10, 22, 11, 14, 17, 155076558, time.UTC)

// processedAt is the insert's processed_at leaf as a Mongo request's two
// readings name it: the raw reading's epoch milliseconds, the typed
// reading's RFC 3339 instant (the adapter's typedScalar).
func processedAt(at time.Time, typed bool) Shape {
	v := strconv.FormatInt(at.UnixMilli(), 10)
	if typed {
		v = at.UTC().Format(time.RFC3339Nano)
	}
	const p = "body.docs.msgs.0.processed_at.$date.$numberLong"
	return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: "body.docs.msgs.0.processed_at", Type: String, Value: v}}}
}

// shownClockRead reports whether the insert's processed_at, recorded at rec
// and live in F, in a reading, is a clock read its provenance shows: moved by
// the lead, or read within its call's window w in both runs, each at the
// resolution the reading writes it at (the raw reading's milliseconds, the
// typed reading's fraction). It is the rule as the design states it, worked
// out here apart from clockIn, so that the rows it decides test clockIn.
func shownClockRead(rec, live time.Time, typed bool, w *Window) bool {
	resOf := func(at time.Time) time.Duration {
		if !typed {
			return time.Millisecond
		}
		s := at.UTC().Format(time.RFC3339Nano)
		i := strings.IndexByte(s, '.')
		if i < 0 {
			return time.Second
		}
		r := time.Second
		for n := len(strings.TrimSuffix(s[i+1:], "Z")); n > 0; n-- {
			r /= 10
		}
		return r
	}
	rr, lr := resOf(rec), resOf(live)
	rec, live = rec.Truncate(rr), live.Truncate(lr)
	res := rr
	if lr > res {
		res = lr
	}
	if res < time.Second {
		res = time.Second
	}
	if d := live.Truncate(res).Sub(rec.Truncate(res)); d > FullRoundLead-res && d < FullRoundLead+res {
		return true
	}
	in := func(s Span, at time.Time, r time.Duration) bool {
		return !s.From.IsZero() && !s.To.IsZero() && !at.Before(s.From.Truncate(r)) && !at.After(s.To.Truncate(r))
	}
	return w != nil && in(w.Rec, rec, rr) && in(w.F, live, lr)
}

// classifyProcessedAt classifies the insert's processed_at, recorded at rec
// and live in F, recorded at insert (r1Insert when zero), against the
// recordings F served before it (raw), read in w.
func classifyProcessedAt(rec, live time.Time, typed bool, insert time.Time, w *Window, mocks ...*models.Mock) Learned {
	const p = "body.docs.msgs.0.processed_at.$date.$numberLong"
	if insert.IsZero() {
		insert = r1Insert
	}
	dc := NewDriftCache()
	var raw []Raw
	for _, m := range mocks {
		raw = append(raw, dc.RawOf(m, true))
	}
	return ClassifyIn(dc, []string{p}, processedAt(live, typed), processedAt(rec, typed), insert, nil, raw, nil, w)[0]
}

// windows are the ways the insert's clock read can fall against its call's
// window (Window): in it, read before the call's previous call (a concurrent
// handler's call sent between the consumer's read and its insert, the E2
// run), and no window at all (the lead alone).
func r1Windows(rec, live time.Time) map[string]*Window {
	f := Span{From: live.Add(-40 * time.Millisecond), To: live.Add(30 * time.Millisecond)}
	return map[string]*Window{
		"in its call's window":          {Rec: Span{From: rec.Add(-2 * time.Millisecond), To: r1Insert}, F: f},
		"read before the previous call": {Rec: Span{From: rec.Add(2 * time.Millisecond), To: r1Insert}, F: f},
		"by the lead alone, no window":  nil,
	}
}

// The consumer's processed_at, a read of its clock, is noise where its
// provenance shows it is one: read within its call's window in both runs
// (Window), whatever the receive's Date header, its X-Amz-Date or its
// ReceiptHandle's epoch second share of its minute, its second or the digits
// of its millisecond, in both readings. Nothing else excuses it: what the text
// looks like decides nothing. Outside its window (a read before the call's
// previous call, or no window at all) the typed reading's words are searched
// as any value's, and the receive's Date header holds its minute: refused,
// naming the receive, a false red that fails closed. The raw reading is one
// word, its milliseconds, searched whole; the receive holds that moment
// nowhere, so it is noise however it was read.
func TestAClockReadIsNoiseOnlyWhereItsWindowShowsIt(t *testing.T) {
	// The receive as recorded, and one whose attributes hold, as words of
	// their own, the instants' minute, second and millisecond digits: a
	// receive count, a sequence number.
	receives := []*models.Mock{r1Receive(""), r1Receive(`{"ApproximateReceiveCount": "14", "SequenceNumber": "17", "MessageGroupId": "012"}`)}
	for _, row := range []struct {
		name string
		rec  time.Time
	}{
		// The E2 run's: one minute with the Date header and the X-Amz-Date.
		{"the same minute", time.Date(2026, 10, 22, 11, 14, 17, 12e6, time.UTC)},
		// The Date header's second.
		{"the same second", time.Date(2026, 10, 22, 11, 14, 16, 12e6, time.UTC)},
		// The ReceiptHandle's 1792667656.4732103, to the millisecond.
		{"a sub-second offset", time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)},
		// The Date header's moment itself: a BSON date on a whole second,
		// which the typed reading writes to the second ("…:16Z").
		{"the Date header's moment", time.Date(2026, 10, 22, 11, 14, 16, 0, time.UTC)},
	} {
		// F's read a minute on (E2's), and within the recording's minute.
		for _, live := range []time.Time{row.rec.Add(60*time.Second + 638*time.Millisecond), row.rec.Add(19*time.Second + 461*time.Millisecond)} {
			w := r1Windows(row.rec, live)["in its call's window"]
			for i, receive := range receives {
				for _, typed := range []bool{false, true} {
					if ln := classifyProcessedAt(row.rec, live, typed, time.Time{}, w, receive); ln.Kind != KindNoise {
						t.Errorf("%s, live %s, in its call's window, receive %d, typed %v: %s (from %q, %s); want noise", row.name, live.Format(time.RFC3339Nano), i, typed, ln.Kind, ln.From.Mock, ln.Why)
					}
				}
			}
		}
	}
	// The E2 run's read outside its window: F's read a minute on, its minute
	// "14" lost, which the Date header holds.
	rec := time.Date(2026, 10, 22, 11, 14, 17, 12e6, time.UTC)
	live := rec.Add(60*time.Second + 638*time.Millisecond)
	for _, wname := range []string{"read before the previous call", "by the lead alone, no window"} {
		w := r1Windows(rec, live)[wname]
		for i, receive := range receives {
			if ln := classifyProcessedAt(rec, live, false, time.Time{}, w, receive); ln.Kind != KindNoise {
				t.Errorf("%s, receive %d, raw: %s (from %q, %s); want noise: the receive holds its milliseconds nowhere", wname, i, ln.Kind, ln.From.Mock, ln.Why)
			}
			if ln := classifyProcessedAt(rec, live, true, time.Time{}, w, receive); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
				t.Errorf("%s, receive %d, typed: %s (from %q, %s); want refused, a variable from mock-4: its minute is a word of the receive's Date header, and nothing shows it is a clock read", wname, i, ln.Kind, ln.From.Mock, ln.Why)
			}
		}
	}
}

// An application that stores an instant it was sent stores, in F, the
// instant of the answer F served, as it stored the recorded one at record:
// the payloads hold the instant both ways, and it is a variable of the place
// that held it when recorded, in each reading, in every layout, and wherever
// the read fell against its call's window. Held one way only (F's value in
// no payload), a data-plane answer's instant is followed as the same value
// whole, in the reading that writes it so (a founder decision); a
// protocol-phase answer's own clock read (instants.go, rule 2) is no place it
// came from: noise, but echoed both ways, or recorded outside its answer's
// span. As another moment (its second, a finer one its digits begin) it is
// none.
func TestAWholeInstantEchoIsStillAVariable(t *testing.T) {
	for _, sent := range []time.Time{
		time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC),
		time.Date(2026, 10, 22, 11, 14, 16, 470e6, time.UTC), // typed "…16.47Z"
	} {
		// The message F's receive delivered: another recording's.
		got := sent.Add(2*time.Minute + 311*time.Millisecond)
		forms := func(at time.Time) []string {
			ms, sec := strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(at.Unix(), 10)
			return []string{
				`{"SentTimestamp": "` + ms + `"}`,
				`{"Note": "sent at ` + at.Format("2006-01-02T15:04:05.000Z07:00") + ` by the producer"}`,
				`{"SentAt": "` + sec + `.` + fmt.Sprintf("%03d", at.Nanosecond()/1e6) + `"}`,
			}
		}
		recorded, served := forms(sent), forms(got)
		// Whether a form holds the value as a reading writes it, text for
		// text (the raw reading's epoch milliseconds; the typed reading's
		// RFC 3339, which drops trailing zeros).
		sameText := func(form string, typed bool) bool {
			for _, l := range processedAt(sent, typed).Leaves {
				return ContainsToken(form, l.Value)
			}
			return false
		}
		for i := range recorded {
			receive, delivered := r1Receive(recorded[i]), r1Receive(served[i])
			delivered.Name = "mock-11"
			// The receive a protocol-phase exchange (a hook's config call),
			// its attribute within its span: its answer's own clock read.
			config := r1Receive(recorded[i])
			config.Spec.Metadata["type"] = "config"
			// That exchange answered before the producer sent: no clock
			// read of its answer.
			early := r1Receive(recorded[i])
			early.Spec.Metadata["type"] = "config"
			early.Spec.ReqTimestampMock = sent.Add(-2 * time.Hour)
			early.Spec.ResTimestampMock = sent.Add(-time.Hour)
			for wname, w := range r1Windows(sent, got) {
				for _, typed := range []bool{false, true} {
					for _, m := range []*models.Mock{receive, config} {
						if ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, m, delivered); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
							t.Errorf("%s then %s (config %v), %s, typed %v: %s from %q; want a variable from mock-4", recorded[i], served[i], m == config, wname, typed, ln.Kind, ln.From.Mock)
						}
					}
					if ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, config); ln.Kind != KindNoise {
						t.Errorf("%s, a protocol-phase answer's clock read, F's value in no payload, %s, typed %v: %s from %q; want noise", recorded[i], wname, typed, ln.Kind, ln.From.Mock)
					}
					for _, m := range []*models.Mock{receive, early} {
						// Held as the reading writes it, it is followed;
						// else noise where its provenance shows a clock
						// read (shownClockRead), and elsewhere the typed
						// reading's words are searched as any value's: the
						// receive's Date header holds its minute, refused.
						want := sameText(recorded[i], typed) || typed && !shownClockRead(sent, got, typed, w)
						ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, m)
						if want && (ln.Kind != KindVariable || ln.From.Mock != "mock-4") || !want && ln.Kind != KindNoise {
							t.Errorf("%s, no protocol-phase answer's clock read (early %v), F's value in no payload, %s, typed %v: %s from %q; want a variable %v", recorded[i], m == early, wname, typed, ln.Kind, ln.From.Mock, want)
						}
					}
				}
			}
		}
		ms, sec := strconv.FormatInt(sent.UnixMilli(), 10), strconv.FormatInt(sent.Unix(), 10)
		gms, gsec := strconv.FormatInt(got.UnixMilli(), 10), strconv.FormatInt(got.Unix(), 10)
		for _, other := range [][2]string{
			{`{"SentTimestamp": "` + sec + `"}`, `{"SentTimestamp": "` + gsec + `"}`},
			{`{"SentTimestamp": "` + ms + `123"}`, `{"SentTimestamp": "` + gms + `123"}`},
			{`{"SentAt": "` + sec + `.4732103"}`, `{"SentAt": "` + gsec + `.4732103"}`},
		} {
			receive, delivered := r1Receive(other[0]), r1Receive(other[1])
			delivered.Name = "mock-11"
			for wname, w := range r1Windows(sent, got) {
				for _, typed := range []bool{false, true} {
					// Where no provenance shows a clock read the typed
					// reading's words are searched: the Date header holds
					// its minute, refused.
					want := KindNoise
					if typed && !shownClockRead(sent, got, typed, w) {
						want = KindVariable
					}
					if ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, receive, delivered); ln.Kind != want {
						t.Errorf("%s then %s, %s, typed %v: %s (from %q); want %s", other[0], other[1], wname, typed, ln.Kind, ln.From.Mock, want)
					}
				}
			}
		}
	}
}

// r2Hello is a Mongo driver's isMaster handshake as S12 E3 leg (b), run r2,
// recorded it (mock-5, mock-7): sent on a new connection while the lane held
// Mongo, answered when the hold was released, its localTime the server's
// clock as it answered.
func r2Hello(name string, conn int32, asked, answered time.Time, localTime int64) *models.Mock {
	return &models.Mock{Name: name, Kind: models.Mongo, ConnectionID: strconv.Itoa(int(conn)), Spec: models.MockSpec{
		Metadata: map[string]string{"type": "config", "connID": strconv.Itoa(int(conn))},
		MongoRequests: []models.MongoRequest{{
			//nolint:staticcheck // SA1019: the driver's isMaster handshake is a genuine legacy OpQuery frame.
			Header: &models.MongoHeader{Length: 276, RequestID: 12, Opcode: wiremessage.OpQuery},
			Message: &models.MongoOpQuery{Flags: 4, FullCollectionName: "admin.$cmd", NumberToReturn: -1,
				Query: `{"isMaster":{"$numberInt":"1"},"helloOk":true,"compression":[],"client":{"driver":{"name":"mongo-go-driver","version":"1.17.3"},"os":{"type":"linux","architecture":"amd64"},"platform":"go1.27.0","env":{"container":{"runtime":"docker"}}}}`},
		}},
		MongoResponses: []models.MongoResponse{{
			Header: &models.MongoHeader{Opcode: wiremessage.OpReply, ResponseTo: 12},
			Message: &models.MongoOpReply{ResponseFlags: 8, NumberReturned: 1, Documents: []string{
				`{"helloOk":true,"ismaster":true,"topologyVersion":{"processId":{"$oid":"6ad9efef4a1daead80f664bf"},"counter":{"$numberLong":"0"}},"maxBsonObjectSize":{"$numberInt":"16777216"},"maxMessageSizeBytes":{"$numberInt":"48000000"},"maxWriteBatchSize":{"$numberInt":"100000"},"localTime":{"$date":{"$numberLong":"` + strconv.FormatInt(localTime, 10) + `"}},"logicalSessionTimeoutMinutes":{"$numberInt":"30"},"connectionId":{"$numberInt":"13"},"minWireVersion":{"$numberInt":"0"},"maxWireVersion":{"$numberInt":"29"},"readOnly":false,"ok":{"$numberDouble":"1.0"}}`,
			}},
		}},
		ReqTimestampMock: asked,
		ResTimestampMock: answered,
	}}
}

// r2Mongo is a Mongo OP_MSG recording of S12 E3 leg (b), run r2: request and
// answer sections, recorded from asked to answered.
func r2Mongo(name string, asked, answered time.Time, request []string, answer string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"operation": "OP_MSG"},
		MongoRequests:    []models.MongoRequest{{Header: &models.MongoHeader{RequestID: 9, Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: request}}},
		MongoResponses:   []models.MongoResponse{{Header: &models.MongoHeader{ResponseTo: 9, Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{answer}}}},
		ReqTimestampMock: asked,
		ResTimestampMock: answered,
	}}
}

// S12 E3, leg (b), run r2 (keploy 6db6cd0f3): the consumer read its clock for
// processed_at (1792694293458, 18:38:13.458) in the millisecond the lane's
// hold released Mongo, which the driver's handshakes on post-url-1's new
// connections (mock-5, mock-7) were answered in, each with that millisecond
// as its localTime; record's full round then refused the boot insert's drift
// pairing ("processed_at carries a value a recording of the test set holds
// earlier (in mock-5)"), and the set failed. These are r2's recordings made
// before the insert, trimmed: the delivering receive (mock-4), the
// handshakes, the duplicate read (mock-6, answered at 13.458619) and
// post-url-1's upsert (mock-8, its created read at 13.219). Two clocks that
// read one millisecond are no copy: a handshake's localTime is its answer's
// own clock read (rule 2), which no value came from, so the raw reading is
// noise wherever the read fell against its call's window. The typed reading
// is noise in its window; outside it its words are searched as any value's,
// and the receive's Date header holds its second: refused, naming the
// receive. The rule is the answer's span alone: a localTime the recorded
// answer came before (a server whose clock ran ahead) is followed, and so is
// that millisecond in a request (by the raw reading, which writes it so); and
// a localTime F served too, as the value it saw, is an echo.
func TestAHandshakesLocalTimeIsNoSourceOfAClockRead(t *testing.T) {
	ts := func(sec, nsec int) time.Time { return time.Date(2026, 10, 22, 18, 38, sec, nsec, time.UTC) }
	const read = 1792694293458 // processed_at, recorded
	receive := r1Receive("")
	receive.Spec.ReqTimestampMock, receive.Spec.ResTimestampMock = ts(12, 106755139), ts(13, 111622044)
	receive.Spec.HTTPReq.Header["X-Amz-Date"] = "20261022T183812Z"
	receive.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 18:38:13 GMT"
	hello5 := r2Hello("mock-5", 8, ts(13, 205118312), ts(13, 458911204), read)
	hello7 := r2Hello("mock-7", 10, ts(13, 301455028), ts(13, 459120377), read)
	count := r2Mongo("mock-6", ts(13, 112600041), ts(13, 458619421),
		[]string{`{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$match":{"event":"S12 message"}},{"$group":{"_id":{"$numberInt":"1"},"n":{"$sum":{"$numberInt":"1"}}}}],"cursor":{},"lsid":{"id":{"$binary":{"base64":"cTrN0a9jSF2Bikv/YvQI4A==","subType":"04"}}},"$db":"keploy"} }`},
		`{ SectionSingle msg: {"cursor":{"firstBatch":[],"id":{"$numberLong":"0"},"ns":"keploy.events"},"ok":{"$numberDouble":"1.0"}} }`)
	upsert := r2Mongo("mock-8", ts(13, 459791587), ts(14, 421828601),
		[]string{`{ SectionSingle msg: {"update":"url-shortener","ordered":true,"writeConcern":{"w":"majority"},"$db":"keploy"} }`,
			`{ SectionSingle identifier: updates , msgs: [ {"q":{"_id":"Lhr4BWAi"},"u":{"$set":{"_id":"Lhr4BWAi","created":{"$date":{"$numberLong":"1792694293219"}},"updated":{"$date":{"$numberLong":"1792694293219"}},"url":"https://google.com"}},"upsert":true} ] }`},
		`{ SectionSingle msg: {"n":{"$numberInt":"1"},"upserted":[{"index":{"$numberInt":"0"},"_id":"Lhr4BWAi"}],"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`)
	insert := ts(13, 459990012)
	rec := time.UnixMilli(read).UTC()
	// F's boot read, the lead past the recording and some.
	for _, live := range []time.Time{rec.Add(FullRoundLead + 813*time.Millisecond), rec.Add(FullRoundLead - 1250*time.Millisecond)} {
		windows := map[string]*Window{
			"in its call's window":          {Rec: Span{From: ts(13, 458619421), To: insert}, F: Span{From: live.Add(-time.Millisecond), To: live.Add(time.Millisecond)}},
			"read before the previous call": {Rec: Span{From: ts(13, 459791587), To: insert}, F: Span{From: live.Add(-time.Millisecond), To: live.Add(time.Millisecond)}},
			"by the lead alone, no window":  nil,
		}
		for wname, w := range windows {
			for _, typed := range []bool{false, true} {
				// The typed reading where no provenance shows a clock read
				// (shownClockRead): refused, the receive's Date header
				// holding its minute.
				byDate := typed && !shownClockRead(rec, live, typed, w)
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, hello7, upsert); byDate && (ln.Kind != KindVariable || ln.From.Mock != "mock-4") || !byDate && ln.Kind != KindNoise {
					t.Errorf("live %s, %s, typed %v: %s (from %q, %s); want refused by mock-4 %v, else noise", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, ln.Why, byDate)
				}
				// The server's clock ran ahead: its localTime is later than
				// its answer reached keploy, no read of its answer's.
				followed := !typed
				from := "mock-5"
				if byDate {
					followed, from = true, "mock-4"
				}
				ahead := r2Hello("mock-5", 8, ts(13, 205118312), ts(13, 457911204), read)
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, ahead, count, upsert); followed && (ln.Kind != KindVariable || ln.From.Mock != from) || !followed && ln.Kind != KindNoise {
					t.Errorf("a localTime after its answer, live %s, %s, typed %v: %s from %q; want a variable from %s %v", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, from, followed)
				}
				// That millisecond in a request: the app's own, followed.
				from = "mock-8"
				if byDate {
					from = "mock-4"
				}
				sent := r2Mongo("mock-8", ts(13, 459791587), ts(14, 421828601),
					[]string{`{ SectionSingle identifier: updates , msgs: [ {"q":{"_id":"Lhr4BWAi"},"u":{"$set":{"created":{"$date":{"$numberLong":"1792694293458"}}}}} ] }`}, `{ SectionSingle msg: {"ok":{"$numberDouble":"1.0"}} }`)
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, sent); followed && (ln.Kind != KindVariable || ln.From.Mock != from) || !followed && ln.Kind != KindNoise {
					t.Errorf("that millisecond in a request, live %s, %s, typed %v: %s from %q; want a variable from %s %v", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, from, followed)
				}
				// F served a handshake whose localTime is the value F saw.
				echo := r2Hello("mock-9", 12, ts(15, 0), ts(15, 1000), live.UnixMilli())
				from = "mock-5"
				if byDate {
					from = "mock-4"
				}
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, echo); ln.Kind != KindVariable || ln.From.Mock != from {
					t.Errorf("F's value a localTime F served, live %s, %s, typed %v: %s from %q; want a variable from %s", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, from)
				}
			}
		}
	}
}

// configReceive is the receive as a protocol-phase exchange: a post-record
// hook's config call, as enterprise's SQS hook tags one.
func configReceive(attrs string) *models.Mock {
	m := r1Receive(attrs)
	m.Spec.Metadata["type"] = "config"
	return m
}

// A protocol-phase answer's own clock reads (Raw.AnswerClock()) are the
// instants its answer holds within the span from its request to its answer,
// at their own resolution, that its request does not hold: none of its
// request's, none recorded outside that span, no time of day (which names
// no day), and none of a mock whose span or answer is not known. A
// data-plane answer has none: it may read back what the application stored.
func TestAnswerClock(t *testing.T) {
	keys := func(r Raw) []string {
		var out []string
		for _, at := range r.AnswerClock() {
			out = append(out, at.Format(time.RFC3339Nano))
		}
		return out
	}
	receive := configReceive(`{"SentTimestamp": "1792667656470", "Created": "2026-10-22T09:00:00Z", "At": "11:14:15"}`)
	if got, want := keys(RawOf(receive, false)), []string{"2026-10-22T11:14:16Z", "2026-10-22T11:14:16.47Z", "2026-10-22T11:14:16.4732103Z"}; !sameSet(got, want) {
		t.Fatalf("the receive's answer's clock reads are %q; want %q (its Date, its SentTimestamp, its ReceiptHandle's instant)", got, want)
	}
	// Its request holding the Date's moment: no clock read of its answer.
	asked := configReceive("")
	asked.Spec.HTTPReq.Header["If-Modified-Since"] = "Thu, 22 Oct 2026 11:14:16 GMT"
	if got := keys(RawOf(asked, false)); !sameSet(got, []string{"2026-10-22T11:14:16.4732103Z"}) {
		t.Fatalf("with its request holding the Date's moment: %q", got)
	}
	// A second read inside the span, at its resolution: the request sent
	// at 11:14:13.778 and an answer of second 13 holds it (the request
	// signed in second 12).
	same := configReceive("")
	same.Spec.HTTPReq.Header["X-Amz-Date"] = "20261022T111412Z"
	same.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:14:13 GMT"
	if got := keys(RawOf(same, false)); !sameSet(got, []string{"2026-10-22T11:14:13Z", "2026-10-22T11:14:16.4732103Z"}) {
		t.Fatalf("a Date of the request's second: %q", got)
	}
	// Its request signed in that second too: none.
	same = configReceive("")
	same.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:14:13 GMT"
	if got := keys(RawOf(same, false)); !sameSet(got, []string{"2026-10-22T11:14:16.4732103Z"}) {
		t.Fatalf("a Date of the second the request was signed in: %q", got)
	}
	for name, m := range map[string]func(*models.Mock){
		"no answer time":  func(m *models.Mock) { m.Spec.ResTimestampMock = time.Time{} },
		"no request time": func(m *models.Mock) { m.Spec.ReqTimestampMock = time.Time{} },
		"no answer":       func(m *models.Mock) { m.Spec.HTTPResp = nil },
		"answered before it was asked": func(m *models.Mock) {
			m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-time.Second)
		},
	} {
		x := configReceive("")
		m(x)
		if got := keys(RawOf(x, false)); len(got) != 0 {
			t.Errorf("%s: %q; want none", name, got)
		}
	}
	if got := RawOf(r1Receive(`{"SentTimestamp": "1792667656470"}`), false).AnswerClock(); got != nil {
		t.Fatalf("a data-plane answer's clock reads: %v; want none", got)
	}
	// A cache reads it once per mock, served or not, as RawOf does.
	dc := NewDriftCache()
	if a, b := dc.RawOf(receive, true), dc.RawOf(receive, false); !reflect.DeepEqual(a.AnswerClock(), RawOf(receive, false).AnswerClock()) || !reflect.DeepEqual(b.AnswerClock(), a.AnswerClock()) || !a.Served || b.Served {
		t.Fatalf("cached reads: %+v, %+v", a.AnswerClock(), b.AnswerClock())
	}
	if r, err := dc.ReadRaw(receive, true); err != nil || !reflect.DeepEqual(r.AnswerClock(), RawOf(receive, false).AnswerClock()) {
		t.Fatalf("ReadRaw: %v, %+v", err, r.AnswerClock())
	}
}

// sameSet reports whether a and b hold the same strings.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		if m[s] == 0 {
			return false
		}
		m[s]--
	}
	return true
}

// Every field of a mock's spec that holds an answer is one answerClock reads
// as the answer's: a new parser's answer field must be named there, or its
// answers' clock reads would be read as its request's (followed: a false
// red).
func TestEveryAnswerFieldIsAnAnswer(t *testing.T) {
	named := map[string]bool{}
	st := reflect.TypeOf(models.MockSpec{})
	for _, n := range answerFieldNames {
		if _, ok := st.FieldByName(n); !ok {
			t.Errorf("models.MockSpec has no field %s", n)
		}
		named[n] = true
	}
	if len(answerFields) != len(answerFieldNames) {
		t.Errorf("answerFields holds %d JSON names for %d fields", len(answerFields), len(answerFieldNames))
	}
	for i := 0; i < st.NumField(); i++ {
		n := st.Field(i).Name
		answer := strings.HasSuffix(n, "Resp") || strings.Contains(n, "Response")
		if answer != named[n] {
			t.Errorf("models.MockSpec.%s: an answer %v, named as one %v", n, answer, named[n])
		}
	}
}

// The instants a text holds, in either case, and a time-ordered UUID's time
// groups; and the moment each names.
func TestInstantSpans(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"Date: Thu, 22 Oct 2026 11:14:16 GMT; seq 14", []string{"Thu, 22 Oct 2026 11:14:16 GMT"}},
		{"date: thu, 22 oct 2026 11:14:16 gmt", []string{"thu, 22 oct 2026 11:14:16 gmt"}},
		{"x-amz-date=20261022t111413z&n=7", []string{"20261022t111413z"}},
		{"86ba7546 1792667656.4732103", []string{"1792667656.4732103"}},
		{"at=" + strconv.FormatInt(at.UnixMilli(), 10) + " ok", []string{strconv.FormatInt(at.UnixMilli(), 10)}},
		{"id=" + uuid7(at, "abc123456789abcdef"), []string{uuid7(at, "abc123456789abcdef")[:13]}},
		{"order 2025-13-45 x", nil},
		{"seq 14 of 41", nil},
	} {
		var got []string
		for _, sp := range instantSpans(tc.text) {
			got = append(got, tc.text[sp.start:sp.end])
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("instantSpans(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
	key := func(s string) instantKey {
		t.Helper()
		var keys []instantKey
		for _, sp := range instantSpans(s) {
			if sp.keyed {
				keys = append(keys, sp.key)
			}
		}
		if len(keys) != 1 {
			t.Fatalf("%q holds %d keyed instants; want 1", s, len(keys))
		}
		return keys[0]
	}
	for _, one := range [][2]string{
		{strconv.FormatInt(at.UnixMilli(), 10), at.Format("2006-01-02T15:04:05.000Z")},
		{"2026-10-22T11:14:16.47Z", "1792667656470"},
		{at.Format(time.RFC3339), at.Truncate(time.Second).Format("2006-01-02T15:04:05.000Z")},
		{at.Format(time.RFC1123), at.Format(time.RFC3339)},
		{at.Format(time.RFC1123), strings.ToLower(at.Format(time.RFC1123))},
		{at.Format("20060102T150405Z"), strconv.FormatInt(at.Unix(), 10)},
		{"1792667656.4732103", "2026-10-22T11:14:16.4732103Z"},
	} {
		if key(one[0]) != key(one[1]) {
			t.Errorf("%q and %q are two instants; want one", one[0], one[1])
		}
	}
	for _, two := range [][2]string{
		{strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(at.UnixMilli(), 10)},
		{"1792667656.4732103", strconv.FormatInt(at.UnixMilli(), 10)},
		{at.Format(time.RFC1123), at.Format("2006-01-02T15:04:05.000Z")},
		{at.Format("15:04:05"), at.Format(time.RFC3339)},
	} {
		if key(two[0]) == key(two[1]) {
			t.Errorf("%q and %q are one instant; want two", two[0], two[1])
		}
	}
}

// A word of a value is never found in a protocol-phase answer's own clock
// read (rule 2): a batch number that drifted from 14 to 15 is no word of a
// hook's config call's Date header, whose minute is 14. A data-plane
// answer's Date is searched as any text is (it may hold what the
// application stored), and so is a protocol-phase exchange's of no known
// answer time and an instant of a request: a date there whose minute is 14
// holds the word, refused. Held outside every instant (a receive count of
// 14), it is found, as before; and in the call's own answer, a word of a
// date there is a word of the answer, refused.
func TestAWordIsNeverFoundInAProtocolPhaseAnswersClockRead(t *testing.T) {
	r := Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: "batch-14"}}}
	l := Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: "batch-15"}}}
	classify := func(receive *models.Mock, resp []Leaf) Learned {
		dc := NewDriftCache()
		return ClassifyIn(dc, []string{"body.batch"}, l, r, r1Insert, nil, []Raw{dc.RawOf(receive, true)}, resp, nil)[0]
	}
	if ln := classify(configReceive(""), nil); ln.Kind != KindNoise {
		t.Fatalf("beside a config call's Date header: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	asked := configReceive("")
	asked.Spec.HTTPReq.Header["If-Modified-Since"] = "Thu, 22 Oct 2026 10:14:09 GMT"
	unknown := configReceive("")
	unknown.Spec.ResTimestampMock = time.Time{}
	for name, m := range map[string]*models.Mock{
		"a data-plane receive's Date":                   r1Receive(""),
		"a date of its request whose minute is 14":      asked,
		"the Date of a config call of no answer time":   unknown,
		"a receive count of 14":                         configReceive(`{"ApproximateReceiveCount": "14"}`),
		"a data-plane receive count of 14 and its Date": r1Receive(`{"ApproximateReceiveCount": "14"}`),
	} {
		if ln := classify(m, nil); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
			t.Errorf("beside %s: %s from %q; want a variable from mock-4", name, ln.Kind, ln.From.Mock)
		}
	}
	for name, v := range map[string]string{"a date of its minute": "Thu, 22 Oct 2026 11:14:16 GMT", "14 as a word": "seen 14 times"} {
		resp := []Leaf{{Path: "body.at", Noise: "body.at", Type: String, Value: v}}
		if ln := classify(configReceive(""), resp); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
			t.Errorf("the call's answer holding %s: %s (%s); want it refused", name, ln.Kind, ln.Why)
		}
	}
	// A word F saw (15) in a config call's Date, its answer's clock read, is
	// no word of the payload for unaccounted either.
	r13 := Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: "batch-13"}}}
	late := configReceive("")
	late.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:15:16 GMT"
	late.Spec.ResTimestampMock = time.Date(2026, 10, 22, 11, 15, 16, 900e6, time.UTC)
	dc := NewDriftCache()
	if ln := ClassifyIn(dc, []string{"body.batch"}, l, r13, r1Insert.Add(time.Minute), nil, []Raw{dc.RawOf(late, true)}, nil, nil)[0]; ln.Kind != KindNoise {
		t.Fatalf("15, a word of a config call's Date: %s (%s); want noise", ln.Kind, ln.Why)
	}
	late.Spec.Metadata["type"] = "HTTP_CLIENT"
	dc = NewDriftCache()
	if ln := ClassifyIn(dc, []string{"body.batch"}, l, r13, r1Insert.Add(time.Minute), nil, []Raw{dc.RawOf(late, true)}, nil, nil)[0]; ln.Kind != KindUnaccounted {
		t.Fatalf("15, a word of a data-plane receive's Date: %s (%s); want refused", ln.Kind, ln.Why)
	}
}

// An instant's words are words like any value's, wherever they are: at an
// earlier leaf F saw, a stamp that drifted in lockstep with an earlier one in
// another precision (minute and second alike) is a value of that leaf,
// refused as one, and a word of it a leaf F saw unchanged holds (an answer's
// expiry it copied at record) is unaccounted for. In the call's own answer an
// id sent inside a filter, whatever its digits read as, which the answer
// echoes, is refused, and so is a stamp whose minute the answer holds: no
// value is a unit of no words because of its shape.
func TestAnInstantsWordsAreWordsWhereverTheyAre(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 16, 470e6, time.UTC)
	f := at.Add(3*time.Minute + 3*time.Second)
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{"body.at": {Path: "body.at", Noise: "body.at", Type: String, Value: v}}}
	}
	audit := []Earlier{{Mock: "mock-1", At: r1Insert.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request,
		Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: at.Format("2006-01-02T15:04:05.000Z")}, Live: f.Format("2006-01-02T15:04:05.000Z"), Place: "body.at"}}}}
	if ln := ClassifyIn(NewDriftCache(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, audit, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
		t.Fatalf("in lockstep with an earlier leaf's stamp: %s from %q; want a variable from mock-1", ln.Kind, ln.From.Mock)
	}
	expiry := at.Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	token := []Earlier{{Mock: "mock-2", At: r1Insert.Add(-time.Second), Own: true, Seen: []Seen{{Side: Response,
		Leaf: Leaf{Path: "body.expiresAt", Noise: "body.expiresAt", Type: String, Value: expiry}, Live: expiry}}}}
	if ln := ClassifyIn(NewDriftCache(), []string{"body.at"}, leaf(f.Add(time.Hour).Format(time.RFC3339)), leaf(expiry), r1Insert, token, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-2") {
		t.Fatalf("a copy of an expiry F served unchanged: %s (%s); want it unaccounted for", ln.Kind, ln.Why)
	}
	resp := []Leaf{{Path: "body.id", Noise: "body.id", Type: String, Value: "1298498081"}, {Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + at.Format("05")}}
	filter := Shape{Leaves: map[string]Leaf{"body.q": {Path: "body.q", Noise: "body.q", Type: String, Value: "user-1298498081"}}}
	now := Shape{Leaves: map[string]Leaf{"body.q": {Path: "body.q", Noise: "body.q", Type: String, Value: "user-1427131847"}}}
	if ln := ClassifyIn(NewDriftCache(), []string{"body.q"}, now, filter, r1Insert, nil, nil, resp, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
		t.Fatalf("an id the answer echoes: %s (%s); want refused", ln.Kind, ln.Why)
	}
	if ln := ClassifyIn(NewDriftCache(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, nil, nil, resp, nil)[0]; ln.Kind != KindNoise {
		t.Fatalf("a stamp no word of whose drift the answer holds: %s (%s); want noise", ln.Kind, ln.Why)
	}
	minute := []Leaf{{Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + at.Format("04")}}
	if ln := ClassifyIn(NewDriftCache(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, nil, nil, minute, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
		t.Fatalf("a stamp whose minute the answer holds: %s (%s); want refused", ln.Kind, ln.Why)
	}
}

// An id whose digits read as a date flows as every id does, from a place
// that held it whole into a longer value, at a leaf F saw drift and in an
// earlier recording's payload: a variable of that place, whatever its
// length, ten digits (a rand.Int31 id), nineteen (a Snowflake id) or twelve
// (no epoch's). What its digits read as decides nothing.
func TestAnIdThatReadsAsADateFlowsAsAnId(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 0, 1, 0, time.UTC)
	const p = "body.ref"
	for _, ids := range [][2]string{{"1298498081", "1427131847"}, {"1541815603606036480", "1584512345678901234"}, {"129849808112", "142713184712"}} {
		rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "user-" + ids[0]}}}
		live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "user-" + ids[1]}}}
		earlier := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request,
			Leaf: Leaf{Path: "body.userId", Noise: "body.userId", Type: String, Value: ids[0]}, Live: ids[1], Place: "body.userId"}}}}
		if ln := ClassifyIn(NewDriftCache(), []string{p}, live, rec, at, earlier, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
			t.Errorf("%s at an earlier leaf: %s from %q; want a variable from mock-1", ids[0], ln.Kind, ln.From.Mock)
		}
		write := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "/users", Body: `{"userId":` + ids[0] + `,"name":"a"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 201, Body: `{"ok":true}`},
			ReqTimestampMock: at.Add(-time.Second),
			ResTimestampMock: at.Add(-time.Second + time.Millisecond),
		}}
		dc := NewDriftCache()
		if ln := ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{dc.RawOf(write, true)}, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
			t.Errorf("%s in an earlier payload: %s from %q; want a variable from mock-1", ids[0], ln.Kind, ln.From.Mock)
		}
	}
}

// S12 E3, leg (b), the archived runs that failed (r1/legb-archive, keploy
// 6db6cd0f3, CI shape): the consumer's insert rode a connection the driver
// opened for it, whose isMaster handshake (mock-6, a config recording) was
// answered in the millisecond processed_at was read, with that millisecond
// as its localTime. Exact values. In its call's window, both readings are
// noise, so the boot insert pairs by drift; and the raw reading is noise
// wherever the read fell, the localTime being the handshake's own clock read
// (rule 2). Outside its window the typed reading is checked as any value.
func TestTheArchivedLegBHandshakeIsNoSourceOfProcessedAt(t *testing.T) {
	for _, run := range []struct {
		name                    string
		asked, answered, insert time.Time
		read                    int64
	}{
		{"run-2", time.Date(2026, 10, 22, 20, 33, 39, 892516772, time.UTC), time.Date(2026, 10, 22, 20, 33, 39, 909520718, time.UTC), time.Date(2026, 10, 22, 20, 33, 39, 909704835, time.UTC), 1792701219909},
		{"run-3", time.Date(2026, 10, 22, 20, 34, 7, 506459279, time.UTC), time.Date(2026, 10, 22, 20, 34, 7, 530402069, time.UTC), time.Date(2026, 10, 22, 20, 34, 7, 530640923, time.UTC), 1792701247530},
	} {
		hello := r2Hello("mock-6", 8, run.asked, run.answered, run.read)
		rec := time.UnixMilli(run.read).UTC()
		for _, live := range []time.Time{rec.Add(FullRoundLead + 211*time.Millisecond), rec.Add(FullRoundLead + 3*time.Second)} {
			f := Span{From: live.Add(-2 * time.Millisecond), To: live.Add(time.Millisecond)}
			for wname, w := range map[string]*Window{
				"in its call's window":          {Rec: Span{From: run.asked, To: run.insert}, F: f},
				"read before the previous call": {Rec: Span{From: rec.Add(time.Millisecond), To: run.insert}, F: f},
				"by the lead alone, no window":  nil,
			} {
				for _, typed := range []bool{false, true} {
					ln := classifyProcessedAt(rec, live, typed, run.insert, w, hello)
					// In its window, or by the raw reading anywhere (the
					// localTime is the handshake's own clock read), noise.
					// The typed reading outside its window is checked as any
					// value: noise where no word of it is found, refused
					// where the handshake holds one, and a refusal names the
					// handshake.
					if wname == "in its call's window" || !typed {
						if ln.Kind != KindNoise {
							t.Errorf("%s, live %s, %s, typed %v: %s (from %q, %s); want noise", run.name, live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, ln.Why)
						}
					} else if ln.Kind != KindNoise && ln.From.Mock != "mock-6" && !strings.Contains(ln.Why, "mock-6") {
						t.Errorf("%s, live %s, %s, typed: %s (from %q, %s); refused, it names no handshake", run.name, live.Format(time.RFC3339Nano), wname, ln.Kind, ln.From.Mock, ln.Why)
					}
				}
			}
		}
	}
}

// The producer's send time reaches the consumer inside a base64 message body
// (an SNS envelope, a Kafka value), and the consumer stores it as
// processed_at; F delivers another recording's message. The payloads hold
// the instant both ways only in their decoded form (rawExtra), and in
// another layout than the typed reading writes (epoch milliseconds): the
// stored instant flowed from the receive's encoded body, a variable of
// mock-4, in both readings. The receives' Date headers are of another hour,
// so no word of the typed reading is found elsewhere: there only the decoded
// payloads' moments (holdsInstant over rawExtra) find it, and without them it
// would be noise.
func TestAnInstantEchoedInsideABase64EnvelopeIsAVariable(t *testing.T) {
	sent := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	body := func(at time.Time) string {
		inner := `{"SentTimestamp":"` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
		return `{"Envelope":"` + base64.StdEncoding.EncodeToString([]byte(inner)) + `"}`
	}
	receive, delivered := r1Receive(body(sent)), r1Receive(body(got))
	receive.Spec.HTTPResp.Body = `{"Messages": [{"Body": ` + strconv.Quote(body(sent)) + `}]}`
	delivered.Spec.HTTPResp.Body = `{"Messages": [{"Body": ` + strconv.Quote(body(got)) + `}]}`
	delivered.Name = "mock-11"
	for _, m := range []*models.Mock{receive, delivered} {
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	for _, typed := range []bool{false, true} {
		if ln := classifyProcessedAt(sent, got, typed, time.Time{}, nil, receive, delivered); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
			t.Errorf("typed %v: %s from %q (%s); want a variable from mock-4: the instant flowed from its encoded body", typed, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
}

// A departure the app picks from a served timetable by its clock: the next
// after now. In F the lead moves the clock, and the app picks the next one,
// which the timetable holds too. The departure flowed from the timetable,
// and is no clock read: it is a variable of mock-0 in every layout, the
// instant whole or inside a longer value, whatever the timetable's layout. A
// value's look never makes it noise.
func TestADepartureChosenFromAServedTimetableStaysChecked(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 10, 0, 0, time.UTC)
	for _, timetable := range []string{
		`{"date":"2026-10-22","departures":["09:05","11:14","12:30"]}`,
		`{"date":"2026-10-22","departures":["09:05:00","11:14:00","12:30:00"]}`,
		`{"departures":[{"day":"2026-10-22","at":"11:14:00"},{"day":"2026-10-22","at":"12:30:00"}]}`,
	} {
		for _, form := range [][2]string{
			{"2026-10-22T11:14:00Z", "2026-10-22T12:30:00Z"},
			{"2026-10-22 11:14", "2026-10-22 12:30"},
			{"at 2026-10-22T11:14:00Z", "at 2026-10-22T12:30:00Z"},
		} {
			tt := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "HTTP_CLIENT"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/timetable"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: timetable},
				ReqTimestampMock: at.Add(-2 * time.Second), ResTimestampMock: at.Add(-1 * time.Second),
			}}
			dc := NewDriftCache()
			const p = "body.departure"
			rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: form[0]}}}
			live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: form[1]}}}
			if ln := ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{dc.RawOf(tt, true)}, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
				t.Errorf("timetable %s, %q -> %q: %s (from %q, %s); want a variable from mock-0", timetable, form[0], form[1], ln.Kind, ln.From.Mock, ln.Why)
			}
		}
	}
	// At record the app booked a departure the timetable does not list (a
	// default, or one it dropped since); in F it books one the timetable
	// lists. What it books in F is a word of the served timetable: keploy
	// cannot tell it came from nowhere, refused, never noise.
	tt := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/timetable"},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"date":"2026-10-22","departures":["09:05","12:30"]}`},
		ReqTimestampMock: at.Add(-2 * time.Second), ResTimestampMock: at.Add(-1 * time.Second),
	}}
	for _, form := range [][2]string{
		{"2026-10-22T10:00:00Z", "2026-10-22T12:30:00Z"},
		{"at 2026-10-22T10:00:00Z", "at 2026-10-22T12:30:00Z"},
	} {
		dc := NewDriftCache()
		const p = "body.departure"
		rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: form[0]}}}
		live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: form[1]}}}
		if ln := ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{dc.RawOf(tt, true)}, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-0") {
			t.Errorf("%q -> %q, F's departure the timetable's: %s (from %q, %s); want it unaccounted for, naming mock-0", form[0], form[1], ln.Kind, ln.From.Mock, ln.Why)
		}
	}
}

// The same pick from a dense timetable (a departure every minute), where F's
// pick is one or two minutes on: by the lead alone at a minute's resolution
// the pick moved as a clock read would, and in its call's window ("the
// latest departure at or before now") it falls in both runs' spans. What
// shows it came from the timetable is the timetable itself: it holds both
// picks, as times of day ("11:15" names 2026-10-22 11:15, heldMoments) or as
// instants in any layout, so the pick is a variable of mock-0, never noise.
// So it is with the call's previous call skipped for being pending across the
// call's send (PreviousCall), which widens the recorded span onto the pick.
func TestAPickFromADenseTimetableStaysChecked(t *testing.T) {
	timetables := []string{
		`{"date":"2026-10-22","departures":["11:13","11:14","11:15","11:16"]}`,
		`{"date":"2026-10-22","departures":["11:13:00","11:14:00","11:15:00","11:16:00"]}`,
		`{"departures":[{"day":"2026-10-22","at":"11:14:00"},{"day":"2026-10-22","at":"11:15:00"}]}`,
		`{"departures":["2026-10-22T11:13:00Z","2026-10-22T11:14:00Z","2026-10-22T11:15:00Z"]}`,
		`{"departures":[1792667580000,1792667640000,1792667700000]}`,
	}
	timetable := func(body string, asked time.Time) *models.Mock {
		return &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/timetable"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(300 * time.Millisecond),
		}}
	}
	classify := func(rec, live string, at time.Time, w *Window, raw ...*models.Mock) Learned {
		dc := NewDriftCache()
		const p = "body.departure"
		var rs []Raw
		for _, m := range raw {
			rs = append(rs, dc.RawOf(m, true))
		}
		r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: rec}}}
		l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: live}}}
		return ClassifyIn(dc, []string{p}, l, r, at, nil, rs, nil, w)[0]
	}
	// By the lead alone: the next departure after now, F one or two
	// minutes on.
	at := time.Date(2026, 10, 22, 11, 13, 30, 0, time.UTC)
	for _, body := range []string{timetables[0], `{"departures":["2026-10-22T11:13","2026-10-22T11:14","2026-10-22T11:15","2026-10-22T11:16"]}`} {
		for _, live := range []string{"2026-10-22 11:15", "2026-10-22 11:16"} {
			if ln := classify("2026-10-22 11:14", live, at, nil, timetable(body, at.Add(-2*time.Second))); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
				t.Errorf("by the lead, %s, -> %q: %s (from %q, %s); want a variable from mock-0", body, live, ln.Kind, ln.From.Mock, ln.Why)
			}
		}
	}
	// A time of day the app formats from a served instant: the timetable's
	// instants hold both picks' times of day.
	if ln := classify("11:14", "11:15", at, nil, timetable(timetables[3], at.Add(-2*time.Second))); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
		t.Errorf("a time of day from an instant of the timetable: %s (from %q, %s); want a variable from mock-0", ln.Kind, ln.From.Mock, ln.Why)
	}
	// In its call's window: the latest departure at or before now.
	now := time.Date(2026, 10, 22, 11, 14, 3, 0, time.UTC)
	get, post := now.Add(-5*time.Second), now.Add(time.Second)
	w := &Window{Rec: Span{From: get, To: post}, F: Span{From: get.Add(FullRoundLead), To: post.Add(FullRoundLead)}}
	for _, body := range timetables {
		for _, form := range [][2]string{{"2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}, {"2026-10-22T11:14:00.000Z", "2026-10-22T11:15:00.000Z"}} {
			if ln := classify(form[0], form[1], post, w, timetable(body, get)); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
				t.Errorf("in its window, %s, %q -> %q: %s (from %q, %s); want a variable from mock-0", body, form[0], form[1], ln.Kind, ln.From.Mock, ln.Why)
			}
		}
	}
	// Another flow's call, sent after the pick and before the POST, pending
	// across the POST's send: the recorded span runs from the timetable.
	ts := func(m, s, ms int) time.Time { return time.Date(2026, 10, 22, 11, m, s, ms*1e6, time.UTC) }
	tt := timetable(timetables[0], ts(13, 50, 0))
	for _, answered := range []time.Time{ts(14, 9, 0), ts(14, 2, 0)} {
		x := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:  &models.HTTPReq{Method: "POST", URL: "/audit"}, HTTPResp: &models.HTTPResp{StatusCode: 204},
			ReqTimestampMock: ts(14, 1, 0), ResTimestampMock: answered,
		}}
		paired := &models.Mock{Name: "mock-2", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: ts(14, 4, 0)}}
		real0 := time.Date(2026, 10, 22, 19, 0, 0, 0, time.UTC)
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r.Add(ts(13, 50, 0).Add(FullRoundLead).Sub(real0)), true })
		tr.ServedMockAt(tt, real0)
		w := tr.CallWindow(paired, []*models.Mock{tt, x}, real0.Add(14*time.Second))
		if ln := classify("2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z", paired.Spec.ReqTimestampMock, w, tt, x); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
			t.Errorf("another flow's call answered at %s, window %+v: %s (from %q, %s); want a variable from mock-0", answered.Format("15:04:05"), w, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
}
