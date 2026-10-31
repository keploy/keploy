package learn

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // TestAnInstantsWrittenTimeOfDayIsNotTheHosts reads Europe/London on any host

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// The SQS sample's delivering ReceiveMessage of the queue consumer's lane,
// leg (b), run 1, as
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

// noiseIn reports whether ln is what a drifted value nothing accounts for is
// learned as in a call read in w: noise, but, for a value that is one instant
// whole in both runs (textClockRead), never noise left unchecked: a plain
// read of the clock (Read), which a replay checks, and in a window that holds
// it neither in both runs nor moved by the lead, refused (outsideWindowWhy).
func noiseIn(ln Learned, w *Window) bool {
	read := textClockRead(ln.Recorded.Value, ln.Live.Value)
	if read == nil {
		// A time of day whole: a read modulo a day (textTodRead).
		read = NewDriftCache().textTodRead(ln.Recorded.Value, ln.Live.Value, nil, time.Time{})
	}
	if read == nil && ln.Recorded.Type != Instant {
		return ln.Kind == KindNoise
	}
	// Written in two layouts in its two runs: refused, naming both.
	if read != nil && read.Layout == models.LayoutApart {
		return ln.Kind == KindUnaccounted && ln.Why == textLayoutChangedWhy(ln.Recorded.Value, ln.Live.Value)
	}
	if w != nil && ln.Kind == KindUnaccounted && ln.Why == outsideWindowWhy {
		return true
	}
	return ln.Kind == KindClockRead && ln.Read != nil
}

// heldWord reports whether text holds, as a word of its own, a word the
// insert's processed_at lost between its recording (rec) and F (live) in a
// reading: of the typed reading, its minute, its second or its fraction.
func heldWord(text string, rec, live time.Time, typed bool) bool {
	r, l := processedAt(rec, typed), processedAt(live, typed)
	for p, leaf := range r.Leaves {
		rw, lw := tokensBy(leaf.Value, isWord), tokensBy(l.Leaves[p].Value, isWord)
		for w := range rw {
			if !lw[w] && ContainsToken(text, w) {
				return true
			}
		}
	}
	return false
}

// classifyProcessedAt classifies the insert's processed_at, recorded at rec
// and live in F, recorded at insert (r1Insert when zero), against the
// recordings F served before it (raw), read in w.
func classifyProcessedAt(rec, live time.Time, typed bool, insert time.Time, w *Window, mocks ...*models.Mock) Learned {
	const p = "body.docs.msgs.0.processed_at.$date.$numberLong"
	if insert.IsZero() {
		insert = r1Insert
	}
	dc := clockedDrifts()
	var raw []Raw
	for _, m := range mocks {
		raw = append(raw, dc.RawOf(m, true).ServedAsRecorded())
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
// as any value's. The receive's Date header is its server's clock read as it
// answered (rule 3: an HTTP Date within the span keploy stamped the exchange
// in), no place a value came from: of the receive as recorded nothing else
// holds a word of it, so it is noise; a receive whose attributes hold its
// minute as a word of their own (a receive count of 14) is refused, naming
// the receive and the field, a false red that fails closed. The raw reading
// is one word, its milliseconds, searched whole; the receive holds that
// moment nowhere, so it is noise however it was read.
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
					if ln := classifyProcessedAt(row.rec, live, typed, time.Time{}, w, receive); !noiseOrRead(ln) {
						t.Errorf("%s, live %s, in its call's window, receive %d, typed %v: %s (from %q, %s); want noise", row.name, live.Format(time.RFC3339Nano), i, typed, ln.Kind, ln.From.Mock, ln.Why)
					}
				}
			}
		}
	}
	// The E2 run's read outside its window: F's read a minute on, its minute
	// "14" lost, which the Date header holds, and the second receive's
	// count.
	rec := time.Date(2026, 10, 22, 11, 14, 17, 12e6, time.UTC)
	live := rec.Add(60*time.Second + 638*time.Millisecond)
	for _, wname := range []string{"read before the previous call", "by the lead alone, no window"} {
		w := r1Windows(rec, live)[wname]
		// The value, one instant whole that the lead does not read, is
		// never noise left unchecked (noiseIn).
		noise := func(ln Learned) bool { return noiseIn(ln, w) }
		for i, receive := range receives {
			if ln := classifyProcessedAt(rec, live, false, time.Time{}, w, receive); !noise(ln) {
				t.Errorf("%s, receive %d, raw: %s (from %q, %s); want noise (a read of the clock with no window, refused outside its window): the receive holds its milliseconds nowhere", wname, i, ln.Kind, ln.From.Mock, ln.Why)
			}
			ln := classifyProcessedAt(rec, live, true, time.Time{}, w, receive)
			if i == 0 && !noise(ln) {
				t.Errorf("%s, the receive as recorded, typed: %s (from %q at %q, %s); want noise (a read of the clock with no window, refused outside its window): its Date header is its server's stamp, and nothing else holds a word of it", wname, ln.Kind, ln.From.Mock, ln.From.Field, ln.Why)
			}
			if i == 1 && (ln.Kind != KindVariable || ln.From.Mock != "mock-4" || ln.From.Field != "spec.resp.body" || ln.From.Held != "14") {
				t.Errorf("%s, a receive count of 14, typed: %s (from %q at %q, %q); want refused, a variable from mock-4's spec.resp.body: its minute is a word there, and nothing shows it is a clock read", wname, ln.Kind, ln.From.Mock, ln.From.Field, ln.From.Held)
			}
			if i == 1 && !strings.Contains(ln.Why, "a part of its recorded value (its characters 15 to 16) stands in mock-4 at spec.resp.body") || i == 1 && !strings.Contains(ln.Why, "declare it noise in keploy.yml") || i == 1 && strings.Contains(ln.Why, `"14"`) || i == 1 && !strings.Contains(ln.Why, "nothing shows the value is a read of the application's clock") {
				t.Errorf("%s, a receive count of 14: the reason %q does not name the part, the field, the missing provenance and the fix, or names the value's text", wname, ln.Why)
			}
		}
		// A word F saw (the minute 15) that a receive holds as a word of its
		// own (a receive count of 15), where nothing holds a word recorded:
		// unaccounted for, the payload's words searched whatever the value
		// looks like.
		counted := r1Receive(`{"ApproximateReceiveCount": "15"}`)
		if ln := classifyProcessedAt(rec, live, true, time.Time{}, w, counted); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "a word of what record's full round saw in it") || !strings.Contains(ln.Why, "in mock-4 at spec.resp.body") {
			t.Errorf("%s, a receive count of 15, typed: %s (%s); want it unaccounted for, naming the part F saw and mock-4's spec.resp.body", wname, ln.Kind, ln.Why)
		}
		// A Date header outside the span keploy stamped the receive in (its
		// server's clock ahead) is no stamp: its words are searched.
		ahead := r1Receive("")
		ahead.Spec.ResTimestampMock = time.Date(2026, 10, 22, 11, 14, 15, 0, time.UTC)
		if ln := classifyProcessedAt(rec, live, true, time.Time{}, w, ahead); ln.Kind != KindVariable || ln.From.Mock != "mock-4" || ln.From.Field != "spec.resp.header.Date" {
			t.Errorf("%s, a Date header after its answer, typed: %s (from %q at %q); want a variable from mock-4's spec.resp.header.Date", wname, ln.Kind, ln.From.Mock, ln.From.Field)
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
// span. As another moment it is none: a second of it is coarser than the
// value, which is no copy of it; but a finer one its digits begin (its
// microsecond) holds the value, its millisecond, at the value's own
// precision, and the value is a variable of the receive. Where no
// provenance shows a clock read, the typed reading's words are searched: a
// payload holding one as a word of its own (a note's text)
// refuses it; the receive's Date header, its server's stamp (rule 3), does
// not.
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
					if ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, config); !noiseIn(ln, w) {
						t.Errorf("%s, a protocol-phase answer's clock read, F's value in no payload, %s, typed %v: %s from %q; want noise", recorded[i], wname, typed, ln.Kind, ln.From.Mock)
					}
					for _, m := range []*models.Mock{receive, early} {
						// Held as the reading writes it, it is followed;
						// else noise where its provenance shows a clock
						// read (shownClockRead), and elsewhere the typed
						// reading's words are searched as any value's: a
						// form holding one as a word refuses it, and so
						// does the early exchange's Date header, outside
						// the span its exchange was stamped in, no stamp.
						held := heldWord(recorded[i], sent, got, typed) || m == early && heldWord(m.Spec.HTTPResp.Header["Date"], sent, got, typed)
						want := sameText(recorded[i], typed) || typed && !shownClockRead(sent, got, typed, w) && held
						ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, m)
						if want && (ln.Kind != KindVariable || ln.From.Mock != "mock-4") || !want && !noiseIn(ln, w) {
							t.Errorf("%s, no protocol-phase answer's clock read (early %v), F's value in no payload, %s, typed %v: %s from %q; want a variable %v", recorded[i], m == early, wname, typed, ln.Kind, ln.From.Mock, want)
						}
						// The variable names the field the value stands
						// in, as the mock file names it: held whole as
						// the reading writes it, the receive's body.
						if sameText(recorded[i], typed) && (ln.From.Field != "spec.resp.body" || !strings.Contains(ln.Why, "mock-4 at spec.resp.body")) {
							t.Errorf("%s, held whole (early %v), %s, typed %v: from %q at %q (%s); want it named at mock-4's spec.resp.body", recorded[i], m == early, wname, typed, ln.From.Mock, ln.From.Field, ln.Why)
						}
					}
				}
			}
		}
		ms, sec := strconv.FormatInt(sent.UnixMilli(), 10), strconv.FormatInt(sent.Unix(), 10)
		gms, gsec := strconv.FormatInt(got.UnixMilli(), 10), strconv.FormatInt(got.Unix(), 10)
		for _, other := range []struct {
			rec, live string
			// copied: the value is the served instant cut to its own
			// precision, both ways (a millisecond of a served
			// microsecond): a variable of the receive, however it was
			// read. A millisecond built on a served second is a read of
			// the clock as much: a second is within F's lead.
			copied bool
		}{
			{`{"SentTimestamp": "` + sec + `"}`, `{"SentTimestamp": "` + gsec + `"}`, false},
			{`{"SentTimestamp": "` + ms + `123"}`, `{"SentTimestamp": "` + gms + `123"}`, true},
			{`{"SentAt": "` + sec + `.4732103"}`, `{"SentAt": "` + gsec + `.4732103"}`, false},
		} {
			receive, delivered := r1Receive(other.rec), r1Receive(other.live)
			delivered.Name = "mock-11"
			for wname, w := range r1Windows(sent, got) {
				for _, typed := range []bool{false, true} {
					// Where no provenance shows a clock read the typed
					// reading's words are searched: the Date header, its
					// server's stamp, holds none, and a form refuses it
					// only where it holds one as a word. A coarser copy
					// is the echo's, whatever the provenance.
					want := KindNoise
					if other.copied || typed && !shownClockRead(sent, got, typed, w) && (heldWord(other.rec, sent, got, typed) || heldWord(other.live, sent, got, typed)) {
						want = KindVariable
					}
					ln := classifyProcessedAt(sent, got, typed, time.Time{}, w, receive, delivered)
					if want == KindNoise && !noiseIn(ln, w) || want != KindNoise && ln.Kind != want || other.copied && ln.From.Mock != "mock-4" {
						t.Errorf("%s then %s, %s, typed %v: %s (from %q); want %s", other.rec, other.live, wname, typed, ln.Kind, ln.From.Mock, want)
					}
				}
			}
		}
	}
}

