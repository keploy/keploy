package mock

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	rec "go.keploy.io/server/v3/pkg/service/record"
)

// case_losses.go is, under the proven switch, what keploy mock record's
// recording of the app's incoming requests as test cases lost.
//
// The agent leaves out of its incoming stream a test case whose window
// overlaps a span keploy could not record as a mock (an exchange a parser left
// out, a hole in a connection's capture, a connection it stopped recording,
// mocks it let go), with the switch off, and only its summary's
// tcs_suppressed_total says so. Under the switch keploy mock record asks the
// agent for such a test case with what it lost (models.IncomingOptions.ChargesLosses,
// incomingOptions), as keploy record does: the test case is stored, and its
// loss is the evidence of the test it was recorded in (the window that holds
// its request), which the judging that follows the record decides
// (chargeWindows). What no test holds (a test case outside every test, the
// test cases the agent left out unnamed, what its stream could not tell) is
// the run's (TestsReceipt.TestCasesLost). A record nothing judges fails on
// any of it, the recording not kept, as it does on any other capture loss
// under the switch (record.go).

// incomingOptions is what keploy mock record asks of the agent's incoming
// stream: its filters, and, under the proven switch with an agent client that
// counts what it could not capture, that the agent stream a test case over a
// span keploy could not record with that loss (ChargesLosses), in place of
// leaving it out unsaid. With the switch off it is the filters alone, as
// before.
func (m *mockService) incomingOptions() models.IncomingOptions {
	opts := models.IncomingOptions{Filters: m.config.Record.Filters}
	if _, counts := m.instrumentation.(CaptureLossReader); counts && provenswitch.On() {
		opts.ChargesLosses = true
	}
	return opts
}

// caseLosses is, under the proven switch, what each test case keploy mock
// record stored lost, and what the agent's incoming stream said of the
// recording it could not name. Safe for concurrent use; the zero value is
// ready.
type caseLosses struct {
	mu sync.Mutex
	// at is each stored test case's request time (caseTime), by name: the
	// test it was recorded in holds it.
	at map[string]time.Time
	// byWindow is the stored test cases by the window the agent streamed
	// each with (rec.StreamedWindowOf): a late loss names its test case by
	// it.
	byWindow map[[2]int64][]string
	// lost is what each stored test case lost, by name.
	lost map[string]models.CaptureLosses
	// leftOut and untold are what the agent's stream said it left out of the
	// recording unnamed (rec.LeftOutUnsaid), and what it could not tell
	// (rec.StreamsUntold): the run's.
	leftOut, untold string
}

// stored notes test case name stored, its request at at, streamed with window
// win and with lost, what the agent found its recording lost (its
// CaptureLost). Under the proven switch only.
func (c *caseLosses) stored(name string, win [2]int64, at time.Time, lost models.CaptureLosses) {
	if c == nil || name == "" || !provenswitch.On() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at, c.byWindow, c.lost = map[string]time.Time{}, map[[2]int64][]string{}, map[string]models.CaptureLosses{}
	}
	c.at[name] = at
	if win != ([2]int64{}) {
		c.byWindow[win] = append(c.byWindow[win], name)
	}
	if len(lost) > 0 {
		c.lost[name] = c.lost[name].PlusNaming(lost)
	}
}

// ended takes what the agent's incoming streams' end markers said (sum,
// AgentClient.TakeStreamEnds), once the stream drained: each late loss
// charges the stored test cases it names (by the window the agent streamed
// each with, or by the name it gave one), and what the agent left out
// unnamed, or its stream could not tell, is the run's. Under the proven switch
// only.
func (c *caseLosses) ended(sum models.StreamEndSummary) {
	if c == nil || !provenswitch.On() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range sum.LateLosses {
		var names []string
		if req, resp := rec.LateLossWindowOf(l); req != 0 {
			names = append(names, c.byWindow[[2]int64{req, resp}]...)
		}
		if _, ok := c.at[l.Name]; l.Name != "" && ok && !slices.Contains(names, l.Name) {
			names = append(names, l.Name)
		}
		for _, n := range names {
			if c.lost == nil {
				c.lost = map[string]models.CaptureLosses{}
			}
			c.lost[n] = c.lost[n].PlusNaming(l.Lost)
		}
	}
	c.leftOut, c.untold = rec.LeftOutUnsaid(sum), rec.StreamsUntold(sum)
}

// names is the stored test cases that lost something, sorted.
func (c *caseLosses) names() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.lost))
	for n := range c.lost {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// text is everything c holds, in a sentence: what the test cases lost, kind
// by kind, each source named, then what the agent left out unnamed, and what
// its stream could not tell; "" for nothing.
func (c *caseLosses) text() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var all models.CaptureLosses
	for _, n := range slices.Sorted(maps.Keys(c.lost)) {
		all = all.PlusNaming(c.lost[n])
	}
	var parts []string
	if t := all.Text(); t != "" {
		parts = append(parts, t)
	}
	for _, s := range []string{c.leftOut, c.untold} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "; ")
}

// chargeWindows charges, in a record that is judged, each stored test case's
// loss to the test it was recorded in: the top-level test window, App and
// suite windows aside, that holds its request (the innermost, as cases are
// correlated), whose evidence then carries it (ScopeEvidence.CaptureIncomplete,
// each kind the test case's own, models.CaptureLossKind.PerTest), so judging
// decides that test on it, as on a loss the agent charged it. It returns what
// no test holds, the run's, in a sentence ("" for none): a test case outside
// every test, what the agent left out unnamed, and what its stream could not
// tell.
func (c *caseLosses) chargeWindows(windows []models.ScopeWindow) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var outside []string
	for _, n := range slices.Sorted(maps.Keys(c.lost)) {
		lost := c.lost[n]
		i := testHolding(windows, c.at[n])
		if i < 0 {
			outside = append(outside, fmt.Sprintf("test case %s, recorded outside every test: %s", n, lost.Text()))
			continue
		}
		w := &windows[i]
		if w.Evidence == nil {
			w.Evidence = &models.ScopeEvidence{}
		}
		w.Evidence.CaptureIncomplete = w.Evidence.CaptureIncomplete.PlusNaming(lost)
		said := "keploy: test case " + n + " recorded in it: " + lost.Text() + "."
		if w.Evidence.Reason == "" {
			w.Evidence.Reason = said
		} else {
			w.Evidence.Reason = strings.TrimSpace(w.Evidence.Reason + " " + said)
		}
	}
	for _, s := range []string{c.leftOut, c.untold} {
		if s != "" {
			outside = append(outside, s)
		}
	}
	return strings.Join(outside, "; ")
}

// testHolding is the index of the top-level test window (no App, suite or
// sub-test window: the ones a record's receipt names) that holds instant at,
// the latest-started of those that do; -1 for none.
func testHolding(windows []models.ScopeWindow, at time.Time) int {
	best := -1
	for i, w := range windows {
		if w.App || w.Suite || parentOf(windows, w.Name) != "" || at.Before(w.Start) || at.After(w.End) {
			continue
		}
		if best == -1 || w.Start.After(windows[best].Start) {
			best = i
		}
	}
	return best
}
