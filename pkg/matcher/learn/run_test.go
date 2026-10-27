package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
)

// sent is an exchange served while no test is open (the start-up's): a test's
// own exchange sets Own.
func sent(mock string, at time.Time, path, place, rec, live string) Earlier {
	return Earlier{Mock: mock, At: at, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: path, Type: String, Value: rec}, Live: live, Place: place, Written: true}}}
}

// What the start-up sends, while no test is open, is an earlier place of
// every later test of the set, and its variables keep one name across them.
func TestTrackerKeepsTheRunsExchangesForEveryTest(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.auth_time", "body.auth_time", "1792000000", "1792000777"))
	tr.Served(0, sent("mock-2", t0, "body.status", "body.status", "ok", "ok"))
	run, _ := tr.RunName(0, "mock-1", "body.auth_time", nil)

	for i := 0; i < 2; i++ {
		tr.Begin()
		n, earlier, _, _ := tr.Test()
		if len(earlier) != 1 || !earlier[0].Run || earlier[0].Mock != "mock-1" {
			t.Fatalf("test %d: the run's exchanges are %+v, want the start-up call that carried a value the run made", i, earlier)
		}
		if name, ok := tr.Name(n, "mock-1", "body.auth_time", nil); !ok || name != run {
			t.Fatalf("test %d: the run's variable is named %q here, %q at the start-up", i, name, run)
		}
		if name, _ := tr.Name(n, "mock-9", "body.auth_time", nil); name == run {
			t.Fatalf("test %d: a test's own variable took the run's name %q", i, run)
		}
		tr.End()
	}
	tr.NewSet()
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("a new set reads the previous set's start-up: %+v", earlier)
	}
	tr.End()
	tr.NewSet()
	n, _, _, _ := tr.Test()
	tr.Served(n, sent("mock-1", t0, "body.auth_time", "body.auth_time", "1792000000", "1792000999"))
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Run {
		t.Fatalf("the next set's start-up is not the run's: %+v", earlier)
	}
	if again, _ := tr.RunName(n, "mock-1", "body.auth_time", nil); again != run {
		t.Fatalf("the next set's run's variable is named %q, want %q again", again, run)
	}
}

// A run's variable is named apart from a test case's template keys: a test
// that reserves the key the run's value sits under uses the run's name as it
// is.
func TestTheRunsVariableTakesNoTemplateKey(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body._id", "body._id", "rec-id", "live-id"))
	tr.Begin()
	n1, _, _, _ := tr.Test()
	run, _ := tr.RunName(n1, "mock-1", "body._id", nil)
	tr.End()
	tr.Begin()
	n, _, _, _ := tr.Test()
	reserved := map[string]bool{"_id": true}
	if again, _ := tr.RunName(n, "mock-1", "body._id", reserved); again != run || reserved[run] {
		t.Fatalf("the run's variable is %q in a test that reserves _id, %q before", again, run)
	}
	if own, _ := tr.Name(n, "mock-5", "body._id", reserved); own == run || reserved[own] {
		t.Fatalf("the test's own variable is %q, next to the run's %q and its reserved _id", own, run)
	}
	if name, _ := (*Tracker)(nil).RunName(0, "mock-1", "body._id", map[string]bool{"run-_id": true}); name != "run-_id-2" {
		t.Fatalf("a nil tracker names the run's variable %q, want run-_id-2", name)
	}
}

// A run's variable has one name for the whole set, which its start-up's
// bind carries: a test whose test case reserves that very name (gives a value
// under it) cannot use it there, so the place is refused in that test, never
// renamed, and named as before in every other. keploy test's template keys
// never take one (they hold no '-'); a name given from elsewhere may.
func TestARunsNameATestReservesIsRefusedThere(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body._id", "body._id", "rec-id", "live-id"))
	tr.Begin()
	n1, _, _, _ := tr.Test()
	run, ok := tr.RunName(n1, "mock-1", "body._id", nil)
	if !ok || run != "run-_id" {
		t.Fatalf("the run's variable is named %q (%v), want run-_id", run, ok)
	}
	if templates.IsKey(run) {
		t.Fatalf("the run's name %q is a template key keploy test can give", run)
	}
	tr.End()
	tr.Begin()
	n, _, _, _ := tr.Test()
	reserved := map[string]bool{run: true}
	if name, ok := tr.Name(n, "mock-1", "body._id", reserved); ok {
		t.Fatalf("a test that reserves %q named the run's place %q", run, name)
	}
	if name, ok := tr.RunName(n, "mock-1", "body._id", reserved); ok {
		t.Fatalf("a test that reserves %q named the run's variable %q", run, name)
	}
	tr.End()
	tr.Begin()
	n, _, _, _ = tr.Test()
	if name, ok := tr.Name(n, "mock-1", "body._id", nil); !ok || name != run {
		t.Fatalf("the next test names the run's place %q (%v), want %q", name, ok, run)
	}
}

