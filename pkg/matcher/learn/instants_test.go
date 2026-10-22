package learn

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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

// The consumer's insert (mock-8) and the call before it at record: a
// concurrent handler's call on another connection (mock-9), sent after the
// consumer read its clock for processed_at under load (11:14:17.012 against
// 11:14:17.014).
var (
	r1Insert   = time.Date(2026, 10, 22, 11, 14, 17, 155076558, time.UTC)
	r1PrevCall = time.Date(2026, 10, 22, 11, 14, 17, 14119590, time.UTC)
)

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

// classifyProcessedAt classifies the insert's processed_at, recorded at rec
// and live in F, against the receives F served before it (raw), read in w.
func classifyProcessedAt(rec, live time.Time, typed bool, receive *models.Mock, w *Window, more ...*models.Mock) Learned {
	const p = "body.docs.msgs.0.processed_at.$date.$numberLong"
	dc := NewDriftCache()
	var raw []Raw
	for _, m := range append([]*models.Mock{receive}, more...) {
		raw = append(raw, dc.RawOf(m, true))
	}
	return ClassifyIn(dc, []string{p}, processedAt(live, typed), processedAt(rec, typed), r1Insert, nil, raw, nil, w)[0]
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

// An instant is compared as one unit: the consumer's processed_at, a read of
// its clock, is no value the receive held because the receive's Date header,
// its X-Amz-Date or its ReceiptHandle's epoch second share its minute, its
// second or the digits of its millisecond. Learned as noise in each reading,
// whatever the offset between the two instants and wherever the read fell
// against its call's window, as the E2 runs that paired it found; before
// this rule, a read that fell before a concurrent call's was refused as a
// value of the receive (its minute "14" is a word of the receive's Date).
func TestAnInstantIsComparedAsOneUnit(t *testing.T) {
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
			for wname, w := range r1Windows(row.rec, live) {
				for i, receive := range receives {
					for _, typed := range []bool{false, true} {
						if ln := classifyProcessedAt(row.rec, live, typed, receive, w); ln.Kind != KindNoise {
							t.Errorf("%s, live %s, %s, receive %d, typed %v: %s (from %q, %s); want noise", row.name, live.Format(time.RFC3339Nano), wname, i, typed, ln.Kind, ln.From.Mock, ln.Why)
						}
					}
				}
			}
		}
	}
}

// An application that stores an instant it was sent stores, in F, the
// instant of the answer F served, as it stored the recorded one at record:
// the payloads hold the instant both ways, and it is a variable of the place
// that held it when recorded, in each reading (the typed reading's RFC 3339
// is the raw reading's epoch milliseconds: one moment in another layout,
// trailing zeros or not), and wherever the read fell against its call's
// window. So is one held inside a longer value, or as an epoch second with
// its millisecond. Held one way only (F's value in no payload) it is no
// echo of an instant: only the reading that writes it as the payload does
// follows it, as the same value whole (a founder decision). As another
// moment (its second, a finer one its digits begin) it is no echo: noise.
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
			for wname, w := range r1Windows(sent, got) {
				for _, typed := range []bool{false, true} {
					if ln := classifyProcessedAt(sent, got, typed, receive, w, delivered); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
						t.Errorf("%s then %s, %s, typed %v: %s from %q; want a variable from mock-4", recorded[i], served[i], wname, typed, ln.Kind, ln.From.Mock)
					}
					ln := classifyProcessedAt(sent, got, typed, receive, w)
					switch {
					case sameText(recorded[i], typed) && (ln.Kind != KindVariable || ln.From.Mock != "mock-4"):
						t.Errorf("%s, F's value in no payload, %s, typed %v: %s from %q; want the same value whole followed", recorded[i], wname, typed, ln.Kind, ln.From.Mock)
					case !sameText(recorded[i], typed) && ln.Kind != KindNoise:
						t.Errorf("%s, F's value in no payload, %s, typed %v: %s from %q; want noise", recorded[i], wname, typed, ln.Kind, ln.From.Mock)
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
					if ln := classifyProcessedAt(sent, got, typed, receive, w, delivered); ln.Kind != KindNoise {
						t.Errorf("%s then %s, %s, typed %v: %s (from %q); want noise", other[0], other[1], wname, typed, ln.Kind, ln.From.Mock)
					}
				}
			}
		}
	}
}

// The instants a text holds, in either case, and a time-ordered UUID's time
// groups; a word is found only outside every instant of the text, and an
// instant only whole.
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
	if got, want := blankInstants("Date: Thu, 22 Oct 2026 11:14:16 GMT; seq 14"), "Date: "+strings.Repeat(" ", len("Thu, 22 Oct 2026 11:14:16 GMT"))+"; seq 14"; got != want {
		t.Fatalf("blankInstants = %q", got)
	}
	var dc *DriftCache
	if dc.holdsUnits("Thu, 22 Oct 2026 11:14:16 GMT", "14") || !dc.holdsUnits("seq 14 at Thu, 22 Oct 2026 11:14:16 GMT", "14") {
		t.Fatal("holdsUnits found a word inside an instant, or missed one outside every instant")
	}
	if dc.holdsUnits("2026-10-22 11:14:16", "2026-10-22 11:14") || !dc.holdsUnits("at 2026-10-22 11:14:16 ok", "2026-10-22 11:14:16") {
		t.Fatal("holdsUnits found an instant inside a longer one, or missed one whole")
	}
	if dc.holdsUnits("1792667656.4732103", "1792667656") {
		t.Fatal("holdsUnits found an epoch second inside its fraction")
	}
}

// A word of a value outside every instant of it is never found inside an
// instant of another text: a batch number that drifted from 14 to 15 is no
// word of the receive's Date header, whose minute is 14, nor of an answer's
// date. Held outside every instant (a receive count of 14), it is found, as
// before.
func TestAWordIsNeverFoundInsideAnotherTextsInstant(t *testing.T) {
	r := Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: "batch-14"}}}
	l := Shape{Leaves: map[string]Leaf{"body.batch": {Path: "body.batch", Noise: "body.batch", Type: String, Value: "batch-15"}}}
	classify := func(receive *models.Mock, resp []Leaf) Learned {
		dc := NewDriftCache()
		return ClassifyIn(dc, []string{"body.batch"}, l, r, r1Insert, nil, []Raw{dc.RawOf(receive, true)}, resp, nil)[0]
	}
	if ln := classify(r1Receive(""), nil); ln.Kind != KindNoise {
		t.Fatalf("beside the receive's Date header: %s (from %q, %s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	date := []Leaf{{Path: "body.at", Noise: "body.at", Type: String, Value: "Thu, 22 Oct 2026 11:14:16 GMT"}}
	if ln := classify(r1Receive(""), date); ln.Kind != KindNoise {
		t.Fatalf("an answer holding a date of its minute: %s (%s); want noise", ln.Kind, ln.Why)
	}
	if ln := classify(r1Receive(`{"ApproximateReceiveCount": "14"}`), nil); ln.Kind != KindVariable || ln.From.Mock != "mock-4" {
		t.Fatalf("beside a receive count of 14: %s from %q; want a variable from mock-4", ln.Kind, ln.From.Mock)
	}
	count := []Leaf{{Path: "body.n", Noise: "body.n", Type: String, Value: "seen 14 times"}}
	if ln := classify(r1Receive(""), count); ln.Kind != KindUnaccounted {
		t.Fatalf("an answer holding 14 as a word: %s; want it refused", ln.Kind)
	}
}