// r2Hello is a Mongo driver's isMaster handshake as the queue consumer's
// lane, leg (b), run r2,
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

// r2Mongo is a Mongo OP_MSG recording of the queue consumer's lane, leg (b),
// run r2: request and
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

// the queue consumer's lane, leg (b), run r2 (keploy 6db6cd0f3): the consumer
// read its clock for
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
// and nothing but the receive's Date header holds one, which is its server's
// clock read as it answered (rule 3): noise, by provenance alone, wherever
// the read fell. The rule is the answer's span alone: a localTime the
// recorded answer came before (a server whose clock ran ahead) is followed,
// and so is that millisecond in a request (by the raw reading, which writes
// it so); and a localTime F served too, as the value it saw, is an echo.
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
				// (shownClockRead) is searched word by word: the
				// receive's Date header holds its minute, and is no place
				// a value came from.
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, hello7, upsert); !noiseIn(ln, w) {
					t.Errorf("live %s, %s, typed %v: %s (from %q at %q, %s); want noise", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, ln.From.Field, ln.Why)
				}
				// The server's clock ran ahead: its localTime is later than
				// its answer reached keploy, no read of its answer's.
				followed := !typed
				ahead := r2Hello("mock-5", 8, ts(13, 205118312), ts(13, 457911204), read)
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, ahead, count, upsert); followed && (ln.Kind != KindVariable || ln.From.Mock != "mock-5") || !followed && !noiseIn(ln, w) {
					t.Errorf("a localTime after its answer, live %s, %s, typed %v: %s from %q; want a variable from mock-5 %v", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, followed)
				}
				// That millisecond in a request: the app's own, followed.
				sent := r2Mongo("mock-8", ts(13, 459791587), ts(14, 421828601),
					[]string{`{ SectionSingle identifier: updates , msgs: [ {"q":{"_id":"Lhr4BWAi"},"u":{"$set":{"created":{"$date":{"$numberLong":"1792694293458"}}}}} ] }`}, `{ SectionSingle msg: {"ok":{"$numberDouble":"1.0"}} }`)
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, sent); followed && (ln.Kind != KindVariable || ln.From.Mock != "mock-8") || !followed && !noiseIn(ln, w) {
					t.Errorf("that millisecond in a request, live %s, %s, typed %v: %s from %q; want a variable from mock-8 %v", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, followed)
				}
				// F served a handshake whose localTime is the value F saw:
				// the echo reads a handshake's answer as it stands.
				echo := r2Hello("mock-9", 12, ts(15, 0), ts(15, 1000), live.UnixMilli())
				if ln := classifyProcessedAt(rec, live, typed, insert, w, receive, hello5, count, echo); ln.Kind != KindVariable || ln.From.Mock != "mock-5" || !ln.From.Echo {
					t.Errorf("F's value a localTime F served, live %s, %s, typed %v: %s from %q (echo %v); want an echo's variable from mock-5", live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, ln.From.Echo)
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
	dc := clockedDrifts()
	if a, b := dc.RawOf(receive, true), dc.RawOf(receive, false); !reflect.DeepEqual(a.AnswerClock(), RawOf(receive, false).AnswerClock()) || !reflect.DeepEqual(b.AnswerClock(), a.AnswerClock()) || !a.Served() || b.Served() {
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
// red). So is every field that holds a request, among requestFields: a new
// parser's recording that holds no answer would otherwise be read whole, as
// if its request were something it gave the application (answerApart).
// Every field of a Postgres v3 query's spec is named: its request among
// postgresV3QueryRequestNames, or its answer here. A new field is read as its
// answer (answerPath), which only refuses more, until it is named here.
func TestEveryPostgresQueryFieldIsNamed(t *testing.T) {
	answer := map[string]bool{"Response": true, "SideEffects": true}
	st := reflect.TypeOf(models.PostgresV3QuerySpec{})
	asked := map[string]bool{}
	for _, n := range postgresV3QueryRequestNames {
		if _, ok := st.FieldByName(n); !ok {
			t.Errorf("models.PostgresV3QuerySpec has no field %s", n)
		}
		asked[n] = true
	}
	if len(postgresV3QueryRequest) != len(postgresV3QueryRequestNames) {
		t.Errorf("postgresV3QueryRequest holds %d JSON names for %d fields", len(postgresV3QueryRequest), len(postgresV3QueryRequestNames))
	}
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if asked[f.Name] == answer[f.Name] {
			t.Errorf("models.PostgresV3QuerySpec.%s: its request %v, its answer %v; name it as one", f.Name, asked[f.Name], answer[f.Name])
		}
		path := postgresV3Query + jsonName(st, f.Name) + "[0].value"
		if answerPath(path) != answer[f.Name] || requestPath(path) != asked[f.Name] {
			t.Errorf("%s: answerPath %v, requestPath %v", path, answerPath(path), requestPath(path))
		}
	}
	if postgresV3Query != "postgresV3.query." {
		t.Errorf("a Postgres v3 query stands at %q", postgresV3Query)
	}
	for _, path := range []string{"postgresV3", "postgresV3.type", "postgresV3.query", "postgresV3.session.serverVersion", "postgresV3.queryX.response"} {
		if answerPath(path) || requestPath(path) {
			t.Errorf("%s: read as a Postgres v3 query's answer %v or request %v", path, answerPath(path), requestPath(path))
		}
	}
}

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
	asked := map[string]bool{}
	for _, n := range requestFieldNames {
		if _, ok := st.FieldByName(n); !ok {
			t.Errorf("models.MockSpec has no field %s", n)
		}
		asked[n] = true
	}
	if len(requestFields) != len(requestFieldNames) {
		t.Errorf("requestFields holds %d JSON names for %d fields", len(requestFields), len(requestFieldNames))
	}
	for i := 0; i < st.NumField(); i++ {
		n := st.Field(i).Name
		answer := strings.HasSuffix(n, "Resp") || strings.Contains(n, "Response")
		if answer != named[n] {
			t.Errorf("models.MockSpec.%s: an answer %v, named as one %v", n, answer, named[n])
		}
		request := strings.HasSuffix(n, "Req") || strings.Contains(n, "Request")
		if request != asked[n] {
			t.Errorf("models.MockSpec.%s: a request %v, named as one %v", n, request, asked[n])
		}
	}
}