// A run's variable is named when a test first uses it, after earlier tests
// named their own: it takes none of their names, and later tests take none
// of its (two tests may share a name: variables are named per test).
func TestTheRunsVariableTakesNoTestsName(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	tr.Begin()
	n1, _, _, _ := tr.Test()
	own, _ := tr.Name(n1, "mock-5", "body.docs.msgs.0._id", nil)
	tr.End()
	tr.Begin()
	n2, _, _, _ := tr.Test()
	run, _ := tr.RunName(n2, "mock-1", "body.docs.msgs.0._id", nil)
	later, _ := tr.Name(n2, "mock-7", "body.docs.msgs.0._id", nil)
	if own != "_id" || run == own || later == run {
		t.Fatalf("test 1 named its own %q, the run's is %q, test 2's own is %q; want the run's apart from both", own, run, later)
	}
}

// Only what the start-up served, before the first test began, is the run's.
// A request of a test answered after it ended, or between it and the next,
// is that test's, forgotten by the next Begin; a start-up exchange that did
// not drift is kept until the first test begins.
func TestOnlyTheStartUpsExchangesAreTheRuns(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.status", "body.status", "ok", "ok"))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Run {
		t.Fatalf("a start-up exchange that did not drift is not kept until the first test: %+v", earlier)
	}
	tr.Begin()
	n, earlier, _, _ := tr.Test()
	if len(earlier) != 0 {
		t.Fatalf("the first test sees a start-up exchange that carried nothing the run made: %+v", earlier)
	}
	tr.End()
	late := sent("mock-4", t0.Add(time.Second), "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id")
	late.Own = true
	tr.Served(n, late)
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Run {
		t.Fatalf("a late exchange of the ended test is not its own: %+v", earlier)
	}
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("the next test sees the previous test's late exchange: %+v", earlier)
	}
	tr.End()
	if !tr.Begun() {
		t.Fatal("a test has begun in this set, and the tracker says none has")
	}
	last, _, _, _ := tr.Test()
	drifts := tr.Drifts()
	tr.NewSet()
	if tr.Begun() {
		t.Fatal("a new set begins with no test begun, and the tracker says one has")
	}
	tr.Served(last, sent("mock-2", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	tr.Served(last+9, sent("mock-8", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	next, _, _, _ := tr.Test()
	straggler := sent("mock-7", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id")
	straggler.Own = true
	tr.Served(next, straggler)
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("a request of the previous set, of a test that never ran, or one that took its scope from the previous set, is kept as the new set's: %+v", earlier)
	}
	if name, ok := tr.Name(last, "mock-2", "body.docs.msgs.0._id", nil); ok {
		t.Fatalf("a request of the previous set named a variable %q in the new set", name)
	}
	if tr.Answer(next, &models.Mock{Name: "mock-7"}) || tr.Answered("mock-7") || len(tr.AnsweredMocks()) != 0 || tr.Drifts() == drifts {
		t.Fatal("with no test open, a request took a mock as a test's own, or the new set reads the previous set's answers or drifts")
	}
	boot := sent("mock-0", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id")
	boot.Own = false
	tr.Served(next, boot)
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Run {
		t.Fatalf("the new set's start-up is not the run's: %+v", earlier)
	}
}

// A new set names its variables afresh: what the previous set's last test
// named, at mock names the new set reuses, names nothing of the new set's.
// A run's variable is the set's: a request of the previous set (a test number
// from before NewSet) is named none of the new set's run's variables, by Name
// or by RunName, and gives none its name, as it is named no test's variable.
// The new set's requests name them, its start-up's and its tests', a test's
// that ended too.
func TestANewSetsNamesAreItsOwn(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.NewSet()
	tr.Begin()
	old, _, _, _ := tr.Test()
	if own, _ := tr.Name(old, "mock-1", "body.code", nil); own != "code" {
		t.Fatalf("the previous set's test names mock-1's body.code %q", own)
	}
	if own, _ := tr.Name(old, "mock-2", "body.code", nil); own != "code-2" {
		t.Fatalf("the previous set's test names mock-2's body.code %q", own)
	}
	tr.End()
	tr.NewSet()
	boot, _, _, _ := tr.Test()
	if name, ok := tr.Name(boot, "mock-2", "body.code", nil); !ok || name != "code" {
		t.Fatalf("the new set's start-up names mock-2's body.code %q (%v); want code, not the previous set's name of it", name, ok)
	}
	tr.Served(boot, sent("mock-1", t0, "body._id", "body._id", "rec-id", "live-id"))
	if name, ok := tr.RunName(old, "mock-1", "body._id", map[string]bool{"run-_id": true}); ok {
		t.Fatalf("a request of the previous set named the new set's run's variable %q", name)
	}
	run, ok := tr.RunName(boot, "mock-1", "body._id", nil)
	if !ok || run != "run-_id" {
		t.Fatalf("the new set's start-up names its run's variable %q (%v); want run-_id", run, ok)
	}
	if name, ok := tr.Name(old, "mock-1", "body._id", nil); ok {
		t.Fatalf("a request of the previous set is named the new set's run's variable %q", name)
	}
	if name, ok := tr.RunName(old, "mock-1", "body._id", nil); ok {
		t.Fatalf("a request of the previous set is named the new set's run's variable %q by RunName", name)
	}
	tr.Begin()
	n, _, _, _ := tr.Test()
	if name, ok := tr.Name(n, "mock-1", "body._id", nil); !ok || name != run {
		t.Fatalf("the new set's test names the run's place %q (%v); want %q", name, ok, run)
	}
	tr.End()
	if name, ok := tr.Name(n, "mock-1", "body._id", nil); !ok || name != run {
		t.Fatalf("a late request of the set's ended test names the run's place %q (%v); want %q", name, ok, run)
	}
	if name, ok := tr.RunName(n, "mock-1", "body._id", nil); !ok || name != run {
		t.Fatalf("a late request of the set's ended test names the run's variable %q (%v); want %q", name, ok, run)
	}
}

// A new set's start-up has none of the previous set's exchanges as earlier
// places, its last test's neither: an app kept alive across sets sends there
// a value of its run, which the new set's start-up may send again. Mock names
// restart in every set, so the previous set's mock-3 would stand for the new
// set's mock-3: a value the start-up sends would be explained by it, and a
// read of the record it wrote read back from it. What the start-up serves is
// all it sees.
func TestANewSetHasNoneOfThePreviousSetsExchanges(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	const order = "orders\x00o-live-1"
	tr.Begin()
	last, _, _, _ := tr.Test()
	own := wrote("mock-3", t0, order, "body.code", String, "SAME-1", "LIVE-1")
	own.Own = true
	tr.Served(last, own)
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Mock != "mock-3" {
		t.Fatalf("the set's last test does not keep its own exchange: %+v", earlier)
	}
	tr.End()
	tr.NewSet()
	boot, earlier, _, _ := tr.Test()
	if len(earlier) != 0 {
		t.Fatalf("the new set's start-up has the previous set's last test's exchanges: %+v", earlier)
	}
	if p, ok := ReadBack(Leaf{Path: "body.value.code", Type: String, Value: "SAME-1"}, order, earlier); ok {
		t.Fatalf("the new set's start-up reads back what the previous set wrote, from %+v", p)
	}
	tr.Served(boot, sent("mock-1", t0.Add(time.Minute), "body.status", "body.status", "ok", "ok"))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Mock != "mock-1" || earlier[0].Run {
		t.Fatalf("the new set's start-up sees %+v, want only its own exchange", earlier)
	}
}

// A value the test's own earlier call sent, which a start-up call sent too,
// comes from the test's own call: only a test's own calls carry its
// variables.
func TestAValueComesFromTheTestsOwnCallBeforeTheRuns(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	boot := Earlier{Mock: "mock-0", At: t0, Run: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "header.x-build", Type: String, Value: "rec-build-1"}, Live: "live-build-7", Place: "header.x-build"}}}
	own := Earlier{Mock: "mock-5", At: t0.Add(time.Minute), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "header.x-build", Type: String, Value: "rec-build-1"}, Live: "live-build-7", Place: "header.x-build"}}}
	p, ok := appeared(clockedDrifts(), "rec-build-1", "live-build-7", t0.Add(2*time.Minute), []Earlier{boot, own}, nil)
	if !ok || !p.Own || p.Mock != "mock-5" {
		t.Fatalf("the value comes from %+v (%v), want the test's own call", p, ok)
	}
	p, ok = appeared(clockedDrifts(), "rec-build-1", "live-build-7", t0.Add(2*time.Minute), []Earlier{boot}, nil)
	if !ok || !p.Run || p.Mock != "mock-0" {
		t.Fatalf("with no own call the value comes from %+v (%v), want the run's", p, ok)
	}
	answered := Earlier{Mock: "mock-6", At: t0.Add(time.Minute), Own: true, Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.items.0.build", Type: String, Value: "rec-build-1"}, Live: "live-build-7", Place: "body.items.0.build"}}}
	p, ok = appeared(clockedDrifts(), "rec-build-1", "live-build-7", t0.Add(2*time.Minute), []Earlier{answered, boot}, nil)
	if !ok || !p.Run || p.Side != Request || p.Mock != "mock-0" {
		t.Fatalf("a value the test's own answer gave, which the start-up sent, comes from %+v (%v), want where it was sent", p, ok)
	}
	p, ok = appeared(clockedDrifts(), "rec-build-1", "live-build-7", t0.Add(2*time.Minute), []Earlier{answered}, nil)
	if !ok || p.Side != Response || p.Mock != "mock-6" {
		t.Fatalf("a value only an answer gave comes from %+v (%v), want that answer", p, ok)
	}
}

