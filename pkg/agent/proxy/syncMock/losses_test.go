package manager

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// at is a fixed instant plus ms milliseconds: far from now, so a span or a
// test case of a test here overlaps nothing the clock gives.
func at(ms int) time.Time {
	return time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
}

// Each orphan source's test case is charged with an exchange keploy could not
// record, named by why its span was recorded: the test cases routes/record.go
// leaves out with the proven switch off are the ones charged, and no other.
func TestATestCaseOverAnOrphanSpanIsChargedWithWhyItWasRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	// A parser's exchange left out (ReportLeftOut: MySQL's response it cannot
	// frame, the incomplete flag, an HTTP response that does not decode).
	m.RecordOrphanWindowWhy(at(10), at(20), "MySQL: mysql exchange not recorded")
	// A span recorded without why (a mongo/v2 resync hole, a Kafka range lost,
	// a mock enterprise's OnDrop reports).
	m.RecordOrphanWindow(at(100), at(110))
	// A connection keploy stopped recording, open until it idles.
	closeStopped := m.OpenOrphanWindowWhy(at(200), "mysql: what a connection carried once keploy stopped recording it")
	closeStopped()
	closeStopped() // idempotent

	for _, c := range []struct {
		name       string
		start, end time.Time
		want       models.CaptureLosses
	}{
		{"over the exchange left out", at(15), at(16),
			models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: mysql exchange not recorded"}}},
		{"over a span with no why", at(105), at(106),
			models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1}}},
		{"over the stopped connection's span", at(150), at(205),
			models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "mysql: what a connection carried once keploy stopped recording it"}}},
		{"over two named spans", at(19), at(201),
			models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 3, What: "MySQL: mysql exchange not recorded; mysql: what a connection carried once keploy stopped recording it"}}},
		{"between the spans", at(30), at(90), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := m.TestCaseLosses("test-1", c.start, c.end)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("charged %+v, want %+v", got, c.want)
			}
			if left, _ := m.WasMockOrphanedInWindow(c.start, c.end); left != (len(c.want) > 0) {
				t.Fatalf("left out with the switch off: %v; charged under it: %+v", left, got)
			}
		})
	}
}

// Memory pressure over a test case's window, and a mock of its own dropped on
// its way to keploy record, charge it with mocks let go, each said.
func TestATestCaseWhoseMocksWereLetGoIsChargedWithIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	before := time.Now()
	m.SetMemoryPressure(true)
	during := time.Now()
	m.SetMemoryPressure(false)
	m.recordDroppedTC("test-7")

	if got, want := m.TestCaseLosses("test-1", before, during), (models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: MockDroppedUnderPressure}}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("over the pressure span: charged %+v, want %+v", got, want)
	}
	if got, want := m.TestCaseLosses("test-7", at(0), at(1)), (models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: MockDroppedOnItsWay}}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("its own mock dropped: charged %+v, want %+v", got, want)
	}
	if got, want := m.TestCaseLosses("test-7", before, during), (models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 2, What: MockDroppedUnderPressure + "; " + MockDroppedOnItsWay}}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("both: charged %+v, want %+v", got, want)
	}
	// Another test case's drop is not this one's.
	if got := m.TestCaseLosses("test-8", at(0), at(1)); len(got) != 0 {
		t.Fatalf("test-8 charged %+v for test-7's dropped mock", got)
	}
}

// With the switch off why a span was recorded is not kept: the span is, and
// leaves out what it did before.
func TestWithTheSwitchOffAnOrphanSpanKeepsNoWhy(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	m := New(zap.NewNop())
	m.RecordOrphanWindowWhy(at(10), at(20), "MySQL: mysql exchange not recorded")
	m.OpenOrphanWindowWhy(at(30), "mysql: stopped")()
	if n := len(m.orphanWhys.all()); n != 0 {
		t.Fatalf("%d reasons kept with the switch off", n)
	}
	for _, w := range [][2]time.Time{{at(15), at(15)}, {at(30), at(30)}} {
		if left, _ := m.WasMockOrphanedInWindow(w[0], w[1]); !left {
			t.Fatalf("a test case over %v is no longer left out with the switch off", w)
		}
	}
	if recorded, _, _ := m.OrphanRangeCount(); recorded != 2 {
		t.Fatalf("%d orphan spans recorded, want 2", recorded)
	}
}

