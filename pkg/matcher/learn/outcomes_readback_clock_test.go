package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The shape of api-server's proxy app registration under keploy mock: the
// test inserts a cluster whose created_at, updated_at and first heartbeat
// are one read of the clock in Unix seconds (mock-1512, learned as that
// read), finds it back (mock-1516, whose answer round F served with this
// run's second at all three places), then creates an app whose createdAt
// is its own read of the clock (time.Now().Unix() in the CreateApp
// resolver), some 30 ms later, and writes it into an app_config_audits
// insert (mock-1533). Both reads fall in one second in both runs.
type readBackShape struct {
	r0, f0         time.Time // each run's test begin
	sendR, sendF   time.Time // the cluster's read of the clock
	findR, findF   time.Time // the find that reads it back
	readR, readF   time.Time // the app's own read of the clock
	callR, callF   time.Time // the audit insert
	clock          bool      // the cluster insert is its call's read of the clock
	oneExchange    bool      // the insert's own answer returns the record (findAndModify)
	secs, answered func(time.Time) string
}

func readBackRuns() readBackShape {
	sR := oAt.Truncate(time.Second).Add(3 * time.Second)
	sF := sR.Add(62 * time.Second)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	return readBackShape{
		r0: sR.Add(800 * time.Millisecond), f0: sF.Add(200 * time.Millisecond),
		sendR: sR.Add(892 * time.Millisecond), sendF: sF.Add(300 * time.Millisecond),
		findR: sR.Add(907 * time.Millisecond), findF: sF.Add(320 * time.Millisecond),
		readR: sR.Add(920 * time.Millisecond), readF: sF.Add(331 * time.Millisecond),
		callR: sR.Add(939 * time.Millisecond), callF: sF.Add(345 * time.Millisecond),
		clock: true, secs: secs, answered: secs,
	}
}

// input is the audit insert's createdAt with the cluster's insert and its
// find before it, in a client test's window of the call alone (the read was
// made before it) that began at each run's test begin.
func (s readBackShape) input() Input {
	const p = "body.docs.msgs.0.app.createdAt.$numberLong"
	rec, live := s.secs(s.readR), s.secs(s.readF)
	insert := iSent("mock-1512", s.sendR, "body.docs.msgs.0.created_at.$numberLong", String, s.secs(s.sendR), s.secs(s.sendF))
	insert.Seen[0].Clock = s.clock
	ansR, ansF := s.answered(s.sendR), s.answered(s.sendF)
	find := Earlier{Mock: "mock-1516", At: s.findR, Own: true, Payload: oPayload("mock-1516", s.findR)}
	for _, f := range []string{"created_at", "heartbeats.0.timestamp", "updated_at"} {
		find.Seen = append(find.Seen, Seen{Side: Response, Leaf: oText("resp.0.0.cursor.firstBatch.0."+f+".$numberLong", ansR), Live: ansF})
	}
	earlier := []Earlier{insert, find}
	if s.oneExchange {
		insert.Seen = append(insert.Seen, find.Seen...)
		earlier = []Earlier{insert}
	}
	w := iWin(s.callR, s.callF, 4*time.Millisecond)
	w.Rec.Began, w.F.Began = s.r0, s.f0
	in := iCall(p, String, rec, live, s.callR, s.callF, w, earlier, nil)
	in.Drifts = oClientDrifts()
	return in
}

func (s readBackShape) decide(t *testing.T) Decision {
	t.Helper()
	return oWord(t, Decide(s.input()), s.secs(s.readR), s.secs(s.readF))
}

