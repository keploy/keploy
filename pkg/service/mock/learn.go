package mock

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// learning reports whether this replay runs the learn step (design v14 §7.2,
// row L; package matcher/learn): `keploy mock record`'s full round, which the
// record marks in memory (config.Test.Learn), under the proven switch.
func (m *mockService) learning() bool {
	return provenswitch.On() && m.config.Test.Learn
}

// learningPool reports whether the learn step may pair pool mocks too: only
// a full record, which replaces the pool (config.Test.LearnPool).
func (m *mockService) learningPool() bool {
	return m.learning() && m.config.Test.LearnPool
}

// learningTests are, for the agent, the tests whose recordings the learn step
// learns onto (config.Test.LearnTests, learnsOnto): nil when it learns onto
// every test's, or does not run.
func (m *mockService) learningTests() map[string]bool {
	if !m.learning() || m.config.Test.LearnTests == nil {
		return nil
	}
	return maps.Clone(m.config.Test.LearnTests)
}

// keepLearned writes what record's full round learned onto the set's mocks
// (persistLearned), where the agent said its learn step reads the session's
// tests as client tests (clientTestsRead). Otherwise it keeps none of it: an
// agent older than this keploy learns a value the test harness gave the
// application as noise where it changed. A round whose agent learned
// anything it would have kept then did not complete, and its error says what
// was not kept and why: the round's tests ran on what was learned, and the
// recording, kept without it, compares every field. One that learned nothing
// lost nothing. consumedErr is why the agent's consumed mocks could not be
// read: then what was learned cannot be read either. elsewhere is what the
// learn step taught recordings from calls made in other windows than
// theirs, for learning only (the agent's LearnedElsewhere): its clock reads
// alone are written onto the recordings with the rest (withLearnedElsewhere),
// and no test's flow holds them.
func (m *mockService) keepLearned(ctx context.Context, set string, consumed []models.MockState, consumedErr error, elsewhere []models.MockState, flows []FlowMocks, expected map[string][]models.MockEntry) (learnedOut, error) {
	if consumedErr != nil {
		return learnedOut{}, fmt.Errorf("what the learn step learned could not be read from keploy's agent: %w", consumedErr)
	}
	if !m.clientTestsRead() {
		notKept := m.learnedOnto(withLearnedElsewhere(consumed, elsewhere), expected)
		if len(notKept) == 0 {
			return learnedOut{}, nil
		}
		// The agent by the version its answer named, or as one that named
		// none (models.AgentVersionHeader).
		didNotSay := pkg.AgentDidNotSay(pkg.AgentVersionOf(m.instrumentation), "it reads keploy mock's tests as a test harness's client tests")
		m.logger.Warn("keploy kept nothing its agent learned in record's full round, and the round did not complete: the agent did not say it reads keploy mock's tests as a test harness's client tests, so a value the harness gave the application may have been learned as noise; the recording keeps comparing every field",
			zap.String("mock-set", set),
			zap.Strings("notKept", notKept),
			zap.String("agent", didNotSay),
			zap.String("next_step", clientTestsRemedy()))
		return learnedOut{}, fmt.Errorf("keploy kept nothing its agent learned in record's full round (%s): %s, so a value the harness gave the application may have been learned as noise, and the round's tests ran on it; %s", firstFields(notKept, 10), didNotSay, clientTestsRemedy())
	}
	return m.persistLearned(ctx, set, consumed, elsewhere, flows, expected)
}

// learnedOut is what record's full round wrote onto the set's mocks
// (persistLearned), as the record says it: byTest by test, each field as
// "<mock> <what was added>"; pool what it wrote onto mocks no test consumed
// (the set's start-up and pool), sorted; elsewhere what calls made in other
// windows than their recordings' taught them, by recording
// (LearnedElsewhere). A field only such a call taught is in elsewhere alone.
type learnedOut struct {
	byTest    map[string][]string
	pool      []string
	elsewhere []LearnedElsewhere
}

