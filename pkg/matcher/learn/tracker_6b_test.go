package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func beforeNames(t *Tracker) []string {
	_, _, _, before := t.Test()
	var out []string
	for _, m := range before {
		out = append(out, m.Name)
	}
	return out
}

// A mock served while no test is open (the start-up, between tests) is an
// earlier place of every later test, once; one served in a test is one of
// the next test only, as before.
func TestTrackerKeepsWhatWasServedOutsideATestForEveryLaterTest(t *testing.T) {
	tr := NewTracker()
	tr.ServedMock(&models.Mock{Name: "startup"})
	tr.Begin() // test 1
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"startup"}) {
		t.Fatalf("test 1: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-1"})
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"})
	tr.ServedMock(&models.Mock{Name: "startup"}) // served again
	tr.Begin()                                   // test 2
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-1", "between", "startup"}) {
		t.Fatalf("test 2: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-2"})
	tr.End()
	tr.Begin() // test 3
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-2", "startup", "between"}) {
		t.Fatalf("test 3: before %v, want test 2's own and everything served outside a test", got)
	}
}

// A new test set forgets what the previous set served: its mocks reuse the
// previous set's names (mock-0 again), and none of the previous set's is an
// earlier place of the new set's tests. Without NewSet, a mock is still kept
// by its identity, not its name: another set's mock-0 never hides this one's.
func TestTrackerTellsTestSetsApart(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 16, s, 0, 0, 0, time.UTC) }
	set1 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(1)}}
	set2 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(2)}}
	for _, newSet := range []bool{true, false} {
		tr := NewTracker()
		tr.ServedMock(set1)
		tr.Begin()
		tr.End()
		if newSet {
			tr.NewSet()
		}
		tr.ServedMock(set2)
		tr.Begin()
		tr.End()
		tr.Begin() // set 2, test 2
		_, _, _, before := tr.Test()
		var has1, has2 bool
		for _, m := range before {
			has1, has2 = has1 || m == set1, has2 || m == set2
		}
		if !has2 || has1 == newSet {
			t.Errorf("NewSet %v: set 2's test 2 has set 1's mock-0 %v, set 2's %v", newSet, has1, has2)
		}
	}
}

// A value the running test made is kept for that test alone, at the first
// place it appeared: a value recorded equal is not noted again, nor one that
// is empty or of another type than a string or a number, nor one noted for a
// test that has ended. The next test begins with none.
func TestTrackerKeepsTheValuesATestMade(t *testing.T) {
	tr := NewTracker()
	tr.Mint(0, Minted{Mock: "m", Place: "body.a", Recorded: "x", Live: "y", Type: String})
	if got := tr.Minted(0); len(got) != 0 {
		t.Fatalf("no test is open: %+v", got)
	}
	tr.Begin()
	n, _, _, _ := tr.Test()
	first := Minted{Mock: "mock-1", Place: "body.name", Recorded: "e2e-1", Live: "e2e-2", Type: String}
	tr.Mint(n, first)
	tr.Mint(n, Minted{Mock: "mock-2", Place: "body.input.name", Recorded: "e2e-1", Live: "e2e-2", Type: String})
	tr.Mint(n, Minted{Mock: "mock-2", Place: "body.flag", Recorded: "true", Live: "false", Type: Bool})
	tr.Mint(n, Minted{Mock: "mock-2", Place: "body.empty", Recorded: "a", Live: "", Type: String})
	tr.Mint(n+1, Minted{Mock: "mock-2", Place: "body.other", Recorded: "o-1", Live: "o-2", Type: String})
	if got := tr.Minted(n); !reflect.DeepEqual(got, []Minted{first}) {
		t.Fatalf("minted %+v, want only the first place of e2e-1", got)
	}
	if got := tr.Minted(n + 1); got != nil {
		t.Fatalf("another test's: %+v", got)
	}
	tr.End()
	tr.Mint(n, Minted{Mock: "mock-3", Place: "body.late", Recorded: "l-1", Live: "l-2", Type: String})
	if got := tr.Minted(n); len(got) != 1 {
		t.Fatalf("a test that ended makes nothing more: %+v", got)
	}
	tr.Begin()
	n2, _, _, _ := tr.Test()
	if got := tr.Minted(n2); len(got) != 0 {
		t.Fatalf("the next test begins with none: %+v", got)
	}
}

