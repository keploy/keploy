package learn

import (
	"encoding/base64"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// payloadPartRun is one run of a client test shaped as a CRM's lead create:
// the test began, the handler read its clock for the lead's CreatedAt, set
// up its Pulsar producer (a look-up, a producer, a schema call: the last
// of them is the call before the PutItem), then wrote the lead.
type payloadPartRun struct{ began, read, prev, call time.Time }

// The recording: the application's Pulsar look-up at start-up, 3 s before
// the test began, then the test. Record's full round ran the test a lead
// and some lateness later, reading its clock 1.06 s after its begin.
var (
	payloadPartLookUp = time.Date(2026, 11, 2, 12, 48, 51, 277_384_000, time.UTC)
	payloadPartRec    = payloadPartRun{
		began: time.Date(2026, 11, 2, 12, 48, 54, 330_000_000, time.UTC),
		read:  time.Date(2026, 11, 2, 12, 48, 55, 350_537_100, time.UTC),
		prev:  time.Date(2026, 11, 2, 12, 48, 55, 423_433_000, time.UTC),
		call:  time.Date(2026, 11, 2, 12, 48, 55, 606_243_000, time.UTC),
	}
	payloadPartF = payloadPartRun{
		began: time.Date(2026, 11, 2, 12, 49, 57, 215_000_000, time.UTC),
		read:  time.Date(2026, 11, 2, 12, 49, 58, 275_116_400, time.UTC),
		prev:  time.Date(2026, 11, 2, 12, 49, 58, 391_020_000, time.UTC),
		call:  time.Date(2026, 11, 2, 12, 49, 58, 570_310_000, time.UTC),
	}
)

// The same application's activity consumer: a lead event pushed to it
// (mock-22, a Pulsar MESSAGE whose payload is the event, carrying the
// lead's CreatedAt), on which it read its clock for a welcome activity's At
// and wrote it in an UpdateItem. The recording's times are a record's own:
// the test began, the push, the read, the UpdateItem. In record's full
// round the push came 1.62 s after the test began too, but another call of
// the test was served between the consumer's read and its UpdateItem (the
// handler's, on its own goroutine), so the call's window starts after the
// read and holds none.
var (
	consumerRec = payloadPartRun{
		began: time.Date(2026, 11, 2, 17, 52, 40, 132_385_987, time.UTC),
		read:  time.Date(2026, 11, 2, 17, 52, 41, 757_046_900, time.UTC),
		prev:  time.Date(2026, 11, 2, 17, 52, 41, 753_935_078, time.UTC),
		call:  time.Date(2026, 11, 2, 17, 52, 41, 761_212_697, time.UTC),
	}
	consumerF = payloadPartRun{
		began: time.Date(2026, 11, 2, 17, 53, 45, 432_385_987, time.UTC),
		read:  time.Date(2026, 11, 2, 17, 53, 47, 56_385_900, time.UTC),
		prev:  time.Date(2026, 11, 2, 17, 53, 47, 58_401_000, time.UTC),
		call:  time.Date(2026, 11, 2, 17, 53, 47, 60_585_900, time.UTC),
	}
)

const consumerAt = "body.ExpressionAttributeValues.:a.L[0].M.At.S"

// leadEventPush is the lead event the broker pushed to the consumer as the
// Pulsar adapter records it: the event, its stamp in the minute of the
// consumer's read, base64 in the MESSAGE's JSON.
func leadEventPush() *models.Mock {
	event := base64.StdEncoding.EncodeToString([]byte(`{"type":"lead.created","id":"90b805b3-75dd-4460-b802-f35bcafea40b","email":"lead.1793641960493py566@example.com","at":"2026-11-02T17:52:41.6858437Z"}`))
	data := `{"commandType":"MESSAGE","consumerId":0,"body":{"messageId":{"entryId":1,"ledgerId":5,"partition":-1},"redeliveryCount":0},"hasPayload":true,"payloadData":"` + event + `","metadataData":"Cg5zdGFuZGFsb25lLTAtMRABGPqxkeuZNIIBCAAAAAAAAAAA"}`
	return &models.Mock{Name: "mock-22", Kind: "Pulsar", Spec: models.MockSpec{
		Metadata:         map[string]string{"commandType": "MESSAGE", "responseType": "MESSAGE", "serverPush": "true", "type": "mocks"},
		GenericResponses: []models.Payload{{Origin: "server", Message: []models.OutputBinary{{Type: "json", Data: data}}}},
		ReqTimestampMock: consumerRec.prev, ResTimestampMock: consumerRec.prev,
	}}
}

func payloadPartWindow(rec, f payloadPartRun) *Window {
	return &Window{
		Rec: Span{From: rec.prev, To: rec.call, Began: rec.began},
		F:   Span{From: f.prev, To: f.call, Began: f.began},
	}
}

// dotnetStamp is t as .NET's DateTime.UtcNow.ToString("O") writes it:
// seven digits of the second and a 'Z'.
func dotnetStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.0000000Z") }

// pulsarLookUp is a Pulsar look-up the application sent at at, as the
// Pulsar adapter records it: an opaque payload, no field of which F's view
// names. request is the frame the application sent.
func pulsarLookUp(name string, at time.Time, request string) *models.Mock {
	return &models.Mock{Name: name, Kind: "Pulsar", Spec: models.MockSpec{
		Metadata:         map[string]string{"commandType": "LOOKUP", "type": "config"},
		GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: request}}}},
		GenericResponses: []models.Payload{{Origin: "server", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"LOOKUP_RESPONSE","requestId":0,"body":{"authoritative":true,"brokerServiceUrl":"pulsar://localhost:6650","proxyThroughServiceUrl":true,"response":"Connect"}}`}}}},
		ReqTimestampMock: at, ResTimestampMock: at.Add(211 * time.Millisecond),
	}}
}

const lookUpRequest = `{"commandType":"LOOKUP","requestId":0,"topic":"persistent://public/default/lead-events","body":{"authoritative":false,"topic":"persistent://public/default/lead-events"}}`

// A client test's clock read made after its test began and before its
// call's window is learned the same way whatever digits the clock gave it:
// as a read since its test began (models.SinceTestBegin), as where no
// recording holds a part of it. Its call's window shows no read, so the
// value's changed words are searched as data, and whether one of them, or
// a decoded or case-folded form of one, stands as a token in an opaque
// payload is chance: .NET's "…55.3505371Z" ends in "3505371Z", which
// decodes as base64 to a text whose word, case-folded, is "y", a token the
// start-up Pulsar look-up's JSON holds; "…55.3805371Z" decodes to no text.
// Before, the first was learned as a variable of the look-up, which no call
// can bind, and refused, so the same test's lead create was kept in one
// record and refused in the next. So is the stamp as one word of a longer
// text: the window from its test begin learns that word as a read since
// the test began (models.ClockOffset.Word), and the rest of the text is
// still compared. So is a stamp a consumer read on an event pushed to it,
// whose minute the event's own stamp holds, where another call of the test
// was served between that read and the consumer's call.
func TestAClientTestsReadSinceItsTestBeganIsLearnedWhateverPartAPayloadHoldsByChance(t *testing.T) {
	str := func(p, v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	w := payloadPartWindow(payloadPartRec, payloadPartF)
	other := payloadPartRec.read.Add(30 * time.Millisecond) // "…55.3805371Z"
	minute := pulsarLookUp("mock-1", payloadPartLookUp, `{"commandType":"LOOKUP","requestId":48,"topic":"persistent://public/default/lead-events"}`)
	for _, tc := range []struct {
		name string
		rec  time.Time
		raw  []Raw
		// before and after, of a stamp that is one word of a longer
		// text, are the text around it: "" both for a value that is the
		// stamp whole.
		before, after string
	}{
		{"a decoded word of it stands in the start-up look-up", payloadPartRec.read, []Raw{RawOf(pulsarLookUp("mock-1", payloadPartLookUp, lookUpRequest), false)}, "", ""},
		{"the same, the look-up served", payloadPartRec.read, []Raw{RawOf(pulsarLookUp("mock-1", payloadPartLookUp, lookUpRequest), true)}, "", ""},
		{"no word of it stands in the look-up", other, []Raw{RawOf(pulsarLookUp("mock-1", payloadPartLookUp, lookUpRequest), false)}, "", ""},
		{"its minute stands in the look-up as a number", other, []Raw{RawOf(minute, false)}, "", ""},
		{"no payload", payloadPartRec.read, nil, "", ""},
		{"a decoded word of it, the stamp one word of a text", payloadPartRec.read, []Raw{RawOf(pulsarLookUp("mock-1", payloadPartLookUp, lookUpRequest), false)}, "lead created at ", " by crm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := "body.Item.CreatedAt.S"
			if tc.before != "" || tc.after != "" {
				p = "body.Item.Note.S"
			}
			rec := tc.before + dotnetStamp(tc.rec) + tc.after
			live := tc.before + dotnetStamp(payloadPartF.read) + tc.after
			var word models.ClockWord
			if p == "body.Item.Note.S" {
				word = models.ClockWordOf(rec, len(tc.before), len(rec)-len(tc.after))
			}
			ln := ClassifyIn(clientTestCache(), []string{p}, str(p, live), str(p, rec), payloadPartRec.call, nil, tc.raw, nil, w)[0]
			o, ok := ln.Clock()
			if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || o.Word != word {
				t.Fatalf("%q -> %q: %s from %q held %q word %+v (%s); want a plain read of the clock a replay checks since its test began, of the word %+v", rec, live, ln.Kind, ln.From.Mock, ln.From.Held, o.Word, ln.Why, word)
			}
		})
	}
	// The consumer's At: its minute, "52", stands in the lead event pushed
	// to it, and its call's window in record's full round holds no read.
	t.Run("a consumer's stamp whose minute the event pushed to it holds", func(t *testing.T) {
		rec, live := dotnetStamp(consumerRec.read), dotnetStamp(consumerF.read)
		ln := ClassifyIn(clientTestCache(), []string{consumerAt}, str(consumerAt, live), str(consumerAt, rec), consumerRec.call, nil, []Raw{RawOf(leadEventPush(), true)}, nil, payloadPartWindow(consumerRec, consumerF))[0]
		o, ok := ln.Clock()
		if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
			t.Fatalf("%q -> %q: %s from %q held %q (%s); want a plain read of the clock a replay checks since its test began", rec, live, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
		}
	})
}

// The window from each run's test begin takes only a plain read of the
// clock, and only for a value its call's window learned as a part found in
// a payload (or refused): where that window does not hold the read, or no
// begin is known, the part found stays the value's place, and a payload
// that holds the value whole (the application sent the stamp in a Pulsar
// message before the call) stays its source, as the copy rules read it,
// even where the window from each run's test begin sets that payload aside
// (under the proven flow, a payload F had not served before the call is no
// source of a read that window holds). So does a part an earlier call F
// served field by field drifted with, in its request or in its answer.
func TestAPartAPayloadHoldsStaysThePlaceWhereNoWindowFromItsTestBeginHoldsTheRead(t *testing.T) {
	const p = "body.Item.CreatedAt.S"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	rec, live := dotnetStamp(payloadPartRec.read), dotnetStamp(payloadPartF.read)
	lookUp := []Raw{RawOf(pulsarLookUp("mock-1", payloadPartLookUp, lookUpRequest), false)}
	w := payloadPartWindow(payloadPartRec, payloadPartF)
	noBegin := payloadPartWindow(payloadPartRec, payloadPartF)
	noBegin.F.Began = time.Time{}
	early := payloadPartRec
	early.began = payloadPartRec.read.Add(100 * time.Millisecond) // read before its test began
	sent := func(served bool) []Raw {
		m := pulsarLookUp("mock-9", payloadPartRec.read.Add(40*time.Millisecond), `{"commandType":"SEND","payload":{"type":"lead.created","at":"`+rec+`"}}`)
		return []Raw{RawOf(m, served)}
	}
	// An earlier call of the test that F served field by field (its
	// adapter gives its payload), whose field, in its request or in its
	// answer, drifted from the value's minute to F's: a place F saw move
	// with it, never a token found by chance, though the window from each
	// run's test begin holds the read. An answer's place stays a variable
	// as a request's does: a replay checks an answer's instant.
	for _, side := range []Side{Request, Response} {
		minute := []Earlier{{Mock: "mock-8", At: payloadPartRec.prev, Payload: payloadOf("mock-8", payloadPartRec.prev), Own: true, Seen: []Seen{
			{Side: side, Leaf: Leaf{Path: "body.minute", Noise: "body.minute", Type: String, Value: "48"}, Live: "49", Place: "body.minute"},
		}}}
		ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), payloadPartRec.call, minute, nil, nil, w)[0]
		if ln.Kind != KindVariable || ln.From.Mock != "mock-8" || ln.From.Side != side || ln.From.Whole {
			t.Fatalf("a served call's %s field that drifted with it: %s from %q %s whole=%v Read=%+v (%s); want the variable of mock-8's %s, a part", side, ln.Kind, ln.From.Mock, ln.From.Side, ln.From.Whole, ln.Read, ln.Why, side)
		}
	}
	for _, tc := range []struct {
		name string
		w    *Window
		raw  []Raw
		mock string
		held string
		// proven: record's full round ran under the proven flow
		// (provenswitch.Env).
		proven bool
	}{
		{"no begin known in full round", noBegin, lookUp, "mock-1", "y", false},
		{"read before the recording's test began", payloadPartWindow(early, payloadPartF), lookUp, "mock-1", "y", false},
		{"sent whole in a payload F served before the call", w, sent(true), "mock-9", rec, false},
		{"sent whole in a payload F had not served before the call", w, sent(false), "mock-9", rec, false},
		{"the same, under the proven flow", w, sent(false), "mock-9", rec, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv(provenswitch.Env, "1")
			}
			ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), payloadPartRec.call, nil, tc.raw, nil, tc.w)[0]
			if ln.Kind != KindVariable || ln.From.Mock != tc.mock || ln.From.Held != tc.held {
				t.Fatalf("%s: %s from %q held %q Read=%+v (%s); want the variable of %s holding %q", tc.name, ln.Kind, ln.From.Mock, ln.From.Held, ln.Read, ln.Why, tc.mock, tc.held)
			}
		})
	}
	// The consumer's At read in record's full round before its test began
	// (an event pushed before the run's clock reached the test's begin): no
	// window of the test holds the read, and the minute the pushed event
	// holds stays its place.
	t.Run("a consumer's stamp read before full round's test began", func(t *testing.T) {
		f := consumerF
		f.read = f.began.Add(-50 * time.Millisecond)
		f.prev, f.call = f.began.Add(6*time.Millisecond), f.began.Add(10*time.Millisecond)
		leaf := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{consumerAt: {Path: consumerAt, Noise: consumerAt, Type: String, Value: v}}}
		}
		rec, live := dotnetStamp(consumerRec.read), dotnetStamp(f.read)
		ln := ClassifyIn(clientTestCache(), []string{consumerAt}, leaf(live), leaf(rec), consumerRec.call, nil, []Raw{RawOf(leadEventPush(), true)}, nil, payloadPartWindow(consumerRec, f))[0]
		if ln.Kind != KindVariable || ln.From.Mock != "mock-22" || ln.From.Held != "52" {
			t.Fatalf("%q -> %q: %s from %q held %q Read=%+v (%s); want the variable of mock-22 holding the minute", rec, live, ln.Kind, ln.From.Mock, ln.From.Held, ln.Read, ln.Why)
		}
	})
}