// withLearnedElsewhere is consumed with a state of its own for each
// recording elsewhere taught that no call of the round consumed: its clock
// reads alone, each ClockOffset as the agent sent it. Anything else a state
// of elsewhere carries (noise, variables) is never written onto a recording
// from a call made in another window, whatever the agent sent. A recording
// a call of the round consumed keeps what its own pairing learned alone
// (ownPairingWins): what another window taught it is voided, never merged.
// Two states elsewhere sends for one recording (an agent that taught a
// recording more than once a round) are merged, never overwritten.
// consumed is not changed. It is read for what is written onto the
// recordings alone: which test used which recording (attributeMocks) reads
// the consumed states as the agent sent them.
func withLearnedElsewhere(consumed, elsewhere []models.MockState) []models.MockState {
	if len(elsewhere) == 0 {
		return consumed
	}
	out := slices.Clone(consumed)
	own := len(out)
	at := make(map[string]int, len(out))
	for i, st := range out {
		at[st.Name] = i
	}
	for _, e := range elsewhere {
		if e.Name == "" || len(e.Clocks) == 0 {
			continue
		}
		i, ok := at[e.Name]
		switch {
		case !ok:
			at[e.Name] = len(out)
			out = append(out, models.MockState{Name: e.Name, Kind: e.Kind, Clocks: maps.Clone(e.Clocks)})
		case i >= own:
			out[i].Clocks = models.MergeClocks(out[i].Clocks, e.Clocks)
		}
	}
	return out
}

// ownPairingWins splits elsewhere, what calls made in other windows than
// their recordings' taught them (the agent's LearnedElsewhere), as DESIGN
// T4 says: one pairing per recording per round, and its own one wins. A
// recording a call of the round consumed was paired by the rules that serve
// it (its own test's call, or the start-up and pool's), in its own window:
// that is stronger evidence than a call of its shape in another window, so
// what such a call taught it is voided, whichever came first. The agent
// never pairs a consumed recording across windows (ownedElsewhere), so a
// teaching beside a consumption came first. kept is the rest, in order.
func ownPairingWins(consumed, elsewhere []models.MockState) (kept, voided []models.MockState) {
	paired := make(map[string]bool, len(consumed))
	for _, st := range consumed {
		paired[st.Name] = true
	}
	for _, e := range elsewhere {
		if e.Name == "" || len(e.Clocks) == 0 {
			continue
		}
		if paired[e.Name] {
			voided = append(voided, e)
			continue
		}
		kept = append(kept, e)
	}
	return kept, voided
}

// clientTestsRemedy is what to do about a round whose agent did not say it
// reads keploy mock's tests as client tests: run it with an agent of this
// keploy's own version, which does.
func clientTestsRemedy() string {
	if utils.Version != "" {
		return "run keploy mock record with the keploy agent of its own version (" + utils.Version + ")"
	}
	return "run keploy mock record with the keploy agent of its own version"
}

// learnedOnto is what the learn step learned on consumed that persistLearned
// would keep (learnsOnto, by each mock's test in expected), each as "<mock>
// <field or variable> (<kind>)", sorted: nil when it learned nothing there.
func (m *mockService) learnedOnto(consumed []models.MockState, expected map[string][]models.MockEntry) []string {
	owner := map[string]string{}
	for test, entries := range expected {
		for _, e := range entries {
			owner[e.Name] = test
		}
	}
	var out []string
	for _, st := range consumed {
		if !m.learnsOnto(owner[st.Name]) {
			continue
		}
		for field := range st.ReqBodyNoise {
			out = append(out, st.Name+" "+field+" (noise)")
		}
		for field, o := range st.Clocks {
			out = append(out, st.Name+" "+field+" ("+o.String()+")")
		}
		for v, mv := range st.Variables {
			kind := "variable"
			if mv.Echo {
				kind = "echo"
			}
			out = append(out, st.Name+" "+v+" ("+kind+")")
		}
		for at, vars := range st.Binds {
			for v := range vars {
				out = append(out, st.Name+" "+v+" (bound at "+at+")")
			}
		}
	}
	sort.Strings(out)
	return out
}

// firstFields joins up to n fields, and says how many more there are.
func firstFields(fields []string, n int) string {
	if len(fields) <= n {
		return strings.Join(fields, ", ")
	}
	return strings.Join(fields[:n], ", ") + " and " + strconv.Itoa(len(fields)-n) + " more"
}