// The app's createdAt beside the cluster's: the find's answer holds the
// second only because it reads back what the cluster's insert wrote as its
// own call's read of the clock, which round F answered with its own run's
// second. Two reads of one second share it only as the runs' timing falls,
// as a send that is its call's read does (6), and a replay answers the find
// with its own run's read, never the recorded stamp: the value is checked
// as a read of the clock its test made (since its begin, and before its
// call), never bound to the find. Bound there, a replay whose two reads fall
// a second apart goes red; with three places of one second in the answer,
// the Mongo side named none, and the test was refused as keploy's limit.
func TestOutcomeASecondAnAnswerReadsBackFromAClockReadIsAClock(t *testing.T) {
	d := readBackRuns().decide(t)
	iClock(t, "a second the find reads back from the cluster's read of the clock", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin && d.Clock.Since != "" {
		t.Errorf("%+v; want a read in its call's window or since its test began", *d.Clock)
	}
}

// One exchange that stamps the record with now and answers with the record
// it wrote (a findAndModify returning the new document): its answer, at the
// exchange's own instant, reads back its request's read of the clock as a
// later find does, and the app's own read of that second is checked as a
// read, never bound to the answer.
func TestOutcomeASecondTheStampingExchangeAnswersBackIsAClock(t *testing.T) {
	s := readBackRuns()
	s.oneExchange = true
	iClock(t, "a second the exchange that stamped it answers back", s.decide(t))
}

// The controls keep the answer as the value's source, as before. A send
// that is no read of the clock (a value the cluster's insert carried from
// elsewhere): the answer reads back that value, the dependency's. A second
// the find answered before the insert sent it: no read-back of that read.
// A copy at exactly a millisecond: no coarse cut, which two reads do not
// share as the runs' timing falls.
func TestOutcomeAnAnswerThatReadsBackNoCoarseClockReadStaysTheSource(t *testing.T) {
	s := readBackRuns()
	s.clock = false
	iValue(t, "the insert's second is no read of the clock", s.decide(t), "mock-1516")
	s.oneExchange = true
	iValue(t, "the stamping exchange's second is no read of the clock", s.decide(t), "mock-1512")

	s = readBackRuns()
	s.findR, s.findF = s.sendR.Add(-time.Millisecond), s.sendF.Add(-time.Millisecond)
	iValue(t, "the find answered before the insert", s.decide(t), "mock-1516")

	s = readBackRuns()
	s.secs, s.answered = iEpochMs, iEpochMs
	s.readR, s.readF = s.sendR, s.sendF
	iValue(t, "a millisecond the find reads back", s.decide(t), "mock-1516")
}

// otherTestsAnswer is an answer of an earlier test (an OAuth code it made,
// recorded 20 s before the test began, given back with its expiry at now
// plus a minute in milliseconds), which round F gave the application before
// the call as recorded.
func (s readBackShape) otherTestsAnswer(expires time.Time) Raw {
	at := s.r0.Add(-20 * time.Second)
	return RawOf(&models.Mock{Name: "mock-1204", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://auth.local/oauth/code", Header: map[string]string{}},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"expires_at":"` + iMs(expires) + `"}`},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}, true).OfAnotherTest()
}

// Round F ran the test slower than the recording, as a loaded box does: the
// app read its second late in it and sent the audit insert in the next
// second, so the call's own window holds the read in the recording alone,
// and it is a read its test made since it began. The set holds an earlier
// test's answer whose time names neither run's second. The echo of what
// round F gave the application takes the value for a copy only by the
// test's own find, which reads back the cluster's read of the clock: no
// answer outside the test takes part, so the value is never refused as one
// standing in such an answer (as it was, naming none), and is checked as a
// read of the clock since its test began. An earlier test's answer that
// does hold the recorded second, in milliseconds, stays a source outside
// the test, refused naming it.
func TestOutcomeASecondAnAnswerReadsBackIsAClockBesideAnotherTestsAnswer(t *testing.T) {
	s := readBackRuns()
	s.readF = s.sendF.Truncate(time.Second).Add(880 * time.Millisecond)
	s.callF = s.sendF.Truncate(time.Second).Add(time.Second + 10*time.Millisecond)
	in := s.input()
	in.Raw = append(in.Raw, s.otherTestsAnswer(s.r0.Add(40*time.Second+28*time.Millisecond)))
	d := oWord(t, Decide(in), s.secs(s.readR), s.secs(s.readF))
	iClock(t, "a second the find reads back, round F's call in the next second, beside another test's answer", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("%+v; want a read since its test began", *d.Clock)
	}

	in = s.input()
	in.Raw = append(in.Raw, s.otherTestsAnswer(s.readR.Truncate(time.Second).Add(28*time.Millisecond)))
	oRefused(t, "another test's answer holding the recorded second", oWord(t, Decide(in), s.secs(s.readR), s.secs(s.readF)), RefusalAnotherScope, "mock-1204")
}