// Under a claim, what the learn step served while no test was open, and the
// values those calls made, are the claim key's: kept across the key's tests
// (each test begins without its predecessor's, not without the key's), and
// dropped by the next claim and the next set. Nothing is kept with no claim
// or while a test is open. The key's variables are named apart from every
// test's of the key, and a test's apart from the key's.
func TestTrackerKeepsWhatTheClaimKeyMadeOutsideItsTests(t *testing.T) {
	tr := NewTracker()
	pool := Earlier{Mock: "pool-0", Own: true}
	made := Minted{Mock: "pool-0", Place: "body.name", Recorded: "r-1", Live: "r-2", Type: String}
	tr.ServedOutside(pool)
	tr.MintOutside(made)
	if e, m := tr.Outside(); e != nil || m != nil || tr.Keyed() {
		t.Fatalf("with no claim nothing is the key's: %+v %+v", e, m)
	}
	if _, ok := tr.OutsideName("pool-0", "body.name"); ok {
		t.Fatal("no key's name with no claim")
	}

	tr.NewKey("e2e/roles.spec.ts")
	tr.ServedOutside(pool)
	tr.MintOutside(made)
	tr.MintOutside(Minted{Mock: "pool-1", Place: "body.again", Recorded: "r-1", Live: "r-2", Type: String})
	e, m := tr.Outside()
	if len(e) != 1 || !e[0].Outside || e[0].Own || len(m) != 1 || !m[0].Outside || m[0].Mock != "pool-0" {
		t.Fatalf("the key's: %+v %+v", e, m)
	}
	name, ok := tr.OutsideName("pool-0", "body.name")
	if !ok || name != "name" {
		t.Fatalf("the key's name %q %v", name, ok)
	}
	if again, _ := tr.OutsideName("pool-0", "body.name"); again != name {
		t.Fatal("one place, one name")
	}

	tr.Begin()
	n, _, _, _ := tr.Test()
	tr.ServedOutside(Earlier{Mock: "mock-1"})
	tr.MintOutside(Minted{Mock: "mock-1", Place: "body.x", Recorded: "x-1", Live: "x-2", Type: String})
	if e, m := tr.Outside(); len(e) != 1 || len(m) != 1 {
		t.Fatalf("a test's calls are not the key's: %+v %+v", e, m)
	}
	if own, _ := tr.Name(n, "mock-1", "body.input.name", nil); own != "name-2" {
		t.Fatalf("a test's variable named %q: the key's name is taken", own)
	}
	tr.End()
	if other, _ := tr.OutsideName("pool-2", "body.name"); other != "name-3" {
		t.Fatalf("another key's variable named %q: the key's tests took name-2", other)
	}
	tr.Begin()
	if e, _ := tr.Outside(); len(e) != 1 {
		t.Fatal("the next test of the key still reads it")
	}

	tr.End()
	tr.NewKey("e2e/roles.spec.ts") // the test process runs again
	if e, m := tr.Outside(); len(e) != 0 || len(m) != 0 {
		t.Fatalf("a new claim starts afresh: %+v %+v", e, m)
	}
	if name, _ := tr.OutsideName("pool-0", "body.name"); name != "name" {
		t.Fatalf("names start afresh: %q", name)
	}
	tr.ServedOutside(pool)
	tr.NewSet()
	if e, _ := tr.Outside(); len(e) != 0 || tr.Keyed() {
		t.Fatal("a new set has no claim yet")
	}
}