// clientTestsRead reports whether the agent said, answering the session's
// MockOutgoing, that its learn step reads the session's tests as client
// tests (pkg.ClientTestsReader, models.ClientTestsReadHeader). An
// instrumentation that cannot say read none so.
func (m *mockService) clientTestsRead() bool {
	r, ok := m.instrumentation.(pkg.ClientTestsReader)
	return ok && r.AgentReadsClientTests()
}

// learnedPersister writes what the learn step learned onto a set's mocks and
// says what it added, by mock (the yaml mock store, PersistLearned).
type learnedPersister interface {
	PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error)
}

// persistLearned writes what the learn step learned onto the set's mocks, so
// the recording the record commits carries it, says per test what was
// learned (design §7.2 step 10), and returns it (learnedOut): by test, each
// field as "<mock> <what mockdb's PersistLearned added>", what it wrote onto
// mocks no test consumed (the set's start-up and pool), and what calls made
// in other windows than their recordings' taught them (elsewhere's clock
// reads, on recordings no call of the round consumed: withLearnedElsewhere),
// each said with the test whose recording it is and the windows that taught
// it. What such a call taught a recording the round consumed is voided, and
// said so (ownPairingWins). With
// config.Test.LearnTests set it writes only onto the mocks of those tests
// (and onto the pool's with LearnPool): a test the record did not record
// again keeps its recording as it is. expected is each test's own mocks, by
// its mapping.
func (m *mockService) persistLearned(ctx context.Context, set string, consumed, elsewhere []models.MockState, flows []FlowMocks, expected map[string][]models.MockEntry) (learnedOut, error) {
	p, ok := m.mockDB.(learnedPersister)
	if !ok {
		return learnedOut{}, errors.New("keploy's mock store cannot write what the learn step learned")
	}
	owner := map[string]string{}
	for test, entries := range expected {
		for _, e := range entries {
			owner[e.Name] = test
		}
	}
	elsewhere, voided := ownPairingWins(consumed, elsewhere)
	written := withLearnedElsewhere(consumed, elsewhere)
	all := make(map[string]models.MockState, len(written))
	for _, st := range written {
		all[st.Name] = st
	}
	all, taken := learn.RunTakesPlaces(all, func(mock string) string { return owner[mock] })
	for _, t := range taken {
		m.logger.Info("keploy follows a test's own variable as the test set's: a later test sends the value it sent",
			zap.String("test", t.Test), zap.String("variable", t.Own), zap.String("setVariable", t.Run), zap.String("mock", t.Mock), zap.String("field", t.Place))
	}
	states := make(map[string]models.MockState, len(written))
	for _, st := range all {
		if !m.learnsOnto(owner[st.Name]) {
			continue
		}
		// A variable is bound at an earlier call of the same test, which the
		// record made too; one bound at a recording it did not make cannot
		// be written whole.
		for target := range st.Binds {
			if !m.learnsOnto(owner[target]) {
				return learnedOut{}, fmt.Errorf("the learn step learned a variable of %s bound at %s, a recording this record did not make", st.Name, target)
			}
		}
		states[st.Name] = st
	}
	added, err := p.PersistLearned(ctx, set, states)
	if err != nil {
		return learnedOut{}, fmt.Errorf("writing what the learn step learned onto keploy/%s: %w", set, err)
	}
	taught := taughtFields(elsewhere, added)
	byTest := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range flows {
		var fields []string
		for _, st := range f.Consumed {
			for _, a := range added[st.Name] {
				if taught.only(st.Name, a) {
					continue
				}
				fields = append(fields, st.Name+" "+a)
			}
			if len(added[st.Name]) > 0 {
				seen[st.Name] = true
			}
		}
		if len(fields) > 0 {
			byTest[f.Flow] = fields
			m.logger.Info("learned fields that changed between record and replay", zap.String("test", f.Flow), zap.Strings("fields", fields))
		}
	}
	var rest []string
	for name, as := range added {
		if seen[name] {
			continue
		}
		for _, a := range as {
			if taught.only(name, a) {
				continue
			}
			rest = append(rest, name+" "+a)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		m.logger.Info("learned fields that changed between record and replay, on mocks no test consumed", zap.Strings("fields", rest))
	}
	out := learnedOut{byTest: byTest, pool: rest, elsewhere: taught.said(owner)}
	for _, e := range out.elsewhere {
		m.logger.Info("learned fields that changed between record and replay, from calls made in other windows than their recording's, for learning only",
			zap.String("mock", e.Mock), zap.String("test", orStartupAndPool(e.Owner)), zap.Strings("fields", e.Fields), zap.String("taughtIn", e.Windows()),
			zap.String("evidence", "a call of its shape in another window: no call of the round consumed the recording"))
	}
	for _, v := range voided {
		if !m.learnsOnto(owner[v.Name]) {
			continue
		}
		m.logger.Info("kept what a recording's own pairing learned and voided what a call made in another window taught it: a call of the round consumed the recording, and its own pairing wins",
			zap.String("mock", v.Name), zap.String("test", orStartupAndPool(owner[v.Name])), zap.Strings("voided", slices.Sorted(maps.Keys(v.Clocks))),
			zap.String("taughtIn", LearnedElsewhere{TaughtIn: v.TaughtIn}.Windows()), zap.String("evidence", "its own pairing in the round"))
	}
	return out, nil
}

// taught is, by recording, what calls made in other windows than its own
// taught it (the agent's LearnedElsewhere) that the round wrote onto it
// (taughtFields).
type taught map[string]taughtMock

type taughtMock struct {
	// fields are the fields written of the paths such calls taught,
	// as mockdb's PersistLearned said it added them.
	fields []string
	from   []models.TeachingWindow
}

// taughtFields is, of added (by recording, what the round wrote onto it),
// the fields at the paths elsewhere taught each recording, with the
// windows that taught it. elsewhere holds no recording a call of the round
// consumed (ownPairingWins): no consumer learned what it says.
func taughtFields(elsewhere []models.MockState, added map[string][]string) taught {
	out := taught{}
	for _, e := range elsewhere {
		if e.Name == "" || len(e.Clocks) == 0 || len(added[e.Name]) == 0 {
			continue
		}
		t := taughtMock{from: e.TaughtIn}
		for _, a := range added[e.Name] {
			if fieldPath(a, e.Clocks) != "" {
				t.fields = append(t.fields, a)
			}
		}
		if len(t.fields) > 0 {
			out[e.Name] = t
		}
	}
	return out
}

// fieldPath is the path of clocks a field as mockdb's PersistLearned says
// it ("<path> (<what>)") is at, "" for none.
func fieldPath(field string, clocks map[string]models.ClockOffset) string {
	path, _, ok := strings.Cut(field, " (")
	if !ok {
		return ""
	}
	if _, at := clocks[path]; !at {
		return ""
	}
	return path
}

// only reports whether field, written onto mock, was taught by calls made
// in other windows: no call of the round consumed mock.
func (t taught) only(mock, field string) bool {
	tm, ok := t[mock]
	return ok && slices.Contains(tm.fields, field)
}

// said is t as the record's receipt says it (LearnedElsewhere), by
// recording, each with its owner's name (owner, "" for the set's start-up
// and pool) and the windows that taught it, in time order.
func (t taught) said(owner map[string]string) []LearnedElsewhere {
	out := make([]LearnedElsewhere, 0, len(t))
	for _, mock := range slices.Sorted(maps.Keys(t)) {
		tm := t[mock]
		from := slices.Clone(tm.from)
		sort.SliceStable(from, func(i, j int) bool { return from[i].At.Before(from[j].At) })
		out = append(out, LearnedElsewhere{Mock: mock, Owner: owner[mock], Fields: slices.Clone(tm.fields), TaughtIn: from})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// orStartupAndPool is test, or what the record calls the set's start-up and
// pool when it is "".
func orStartupAndPool(test string) string {
	if test == "" {
		return "the set's start-up and pool"
	}
	return test
}

// learnsOnto reports whether what the learn step learned is kept on a mock
// whose test (by mapping) is test, "" for the pool's. A test's mapping is
// by its top-level ID (a Go subtest's mocks map to its test), and an ID is
// matched whole: a "/" in it is no subtest separator in every framework (a
// test named after a route).
func (m *mockService) learnsOnto(test string) bool {
	switch {
	case m.config.Test.LearnTests == nil:
		return true
	case test == "":
		return m.config.Test.LearnPool
	}
	return m.config.Test.LearnTests[test]
}
