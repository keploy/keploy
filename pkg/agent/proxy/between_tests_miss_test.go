package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy test's between-test rule (founder D1, D5: judged as keploy mock
// judges): a miss made after one test's misses were read and before the next
// test's window opened was made outside every test's window. The next test's
// window still carries it, in order, but its read marks it Background: the
// application's background work, charged to no test.

// timedMiss is a miss its parser said it made at at.
func timedMiss(summary string, at time.Time) models.ParserError {
	m := mockMiss(summary)
	m.MismatchReport.At = at
	return m
}

// Through the drain and the proxy's own stamping, as keploy test reads it: the
// first test's miss is its own, a timer's call made after that read is marked
// background in the second test's read, and the second test's own is not.
func TestUnderTheSwitchAMissMadeBetweenTwoTestsIsBackground(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 16)
	ctx := context.Background()
	routed := func() {
		t.Helper()
		if ok, why := p.flushOnEvents(ctx, &flushMarker{done: make(chan struct{})}); !ok {
			t.Fatalf("the drain did not route the miss: %s", why)
		}
	}
	send := func(summary string) {
		p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched,
			&models.MockMismatchReport{Protocol: "HTTP", ActualSummary: summary}))
		routed()
	}

	p.BeginTestErrorCapture()
	send("GET /t1")
	got, err := p.GetMockErrors(ctx)
	if err != nil || len(got) != 1 || got[0].Background {
		t.Fatalf("test 1 read %+v (%v); want its own miss, not background", got, err)
	}

	send("POST /timer") // no window is open: the application's timer
	p.ContinueTestErrorCapture()
	send("GET /t2")
	got, err = p.GetMockErrors(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("test 2 read %+v (%v); want the timer's call and its own", got, err)
	}
	if got[0].ActualSummary != "POST /timer" || !got[0].Background || got[0].CountsAgainstTest() {
		t.Fatalf("the timer's call is %+v; want it background, charged to no test", got[0])
	}
	if got[1].ActualSummary != "GET /t2" || got[1].Background || !got[1].CountsAgainstTest() {
		t.Fatalf("test 2's own miss is %+v; want it its own", got[1])
	}
}

// Each boundary of the rule, on misses the gap holds when the next window
// opens: made before the read (filed late: the reading test's), at it or after
// it (background), of unknown time (the reading test's), with only the time it
// reached the proxy (read by it), of a call its parser found made in a test or
// as one more than recorded (that test's), and those that count against no
// test anyway (DNS, an answer: left as they are). With the switch off nothing
// is marked.
func TestTheBetweenTestsRuleBoundaries(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			v := ""
			if on {
				v = "1"
			}
			t.Setenv(provenswitch.Env, v)
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
			ctx := context.Background()

			p.BeginTestErrorCapture()
			if _, err := p.GetMockErrors(ctx); err != nil {
				t.Fatal(err)
			}
			readAt := p.windowReadAt
			if readAt.IsZero() {
				t.Fatal("the read did not note when it took the window")
			}
			proxyOnly := mockMiss("proxy time")
			proxyOnly.At = readAt.Add(time.Millisecond)
			inTest := timedMiss("in test", readAt.Add(time.Millisecond))
			inTest.MissKind = models.MissInTest
			extra := timedMiss("extra", readAt.Add(time.Millisecond))
			extra.MissKind = models.MissExtraCall
			outside := timedMiss("outside", readAt.Add(time.Millisecond))
			outside.MissKind = models.MissOutsideTest
			dns := timedMiss("A mongo.", readAt.Add(time.Millisecond))
			dns.MismatchReport.Protocol = "DNS"
			answer := timedMiss("insert events", readAt.Add(time.Millisecond))
			answer.MismatchReport.Answer = models.AnswerByKind
			for _, err := range []error{
				timedMiss("before", readAt.Add(-time.Millisecond)),
				timedMiss("at", readAt),
				timedMiss("after", readAt.Add(time.Millisecond)),
				mockMiss("unknown time"),
				proxyOnly, inTest, extra, outside, dns, answer,
			} {
				p.pendingMockErrors.addBounded(err, maxPendingMockErrors)
			}
			p.ContinueTestErrorCapture()
			got, err := p.GetMockErrors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{"at": on, "after": on, "proxy time": on, "outside": on}
			if len(got) != 10 {
				t.Fatalf("the read took %d call(s), want all 10 in order: %+v", len(got), got)
			}
			for _, c := range got {
				if c.Background != want[c.ActualSummary] {
					t.Errorf("%q: background %v, want %v", c.ActualSummary, c.Background, want[c.ActualSummary])
				}
			}
		})
	}
}