// The instants a text holds, in either case, and a time-ordered UUID's time
// groups; and the moment each names.
// A text that names its zone, ahead of UTC or behind it, is zoned; one of no
// zone is not.
func TestAnInstantNamesTheZoneItsTextNames(t *testing.T) {
	for text, want := range map[string]bool{
		"2026-10-22T20:00:00-10:00": true, "2026-10-22T20:00:00+05:30": true, "2026-10-22T20:00:00-0500": true,
		"2026-10-22T20:00:00Z": true, "1792667656": true, "Thu, 22 Oct 2026 11:14:16 GMT": true, "20261022T111413Z": true,
		"2026-10-22 20:00:00": false, "2026-10-22": false, "20261022T111413": false,
	} {
		sp := instantSpans(text)
		if len(sp) != 1 || !sp[0].keyed || sp[0].zoned != want {
			t.Errorf("%q: spans %+v; want one zoned %v", text, sp, want)
		}
	}
}

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
		// ISO 8601 to the hour, after a T only; a space and an hour are
		// words of their own.
		{"due 2026-10-22T11Z.", []string{"2026-10-22T11Z"}},
		{"due 2026-10-22T11+05:30", []string{"2026-10-22T11+05:30"}},
		{"due 2026-10-22T11 ok", []string{"2026-10-22T11"}},
		{"due 2026-10-22 11 ok", []string{"2026-10-22"}},
		{"id 2026-10-22T11a", nil},
	} {
		var got []string
		for _, sp := range instantSpans(tc.text) {
			got = append(got, tc.text[sp.start:sp.end])
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("instantSpans(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
	for text, res := range map[string]time.Duration{"2026-10-22T11Z": time.Hour, "2026-10-22T11+05:30": time.Hour, "2026-10-22T11": time.Hour, "2026-10-22T11:14Z": time.Minute, "2026-10-22": 24 * time.Hour} {
		if sp := instantSpans(text); len(sp) != 1 || sp[0].res != res {
			t.Errorf("instantSpans(%q) = %+v; want one instant to the %v", text, sp, res)
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

// A drifted value's changed instant is dated on both sides or a time of day
// on both: its sides are the k-th of one layout, and a layout reads one or
// the other. A time of day whose value turns into an instant (or the other
// way) is no changed instant, so an offset is never read between a date and
// a time of day.
func TestAChangedInstantIsDatedOnBothSidesOrATimeOfDayOnBoth(t *testing.T) {
	for _, tc := range []struct {
		recorded, live string
		want           int
		day            bool
	}{
		{"opens 11:15", "opens 11:16", 1, true},
		{"opens 2026-10-22T11:15:00Z", "opens 2026-10-22T11:16:00Z", 1, false},
		{"opens 11:15", "opens 2026-10-22T11:16:00Z", 0, false},
		{"opens 2026-10-22T11:15:00Z", "opens 11:16", 0, false},
		{"opens 11:15 on 2026-10-22T11:15:00Z", "opens 2026-10-22T11:16:00Z then 11:20", 2, false},
	} {
		got := driftedInstants(tc.recorded, tc.live)
		if len(got) != tc.want {
			t.Fatalf("%q -> %q: %d changed instants %+v, want %d", tc.recorded, tc.live, len(got), got, tc.want)
		}
		for _, p := range got {
			if p.rec.key.day != p.live.key.day {
				t.Fatalf("%q -> %q: %+v pairs a time of day with a dated instant", tc.recorded, tc.live, p)
			}
			if tc.want == 1 && p.rec.key.day != tc.day {
				t.Fatalf("%q -> %q: %+v, want a time of day %v", tc.recorded, tc.live, p, tc.day)
			}
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
		dc := clockedDrifts()
		return ClassifyIn(dc, []string{"body.batch"}, l, r, r1Insert, nil, []Raw{dc.RawOf(receive, true).ServedAsRecorded()}, resp, nil)[0]
	}
	if ln := classify(configReceive(""), nil); ln.Kind != KindNoise {
		t.Fatalf("beside a config call's Date header: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	asked := configReceive("")
	asked.Spec.HTTPReq.Header["If-Modified-Since"] = "Thu, 22 Oct 2026 10:14:09 GMT"
	unknown := configReceive("")
	unknown.Spec.ResTimestampMock = time.Time{}
	// A data-plane receive's Date header, within the span its exchange was
	// stamped in, is its server's own clock read too (rule 3): no source.
	if ln := classify(r1Receive(""), nil); ln.Kind != KindNoise {
		t.Errorf("beside a data-plane receive's Date: %s from %q at %q; want noise", ln.Kind, ln.From.Mock, ln.From.Field)
	}
	for name, m := range map[string]*models.Mock{
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
	dc := clockedDrifts()
	if ln := ClassifyIn(dc, []string{"body.batch"}, l, r13, r1Insert.Add(time.Minute), nil, []Raw{dc.RawOf(late, true).ServedAsRecorded()}, nil, nil)[0]; ln.Kind != KindNoise {
		t.Fatalf("15, a word of a config call's Date: %s (%s); want noise", ln.Kind, ln.Why)
	}
	late.Spec.Metadata["type"] = "HTTP_CLIENT"
	dc = clockedDrifts()
	if ln := ClassifyIn(dc, []string{"body.batch"}, l, r13, r1Insert.Add(time.Minute), nil, []Raw{dc.RawOf(late, true).ServedAsRecorded()}, nil, nil)[0]; ln.Kind != KindNoise {
		t.Fatalf("15, a word of a data-plane receive's Date, its stamp: %s (%s); want noise", ln.Kind, ln.Why)
	}
	// Answered before its Date header (its server's clock ahead): no stamp.
	late.Spec.ResTimestampMock = time.Date(2026, 10, 22, 11, 15, 15, 900e6, time.UTC)
	dc = clockedDrifts()
	if ln := ClassifyIn(dc, []string{"body.batch"}, l, r13, r1Insert.Add(time.Minute), nil, []Raw{dc.RawOf(late, true).ServedAsRecorded()}, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in mock-4 at spec.resp.header.Date") || !strings.Contains(ln.Why, "declare it noise in keploy.yml") {
		t.Fatalf("15, a word of a Date header after its answer: %s (%s); want refused, naming mock-4's spec.resp.header.Date and the fix", ln.Kind, ln.Why)
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
	if ln := ClassifyIn(clockedDrifts(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, audit, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
		t.Fatalf("in lockstep with an earlier leaf's stamp: %s from %q; want a variable from mock-1", ln.Kind, ln.From.Mock)
	}
	expiry := at.Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	token := []Earlier{{Mock: "mock-2", At: r1Insert.Add(-time.Second), Own: true, Seen: []Seen{{Side: Response,
		Leaf: Leaf{Path: "body.expiresAt", Noise: "body.expiresAt", Type: String, Value: expiry}, Live: expiry}}}}
	if ln := ClassifyIn(clockedDrifts(), []string{"body.at"}, leaf(f.Add(time.Hour).Format(time.RFC3339)), leaf(expiry), r1Insert, token, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-2") {
		t.Fatalf("a copy of an expiry F served unchanged: %s (%s); want it unaccounted for", ln.Kind, ln.Why)
	}
	resp := []Leaf{{Path: "body.id", Noise: "body.id", Type: String, Value: "1298498081"}, {Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + at.Format("05")}}
	filter := Shape{Leaves: map[string]Leaf{"body.q": {Path: "body.q", Noise: "body.q", Type: String, Value: "user-1298498081"}}}
	now := Shape{Leaves: map[string]Leaf{"body.q": {Path: "body.q", Noise: "body.q", Type: String, Value: "user-1427131847"}}}
	if ln := ClassifyIn(clockedDrifts(), []string{"body.q"}, now, filter, r1Insert, nil, nil, resp, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
		t.Fatalf("an id the answer echoes: %s (%s); want refused", ln.Kind, ln.Why)
	}
	if ln := ClassifyIn(clockedDrifts(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, nil, nil, resp, nil)[0]; !noiseIn(ln, nil) {
		t.Fatalf("a stamp no word of whose drift the answer holds: %s (%s); want noise", ln.Kind, ln.Why)
	}
	minute := []Leaf{{Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + at.Format("04")}}
	if ln := ClassifyIn(clockedDrifts(), []string{"body.at"}, leaf(f.Format(time.RFC3339)), leaf(at.Format(time.RFC3339)), r1Insert, nil, nil, minute, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
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
		if ln := ClassifyIn(clockedDrifts(), []string{p}, live, rec, at, earlier, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
			t.Errorf("%s at an earlier leaf: %s from %q; want a variable from mock-1", ids[0], ln.Kind, ln.From.Mock)
		}
		write := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "/users", Body: `{"userId":` + ids[0] + `,"name":"a"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 201, Body: `{"ok":true}`},
			ReqTimestampMock: at.Add(-time.Second),
			ResTimestampMock: at.Add(-time.Second + time.Millisecond),
		}}
		dc := clockedDrifts()
		if ln := ClassifyIn(dc, []string{p}, live, rec, at, nil, []Raw{dc.RawOf(write, true)}, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
			t.Errorf("%s in an earlier payload: %s from %q; want a variable from mock-1", ids[0], ln.Kind, ln.From.Mock)
		}
	}
}

// the queue consumer's lane, leg (b), the archived runs that failed
// (r1/legb-archive, keploy
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
						if !noiseIn(ln, w) {
							t.Errorf("%s, live %s, %s, typed %v: %s (from %q, %s); want noise", run.name, live.Format(time.RFC3339Nano), wname, typed, ln.Kind, ln.From.Mock, ln.Why)
						}
					} else if ln.Kind != KindNoise && !noiseIn(ln, w) && ln.From.Mock != "mock-6" && !strings.Contains(ln.Why, "mock-6") {
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
			dc := clockedDrifts()
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
		dc := clockedDrifts()
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
		dc := clockedDrifts()
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

// servedTimetable is a timetable the learn step served earlier in the test,
// leaf by leaf (an Earlier, never a Raw: the HTTP adapter's earlierRaw leaves
// out every exchange it served so), each departure as recorded and as F
// served it: its recorded value.
func servedTimetable(at time.Time, departures ...string) []Earlier {
	e := Earlier{Mock: "mock-0", At: at, Payload: payloadOf("mock-0", at), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "url.path.1", Noise: "url.path.1", Type: String, Value: "timetable"}, Live: "timetable"},
		{Side: Response, Leaf: Leaf{Path: "body.date", Noise: "body.date", Type: String, Value: "2026-10-22"}, Live: "2026-10-22"},
	}}
	for i, d := range departures {
		p := "body.departures[" + strconv.Itoa(i) + "]"
		e.Seen = append(e.Seen, Seen{Side: Response, Leaf: Leaf{Path: p, Noise: "body.departures[]", Type: String, Value: d}, Live: d})
	}
	return []Earlier{e}
}

// classifyDeparture classifies a booking's departure, recorded rec and live
// in F, at at, against what the test served earlier (earlier, raw), read in
// w.
func classifyDeparture(rec, live string, at time.Time, w *Window, earlier []Earlier, raw ...*models.Mock) Learned {
	dc := clockedDrifts()
	const p = "body.departure"
	var rs []Raw
	for _, m := range raw {
		rs = append(rs, dc.RawOf(m, true))
	}
	r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: rec}}}
	l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: live}}}
	return ClassifyIn(dc, []string{p}, l, r, at, earlier, rs, nil, w)[0]
}

// A departure the app picks from a timetable the learn step served leaf by
// leaf stays checked, as one from a timetable read as a payload does: the
// moment F saw in the pick is a departure the served answer lists, so the
// echo reads it before the lead or the call's window excuses a word of it,
// and the pick is a variable of the departure that held its recorded moment,
// an answer's place (which keploy cannot bind: refused), however sparse or
// dense the timetable, by the lead or in the window, in either layout. A
// pick the timetable did not list at record, and lists in F, is refused,
// naming where F's moment stands. Of the pick in its window widened by a
// call pending across the POST (PreviousCall), the same holds.
func TestAPickFromATimetableTheTestWasServedStaysChecked(t *testing.T) {
	isPick := func(ln Learned, path string) bool {
		return ln.Kind == KindVariable && ln.From.Mock == "mock-0" && ln.From.Side == Response && ln.From.Path == path && ln.From.Echo
	}
	// Sparse: no lead, no window.
	at := time.Date(2026, 10, 22, 11, 10, 0, 0, time.UTC)
	if ln := classifyDeparture("2026-10-22T11:14:00Z", "2026-10-22T12:30:00Z", at, nil, servedTimetable(at.Add(-2*time.Second), "09:05", "11:14", "12:30")); !isPick(ln, "body.departures[1]") {
		t.Errorf("sparse: %s from %+v (%s); want a variable of mock-0's body.departures[1]", ln.Kind, ln.From, ln.Why)
	} else if !strings.Contains(ln.Why, "the moment it was recorded with stands in mock-0 at body.departures[1]") || !strings.Contains(ln.Why, "declare it noise in keploy.yml") {
		t.Errorf("sparse: the reason %q does not name the place, the echo and the fix", ln.Why)
	}
	// Dense, by the lead.
	at = time.Date(2026, 10, 22, 11, 13, 30, 0, time.UTC)
	for _, table := range [][]string{{"11:13", "11:14", "11:15", "11:16"}, {"2026-10-22T11:13", "2026-10-22T11:14", "2026-10-22T11:15", "2026-10-22T11:16"}} {
		for _, live := range []string{"2026-10-22 11:15", "2026-10-22 11:16"} {
			if ln := classifyDeparture("2026-10-22 11:14", live, at, nil, servedTimetable(at.Add(-2*time.Second), table...)); !isPick(ln, "body.departures[1]") {
				t.Errorf("dense by the lead, %v, -> %q: %s from %+v (%s); want a variable of mock-0's body.departures[1]", table, live, ln.Kind, ln.From, ln.Why)
			}
		}
	}
	// Dense, in the call's window: the latest departure at or before now.
	now := time.Date(2026, 10, 22, 11, 14, 3, 0, time.UTC)
	get, post := now.Add(-5*time.Second), now.Add(time.Second)
	w := &Window{Rec: Span{From: get, To: post}, F: Span{From: get.Add(FullRoundLead), To: post.Add(FullRoundLead)}}
	for _, table := range [][]string{{"11:13", "11:14", "11:15", "11:16"}, {"2026-10-22T11:13:00Z", "2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}} {
		if ln := classifyDeparture("2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z", post, w, servedTimetable(get, table...)); !isPick(ln, "body.departures[1]") {
			t.Errorf("dense in its window, %v: %s from %+v (%s); want a variable of mock-0's body.departures[1]", table, ln.Kind, ln.From, ln.Why)
		}
	}
	// At record the app booked a departure the timetable did not list; in F
	// one it lists, by the lead.
	ln := classifyDeparture("2026-10-22 11:14", "2026-10-22 11:15", at, nil, servedTimetable(at.Add(-2*time.Second), "11:13", "11:15", "11:16"))
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in mock-0 at body.departures[1]") {
		t.Errorf("a pick listed in F only: %s (%s); want it refused, naming mock-0's body.departures[1]", ln.Kind, ln.Why)
	}
	// The same pick from a timetable its protocol types (a decoder's
	// datetimes, Leaf.Type Instant): a typed instant has no words the word
	// rule could search (unaccounted skips it), so only the echo, held one
	// way, finds F's moment there; it is refused all the same, naming it.
	typed := servedTimetable(at.Add(-2*time.Second), "2026-10-22T11:13:00Z", "2026-10-22T11:15:00Z", "2026-10-22T11:16:00Z")
	for i := range typed[0].Seen {
		if strings.HasPrefix(typed[0].Seen[i].Leaf.Path, "body.departures[") {
			typed[0].Seen[i].Leaf.Type = Instant
		}
	}
	if ln := classifyDeparture("2026-10-22 11:14", "2026-10-22 11:15", at, nil, typed); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in mock-0 at body.departures[1]") {
		t.Errorf("a pick a typed timetable lists in F only: %s (%s); want it refused, naming mock-0's body.departures[1]", ln.Kind, ln.Why)
	}
	// In a window a pending call widened: the timetable served at 11:13:50,
	// another flow's call sent at 11:14:01 and answered after the POST
	// (11:14:04), which is no previous call of the POST's.
	ts := func(m, s int) time.Time { return time.Date(2026, 10, 22, 11, m, s, 0, time.UTC) }
	tt := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: ts(13, 50), ResTimestampMock: ts(13, 50).Add(300 * time.Millisecond)}}
	x := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"type": "HTTP_CLIENT"}, ReqTimestampMock: ts(14, 1), ResTimestampMock: ts(14, 9)}}
	paired := &models.Mock{Name: "mock-2", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: ts(14, 4)}}
	real0 := time.Date(2026, 10, 22, 19, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r.Add(ts(13, 50).Add(FullRoundLead).Sub(real0)), true })
	tr.ServedMockAt(tt, real0)
	pw := tr.CallWindow(paired, []*models.Mock{tt, x}, real0.Add(14*time.Second))
	if pw == nil || !pw.Rec.From.Equal(ts(13, 50)) {
		t.Fatalf("the POST's window %+v; want it from the timetable's call", pw)
	}
	if ln := classifyDeparture("2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z", paired.Spec.ReqTimestampMock, pw, servedTimetable(ts(13, 50), "11:13", "11:14", "11:15", "11:16")); !isPick(ln, "body.departures[1]") {
		t.Errorf("in a window a pending call widened: %s from %+v (%s); want a variable of mock-0's body.departures[1]", ln.Kind, ln.From, ln.Why)
	}
}

// A served exchange's Date header, its server's clock read as it answered
// (Seen.Stamp, rule 3), is no place a value came from: a word of it is no
// word of the value's source. Not stamped (its exchange's span does not
// hold it), its words are searched as any leaf's. The echo reads it as it
// stands: a value F wrote as the Date header F served is a variable of the
// header that held its recorded moment.
func TestAServedDateHeaderIsNoSourceButIsEchoed(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	date := func(v string, stamp bool) []Earlier {
		return []Earlier{{Mock: "mock-3", At: at.Add(-time.Second), Payload: payloadOf("mock-3", at.Add(-time.Second)), Own: true, Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: v}, Live: v, Stamp: stamp},
		}}}
	}
	const p = "body.batch"
	r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "batch-14"}}}
	l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "batch-15"}}}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, date("Thu, 22 Oct 2026 11:14:16 GMT", true), nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("a word of a stamped Date header: %s (%s); want noise", ln.Kind, ln.Why)
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, date("Thu, 22 Oct 2026 11:14:16 GMT", false), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-3 at header.date") {
		t.Errorf("a word of a Date header no span holds: %s (%s); want it refused, naming mock-3's header.date", ln.Kind, ln.Why)
	}
	// The app writes the Date it was served as its "fetched" instant: F
	// served another exchange's, a minute on.
	served := append(date("Thu, 22 Oct 2026 11:14:16 GMT", true), date("Thu, 22 Oct 2026 11:15:17 GMT", true)...)
	served[1].Mock = "mock-4"
	if ln := classifyDeparture("2026-10-22T11:14:16Z", "2026-10-22T11:15:17Z", at, nil, served); ln.Kind != KindVariable || ln.From.Mock != "mock-3" || ln.From.Path != "header.date" || !ln.From.Echo {
		t.Errorf("a copy of a served Date header: %s from %+v (%s); want a variable of mock-3's header.date", ln.Kind, ln.From, ln.Why)
	}
	// A stamp F served otherwise than recorded (a variable's value answered
	// there) is no stamp: rule 3's premise, that F served the stamp the
	// recording holds, fails, and its words drifted with the value's.
	moved := []Earlier{{Mock: "mock-3", At: at.Add(-time.Second), Payload: payloadOf("mock-3", at.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: "Thu, 22 Oct 2026 11:14:16 GMT"}, Live: "Thu, 22 Oct 2026 11:15:46 GMT", Stamp: true},
	}}}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, moved, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-3" {
		t.Errorf("a stamp served otherwise than recorded: %s from %+v (%s); want a variable of mock-3", ln.Kind, ln.From, ln.Why)
	}
	// A stamp F served otherwise than recorded, whose recorded words the
	// value lost but whose words F served it did not gain, is no co-drift
	// source; it is no stamp either, and a word it held when recorded is
	// unaccounted for.
	otherwise := []Earlier{{Mock: "mock-3", At: at.Add(-time.Second), Payload: payloadOf("mock-3", at.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "header.date", Noise: "header.date", Type: String, Value: "Thu, 22 Oct 2026 11:14:16 GMT"}, Live: "Thu, 22 Oct 2026 11:17:46 GMT", Stamp: true},
	}}}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, otherwise, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-3 at header.date") {
		t.Errorf("a recorded word of a stamp served otherwise than recorded: %s (%s); want it unaccounted for, naming mock-3's header.date", ln.Kind, ln.Why)
	}
	// The application labels its batch by its answer's minute: at record it
	// was given mock-1's Date (minute 14), in F another recording of the
	// call, mock-2 (minute 15). The value drifted from a part of one stamp
	// to the same part of another F served: rule 3's premise fails, and the
	// stamps are searched as any text, as a payload and leaf by leaf. A word
	// F saw that a stamp holds at another part (the second) is no such
	// drift: noise.
	poll := func(name, date string, asked time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/status"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": date}, Body: `{"ok":true}`},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Second),
		}}
	}
	day := func(h, m, sec int) time.Time { return time.Date(2026, 10, 22, h, m, sec, 0, time.UTC) }
	first, second := poll("mock-1", "Thu, 22 Oct 2026 11:14:16 GMT", day(11, 14, 15)), poll("mock-2", "Thu, 22 Oct 2026 11:15:20 GMT", day(11, 15, 19))
	dc := clockedDrifts()
	raws := []Raw{dc.RawOf(first, false), dc.RawOf(second, true)}
	raws[0].at = at.Add(-time.Hour)
	if ln := ClassifyIn(dc, []string{p}, l, r, at, nil, raws, nil, nil)[0]; ln.Kind == KindNoise || ln.Kind == KindEcho {
		t.Errorf("a batch labelled by its answer's minute, F serving another recording of the call: %s; want it refused", ln.Kind)
	}
	both := append(date("Thu, 22 Oct 2026 11:14:16 GMT", true), date("Thu, 22 Oct 2026 11:15:20 GMT", true)...)
	both[0].Mock, both[1].Mock = "mock-1", "mock-2"
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, both, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-1 at header.date") {
		t.Errorf("the same, served leaf by leaf: %s (%s); want it refused, naming mock-1's header.date", ln.Kind, ln.Why)
	}
	other := append(date("Thu, 22 Oct 2026 11:14:16 GMT", true), date("Thu, 22 Oct 2026 11:16:15 GMT", true)...)
	other[0].Mock, other[1].Mock = "mock-1", "mock-2"
	if ln := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, other, nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("a word F saw at another part of a stamp F served: %s (%s); want noise", ln.Kind, ln.Why)
	}
	// An answer F served with this run's value of a variable (its epoch
	// second moved with the lead), which the app writes as an instant: the
	// echo reads what F served, and the value is a variable of that leaf.
	expiry := []Earlier{{Mock: "mock-5", At: at.Add(-time.Second), Payload: payloadOf("mock-5", at.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.exp", Noise: "body.exp", Type: Number, Value: "1792667656"}, Live: "1792667717"},
	}}}
	if ln := classifyDeparture("2026-10-22T11:14:16Z", "2026-10-22T11:15:17Z", at, nil, expiry); ln.Kind != KindVariable || ln.From.Mock != "mock-5" || ln.From.Path != "body.exp" {
		t.Errorf("an instant F served with this run's value: %s from %+v (%s); want a variable of mock-5's body.exp", ln.Kind, ln.From, ln.Why)
	}
}

// The echo reads an instant's time of day as it is written in its own zone,
// as well as in UTC: a zoned pick from a timetable of local times of day
// (the Raw and the leaf-by-leaf path), and a time of day picked from a
// timetable of zoned instants, are variables of the timetable, never noise,
// whether the lead or the call's window would excuse them.
func TestAZonedPickFromALocalTimetableStaysChecked(t *testing.T) {
	at := time.Date(2026, 10, 22, 5, 43, 30, 0, time.UTC)
	table := func(body string) *models.Mock {
		return &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/timetable"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
			ReqTimestampMock: at.Add(-2 * time.Second), ResTimestampMock: at.Add(-1 * time.Second),
		}}
	}
	local := `{"date":"2026-10-22","departures":["11:13","11:14","11:15","11:16"]}`
	for _, form := range [][2]string{
		{"2026-10-22T11:14+05:30", "2026-10-22T11:15+05:30"},
		{"2026-10-22T11:14:00+05:30", "2026-10-22T11:15:00+05:30"},
		{"Thu, 22 Oct 2026 11:14:00 +0530", "Thu, 22 Oct 2026 11:15:00 +0530"},
	} {
		if ln := classifyDeparture(form[0], form[1], at, nil, nil, table(local)); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
			t.Errorf("a zoned pick %q -> %q from local times, as a payload: %s from %q (%s); want a variable of mock-0", form[0], form[1], ln.Kind, ln.From.Mock, ln.Why)
		}
		if ln := classifyDeparture(form[0], form[1], at, nil, servedTimetable(at.Add(-2*time.Second), "11:13", "11:14", "11:15", "11:16")); ln.Kind != KindVariable || ln.From.Mock != "mock-0" || ln.From.Path != "body.departures[1]" {
			t.Errorf("a zoned pick %q -> %q from local times, served leaf by leaf: %s from %+v (%s); want a variable of mock-0's body.departures[1]", form[0], form[1], ln.Kind, ln.From, ln.Why)
		}
	}
	// The other way: a time of day the app formats from a zoned instant of
	// the timetable, moved by the lead.
	zoned := []string{"2026-10-22T11:13+05:30", "2026-10-22T11:14+05:30", "2026-10-22T11:15+05:30"}
	if ln := classifyDeparture("11:14", "11:15", at, nil, nil, table(`{"departures":["`+strings.Join(zoned, `","`)+`"]}`)); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
		t.Errorf("a time of day from zoned instants, as a payload: %s from %q (%s); want a variable of mock-0", ln.Kind, ln.From.Mock, ln.Why)
	}
	if ln := classifyDeparture("11:14", "11:15", at, nil, servedTimetable(at.Add(-2*time.Second), zoned...)); ln.Kind != KindVariable || ln.From.Path != "body.departures[1]" {
		t.Errorf("a time of day from zoned instants, served leaf by leaf: %s from %+v (%s); want a variable of mock-0's body.departures[1]", ln.Kind, ln.From, ln.Why)
	}
	// Only F's departures listed, in their zone: F's pick stands at its
	// written time of day, the recorded one nowhere: refused, naming it.
	if ln := classifyDeparture("11:14", "11:15", at, nil, servedTimetable(at.Add(-2*time.Second), "2026-10-22T11:15+05:30", "2026-10-22T11:16+05:30")); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in mock-0 at body.departures[0]") {
		t.Errorf("a time of day only F's zoned timetable lists: %s (%s); want it refused, naming where F's moment stands", ln.Kind, ln.Why)
	}
	// A zone the timetable's times are not written in holds no pick: F's
	// "11:15+05:30" is 05:45 UTC, and a table of UTC instants holds neither
	// its written time of day nor its moment.
	utc := `{"departures":["2026-10-22T11:13:00Z","2026-10-22T11:14:00Z","2026-10-22T11:15:00Z"]}`
	if ln := classifyDeparture("2026-10-22T11:14+05:30", "2026-10-22T11:15+05:30", at, nil, nil, table(utc)); ln.Kind == KindVariable && ln.From.Echo {
		t.Errorf("a zoned pick against UTC instants of other moments: an echo of %q", ln.From.Mock)
	}
}

// AnswerStamp reads an HTTP Date header as its answer's stamp only within
// the span its exchange was stamped in, to the second; RawOf finds it by the
// field it is (an HTTP or HTTP/2 answer's Date, whatever its name's case),
// never by what a value looks like: a body field holding the same text is no
// stamp.
func TestAnHTTPDateIsItsAnswersStampWithinItsSpan(t *testing.T) {
	asked, answered := time.Date(2026, 10, 22, 11, 14, 13, 778e6, time.UTC), time.Date(2026, 10, 22, 11, 14, 16, 476e6, time.UTC)
	for _, tc := range []struct {
		date            string
		asked, answered time.Time
		want            bool
	}{
		{"Thu, 22 Oct 2026 11:14:16 GMT", asked, answered, true},
		{"Thu, 22 Oct 2026 11:14:13 GMT", asked, answered, true},
		{"  Thu, 22 Oct 2026 11:14:15 GMT", asked, answered, true},
		{"Thu, 22 Oct 2026 11:14:12 GMT", asked, answered, false},
		{"Thu, 22 Oct 2026 11:14:17 GMT", asked, answered, false},
		{"Thu, 22 Oct 2026 11:14:16 GMT", time.Time{}, answered, false},
		{"Thu, 22 Oct 2026 11:14:16 GMT", asked, time.Time{}, false},
		{"Thu, 22 Oct 2026 11:14:16 GMT", answered, asked, false},
		{"2026-10-22T11:14:16Z", asked, answered, false},
		{"soon", asked, answered, false},
	} {
		if got := AnswerStamp(tc.date, tc.asked, tc.answered); got != tc.want {
			t.Errorf("AnswerStamp(%q, %s, %s) = %v; want %v", tc.date, tc.asked.Format(time.RFC3339Nano), tc.answered.Format(time.RFC3339Nano), got, tc.want)
		}
	}
	m := r1Receive(`{"Note": "Thu, 22 Oct 2026 11:14:16 GMT"}`)
	stamps := func(m *models.Mock) []string {
		var out []string
		r := RawOf(m, true)
		for _, f := range r.payload().stamps {
			out = append(out, f.path+"="+r.payload().text[f.start:f.end])
		}
		return out
	}
	if got := stamps(m); !reflect.DeepEqual(got, []string{"Res.header.Date=Thu, 22 Oct 2026 11:14:16 GMT"}) {
		t.Errorf("the receive's stamps are %q; want its Date header alone", got)
	}
	lower := r1Receive("")
	lower.Spec.HTTPResp.Header = map[string]string{"date": "Thu, 22 Oct 2026 11:14:16 GMT"}
	if got := stamps(lower); len(got) != 1 {
		t.Errorf("a date header in lower case: stamps %q; want one", got)
	}
	h2 := &models.Mock{Name: "h2", Kind: models.HTTP2, Spec: models.MockSpec{
		HTTP2Req:         &models.HTTP2Req{},
		HTTP2Resp:        &models.HTTP2Resp{StatusCode: 200, Headers: map[string]string{"date": "Thu, 22 Oct 2026 11:14:16 GMT"}},
		ReqTimestampMock: asked, ResTimestampMock: answered,
	}}
	if got := stamps(h2); !reflect.DeepEqual(got, []string{"http2Resp.headers.date=Thu, 22 Oct 2026 11:14:16 GMT"}) {
		t.Errorf("an HTTP/2 answer's stamps are %q; want its date header", got)
	}
	req := r1Receive("")
	req.Spec.HTTPReq.Header["Date"] = "Thu, 22 Oct 2026 11:14:16 GMT"
	req.Spec.HTTPResp.Header = map[string]string{}
	if got := stamps(req); len(got) != 0 {
		t.Errorf("a request's Date header: stamps %q; want none (the app's, no answer's)", got)
	}
}

// RawOf's text is the payload as textOf writes it, whatever fields it
// indexes: each field's span is the text of that field's scalar or key.
func TestARawsFieldsIndexItsText(t *testing.T) {
	for _, m := range []*models.Mock{r1Receive(`{"SentTimestamp": "1792667656473"}`), r2Hello("mock-5", 8, r1Insert, r1Insert.Add(time.Millisecond), 1792694293458)} {
		b, err := json.Marshal(payloadSpec(m.Spec))
		if err != nil {
			t.Fatal(err)
		}
		v, ok := DecodeJSON(b)
		if !ok {
			t.Fatal("undecodable spec")
		}
		var sb strings.Builder
		textOf(dropKeys(v, 0), &sb)
		r := RawOf(m, true)
		if r.payload().text != sb.String() {
			t.Fatalf("%s: RawOf's text differs from textOf's", m.Name)
		}
		for _, f := range r.payload().fields {
			if f.start > f.end || f.end > len(r.payload().text) || f.end < len(r.payload().text) && r.payload().text[f.end] != '\n' {
				t.Fatalf("%s: field %s spans [%d, %d), no line of the text", m.Name, f.path, f.start, f.end)
			}
		}
	}
	r := RawOf(r1Receive(""), true)
	var dates []string
	for _, f := range r.payload().fields {
		if strings.HasSuffix(f.path, ".Date") {
			dates = append(dates, fmt.Sprintf("%s %v %s", f.path, f.key, r.payload().text[f.start:f.end]))
		}
	}
	if !reflect.DeepEqual(dates, []string{"Res.header.Date true Date", "Res.header.Date false Thu, 22 Oct 2026 11:14:16 GMT"}) {
		t.Fatalf("the receive's Date fields are %q", dates)
	}
}

// Rule 1 binds a value to an earlier leaf only where the leaf moved from
// the value's recorded moment to the one F saw and held neither on its other
// side: a leaf that holds the value's recorded moment in F too (a range that
// grew to hold the next minute) did not move with it, and the value is no
// variable of it.
func TestAMomentHeldOnBothSidesOfALeafIsNoCoDrift(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	earlier := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.range", Noise: "body.range", Type: String, Value: rec}, Live: live, Place: "body.range"},
		}}}
	}
	const p = "body.at"
	r := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "2026-10-22T11:14:00Z"}}}
	l := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "2026-10-22T11:15:00Z"}}}
	moved := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, earlier("since 1792667640000", "since 1792667700000"), nil, nil, nil)[0]
	if moved.Kind != KindVariable || moved.From.Mock != "mock-1" {
		t.Fatalf("a leaf that moved from the recorded moment to F's: %s from %q; want a variable of mock-1", moved.Kind, moved.From.Mock)
	}
	both := ClassifyIn(clockedDrifts(), []string{p}, l, r, at, earlier("since 1792667640000", "since 1792667640000 until 1792667700000"), nil, nil, nil)[0]
	if both.Kind == KindVariable && both.From.Mock == "mock-1" {
		t.Fatalf("a leaf that holds the recorded moment in F too: a variable of mock-1 (%+v); want it no co-drift", both.From)
	}
}

// The time of day an instant's text writes is the text's own, whatever zone
// keploy runs in: an HTTP Date header's "11:15:17 GMT" writes 11:15:17 under
// a host in British Summer Time too, where Go would read the abbreviation
// "GMT" in the host's zone and give its wall clock 12:15:17. So a drifted
// time of day 12:14:16 -> 12:15:17 beside it is no pick from it, on every
// host: a read of the clock modulo a day, in UTC.
func TestAnInstantsWrittenTimeOfDayIsNotTheHosts(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []*time.Location{time.UTC, london} {
		saved := time.Local
		time.Local = loc
		func() {
			defer func() { time.Local = saved }()
			sp := instantSpans("Thu, 22 Oct 2026 11:15:17 GMT")
			if want := int64(11*time.Hour + 15*time.Minute + 17*time.Second); len(sp) != 1 || sp[0].wall != want {
				t.Fatalf("host zone %s: the Date's spans %+v; want one, writing 11:15:17", loc, sp)
			}
			at := time.Date(2026, 10, 22, 11, 15, 30, 0, time.UTC)
			m := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "HTTP_CLIENT"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/status"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": "Thu, 22 Oct 2026 11:15:17 GMT"}, Body: `{"ok":true}`},
				ReqTimestampMock: at.Add(-14 * time.Second), ResTimestampMock: at.Add(-12 * time.Second),
			}}
			if ln := classifyDeparture("12:14:16", "12:15:17", at, nil, nil, m); !todRead(ln, models.ZoneUTC) {
				t.Errorf("host zone %s: %s from %q Read=%+v (%s); want a read of the clock modulo a day, in UTC", loc, ln.Kind, ln.From.Mock, ln.Read, ln.Why)
			}
		}()
	}
}

// A coarser copy of a served instant is the echo's: the queue consumer
// stores the SentTimestamp of the message it was delivered (milliseconds) at
// a coarser precision (a second, a minute, or milliseconds ending in 000),
// truncated or rounded, typed or as text: RFC 3339, epoch seconds or
// milliseconds, a MySQL DATETIME, a Postgres timestamp(0). F delivers another
// recording's message (mock-11, sent 2m0.311s later). The value came from
// served data in every form: a variable of the receive that held its
// recorded moment (mock-4), compared as instants at the value's own
// precision, never by text; held as F saw it only (the receive not among the
// places), refused, naming mock-11. Never noise.
func TestACoarserCopyOfAServedInstantIsTheEchos(t *testing.T) {
	sent := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	receive, delivered := r1Receive(attrs(sent)), r1Receive(attrs(got))
	delivered.Name = "mock-11"
	for _, m := range []*models.Mock{receive, delivered} {
		// No Date header of the moment: only the attribute holds it.
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	// The delivering receive alone, as nothing else of the recorded
	// message's moment: its attribute holds F's.
	alone := &models.Mock{Name: "mock-11", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "/000000000000/localstack-queue/"},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"Messages": [{"Body": "S12 message", "Attributes": ` + attrs(got) + `}]}`},
		ReqTimestampMock: r1Insert.Add(-3 * time.Second), ResTimestampMock: r1Insert.Add(-2 * time.Second),
	}}
	sec := func(at time.Time) time.Time { return at.Truncate(time.Second) }
	round := func(at time.Time) time.Time { return at.Round(time.Second) }
	minute := func(at time.Time) time.Time { return at.Truncate(time.Minute) }
	const p = "body.processed_at"
	text := func(f func(time.Time) string) func(time.Time) Leaf {
		return func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: f(at)} }
	}
	for _, tc := range []struct {
		name string
		cut  func(time.Time) time.Time
		leaf func(time.Time) Leaf
	}{
		{"RFC 3339 to the second", sec, text(func(at time.Time) string { return at.Format(time.RFC3339) })},
		{"RFC 3339 rounded to the second", round, text(func(at time.Time) string { return at.Format(time.RFC3339) })},
		{"epoch seconds", sec, text(func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) })},
		{"epoch milliseconds ending 000", sec, text(func(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) })},
		{"a MySQL DATETIME", sec, text(func(at time.Time) string { return at.Format("2006-01-02 15:04:05") })},
		{"a Postgres timestamp(0) with its zone", sec, text(func(at time.Time) string { return at.Format("2006-01-02 15:04:05") + "+00" })},
		{"a Postgres timestamp(0) rounded", round, text(func(at time.Time) string { return at.Format("2006-01-02 15:04:05") })},
		{"to the minute", minute, text(func(at time.Time) string { return at.Format("2006-01-02T15:04Z07:00") })},
		{"a typed second", sec, func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Second) }},
		{"a typed rounded second", round, func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Second) }},
		{"a typed millisecond ending 000", sec, func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Millisecond) }},
		{"a typed minute", minute, func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Minute) }},
	} {
		rec := Shape{Leaves: map[string]Leaf{p: tc.leaf(tc.cut(sent))}}
		live := Shape{Leaves: map[string]Leaf{p: tc.leaf(tc.cut(got))}}
		dc := clockedDrifts()
		ln := ClassifyIn(dc, []string{p}, live, rec, r1Insert, nil, []Raw{dc.RawOf(receive, true).ServedAsRecorded(), dc.RawOf(delivered, true).ServedAsRecorded()}, nil, nil)[0]
		if ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
			t.Errorf("%s (%q -> %q): %s from %+v (%s); want an echo's variable from mock-4", tc.name, rec.Leaves[p].Value, live.Leaves[p].Value, ln.Kind, ln.From, ln.Why)
		}
		dc = clockedDrifts()
		ln = ClassifyIn(dc, []string{p}, live, rec, r1Insert, nil, []Raw{dc.RawOf(alone, true).ServedAsRecorded()}, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-11") {
			t.Errorf("%s, mock-11 alone: %s (%s); want it refused, naming mock-11", tc.name, ln.Kind, ln.Why)
		}
	}
	// A finer value is no copy of a coarser served instant of a unit within
	// F's lead: the receive's second is the second of each run's value, as
	// it is of a read of the clock in that second.
	secAttrs := func(at time.Time) string { return `{"SentTimestamp": "` + strconv.FormatInt(at.Unix(), 10) + `"}` }
	coarse, coarser := r1Receive(secAttrs(sent)), r1Receive(secAttrs(got))
	coarser.Name = "mock-11"
	for _, m := range []*models.Mock{coarse, coarser} {
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	ms := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: strconv.FormatInt(at.UnixMilli(), 10)}}}
	}
	dc := clockedDrifts()
	if ln := ClassifyIn(dc, []string{p}, ms(got), ms(sent), r1Insert, nil, []Raw{dc.RawOf(coarse, true).ServedAsRecorded(), dc.RawOf(coarser, true).ServedAsRecorded()}, nil, nil)[0]; !noiseIn(ln, nil) {
		t.Errorf("milliseconds beside served seconds: %s from %+v (%s); want noise", ln.Kind, ln.From, ln.Why)
	}
	// An exact copy a formatter wrote dropping its fraction's trailing
	// zeros: the recorded message sent in a whole second, F's not
	// ("…T11:14:16Z", then "…T11:14:16.784Z", F delivering a neighbour of
	// the same second; a DATETIME of its own zone, F delivering a message
	// two minutes later). Its text's resolution is no bound on the rule.
	nano := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: at.Format(time.RFC3339Nano)}}}
	}
	loc := time.FixedZone("+05:30", 5*3600+1800)
	local := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: at.In(loc).Format("2006-01-02 15:04:05.999")}}}
	}
	rec := sent.Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		got   time.Time
		shape func(time.Time) Shape
	}{
		{"RFC 3339, a neighbour of the same second", rec.Add(784 * time.Millisecond), nano},
		{"a DATETIME of its zone, two minutes later", got, local},
	} {
		whole, other := r1Receive(attrs(rec)), r1Receive(attrs(tc.got))
		other.Name = "mock-11"
		for m, at := range map[*models.Mock]time.Time{whole: rec, other: tc.got} {
			m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
			// The message alone: no ReceiptHandle, whose epoch of its own
			// names the recorded second too.
			m.Spec.HTTPResp.Body = `{"Messages": [{"Body": "S12 message", "Attributes": ` + attrs(at) + `}]}`
		}
		dc = clockedDrifts()
		if ln := ClassifyIn(dc, []string{p}, tc.shape(tc.got), tc.shape(rec), r1Insert, nil, []Raw{dc.RawOf(whole, true).ServedAsRecorded(), dc.RawOf(other, true).ServedAsRecorded()}, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
			t.Errorf("an exact copy dropping trailing zeros, %s (%q -> %q): %s from %+v (%s); want an echo's variable from mock-4", tc.name, tc.shape(rec).Leaves[p].Value, tc.shape(tc.got).Leaves[p].Value, ln.Kind, ln.From, ln.Why)
		}
	}
	// Typed, it is no copy that way: an instant its protocol types is
	// matched at its own precision only, and is refused for its provenance.
	typedMs := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	dc = clockedDrifts()
	if ln := ClassifyIn(dc, []string{p}, typedMs(got), typedMs(sent), r1Insert, nil, []Raw{dc.RawOf(coarse, true).ServedAsRecorded(), dc.RawOf(coarser, true).ServedAsRecorded()}, nil, nil)[0]; !plainRead(ln) {
		t.Errorf("typed milliseconds beside served seconds: %s from %+v (%s); want no copy: a plain read of the clock, with no call window, checked at replay", ln.Kind, ln.From, ln.Why)
	}
}

