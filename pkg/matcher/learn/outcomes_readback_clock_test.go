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
