package http

import (
	"io"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
)

// endsJudged is what recordV2 decides of a call whose response stream ended
// before the response did, or got no byte of an answer, from the session's
// marks alone.
type endsJudged struct {
	leftOut      string // partAnswerLeftOut's reason; "" where it keeps the exchange
	part, noneOf models.FaultKind
}

func judge(s *supervisor.Session) endsJudged {
	var j endsJudged
	if reason, ok := partAnswerLeftOut(s); ok {
		j.leftOut = reason
	}
	if f := partAnswerFault(s); f != nil {
		j.part = f.Kind
	}
	if f := noAnswerFault(s, io.EOF, 1); f != nil {
		j.noneOf = f.Kind
	}
	return j
}

// The client's own end judges a call where the relay says the client stopped
// taking the stream (Session.EndedWhereClientStopped), or that the client's
// end was in before the upstream's reached it (Session.ClientEndedBeforeDest),
// or the client's end came first, whichever end the session marked first and whatever the
// upstream's end was (read before the stop or after it began): an end that
// records no fault leaves a part answer out with its reason and records no
// fault; one that may be a fault records the app's stop (stopped_reading), or
// with no answer, its giving up (timeout). An app's end read before the stop
// began is its own, though the upstream's end, read first, came after it.
func TestRecordV2Faults_TheClientsOwnEndJudgesWhicheverEndIsMarkedFirst(t *testing.T) {
	for _, tc := range []struct {
		why  supervisor.NoFaultEnd
		want endsJudged
	}{
		{supervisor.EndAtStop, endsJudged{leftOut: supervisor.LeftOutPartwayAtStop}},
		{supervisor.EndAtExit, endsJudged{leftOut: supervisor.LeftOutPartwayAtExit}},
		{supervisor.EndUnjudged, endsJudged{leftOut: supervisor.LeftOutPartwayUnjudged}},
		{supervisor.EndMayBeFault, endsJudged{part: models.FaultStoppedReading, noneOf: models.FaultTimeout}},
	} {
		for _, dest := range []supervisor.NoFaultEnd{supervisor.EndMayBeFault, supervisor.EndAtStop} {
			for _, clientFirst := range []bool{true, false} {
				for _, word := range []struct{ stopped, before bool }{{true, false}, {false, true}, {true, true}, {false, false}} {
					if !clientFirst && !word.stopped && !word.before {
						continue // the upstream's own end, below
					}
					for _, reset := range []bool{false, true} {
						s := &supervisor.Session{}
						if clientFirst {
							s.MarkPeerEndedBy(fakeconn.FromClient, false, tc.why)
							s.MarkPeerEndedBy(fakeconn.FromDest, reset, dest)
						} else {
							s.MarkPeerEndedBy(fakeconn.FromDest, reset, dest)
							s.MarkPeerEndedBy(fakeconn.FromClient, false, tc.why)
						}
						if word.stopped {
							s.MarkEndedWhereClientStopped()
						}
						if word.before {
							s.MarkClientEndedBeforeDest()
						}
						if got := judge(s); got != tc.want {
							t.Errorf("the client's end %d, the upstream's %d, the client's first: %v, the relay's word: %v, the client's end in before the upstream's reached it: %v, the upstream's reset: %v: judged %+v, want %+v",
								tc.why, dest, clientFirst, word.stopped, word.before, reset, got, tc.want)
						}
					}
				}
			}
		}
	}
}

// Where the client's end does not judge the call, the end the relay read
// does, as before: the upstream's close or reset came first with no word from
// the relay that the client stopped or had ended its side before (its own
// fault, whatever the client's end after it), and a client's end the relay
// never read leaves the call to the upstream's end it did read, the relay's
// word or not.
func TestRecordV2Faults_TheUpstreamsEndJudgesWhereTheClientsDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(s *supervisor.Session)
		want endsJudged
	}{
		{"the upstream's close first, then the client's exit", func(s *supervisor.Session) {
			s.MarkPeerEndedBy(fakeconn.FromDest, false, supervisor.EndMayBeFault)
			s.MarkPeerEndedBy(fakeconn.FromClient, false, supervisor.EndAtExit)
		}, endsJudged{part: models.FaultTruncate, noneOf: models.FaultEOF}},
		{"the upstream's reset first, then the client's exit", func(s *supervisor.Session) {
			s.MarkPeerEndedBy(fakeconn.FromDest, true, supervisor.EndMayBeFault)
			s.MarkPeerEndedBy(fakeconn.FromClient, false, supervisor.EndAtExit)
		}, endsJudged{part: models.FaultReset, noneOf: models.FaultReset}},
		{"the relay's word, the upstream's close read, the client's end never", func(s *supervisor.Session) {
			s.MarkPeerEndedBy(fakeconn.FromDest, false, supervisor.EndMayBeFault)
			s.MarkEndedWhereClientStopped()
		}, endsJudged{part: models.FaultStoppedReading, noneOf: models.FaultEOF}},
		{"the relay's word, the upstream's close read at the stop, the client's end never", func(s *supervisor.Session) {
			s.MarkPeerEndedBy(fakeconn.FromDest, false, supervisor.EndAtStop)
			s.MarkEndedWhereClientStopped()
		}, endsJudged{}},
		{"the client's end in before the upstream's reached it, the upstream's close read, the client's end never", func(s *supervisor.Session) {
			s.MarkPeerEndedBy(fakeconn.FromDest, false, supervisor.EndMayBeFault)
			s.MarkClientEndedBeforeDest()
		}, endsJudged{part: models.FaultTruncate, noneOf: models.FaultEOF}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &supervisor.Session{}
			tc.mark(s)
			if got := judge(s); got != tc.want {
				t.Errorf("judged %+v, want %+v", got, tc.want)
			}
		})
	}
}