// covers cuts the held instant to the drifted one's own precision: its
// resolution, or a coarser one it is whole at, truncated or rounded, in UTC
// or either one's zone; a time of day by its time of day, below a day.
func TestCovers(t *testing.T) {
	mo := func(s string) moment {
		sp := instantSpans(s)
		if len(sp) != 1 || !sp[0].keyed {
			t.Fatalf("%q: spans %+v", s, sp)
		}
		return sp[0].moment()
	}
	for _, tc := range []struct {
		held, m string
		want    bool
	}{
		{"1792667656473", "2026-10-22T11:14:16Z", true},
		{"1792667656473", "2026-10-22T11:14:17Z", false},
		{"1792667656784", "2026-10-22T11:14:17Z", true},
		{"1792667656784", "2026-10-22T11:14:16Z", true},
		{"1792667656473", "1792667656000", true},
		{"1792667656473", "1792667656474", false},
		{"2026-10-22T11:14:16Z", "1792667656473", false},
		{"1792667656473", "2026-10-22T11:14Z", true},
		{"1792667656473", "2026-10-22T11:15Z", false},
		{"1792667656473", "2026-10-22", true},
		{"1792667656473", "2026-10-23", false},
		{"2026-10-22T11:14:16.473+05:30", "2026-10-22", true},
		{"2026-10-22T23:44:16.473-05:00", "2026-10-22", true},
		{"2026-10-22T23:44:16.473-05:00", "2026-10-23", true},
		{"1792667656473", "11:14", true},
		{"1792667656473", "11:14:16", true},
		{"1792667656473", "11:13", false},
		{"11:14:16.473", "11:14", true},
		{"11:14:16.473", "2026-10-22T11:14:16Z", true},
		{"11:14", "2026-10-22T11:14:16Z", false},
		{"11:14", "2026-10-22", false},
		// A finer value built on a coarser held instant: the value cut to
		// the held instant's unit, truncated only, where that unit is
		// longer than F's lead (an hour, a day): within it (a second, a
		// minute) a read of the clock is as finer.
		{"2026-10-23", "2026-10-23T09:00:00Z", true},
		{"2026-10-23", "2026-10-22T09:00:00Z", false},
		// Rounded to the held unit it is no value built on it: a time past
		// noon is not the next day's.
		{"2026-10-23", "2026-10-22T18:00:00Z", false},
		{"2026-10-22T10:00:00Z", "2026-10-22T09:44:16Z", false},
		{"2026-10-22T09:00:00Z", "2026-10-22T09:14:16Z", true},
		{"2026-10-22T09:00:00Z", "2026-10-22T10:14:16Z", false},
		{"2026-10-22T11:14:16Z", "1792667656999", false},
		{"11:14", "11:14:59", false},
		{"09:00", "09:14:59", true},
		{"2026-10-22", "2026-10-22T23:59:59+05:30", true},
		{"2026-10-23T00:00+05:30", "2026-10-23T09:00+05:30", true},
		// A date of no zone a zoned value's own day is built on, whose UTC
		// day is the day before.
		{"2026-10-23", "2026-10-23T02:00:00+05:30", true},
		{"2026-10-22", "2026-10-23T02:00:00+05:30", true},
		{"2026-10-24", "2026-10-23T02:00:00+05:30", false},
		{"2026-10-22T11:14:16.473Z", "1792667656473", true},
		{"2026-10-22T20:00:00Z", "2026-10-23T00:00+05:30", true},
		{"2026-10-22T20:00:00Z", "2026-10-22T00:00+05:30", false},
		// Zones behind UTC by more than ten hours: the instant's own day
		// and wall clock, and its UTC day, never a day or a wall clock a
		// zone ahead would write.
		{"2026-10-22T20:00:00-10:00", "2026-10-22", true},
		{"2026-10-22T20:00:00-10:00", "2026-10-22 20:00:00", true},
		{"2026-10-22T20:00:00-10:00", "2026-10-23", true},
		{"2026-10-22T20:00:00-10:00", "2026-10-24", false},
		{"2026-10-22T20:00:00-10:00", "2026-10-23 20:00:00", false},
		{"2026-10-22T20:00:00-12:00", "2026-10-22 20:00:00", true},
		{"2026-10-22T20:00:00-12:00", "2026-10-23 20:00:00", false},
		// A text that names its zone is that zone's: no other zone's wall
		// clock.
		{"2026-10-22T16:44:16+05:30", "2026-10-22T16:44:16Z", false},
	} {
		if got := covers(mo(tc.held), mo(tc.m)); got != tc.want {
			t.Errorf("covers(%q, %q) = %v; want %v", tc.held, tc.m, got, tc.want)
		}
	}
	// A typed moment is held only as a held instant cut to its own
	// precision: never as a value built on a coarser one.
	typed := func(s string, res time.Duration) moment {
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return instantMoment(at, res)
	}
	for _, tc := range []struct {
		held string
		m    moment
		want bool
	}{
		{"2026-10-23", typed("2026-10-23T09:00:00Z", time.Second), false},
		{"2026-10-22T11:14:16Z", typed("2026-10-22T11:14:16.473Z", time.Millisecond), false},
		{"1792667656473", typed("2026-10-22T11:14:16Z", time.Second), true},
		{"1792667656473", typed("2026-10-22T11:14:16.473Z", time.Millisecond), true},
	} {
		if got := covers(mo(tc.held), tc.m); got != tc.want {
			t.Errorf("covers(%q, typed %v) = %v; want %v", tc.held, tc.m.key, got, tc.want)
		}
	}
}