// A request place that holds no variable, or whose live value is unknown,
// does not make a start-up exchange the run's; nor does a drift in its
// answer alone, at a place a variable can hold: only what the application
// sent is a value it made. A request place that holds one and drifted does.
func TestAStartUpExchangeDriftsOnlyAtAVariablePlace(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, Earlier{Mock: "mock-1", At: t0, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "request{names}", Type: Keys, Value: "a,b"}, Live: "a,b,c"},
		{Side: Request, Leaf: Leaf{Path: "body.x", Type: String, Value: "rec"}, Live: "", Place: "body.x"},
	}})
	tr.Served(0, Earlier{Mock: "mock-2", At: t0.Add(time.Second), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.session", Type: String, Value: "s-rec"}, Live: "s-live", Place: "body.session"},
	}})
	tr.Served(0, Earlier{Mock: "mock-3", At: t0.Add(2 * time.Second), Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.boot", Type: String, Value: "b-rec"}, Live: "b-live", Place: "body.boot"},
	}})
	tr.Begin()
	_, earlier, _, _ := tr.Test()
	if len(earlier) != 1 || earlier[0].Mock != "mock-3" || !earlier[0].Run {
		t.Fatalf("the run's exchanges are %+v; want only mock-3, which sent a drifted value at a variable place", earlier)
	}
}

