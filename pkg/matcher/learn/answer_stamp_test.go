package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A DynamoDB UpdateItem in a client test carries, deep in a nested list and
// map (:a.L[0].M.At.S), the time the application read its clock to handle a
// Pulsar push: the lead.created event of the lead the test had just put. The
// same second's words stand in earlier places of the test (the lead's
// CreatedAt in its PutItem, the event's "at" in the SQS message and in the
// push), so the value is a read only if its own call's window holds it, in
// both runs. The window starts at the previous answer, the push. Recorded
// with the push stamped before the relay wrote it to the application, the
// window holds the read, and the value is learned as a plain read of the
// clock in its own call's window. Recorded with the push stamped once the
// relay's write returned (on a starved host 0.77 ms after the application
// had read the push and its clock), the window starts after the read, and
// the value is no read of its own window: at best a read since its test
// began, and refused where the earlier places' words cannot be bound.
func TestAPushHandlersClockReadIsLearnedWhereThePushIsStampedBeforeTheAppActedOnIt(t *testing.T) {
	const at = "body.ExpressionAttributeValues.:a.L.0.M.At.S"
	day := func(h, m, s, ns int) time.Time { return time.Date(2026, 11, 3, h, m, s, ns, time.UTC) }
	str := func(p, v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.0000000Z") }

	// The recording: the test began, the lead was put (its CreatedAt read
	// first), its event sent to SQS and to Pulsar, Pulsar pushed it back to
	// the application's consumer, which read its clock for the activity's
	// At and sent the UpdateItem.
	recBegan := day(2, 19, 7, 538224000)
	recCreated := day(2, 19, 8, 43512900)
	recPut, recSQS := day(2, 19, 8, 44297755), day(2, 19, 8, 50333108)
	recRead := day(2, 19, 8, 62186100)
	recCall := day(2, 19, 8, 63259394)
	// The full round, three minutes later.
	fBegan := day(2, 22, 1, 826810000)
	fCreated := day(2, 22, 1, 827123400)
	fPush := day(2, 22, 1, 828108000)
	fRead := day(2, 22, 1, 828513500)
	fCall := day(2, 22, 1, 829298000)

	learn := func(recPush time.Time) Learned {
		earlier := func(name string, t time.Time, side Side, p string, rec, live time.Time) Earlier {
			m := RawOf(&models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: t, ResTimestampMock: t.Add(time.Millisecond)}}, true)
			return Earlier{Mock: name, At: t, Own: true, Payload: m.ServedAs(1), Seen: []Seen{
				{Side: side, Leaf: str(p, iso(rec)), Live: iso(live)},
			}}
		}
		ers := []Earlier{
			earlier("mock-38", recPut, Request, "body.Item.CreatedAt.S", recCreated, fCreated),
			earlier("mock-39", recSQS, Request, "body.MessageBody.at", recCreated, fCreated),
			earlier("mock-41", recPush, Response, "payload.at", recCreated, fCreated),
		}
		w := &Window{
			Rec: Span{From: recPush, To: recCall, Began: recBegan},
			F:   Span{From: fPush, To: fCall, Began: fBegan},
		}
		shape := func(t time.Time) Shape { return Shape{Leaves: map[string]Leaf{at: str(at, iso(t))}} }
		for _, ln := range ClassifyFollowing(clientTestCache(), []string{at}, shape(fRead), shape(recRead), recCall, ers, nil, nil, w, nil) {
			if ln.Recorded.Path == at {
				return ln
			}
		}
		t.Fatalf("%s: not classified", at)
		return Learned{}
	}

	// Stamped before the relay's write: no later than the application could
	// read the push.
	ln := learn(day(2, 19, 8, 62100000))
	if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() || ln.From.Mock != "" {
		t.Fatalf("push stamped before the app read it: %s Read=%+v from %+v (%s); want a plain read of the clock, no variable", ln.Kind, ln.Read, ln.From, ln.Why)
	}

	// Stamped once the write returned, after the application had read the
	// push and its clock.
	ln = learn(day(2, 19, 8, 62959437))
	if o, ok := ln.Clock(); ok && o.Plain() && o.Since == "" {
		t.Fatalf("push stamped after the app read it: a plain read of its own window (%+v); the window starts after the read and cannot hold it", ln.Read)
	}
	if ln.From.Mock != "" || ln.Kind == KindNoise {
		t.Fatalf("push stamped after the app read it: %s from %+v (%s); want no variable and no noise", ln.Kind, ln.From, ln.Why)
	}
}
