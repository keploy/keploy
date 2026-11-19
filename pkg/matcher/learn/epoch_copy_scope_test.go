package learn

import (
	"strconv"
	"testing"
	"time"
)

// eaPath is where api-server's eventsV2 insert carries the app's creation
// second (an int64 epoch), which the test's own createApp audit insert sent
// at cPath a moment before, on the same connection.
const eaPath = "body.docs.msgs.0.eventDetails.app.createdAt.$numberLong"

// eaCase is one shape of that call.
type eaCase struct {
	// back is how long before the call, in each run, the second was read.
	back time.Duration
	// own: the test's audit insert (mock-1126) sent the second before the
	// call in both runs; clock: its own learn step learned it a plain read
	// of the clock.
	own, clock bool
	// outside: an earlier test's answer (mock-7, in test-1), given before
	// the call, holds the second in both runs by chance, as round F's tests
	// fell in it too.
	outside bool
	// client: one of keploy mock's client tests (T2).
	client bool
}

// input is the eventsV2 insert at eaPath, recorded at cRAt and seen in
// round F at cFAt in a window of 60 ms, holding the second c.back before
// each, in mode timeForm (Input.TimeForm).
func (c eaCase) input(timeForm bool) Input {
	r := strconv.FormatInt(cRAt.Add(-c.back).Unix(), 10)
	f := strconv.FormatInt(cFAt.Add(-c.back).Unix(), 10)
	var earlier []Earlier
	if c.own {
		a := iSent("mock-1126", cRAt.Add(-450*time.Microsecond), cPath, String, r, f)
		a.Seen[0].Clock = c.clock
		earlier = append(earlier, a)
	}
	var raw []Raw
	if c.outside {
		raw = append(raw, cAns(r, f))
	}
	in := iCall(eaPath, String, r, f, cRAt, cFAt, iWin(cRAt, cFAt, 60*time.Millisecond), earlier, raw)
	if c.client {
		in.Drifts = oClientDrifts()
	}
	in.Scope = func(m string) Scope {
		if m == "mock-7" {
			return ScopeTest("test-1")
		}
		return ScopeTest(in.Test)
	}
	in.TimeForm = timeForm
	return in
}

// In keploy mock's client tests (T2), a second the test's own call sent
// before the call in both runs, which the call's window does not hold, is
// that call's value copied (a coarse copy), even where an earlier test's
// answer holds the same second: tests that run fast fall in one second in
// both runs, so its answer holds it by chance. Before, that answer refused
// it as another-scope and set the test aside (api-server's
// TestGenerateTestSuitesOnClient: its eventsV2 insert's
// eventDetails.app.createdAt, the second its audit insert sent). The bind
// is checked at replay: a replay whose eventsV2 insert carries another
// second than its audit insert misses. keploy test's server tests (T1)
// refuse it as before, and so does every other shape below.
func TestAnInScopeCoarseCopyOutranksAnotherTestsAnswerOfTheSameSecond(t *testing.T) {
	s, ms := time.Second, time.Millisecond
	for _, tf := range []bool{false, true} {
		mode := map[bool]string{false: "strict times", true: "the time default"}[tf]
		for _, c := range []struct {
			what string
			c    eaCase
			want string // "bound", "clock" or a refusal kind
		}{
			{"FALSE RED: T2, the own audit insert and another test's answer hold the second", eaCase{back: s, own: true, outside: true, client: true}, "bound"},
			{"T2, the own audit insert alone", eaCase{back: s, own: true, client: true}, "bound"},
			{"T1, the own audit insert alone", eaCase{back: s, own: true}, "bound"},
			// T1 is unchanged.
			{"T1, the own audit insert and another test's answer", eaCase{back: s, own: true, outside: true}, string(RefusalAnotherScope)},
			// No send of the scope: the other test's answer is all that
			// holds it.
			{"T2, another test's answer alone", eaCase{back: s, outside: true, client: true}, string(RefusalAnotherScope)},
			// A send that is itself its call's plain read of the clock is
			// no copy (two reads of one coarse cut): the answer refuses it.
			{"T2, the own send is its call's read of the clock", eaCase{back: s, own: true, clock: true, outside: true, client: true}, string(RefusalAnotherScope)},
			// The call's own window holds the second: a read of the clock.
			{"T2, the call's window holds the second", eaCase{back: ms, own: true, outside: true, client: true}, "clock"},
			{"T1, the call's window holds the second", eaCase{back: ms, own: true, outside: true}, "clock"},
		} {
			ds := Decide(c.c.input(tf))
			switch c.want {
			case "bound":
				if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-1126" {
					t.Errorf("%s (%s): %s; want bound to mock-1126, the test's own audit insert", c.what, mode, oDescribe(ds))
				}
			case "clock":
				if len(ds) != 1 || ds[0].Outcome != OutcomeClock {
					t.Errorf("%s (%s): %s; want a read of the clock", c.what, mode, oDescribe(ds))
				}
			default:
				if len(ds) != 1 || ds[0].Refusal == nil || string(ds[0].Refusal.Kind) != c.want {
					t.Errorf("%s (%s): %s; want refused %s", c.what, mode, oDescribe(ds), c.want)
				}
			}
		}
	}
}
