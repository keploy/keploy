package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A follower registered for a kind is that kind's; a nil one unregisters it,
// and a kind with none has none.
func TestRegisterVariableFollower(t *testing.T) {
	const kind models.Kind = "RegisteredKind"
	if _, ok := VariableFollowerOf(kind); ok {
		t.Fatal("a kind nothing registered has a follower")
	}
	RegisterVariableFollower(kind, func(*models.Mock) (bool, string) { return true, "" })
	f, ok := VariableFollowerOf(kind)
	if !ok {
		t.Fatal("the registered follower is not found")
	}
	if follows, _ := f(&models.Mock{}); !follows {
		t.Fatal("another follower was found")
	}
	RegisterVariableFollower(kind, nil)
	if _, ok := VariableFollowerOf(kind); ok {
		t.Fatal("an unregistered follower is still found")
	}
}

// varsOnly is a store that keeps a variable table and no earlier set's
// bound values (no RoundBoundReader).
type varsOnly struct {
	MockMemDb
	t *variables.Table
}

func (s varsOnly) Variables() *variables.Table { return s.t }

// A store that keeps no earlier set's bound values reads those in force,
// as the call reads them: its own test's are the scope's of the test its
// mark names (variables.Table.OwnOf), not the next test's once it began;
// a call of no mark reads the running scope's.
func TestBoundValuesAtReadsTheBindingsOfTheTestTheCallWasMarkedIn(t *testing.T) {
	tb := &variables.Table{}
	tb.KeepEnded()
	db := varsOnly{t: tb}
	tb.StartOpeningTest("test-1", func() {}, nil, nil, nil)
	tb.Bind(variables.Binding{Name: "key", Value: "one", Recorded: "rec-1", Mock: "own-1", Place: "body.key"})
	mark := NewCallRound(1, nil).OfTest("test-1")
	tb.StartOpeningTest("test-2", func() {}, nil, nil, nil)
	tb.Bind(variables.Binding{Name: "other", Value: "two", Recorded: "rec-2", Mock: "own-2", Place: "body.key"})
	if got := BoundValuesAt(db, mark).Own(); len(got) != 1 || got[0].Value != "one" {
		t.Fatalf("test-1's call decided after test-2 began reads %+v as its own test's; want test-1's key=one", got)
	}
	if got := BoundValuesAt(db, CallRound{}).Own(); len(got) != 1 || got[0].Value != "two" {
		t.Fatalf("a call of no mark reads %+v as its own test's; want the running scope's other=two", got)
	}
	if got := BoundValuesAt(varsOnly{}, mark); got != nil {
		t.Fatalf("a store with no table reads %+v; want none", got)
	}
}