// Named leaves the exchange it was given as it was, and names a place two
// variables hold after the first by name.
func TestNamedLeavesTheExchangeItWasGiven(t *testing.T) {
	e := Earlier{Mock: "mock-1", Seen: []Seen{{Side: Request, Place: "body.id", Leaf: Leaf{Path: "body.id"}}}}
	named := Named(e, models.MockVariables{"second": {Bind: "body.id"}, "first": {Use: []string{"body.id"}}})
	if e.Seen[0].Name != "" {
		t.Fatalf("Named wrote %q into the exchange it was given", e.Seen[0].Name)
	}
	if named.Seen[0].Name != "first" {
		t.Fatalf("the place two variables hold is named %q, want the first by name", named.Seen[0].Name)
	}
	named.Seen = append(named.Seen, Seen{Side: Response, Name: "stale"})
	again := Named(named, nil)
	if again.Seen[0].Name != "" || again.Seen[1].Name != "" || &again.Seen[0] == &named.Seen[0] {
		t.Fatalf("Named with no variables keeps a stale name or the exchange's own Seen: %+v", again.Seen)
	}
}

// wrote is an exchange of the run that wrote rec, made live, into record at
// path (Seen.Written, Seen.Record), typed typ.
func wrote(mock string, at time.Time, record, path string, typ Type, rec, live string) Earlier {
	return Earlier{Mock: mock, At: at, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: path, Type: typ, Value: rec}, Live: live, Place: path, Written: true, Record: record}}}
}