// The review's rows for what the echo reads of an exchange F served: a
// base64 leaf F served otherwise than recorded holds, decoded, the moment F
// saw in the value and the one it was recorded with (a cursor's slots): the
// echo reads the recorded leaf in its every form, and names it, so the pick
// is a variable of that leaf, never noise and never left unaccounted for.
// And a stamp-to-stamp drift in a payload (a batch labelled by its answer's
// minute, F serving another recording of the call) is a variable of the
// stamp's mock, at its Date header: the stamps are searched as any text
// where the premise of rule 3 fails (stampsEchoed).
func TestTheEchoReadsAServedLeafInEveryForm(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 30, 0, time.UTC)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	served := []Earlier{{Mock: "mock-2", At: at.Add(-2 * time.Second), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.cursor", Noise: "body.cursor", Type: String, Value: b64(`{"slots":["2026-10-22T11:13:00Z","2026-10-22T11:14:00Z","2026-10-22T11:15:00Z"]}`)}, Live: b64(`{"slots":["2026-10-22T09:00:00Z"]}`)},
	}}}
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, served, nil, nil, nil)[0]
	if ln.Kind != KindVariable || ln.From.Mock != "mock-2" || ln.From.Path != "body.cursor" || !ln.From.Echo || ln.From.Side != Response {
		t.Errorf("a pick from a base64 leaf served otherwise: %s from %+v (%s); want an echo's variable of mock-2's body.cursor", ln.Kind, ln.From, ln.Why)
	}
	// The stamp-to-stamp drift.
	poll := func(name, date string, asked time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/status"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": date}, Body: `{"ok":true}`},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Second),
		}}
	}
	day := func(h, m, s int) time.Time { return time.Date(2026, 10, 22, h, m, s, 0, time.UTC) }
	first, second := poll("mock-1", "Thu, 22 Oct 2026 11:14:16 GMT", day(11, 14, 15)), poll("mock-2", "Thu, 22 Oct 2026 11:16:20 GMT", day(11, 16, 19))
	dc := clockedDrifts()
	raws := []Raw{dc.RawOf(first, true), dc.RawOf(second, true)}
	batch := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: v}}}
	}
	ln = ClassifyIn(dc, []string{"body.batch"}, batch("batch-16"), batch("batch-14"), day(11, 17, 0), nil, raws, nil, nil)[0]
	if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Field != "spec.resp.header.Date" {
		t.Errorf("a stamp-to-stamp drift: %s from %+v (%s); want a variable of mock-1 at spec.resp.header.Date", ln.Kind, ln.From, ln.Why)
	}
}