// A set's first test opens with no earlier read bounding it: what it carries
// is the set's (BeforeSet), never background, and a later carry measures from
// that test's own read, not the previous set's.
func TestASetsFirstTestForgetsThePreviousSetsRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	ctx := context.Background()

	p.BeginTestErrorCapture()
	if _, err := p.GetMockErrors(ctx); err != nil {
		t.Fatal(err)
	}
	previous := p.windowReadAt
	p.pendingMockErrors.addBounded(timedMiss("boot write", previous.Add(time.Millisecond)), maxPendingMockErrors)
	p.BeginTestErrorCapture() // the next set's first test
	if !p.windowReadAt.IsZero() {
		t.Fatalf("a set's first test kept the previous set's read %v", p.windowReadAt)
	}
	got, err := p.GetMockErrors(ctx)
	if err != nil || len(got) != 1 || !got[0].BeforeSet || got[0].Background {
		t.Fatalf("the set's first read %+v (%v); want the boot write the set's, not background", got, err)
	}
}

// A carry with no read before it in the set (keploy mock opens its first test
// so, carrying what its application did as it started) marks nothing: the
// agent's own rule judges those (scope_proven.go backgroundMiss).
func TestACarryWithNoReadBeforeItMarksNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.pendingMockErrors.addBounded(timedMiss("boot write", time.Now()), maxPendingMockErrors)
	p.ContinueTestErrorCapture()
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 1 || got[0].Background {
		t.Fatalf("the first carry's read %+v (%v); want the boot write unmarked", got, err)
	}
}

// Every read that takes a window notes when: one that closes it
// (EndTestErrorCapture too), and one that could not close it, whose window
// keeps what the drain files later: a miss made after that read is
// background when the next window carries it.
func TestEveryReadOfAWindowBoundsTheGapAfterIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	p.EndTestErrorCapture()
	if p.windowReadAt.IsZero() {
		t.Fatal("EndTestErrorCapture took the window and did not note when")
	}

	p = &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.errDrainDone = make(chan struct{}) // a drain that runs and never reaches the marker
	p.errDrainActive.Store(true)
	ctx, giveUp := context.WithCancel(context.Background())
	giveUp()
	p.BeginTestErrorCapture()
	if _, err := p.GetMockErrors(ctx); err == nil {
		t.Fatal("premise: the read could not close its window, and says so")
	}
	acc := p.activeTestErrors.Load()
	if acc == nil || p.windowReadAt.IsZero() {
		t.Fatalf("premise: the window stays (%v) and the read noted when (%v)", acc != nil, p.windowReadAt)
	}
	acc.addBounded(timedMiss("POST /timer", p.windowReadAt.Add(time.Millisecond)), maxPendingMockErrors)
	p.ContinueTestErrorCapture()
	p.errDrainActive.Store(false) // the next read closes the window normally
	got, _ := p.GetMockErrors(context.Background())
	if len(got) != 1 || !got[0].Background {
		t.Fatalf("the next read %+v; want the timer's call, made after the read, background", got)
	}
}