// Past maxOrphanWhys reasons a span is still recorded, and charged, its why
// told as one past the bound (WhyPastBound), not dropped unsaid.
func TestASpanPastTheBoundOnReasonsIsChargedAsOnePastIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	for i := 0; i < maxOrphanWhys; i++ {
		m.RecordOrphanWindowWhy(at(i*10), at(i*10+1), fmt.Sprintf("cause %d", i))
	}
	past := at(maxOrphanWhys * 10)
	m.RecordOrphanWindowWhy(past, past, "one too many")
	m.OpenOrphanWindowWhy(past.Add(time.Millisecond), "two too many")()
	got := m.TestCaseLosses("test-1", past, past)
	if want := (models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: WhyPastBound}}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("charged %+v, want %+v", got, want)
	}
	if got := m.TestCaseLosses("test-1", at(0), at(0)); len(got) != 1 || got[0].What != "cause 0" {
		t.Fatalf("a span within the bound is charged %+v, want its why", got)
	}
	// Both its own why and one past the bound: each named, the bound's last.
	got = m.TestCaseLosses("test-1", at((maxOrphanWhys-1)*10), past)
	if len(got) != 1 || got[0].What != fmt.Sprintf("cause %d; %s", maxOrphanWhys-1, WhyPastBound) {
		t.Fatalf("over a span within the bound and one past it: charged %+v", got)
	}
}

// Past their cap the spans are joined over gaps (Spans). A test case checked
// after a join that overlaps a span only where it was joined over a gap is
// charged, as the switch-off check leaves it out, but told that it may have
// run in the gap, beside what the span stands for (JoinedNote): what it is
// told it lost stays true. One over a span as it was recorded is told no such
// thing, whatever was joined near it.
func TestATestCaseOverOnlyAJoinedGapIsToldItMayHaveRunBesideTheSpan(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	base := at(0)
	us := func(n int) time.Time { return base.Add(time.Duration(n) * time.Microsecond) }
	// Two spans 500us apart, a test case in the gap between them, and one
	// over the first.
	m.RecordOrphanWindowWhy(us(0), us(100), "X: real")
	m.RecordOrphanWindowWhy(us(600), us(700), "X: real")
	gapStart, gapEnd := us(200), us(300)
	if got := m.TestCaseLosses("", gapStart, gapEnd); len(got) != 0 {
		t.Fatalf("before any join the gap test case is charged %+v", got)
	}
	// Spans far apart past the cap: the nearest gap, the one above, joins.
	for i := 0; i < maxPressureRanges+10; i++ {
		s := base.Add(time.Hour + time.Duration(i)*time.Second)
		m.RecordOrphanWindowWhy(s, s.Add(time.Microsecond), "X: real")
	}
	if left, _ := m.WasMockOrphanedInWindow(gapStart, gapEnd); !left {
		t.Fatal("precondition: the gap was not joined")
	}
	got := m.TestCaseLosses("", gapStart, gapEnd)
	want := models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "X: real " + JoinedNote}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the gap test case is charged %+v, want %+v", got, want)
	}
	if got := m.TestCaseLosses("", us(50), us(60)); len(got) != 1 || got[0].What != "X: real "+JoinedNote {
		// Its span was joined into the one over the gap: it overlaps that
		// one alone, which cannot tell the two apart.
		t.Fatalf("a test case over the first span, joined since: charged %+v", got)
	}
	// One over a span the join left alone is told nothing of joins.
	late := base.Add(time.Hour + time.Duration(maxPressureRanges+9)*time.Second)
	if got := m.TestCaseLosses("", late, late); len(got) != 1 || got[0].What != "X: real" {
		t.Fatalf("a test case over a span as it was recorded: charged %+v", got)
	}
	// A span with no why, joined: the note alone.
	n := New(zap.NewNop())
	n.RecordOrphanWindow(us(0), us(100))
	n.RecordOrphanWindow(us(600), us(700))
	for i := 0; i < maxPressureRanges+10; i++ {
		s := base.Add(time.Hour + time.Duration(i)*time.Second)
		n.RecordOrphanWindow(s, s.Add(time.Microsecond))
	}
	if got := n.TestCaseLosses("", gapStart, gapEnd); len(got) != 1 || got[0].What != JoinedNote {
		t.Fatalf("the gap test case over a joined span with no why: charged %+v", got)
	}
}

// A mock dropped on its way to keploy record that SpanDroppedMock is given
// (the OSS outChan path with no owner and no hook, or enterprise's OnDrop)
// charges the test cases recorded over it with mocks let go, its kind named,
// and nothing with the switch off.
func TestADroppedMocksSpanNamesItsKind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	m.SpanDroppedMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(10), ResTimestampMock: at(12)}})
	m.SpanDroppedMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: at(20), ResTimestampMock: at(22)}})
	m.SpanDroppedMock(&models.Mock{Kind: models.HTTP}) // no request time: no window
	got := m.TestCaseLosses("", at(11), at(21))
	want := models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 2, What: "MySQL: " + MockDroppedWhileItRan + "; " + MockDroppedWhileItRan}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("charged %+v, want %+v", got, want)
	}
	if ok, _ := m.WasMockOrphanedInWindow(at(11), at(21)); ok {
		t.Fatal("a dropped mock's span is an orphan span")
	}
	t.Setenv(provenswitch.Env, "")
	off := New(zap.NewNop())
	off.SpanDroppedMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(10), ResTimestampMock: at(12)}})
	if got := off.TestCaseLosses("", at(11), at(21)); len(got) != 0 {
		t.Fatalf("with the switch off a dropped mock's span charged %+v", got)
	}
}