// A place holds each spelling of an instant: one it names in UTC and in a
// zone of its own holds the zone's wall clock, whichever it names first.
func TestAPlaceHoldsEverySpellingOfAnInstant(t *testing.T) {
	tod := func(s string) moment {
		sp := instantSpans(s)
		if len(sp) != 1 || !sp[0].keyed {
			t.Fatalf("%q: spans %+v", s, sp)
		}
		return sp[0].moment()
	}
	for _, text := range []string{`{"utc":"2026-10-22T11:15:00Z","local":"2026-10-22T16:45:00+05:30"}`, `{"local":"2026-10-22T16:45:00+05:30","utc":"2026-10-22T11:15:00Z"}`} {
		var held heldMoments
		held.add(instantSpans(text))
		for _, m := range []string{"16:45", "11:15"} {
			if !held.holds(tod(m)) {
				t.Errorf("%s holds no %s", text, m)
			}
		}
	}
}

// The coarser copy of a served instant in a zone neither it nor the place
// names: the consumer writes the delivered message's SentTimestamp as a
// DATETIME of its own zone (a JDBC LocalDateTime, PHP's date()). Read in
// each zone a zone may be in, one zone makes the moment F saw the delivering
// receive's and the moment it was recorded with the recorded receive's: a
// variable of the receive. A wall clock that only F's moment, or nothing,
// fits in some zone is no echo: a DATETIME read of the clock stays noise.
func TestACoarserCopyInTheApplicationsOwnZoneIsTheEchos(t *testing.T) {
	sent := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	receive, delivered := r1Receive(attrs(sent)), r1Receive(attrs(got))
	delivered.Name = "mock-11"
	for _, m := range []*models.Mock{receive, delivered} {
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
	}
	const p = "body.processed_at"
	for _, zone := range []string{"Asia/Kolkata", "America/New_York", "Pacific/Honolulu", "Asia/Kathmandu"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("no zone data: %v", err)
		}
		dt := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: at.In(loc).Truncate(time.Second).Format("2006-01-02 15:04:05")}}}
		}
		dc := clockedDrifts()
		ln := ClassifyIn(dc, []string{p}, dt(got), dt(sent), r1Insert, nil, []Raw{dc.RawOf(receive, true).ServedAsRecorded(), dc.RawOf(delivered, true).ServedAsRecorded()}, nil, nil)[0]
		if ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
			t.Errorf("%s: %s from %+v (%s); want an echo's variable from mock-4", zone, ln.Kind, ln.From, ln.Why)
		}
		// A DATETIME read of the clock, moved by the lead, beside the
		// receives: no zone makes both moments theirs.
		now := r1Insert.Add(-40 * time.Millisecond)
		dc = clockedDrifts()
		ln = ClassifyIn(dc, []string{p}, dt(now.Add(FullRoundLead)), dt(now), r1Insert, nil, []Raw{dc.RawOf(receive, true).ServedAsRecorded(), dc.RawOf(delivered, true).ServedAsRecorded()}, nil, nil)[0]
		if !noiseIn(ln, nil) {
			t.Errorf("%s, a read of the clock: %s from %+v (%s); want noise", zone, ln.Kind, ln.From, ln.Why)
		}
		// An instant its protocol types is the moment it names, in UTC:
		// the wall clock it would write in another zone is no copy of it,
		// and no variable. It is the receives' instants moved by the zone's
		// offset, as the delivery moved them: refused, naming the two
		// receives and the offset (offset.go), where F delivered the one in
		// place of the other: the recording gave the application the
		// receive before the call, and F the delivering one instead.
		typed := func(at time.Time) Shape {
			wall := at.In(loc).Truncate(time.Second)
			return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC), time.Second)}}
		}
		dc = clockedDrifts()
		ln = ClassifyIn(dc, []string{p}, typed(got), typed(sent), r1Insert, nil, []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, true).ServedAsRecorded()}, nil, nil)[0]
		_, off := sent.In(loc).Zone()
		rel := (time.Duration(off) * time.Second).String() + " after"
		if off < 0 {
			rel = (-time.Duration(off) * time.Second).String() + " before"
		}
		if ln.Kind != KindUnaccounted || ln.From.Mock != "" || !strings.Contains(ln.Why, "stands "+rel+" the moment mock-4 at spec.resp.body holds as the recording gave it") || !strings.Contains(ln.Why, rel+" the moment mock-11 at spec.resp.body holds") {
			t.Errorf("%s, typed: %s from %+v (%s); want it refused, built on the receives by %s", zone, ln.Kind, ln.From, ln.Why, rel)
		}
	}
}