// An answer that holds a value an earlier call wrote with another value in
// this run, into the record the read names, is answered with the value where
// it was first written, whatever its length, script or type. A value no
// linked write stored is not: one the write did not change, one a call only
// named (not Written), one whose live value is unknown, one written into
// another record, and any read the adapter links to no record.
func TestReadBack(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	const order = "orders\x00o-live-1"
	named := wrote("mock-7", t0, order, "body.note", String, "abc", "xyz")
	named.Seen[0].Written = false
	unknown := wrote("mock-8", t0, order, "body.tag", String, "t-rec", "")
	earlier := []Earlier{
		wrote("mock-3", t0.Add(time.Second), order, "body.code", String, "BJBM-NZBS", "FRST-XKCD"),
		wrote("mock-1", t0.Add(-time.Second), order, "body.code", String, "BJBM-NZBS", "FRST-XKCD"),
		wrote("mock-2", t0, order, "body.created", Number, "1792000000000", "1792000555000"),
		wrote("mock-4", t0, order, "body.pin", String, "48213", "90717"),
		wrote("mock-5", t0, order, "body.kind", String, "pending", "pending"),
		wrote("mock-6", t0, order, "body.name", String, "日本", "韓国"),
		wrote("mock-9", t0, "orders\x00o-live-2", "body.code2", String, "ZZZZ-1111", "YYYY-2222"),
		// A write the adapter names no record of: no read is linked to it.
		wrote("mock-10", t0, "", "body.ref", String, "REF-1", "REF-9"),
		// A write at no place a variable can hold: nothing to bind there.
		{Mock: "mock-11", At: t0, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.blob", Type: String, Value: "NOPLACE-1"}, Live: "NOPLACE-9", Written: true, Record: order}}},
		named, unknown,
	}
	for _, c := range []struct {
		value  string
		typ    Type
		record string
		mock   string
		live   string
	}{
		{"BJBM-NZBS", String, order, "mock-1", "FRST-XKCD"},
		{"1792000000000", Number, order, "mock-2", "1792000555000"},
		{"1792000000000", String, order, "mock-2", "1792000555000"},
		{"48213", Number, order, "mock-4", "90717"},
		{"日本", String, order, "mock-6", "韓国"},
		{"pending", String, order, "", ""},
		{"abc", String, order, "", ""},
		{"t-rec", String, order, "", ""},
		{"ZZZZ-1111", String, order, "", ""},
		{"BJBM-NZBS", String, "orders\x00o-live-2", "", ""},
		{"BJBM-NZBS", String, "", "", ""},
		{"REF-1", String, "", "", ""},
		{"REF-1", String, order, "", ""},
		{"NOPLACE-1", String, order, "", ""},
		{"", String, order, "", ""},
	} {
		p, ok := ReadBack(Leaf{Path: "body.value.x", Type: c.typ, Value: c.value}, c.record, earlier)
		if c.mock == "" {
			if ok {
				t.Errorf("%q (%c) of record %q was read back from %+v", c.value, c.typ, c.record, p)
			}
			continue
		}
		if !ok || p.Mock != c.mock || p.Live != c.live || p.Side != Request || !p.Whole {
			t.Errorf("%q (%c) of record %q read back as %+v (%v), want it from %s with %q", c.value, c.typ, c.record, p, ok, c.mock, c.live)
		}
	}
	if _, ok := ReadBack(Leaf{Path: "body.value.x", Type: String, Value: "BJBM-NZBS", Unnamed: true}, order, earlier); ok {
		t.Error("an unnamed leaf was read back")
	}
}

// Writes into one record of the same recorded value that hold two live
// values are two values a read of it may be: which write the read reads, the
// evidence does not say, so it is read back from none (the answer's drift is
// then unexplained, and refused). That holds whether they are two fields or
// one field written twice, whether both drifted, one drifted and the other
// kept the recorded value (in either order), or F could not read the other's
// live value, and whether the other is at a place a variable can hold or not
// (a field keploy binds no variable at holds a value the store answers all
// the same). Where every write drifted to one live value, the read is read
// back from the earliest at a place a variable can hold.
func TestReadBackOfTwoFieldsWithOneRecordedValue(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	const order = "orders\x00o-live-1"
	r := Leaf{Path: "body.value.x", Type: String, Value: "SAME-1"}
	for _, c := range []struct {
		name    string
		earlier []Earlier
		mock    string // "" when not read back
		live    string
	}{
		{"two fields, two live values", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", "LIVE-2"),
		}, "", ""},
		{"two fields of one exchange, two live values", []Earlier{{Mock: "mock-1", At: t0, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.first", Type: String, Value: "SAME-1"}, Live: "LIVE-1", Place: "body.first", Written: true, Record: order},
			{Side: Request, Leaf: Leaf{Path: "body.second", Type: String, Value: "SAME-1"}, Live: "LIVE-2", Place: "body.second", Written: true, Record: order},
		}}}, "", ""},
		{"two fields, one live value", []Earlier{
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", "LIVE-1"),
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
		}, "mock-1", "LIVE-1"},
		{"one field written twice, two live values", []Earlier{
			wrote("mock-2", t0.Add(time.Second), order, "body.first", String, "SAME-1", "LIVE-2"),
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
		}, "", ""},
		{"one field written twice, one live value", []Earlier{
			wrote("mock-2", t0.Add(time.Second), order, "body.first", String, "SAME-1", "LIVE-1"),
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
		}, "mock-1", "LIVE-1"},
		{"a field of another record", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
			wrote("mock-2", t0.Add(time.Second), "orders\x00o-live-2", "body.second", String, "SAME-1", "LIVE-2"),
		}, "mock-1", "LIVE-1"},
		// A field that kept the recorded value is a value the read may be
		// too: the store answers it as it was written.
		{"two fields of one exchange, one drifted and one not", []Earlier{{Mock: "mock-1", At: t0, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.code", Type: String, Value: "SAME-1"}, Live: "LIVE-1", Place: "body.code", Written: true, Record: order},
			{Side: Request, Leaf: Leaf{Path: "body.altCode", Type: String, Value: "SAME-1"}, Live: "SAME-1", Place: "body.altCode", Written: true, Record: order},
		}}}, "", ""},
		{"a field that did not drift, written after one that did", []Earlier{
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", "SAME-1"),
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
		}, "", ""},
		{"a field that did not drift, written before one that did", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "SAME-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", "LIVE-1"),
		}, "", ""},
		// A field whose live value F could not read may hold anything.
		{"a field whose live value is unknown", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", ""),
		}, "", ""},
		// One field written with two values held each in turn: the read
		// may be either, in either order.
		{"one field written as recorded, then drifted", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "SAME-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.first", String, "SAME-1", "LIVE-1"),
		}, "", ""},
		{"one field drifted, then written as recorded", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "LIVE-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.first", String, "SAME-1", "SAME-1"),
		}, "", ""},
		// A field at no place a variable can hold, written with the
		// recorded value, is a value the read may be too.
		{"two fields of one exchange, one at no place, two live values", []Earlier{{Mock: "mock-1", At: t0, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.code", Type: String, Value: "SAME-1"}, Live: "LIVE-1", Place: "body.code", Written: true, Record: order},
			{Side: Request, Leaf: Leaf{Path: "body.blob", Type: String, Value: "SAME-1"}, Live: "LIVE-2", Written: true, Record: order},
		}}}, "", ""},
		{"a field at no place written first, two live values", []Earlier{
			{Mock: "mock-0", At: t0, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.blob", Type: String, Value: "SAME-1"}, Live: "LIVE-2", Written: true, Record: order}}},
			wrote("mock-1", t0.Add(time.Second), order, "body.code", String, "SAME-1", "LIVE-1"),
		}, "", ""},
		{"a field at no place written first, one live value", []Earlier{
			{Mock: "mock-0", At: t0, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.blob", Type: String, Value: "SAME-1"}, Live: "LIVE-1", Written: true, Record: order}}},
			wrote("mock-1", t0.Add(time.Second), order, "body.code", String, "SAME-1", "LIVE-1"),
		}, "mock-1", "LIVE-1"},
		{"a field at no place that kept the recorded value", []Earlier{
			wrote("mock-1", t0, order, "body.code", String, "SAME-1", "LIVE-1"),
			{Mock: "mock-2", At: t0.Add(time.Second), Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.blob", Type: String, Value: "SAME-1"}, Live: "SAME-1", Written: true, Record: order}}},
		}, "", ""},
		{"no field drifted", []Earlier{
			wrote("mock-1", t0, order, "body.first", String, "SAME-1", "SAME-1"),
			wrote("mock-2", t0.Add(time.Second), order, "body.second", String, "SAME-1", "SAME-1"),
		}, "", ""},
	} {
		p, ok := ReadBack(r, order, c.earlier)
		if c.mock == "" {
			if ok {
				t.Errorf("%s: read back from %+v", c.name, p)
			}
			continue
		}
		if !ok || p.Mock != c.mock || p.Live != c.live {
			t.Errorf("%s: read back as %+v (%v), want it from %s with %q", c.name, p, ok, c.mock, c.live)
		}
	}
}

// ReplaceToken replaces a value only where it stands as a token.
func TestReplaceToken(t *testing.T) {
	for _, c := range []struct {
		s, v, with, want string
		found            bool
	}{
		{"USER:42", "42", "7", "USER:7", true},
		{"order-42x", "42", "7", "order-42x", false},
		{"42/42x/42", "42", "7", "7/42x/7", true},
		{"abcdef", "abcdef", "LIVE01", "LIVE01", true},
		{"Xabcdef9", "abcdef", "LIVE01", "Xabcdef9", false},
		{"", "42", "7", "", false},
		{"42", "", "7", "42", false},
	} {
		got, found := ReplaceToken(c.s, c.v, c.with)
		if got != c.want || found != c.found {
			t.Fatalf("ReplaceToken(%q, %q, %q) = %q, %v; want %q, %v", c.s, c.v, c.with, got, found, c.want, c.found)
		}
	}
}

// ReplaceTokens reads s once: what one replacement puts in is not read
// again, the longer key wins where two begin together, and a key that stands
// nowhere is missing.
func TestReplaceTokens(t *testing.T) {
	out, missing := ReplaceTokens("aaaaaa:bbbbbb", map[string]string{"aaaaaa": "xx-bbbbbb-yy", "bbbbbb": "cccccc"})
	if out != "xx-bbbbbb-yy:cccccc" || len(missing) != 0 {
		t.Fatalf("got %q, missing %v", out, missing)
	}
	out, missing = ReplaceTokens("USER:42 42 42x", map[string]string{"42": "7", "USER:42": "ADMIN:7", "nowhere": "x"})
	if out != "ADMIN:7 7 42x" || len(missing) != 1 || missing[0] != "nowhere" {
		t.Fatalf("got %q, missing %v", out, missing)
	}
}

// Among the places that hold a value, one that sent it whole comes before
// one that sent it inside a longer value, whoever's call it was; within one
// kind, the earliest.
func TestAValueComesFromWhereItWasSentWhole(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	seen := func(path, rec, live string) []Seen {
		return []Seen{{Side: Request, Leaf: Leaf{Path: path, Type: String, Value: rec}, Live: live, Place: path}}
	}
	boot := Earlier{Mock: "mock-0", At: t0, Run: true, Seen: seen("body.docs.msgs.0._id", "rec-id-aaaaaa", "live-id-bbbbbb")}
	ownPart := Earlier{Mock: "mock-5", At: t0.Add(time.Minute), Own: true, Seen: seen("body.cmd.filter.ref", "USER:rec-id-aaaaaa", "USER:live-id-bbbbbb")}
	p, ok := appeared(clockedDrifts(), "rec-id-aaaaaa", "live-id-bbbbbb", t0.Add(2*time.Minute), []Earlier{ownPart, boot}, nil)
	if !ok || !p.Whole || !p.Run || p.Mock != "mock-0" {
		t.Fatalf("the value comes from %+v (%v), want the start-up's whole place over the test's own part", p, ok)
	}
	other := Earlier{Mock: "mock-9", At: t0.Add(-time.Minute), Seen: seen("body.x", "rec-id-aaaaaa", "live-id-bbbbbb")}
	p, _ = appeared(clockedDrifts(), "rec-id-aaaaaa", "live-id-bbbbbb", t0.Add(2*time.Minute), []Earlier{other, boot}, nil)
	if !p.Run || p.Mock != "mock-0" {
		t.Fatalf("the value comes from %+v, want the run's place over another's that is earlier", p)
	}
	later := Earlier{Mock: "mock-1", At: t0.Add(time.Second), Run: true, Seen: seen("body.docs.msgs.0._id", "rec-id-aaaaaa", "live-id-bbbbbb")}
	p, _ = appeared(clockedDrifts(), "rec-id-aaaaaa", "live-id-bbbbbb", t0.Add(2*time.Minute), []Earlier{boot, later}, nil)
	if p.Mock != "mock-0" {
		t.Fatalf("the value comes from %+v, want the earliest of two places of one kind", p)
	}
}

// A run's name and a test's name never meet, whichever is named first: a
// test's own variable at a place named like the run's takes another name, and
// a run's variable first named after a test took its name takes another.
func TestARunsNameAndATestsNameNeverMeet(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	// The run's first: run-x, then a test's own at body.run-x.
	tr := NewTracker()
	tr.Served(0, sent("mock-1", t0, "body.x", "body.x", "rec-x", "live-x"))
	tr.Begin()
	n, _, _, _ := tr.Test()
	run, _ := tr.RunName(n, "mock-1", "body.x", nil)
	own, _ := tr.Name(n, "mock-5", "body.run-x", nil)
	if run != "run-x" || own == run {
		t.Fatalf("run's first: the run's is %q, the test's own %q; want run-x and another", run, own)
	}
	// The test's first: its own run-x, then the run's, first named after.
	tr = NewTracker()
	tr.Served(0, sent("mock-1", t0, "body.x", "body.x", "rec-x", "live-x"))
	tr.Begin()
	n, _, _, _ = tr.Test()
	own, _ = tr.Name(n, "mock-5", "body.run-x", nil)
	tr.End()
	tr.Begin()
	n, _, _, _ = tr.Test()
	run, _ = tr.RunName(n, "mock-1", "body.x", nil)
	if own != "run-x" || run == own {
		t.Fatalf("test's first: the test's own is %q, the run's %q; want run-x and another", own, run)
	}
}

// A run's place is named the run's name whatever test number asks: a late
// call of a test that ended uses the set's name, which no test owns.
func TestARunsPlaceIsNamedForAStaleTest(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body._id", "body._id", "rec-id", "live-id"))
	tr.Begin()
	stale, _, _, _ := tr.Test()
	run, _ := tr.RunName(stale, "mock-1", "body._id", nil)
	tr.End()
	tr.Begin()
	if name, ok := tr.Name(stale, "mock-1", "body._id", nil); !ok || name != run {
		t.Fatalf("an ended test's call names the run's place %q (%v), want %q", name, ok, run)
	}
	if name, ok := tr.Name(stale, "mock-5", "body._id", nil); ok {
		t.Fatalf("an ended test's call named its own variable %q", name)
	}
}

// Named names a place by the variables bound or used there, never by an
// echo: an echo is its call's alone.
func TestNamedSkipsAnEcho(t *testing.T) {
	e := Earlier{Mock: "mock-1", Seen: []Seen{
		{Side: Request, Place: "body.id", Leaf: Leaf{Path: "body.id"}},
		{Side: Request, Place: "body.ref", Leaf: Leaf{Path: "body.ref"}},
	}}
	named := Named(e, models.MockVariables{
		"echoed": {Bind: "body.id", Echo: true},
		"ref":    {Bind: "body.ref"},
	})
	if named.Seen[0].Name != "" || named.Seen[1].Name != "ref" {
		t.Fatalf("Named named the places %q and %q; want none for the echo, ref for ref", named.Seen[0].Name, named.Seen[1].Name)
	}
}

// A variable is named by the key above an operator's key ($date, $oid): the
// value's own name, not its encoding's.
func TestAVariableIsNamedAboveADollarKey(t *testing.T) {
	for place, want := range map[string]string{
		"body.created.$date":     "created",
		"body.order.$oid":        "order",
		"body.items[0].ref.$oid": "ref",
		"body.created":           "created",
		"body.$date":             "var",
	} {
		if got := varName(place); got != want {
			t.Errorf("varName(%q) = %q, want %q", place, got, want)
		}
	}
}

// A call made at the start-up, before the set's first test began, has no
// test: a word of what changed that an exchange of the start-up holds is
// named as earlier in the recording, never earlier in the test. In a test,
// and between tests (the previous test's), it is earlier in the test.
func TestAStartUpCallNamesItsEarlierPlacesEarlierInTheRecording(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	const path = "body.count"
	classify := func(tr *Tracker) string {
		t.Helper()
		_, earlier, _, _ := tr.Test()
		got := Classify(clockedDrifts(), []string{path},
			Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "7"}}},
			Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}},
			t0.Add(time.Second), earlier, nil, nil)
		if len(got) != 1 || got[0].Kind != KindUnaccounted {
			t.Fatalf("FALSE GREEN? 82 recorded, 7 live, 7 sent earlier: %+v, want refused", got)
		}
		return got[0].Why
	}
	tr := NewTracker()
	tr.Served(0, sent("mock-1", t0, "body.retries", "body.retries", "7", "7"))
	if why := classify(tr); !strings.Contains(why, "appears earlier in the recording, in mock-1 at body.retries") {
		t.Errorf("a start-up call: %q, want the start-up's exchange named earlier in the recording", why)
	}
	tr.Begin()
	n, _, _, _ := tr.Test()
	tr.Served(n, sent("mock-2", t0, "body.retries", "body.retries", "7", "7"))
	if why := classify(tr); !strings.Contains(why, "appears earlier in the test, in mock-2 at body.retries") {
		t.Errorf("a call in a test: %q, want the test's exchange named earlier in the test", why)
	}
	tr.End()
	between := NewTracker()
	between.Begin()
	between.End()
	n, _, _, _ = between.Test()
	between.Served(n, sent("mock-3", t0, "body.retries", "body.retries", "7", "7"))
	if why := classify(between); !strings.Contains(why, "appears earlier in the test, in mock-3 at body.retries") {
		t.Errorf("a call between tests: %q, want the previous test's exchange named earlier in the test", why)
	}
}
