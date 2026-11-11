package replay

import (
	"sort"
	"strings"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// The replay marks, as it reads a set, the recordings of calls the app made
// to its own port that it can tell from the recording alone
// (markOwnListenerMocks: a loopback destination at a test port). A recording
// of a call the app made to another of its addresses (its container's own IP)
// it cannot tell so. The agent can: as keploy test's proxy passes a call
// through to a listener of the app's own, it names that call's destination
// (models.OwnListenerCallsHeader), and the replay marks each recording made
// at one of those destinations too. It does so after every read of what a
// test consumed, before the test is judged, and after every read at the
// set's end, before the worker scope is judged. A recording at a destination
// the agent never named is judged as before.

// ownListenerReporter is the instrumentation that names the destinations
// keploy test's agent passed through as the app's own listener
// (platform/http.AgentClient.OwnListenerCalls). One that does not names none.
type ownListenerReporter interface {
	OwnListenerCalls() []string
}

// ownListenerReports is one set's recordings by where they were made, and the
// names of those marked as the app's calls to its own listener, at load or
// from the agent's reports. Its zero value marks nothing.
type ownListenerReports struct {
	// byDest: the names of the set's recordings by their destination, each
	// in models.OwnListenerDestination's spelling (the agent's).
	byDest map[string][]string
	// marked: the names of the recordings marked, read by every site that
	// charges a recording mid-set (exactPlan.ownListener).
	marked map[string]bool
}

// newOwnListenerReports indexes a set's recordings, from pools, by where they
// were made: a recording's destAddr, else an HTTP recording's Host, when it
// names an IP and a port. One marked at load is marked from the start.
func newOwnListenerReports(pools ...[]*models.Mock) ownListenerReports {
	o := ownListenerReports{byDest: map[string][]string{}, marked: map[string]bool{}}
	for _, pool := range pools {
		for _, m := range pool {
			if m == nil {
				continue
			}
			if m.OwnListenerCall() {
				o.marked[m.Name] = true
				continue
			}
			addr := strings.TrimSpace(m.Spec.Metadata["destAddr"])
			if addr == "" {
				addr, _ = m.RecordedDestination()
			}
			if dest, ok := models.OwnListenerDestination(addr); ok {
				o.byDest[dest] = append(o.byDest[dest], m.Name)
			}
		}
	}
	return o
}

// apply marks each recording made at one of dests (the agent's spelling) and
// not marked yet, and returns their names, sorted.
func (o ownListenerReports) apply(dests []string) []string {
	var names []string
	for _, dest := range dests {
		for _, name := range o.byDest[dest] {
			if !o.marked[name] {
				o.marked[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// applyOwnListenerReport marks, from what the agent's last answer named
// (ownListenerReporter), each of the set's recordings made at a destination
// the agent passed through as the app's own listener, and leaves it out of
// what a test requires: each map of excluded (the set's reusableMockNames and
// unservedMockNames) gets its name. The exact plan reads the same marks
// (exactPlan.ownListener) when it judges the worker scope and the shares. It
// says which it marked, and returns their names.
func (r *Replayer) applyOwnListenerReport(testSetID string, reports ownListenerReports, excluded ...map[string]bool) []string {
	var dests []string
	if rep, ok := r.instrumentation.(ownListenerReporter); ok {
		dests = rep.OwnListenerCalls()
	}
	names := reports.apply(dests)
	for _, m := range excluded {
		for _, name := range names {
			m[name] = true
		}
	}
	if len(names) > 0 {
		r.logger.Info("the application called its own listener at an address this test set's recordings hold calls to: keploy passed each such call through to the application, so these recordings go unused, and no test or worker scope requires them",
			zap.String("testSetID", testSetID), zap.Int("count", len(names)),
			zap.Strings("mocks", names[:min(len(names), maxNamedOwnListenerMocks)]))
	}
	return names
}