// One instant an earlier place holds, cut by one rule, is one value: a
// minute read of the clock ("11:14" then "11:15", moved by the lead) beside
// a served created_at of 11:14:40.120, which names 11:14 truncated and 11:15
// rounded, is no pick from it, and is a read of the clock modulo a day
// (never noise) wherever in the minute the neighbour was recorded.
func TestAnInstantThatNamesBothMomentsIsNoSource(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 50, 0, time.UTC)
	const p = "body.at"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	// Typed to the minute, the same read by the lead: noise by its
	// provenance, the served instant no copy of it either.
	typed := func(h, m int) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, time.Date(2026, 10, 22, h, m, 0, 0, time.UTC), time.Minute)}}
	}
	for _, created := range []string{"2026-10-22T11:14:40.120Z", "2026-10-22T11:14:20.000Z"} {
		served := []Earlier{{Mock: "mock-3", At: at.Add(-5 * time.Second), Payload: payloadOf("mock-3", at.Add(-5*time.Second)), Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.created_at", Noise: "body.created_at", Type: String, Value: created}, Live: created},
		}}}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, served, nil, nil, nil)[0]; !todRead(ln, "") {
			t.Errorf("beside %s: %s from %+v Read=%+v (%s); want a read of the clock modulo a day", created, ln.Kind, ln.From, ln.Read, ln.Why)
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(11, 15), typed(11, 14), at, served, nil, nil, nil)[0]; !noiseOrRead(ln) {
			t.Errorf("typed, beside %s: %s from %+v (%s); want noise", created, ln.Kind, ln.From, ln.Why)
		}
	}
}

