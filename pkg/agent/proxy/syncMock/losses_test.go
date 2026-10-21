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

// Past maxOrphanWhys reasons a span is still recorded, and charged, without
// its why.
func TestASpanPastTheBoundOnReasonsIsChargedWithoutIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	for i := 0; i < maxOrphanWhys; i++ {
		m.RecordOrphanWindowWhy(at(i*10), at(i*10+1), fmt.Sprintf("cause %d", i))
	}
	past := at(maxOrphanWhys * 10)
	m.RecordOrphanWindowWhy(past, past, "one too many")
	got := m.TestCaseLosses("test-1", past, past)
	if want := (models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1}}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("charged %+v, want %+v", got, want)
	}
	if got := m.TestCaseLosses("test-1", at(0), at(0)); len(got) != 1 || got[0].What != "cause 0" {
		t.Fatalf("a span within the bound is charged %+v, want its why", got)
	}
}