// The set's end reads with no test's window open (item b): under the switch a
// miss made after the set's last test was read is the application's
// background work (Background), and one made before that read and filed late
// is still the set's to charge. With the switch off both read unmarked, as
// before: the switch-off read is unchanged.
func TestTheSetsEndMarksAMissMadeAfterTheLastReadBackgroundUnderTheSwitchOnly(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			v := ""
			if on {
				v = "1"
			}
			t.Setenv(provenswitch.Env, v)
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
			ctx := context.Background()

			p.BeginTestErrorCapture()
			if _, err := p.GetMockErrors(ctx); err != nil {
				t.Fatal(err)
			}
			readAt := p.windowReadAt
			if readAt.IsZero() || p.activeTestErrors.Load() != nil {
				t.Fatalf("premise: the last test's read closed its window (%v) and noted when (%v)", p.activeTestErrors.Load() == nil, readAt)
			}
			p.pendingMockErrors.addBounded(timedMiss("late in the last test", readAt.Add(-time.Millisecond)), maxPendingMockErrors)
			p.pendingMockErrors.addBounded(timedMiss("POST /publish", readAt.Add(time.Millisecond)), maxPendingMockErrors)
			inTest := timedMiss("in test", readAt.Add(time.Millisecond))
			inTest.MissKind = models.MissInTest
			p.pendingMockErrors.addBounded(inTest, maxPendingMockErrors)

			got, err := p.GetMockErrors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 || got[0].ActualSummary != "late in the last test" || got[1].ActualSummary != "POST /publish" || got[2].ActualSummary != "in test" {
				t.Fatalf("the set's end read %+v; want the three calls, in order", got)
			}
			want := map[string]bool{"POST /publish": on}
			for _, c := range got {
				if c.Background != want[c.ActualSummary] || c.BeforeSet {
					t.Errorf("%q: background %v before-set %v, want background %v and not before the set", c.ActualSummary, c.Background, c.BeforeSet, want[c.ActualSummary])
				}
			}
		})
	}
}

// A call of a series judging marked as the application's background work
// that no recording of the series answered (its parser's report says
// Background) is background wherever it was made: in the open test's
// window it is charged to no test, and before the set's first test it is
// no call of the set's startup (never BeforeSet), failing nothing. An
// unmarked miss beside each keeps its rule. With the switch off the mark is
// not read: no parser reports a series then, and the read is as it was.
func TestAMarkedSeriesCallNothingAnswersIsBackgroundWhereverItWasMade(t *testing.T) {
	series := func(summary, kind string) models.ParserError {
		m := mockMiss(summary)
		m.MismatchReport.Background = true
		m.MissKind = kind
		return m
	}
	read := func(t *testing.T, p *Proxy) map[string]models.UnmatchedCall {
		t.Helper()
		got, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]models.UnmatchedCall{}
		for _, c := range got {
			out[c.ActualSummary] = c
		}
		if len(out) != len(got) {
			t.Fatalf("the read took %+v; want each call once", got)
		}
		return out
	}

	t.Run("switch on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
		p.pendingMockErrors.addBounded(series("claim at boot", ""), maxPendingMockErrors)
		p.pendingMockErrors.addBounded(mockMiss("insert on events"), maxPendingMockErrors)
		p.BeginTestErrorCapture()
		p.errChannel <- series("claim in t1", models.MissInTest)
		p.errChannel <- mockMiss("POST /t1")
		got := read(t, p)
		if len(got) != 4 {
			t.Fatalf("the first test read %+v; want all four calls", got)
		}
		for _, s := range []string{"claim at boot", "claim in t1"} {
			if c := got[s]; !c.Background || c.CountsAgainstTest() || c.BeforeSet {
				t.Errorf("%q reads %+v; want it background, charged to no test, never the set's startup", s, c)
			}
		}
		if c := got["insert on events"]; c.Background || !c.BeforeSet {
			t.Errorf("the startup's own miss reads %+v; want it the set's (BeforeSet)", c)
		}
		if c := got["POST /t1"]; c.Background || !c.CountsAgainstTest() || c.BeforeSet {
			t.Errorf("the test's own miss reads %+v; want it its own", c)
		}
	})
	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
		p.BeginTestErrorCapture()
		p.errChannel <- series("claim in t1", "")
		got := read(t, p)
		if c, ok := got["claim in t1"]; !ok || c.Background || !c.CountsAgainstTest() {
			t.Errorf("without the switch the claim reads %+v (%v); want the test's miss, as it was", c, ok)
		}
	})
}