// A neighbour recording within one unit of the recorded one, which F
// delivers (an SQS batch: messages sent milliseconds apart): the consumer
// stores the delivered message's SentTimestamp rounded (MySQL's DATETIME(0)
// rounds a fraction of a second; RFC 3339 rounded; to the minute; typed to
// the minute). F's message names F's value rounded and the recorded one
// truncated, the recorded message names the recorded value both ways: one
// rule, rounding, names the recorded value in the recorded message and F's
// in F's, so the value is a copy, a variable of the recorded receive (never
// noise, whichever rule F's message names the recorded value by too). F's
// message two minutes later is the control.
func TestANeighbourRecordingWithinOneUnitIsTheEchos(t *testing.T) {
	attrs := func(at time.Time) string {
		return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}`
	}
	const p = "body.processed_at"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	at := func(m, s, ms int) time.Time { return time.Date(2026, 10, 22, 11, m, s, ms*1e6, time.UTC) }
	for _, tc := range []struct {
		name      string
		sent, got time.Time
		unit      time.Duration
		layout    string
	}{
		{"a MySQL DATETIME rounded", at(14, 16, 473), at(14, 16, 784), time.Second, "2006-01-02 15:04:05"},
		{"RFC 3339 rounded", at(14, 16, 473), at(14, 16, 784), time.Second, time.RFC3339},
		{"rounded to the minute", at(14, 29, 900), at(14, 30, 211), time.Minute, "2006-01-02T15:04Z07:00"},
		{"the control, two minutes later", at(14, 16, 473), at(16, 16, 784), time.Second, "2006-01-02 15:04:05"},
	} {
		receive, delivered := r1Receive(attrs(tc.sent)), r1Receive(attrs(tc.got))
		delivered.Name = "mock-11"
		for _, m := range []*models.Mock{receive, delivered} {
			m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 10:02:51 GMT"
		}
		rec, live := tc.sent.Round(tc.unit), tc.got.Round(tc.unit)
		for _, form := range []struct {
			name      string
			rec, live Shape
		}{
			{"as text", str(rec.Format(tc.layout)), str(live.Format(tc.layout))},
			{"typed", Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, rec, tc.unit)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, live, tc.unit)}}},
		} {
			dc := clockedDrifts()
			ln := ClassifyIn(dc, []string{p}, form.live, form.rec, r1Insert, nil, []Raw{dc.RawOf(receive, true), dc.RawOf(delivered, true)}, nil, nil)[0]
			if ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
				t.Errorf("%s, %s (%q -> %q): %s from %+v (%s); want an echo's variable from mock-4", tc.name, form.name, form.rec.Leaves[p].Value, form.live.Leaves[p].Value, ln.Kind, ln.From, ln.Why)
			}
		}
	}
}

// The echo's source is a place that names the recorded moment by the rule
// the echo matched, apart from F's moment: where F's message names F's value
// truncated only, a message that names the recorded value rounded only
// (mock-2, 15.700 of a recorded 16) is no source, though it stands first;
// the one that names it truncated (mock-4) is.
func TestTheEchosSourceIsAPlaceItsRuleNames(t *testing.T) {
	sent := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "/queue/receive", Body: `{"MaxNumberOfMessages":10}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"Messages":[{"Attributes":{"SentTimestamp":"` + strconv.FormatInt(at.UnixMilli(), 10) + `"}}]}`},
			ReqTimestampMock: time.Date(2026, 10, 22, 11, 14, 10, 0, time.UTC),
		}}
	}
	at := func(m, s, ms int) time.Time { return time.Date(2026, 10, 22, 11, m, s, ms*1e6, time.UTC) }
	dc := clockedDrifts()
	raw := []Raw{dc.RawOf(sent("mock-2", at(14, 15, 700)), true), dc.RawOf(sent("mock-4", at(14, 16, 300)), true), dc.RawOf(sent("mock-11", at(15, 18, 700)), true)}
	const p = "body.processed_at"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	ln := ClassifyIn(dc, []string{p}, str("2026-10-22T11:15:18Z"), str("2026-10-22T11:14:16Z"), at(14, 17, 0), nil, raw, nil, nil)[0]
	if ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
		t.Fatalf("%s from %+v (%s); want an echo's variable from mock-4", ln.Kind, ln.From, ln.Why)
	}
}

// A value built on a date the application was given, with a time of day of
// its own (the next opening of the dates its test case's request lists, or
// that a served answer lists, picked by its clock): read from a text, its
// date is held, both runs' (the value cut to the date's resolution), so it
// is a copy: a variable of the date it was recorded with, which nothing
// binds at, never noise. Where only F's date is held, it is refused, naming
// it. Where its date held in both runs is one date, nothing of it changed
// that a date accounts for: its time is the application's own. Typed, an
// instant is matched at its own precision only, and is refused for its
// provenance.
func TestAValueBuiltOnAGivenDateIsTheEchos(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.opens"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	body := `{"dates":["2026-10-22","2026-10-23"]}`
	request := func(body string) []Earlier {
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: body, Sent: body}}})}
	}
	served := func(body string) []Earlier {
		v, _ := DecodeJSON([]byte(body))
		leaves := map[string]Leaf{}
		JSONLeaves("body", v, leaves)
		e := Earlier{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true}
		for _, path := range []string{"body.dates[0]", "body.dates[1]"} {
			if l, ok := leaves[path]; ok {
				e.Seen = append(e.Seen, Seen{Side: Response, Leaf: l, Live: l.Value})
			}
		}
		return []Earlier{e}
	}
	for _, c := range []struct {
		name    string
		earlier []Earlier
		mock    string
	}{{"the request's", request(body), "test case test-1"}, {"a served answer's", served(body), "mock-1"}} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-23T09:00:00Z"), str("2026-10-22T09:00:00Z"), at, c.earlier, nil, nil, nil)[0]
		if ln.Kind != KindVariable || ln.From.Mock != c.mock || ln.From.Path != "body.dates[0]" || !ln.From.Echo {
			t.Errorf("built on %s dates: %s from %+v (%s); want an echo's variable of %s at body.dates[0]", c.name, ln.Kind, ln.From, ln.Why, c.mock)
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-22T10:00:07Z"), str("2026-10-22T09:00:07Z"), at, c.earlier, nil, nil, nil)[0]; !noiseIn(ln, nil) || ln.Kind == KindUnaccounted {
			t.Errorf("its time alone changed, beside %s dates: %s from %+v (%s); want noise", c.name, ln.Kind, ln.From, ln.Why)
		}
		typed := func(d int) Shape {
			return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, time.Date(2026, 10, d, 9, 0, 7, 0, time.UTC), time.Second)}}
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(23), typed(22), at, c.earlier, nil, nil, nil)[0]; !plainRead(ln) {
			t.Errorf("typed, beside %s dates: %s from %+v (%s); want no echo: a plain read of the clock, with no call window, checked at replay", c.name, ln.Kind, ln.From, ln.Why)
		}
	}
	// Only F's date held: refused, naming it.
	only := `{"dates":["2026-10-23"]}`
	for _, c := range []struct {
		name, at string
		earlier  []Earlier
	}{{"the request's", "test case test-1 at body.dates[0]", request(only)}, {"a served answer's", "mock-1 at body.dates[0]", served(only)}} {
		if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-23T09:00:00Z"), str("2026-10-22T09:00:00Z"), at, c.earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "the moment record's full round saw in it stands in "+c.at+", while") {
			t.Errorf("F's date alone in %s: %s (%s); want it refused, naming %s", c.name, ln.Kind, ln.Why, c.at)
		}
	}
}

// A value one side of which names its zone is read in that zone: no other
// zone's wall clock is tried for it, so a recorded instant in UTC whose time
// is a served instant's in another zone, beside a wall clock of no zone, is
// no copy. And what the recordings hold is what they were recorded with: an
// instant F served in a recorded place's stead (another value answered
// there) is no place the recorded moment stands, so a wall clock of no zone
// whose recorded moment only that served instant names, in some zone, is no
// copy either.
func TestTheZoneSearchReadsWallClocksOfNoZoneAgainstRecordings(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	const p = "body.at"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	leaf := func(mock, rec, live string) Earlier {
		return Earlier{Mock: mock, At: at.Add(-time.Second), Payload: payloadOf(mock, at.Add(-time.Second)), Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.sentAt", Noise: "body.sentAt", Type: String, Value: rec}, Live: live},
		}}
	}
	const t1, t2 = "1792667656473", "1792667776784" // 11:14:16.473Z, 11:16:16.784Z: 16:44:16 and 16:46:16 at +05:30
	both := []Earlier{leaf("mock-4", t1, t1), leaf("mock-11", t2, t2)}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-22 16:46:16"), str("2026-10-22T16:44:16Z"), at, both, nil, nil, nil)[0]; !noiseIn(ln, nil) {
		t.Errorf("a zoned recorded instant beside a wall clock of no zone: %s from %+v (%s); want noise", ln.Kind, ln.From, ln.Why)
	}
	// The control: both of no zone, one zone (+05:30) names both moments.
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-22 16:46:16"), str("2026-10-22 16:44:16"), at, both, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
		t.Errorf("both of no zone: %s from %+v (%s); want an echo's variable of mock-4", ln.Kind, ln.From, ln.Why)
	}
	// A wall clock of no zone built on a served zoned date: the next
	// opening's local date (+05:30's midnights, in UTC), with a time of the
	// application's own, written in its zone with none named.
	dates := []Earlier{leaf("mock-4", "2026-10-21T18:30:00Z", "2026-10-21T18:30:00Z"), leaf("mock-11", "2026-10-22T18:30:00Z", "2026-10-22T18:30:00Z")}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-23 09:07:13"), str("2026-10-22 09:07:13"), at, dates, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-4" || !ln.From.Echo {
		t.Errorf("a wall clock of no zone built on a served zoned date: %s from %+v (%s); want an echo's variable of mock-4", ln.Kind, ln.From, ln.Why)
	}
	swapped := []Earlier{leaf("mock-4", t1, t2)}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-22 16:44:16"), str("2026-10-22 16:46:16"), at, swapped, nil, nil, nil)[0]; !noiseIn(ln, nil) {
		t.Errorf("a recorded moment only what F served names: %s from %+v (%s); want noise", ln.Kind, ln.From, ln.Why)
	}
}

// An instant its protocol types names its moment in UTC: one that is a
// served zoned instant's wall clock written as UTC is no copy of it, and is
// noise only by its provenance.
func TestATypedInstantIsNoZonedInstantsWallClock(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	const p = "body.processed_at"
	typed := func(t time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, t, time.Second)}} }
	served := []Earlier{{Mock: "mock-4", At: at.Add(-time.Second), Payload: payloadOf("mock-4", at.Add(-time.Second)), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.sentAt", Noise: "body.sentAt", Type: String, Value: "2026-10-22T16:44:16+05:30"}, Live: "2026-10-22T16:46:16+05:30"},
	}}}
	ln := ClassifyIn(clockedDrifts(), []string{p}, typed(time.Date(2026, 10, 22, 16, 46, 16, 0, time.UTC)), typed(time.Date(2026, 10, 22, 16, 44, 16, 0, time.UTC)), at, served, nil, nil, nil)[0]
	if ln.Kind != KindUnaccounted || ln.From.Mock != "" || ln.Why != instantNotClock {
		t.Errorf("a typed instant of a zoned instant's wall clock: %s from %+v (%s); want it refused for its provenance alone", ln.Kind, ln.From, ln.Why)
	}
}

// A wall clock of no zone that only F's moment fits in some zone, beside a
// recording that holds no moment it was recorded with in that zone, is no
// echo: a coincidence on one side, which a read of the clock can meet.
func TestAWallClockOfNoZoneFitOnOneSideIsNoEcho(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	const p = "body.at"
	dt := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	served := []Earlier{{Mock: "mock-4", At: at.Add(-time.Second), Payload: payloadOf("mock-4", at.Add(-time.Second)), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.sentAt", Noise: "body.sentAt", Type: String, Value: "1792667656473"}, Live: "1792667776784"},
	}}}
	// F's 16:46:16 is mock-4's live SentTimestamp in +05:30; the recorded
	// 16:44:01 is no moment mock-4 recorded, in that zone or any.
	ln := ClassifyIn(clockedDrifts(), []string{p}, dt("2026-10-22 16:46:16"), dt("2026-10-22 16:44:01"), at, served, nil, nil, nil)[0]
	if !noiseIn(ln, nil) {
		t.Errorf("a wall clock F's side alone fits: %s from %+v (%s); want noise", ln.Kind, ln.From, ln.Why)
	}
}

// cutsNone turns a pair of instants away only where no rule of cuts names
// one by the other: of instants of every resolution and zone, whole at each
// unit, standing apart by a unit, by a zone, by a zone and a unit, or by
// days, no pair it turns away is one cutsEach finds a rule for.
func TestCutsNoneTurnsAwayOnlyPairsNoRuleNames(t *testing.T) {
	rng := rand.New(rand.NewPCG(5016, 1))
	base := time.Date(2026, 10, 31, 19, 12, 30, 432877056, time.UTC).UnixNano()
	zones := []int64{0, int64(5*time.Hour + 30*time.Minute), int64(-10 * time.Hour), int64(14 * time.Hour),
		int64(-12 * time.Hour), int64(45 * time.Minute), int64(time.Second), int64(17 * time.Second), int64(90 * time.Second)}
	unit := func() int64 { return int64(precisionUnits[rng.IntN(len(precisionUnits))]) }
	zone := func() int64 {
		if rng.IntN(3) == 0 {
			return int64(rng.IntN(105)-48) * int64(15*time.Minute)
		}
		return zones[rng.IntN(len(zones))]
	}
	near := func(u int64) int64 { return rng.Int64N(2*u+1) - u }
	mk := func(at, z int64) moment {
		return moment{key: instantKey{at: at}, wall: timeOfDay(at + z), res: precisionUnits[rng.IntN(len(precisionUnits))],
			zone: z, zoned: rng.IntN(2) == 0, typed: rng.IntN(2) == 0}
	}
	var turned, named int
	for i := 0; i < 400000; i++ {
		mz, hz := zone(), zone()
		at := base + near(int64(3*24*time.Hour))
		if rng.IntN(4) != 0 {
			// Whole at a unit, as m's zone writes it.
			u := unit()
			at -= ((at+mz)%u + u) % u
		}
		m := mk(at, mz)
		var hat int64
		switch rng.IntN(5) {
		case 0:
			hat = at
		case 1:
			hat = at + near(unit())
		case 2:
			hat = at - hz + near(unit())
		case 3:
			hat = at + mz + near(unit())
		default:
			hat = at + near(int64(3*24*time.Hour))
		}
		if rng.IntN(3) == 0 {
			u := unit()
			hat -= ((hat+hz)%u + u) % u
		}
		h := mk(hat, hz)
		some := cutsEach(h, m, func(cutRule) bool { return true })
		if some {
			named++
		}
		if cutsNone(h, m) {
			turned++
			if some {
				t.Fatalf("cutsNone turned away %+v of %+v, which cutsEach names", h, m)
			}
		}
	}
	if turned < 100000 || named < 10000 {
		t.Fatalf("the pairs do not test cutsNone: it turned away %d, cutsEach named %d", turned, named)
	}
	t.Logf("turned away %d of 400000 pairs, cutsEach named %d", turned, named)
}
