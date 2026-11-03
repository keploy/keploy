package replayer

import (
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// The learner reads a COM_QUERY's inline literals by position, a quoted one
// as text and a number as a number, as the statement holds them.
func TestALiteralCallReadsAQuotedLiteralAsTextAndANumberAsANumber(t *testing.T) {
	got := literalLeaves("SELECT name FROM tabNote WHERE id = 7 AND owner = 'a@x.io' AND score > 1.5 AND weight < 2.5e3 LIMIT 20")
	want := map[string]learn.Leaf{
		"body.query.literals.0": {Path: "body.query.literals.0", Type: learn.Number, Value: "7"},
		"body.query.literals.1": {Path: "body.query.literals.1", Type: learn.String, Value: "a@x.io"},
		"body.query.literals.2": {Path: "body.query.literals.2", Type: learn.Number, Value: "1.5"},
		"body.query.literals.3": {Path: "body.query.literals.3", Type: learn.Number, Value: "2.5e3"},
		"body.query.literals.4": {Path: "body.query.literals.4", Type: learn.Number, Value: "20"},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d literals %v; want %v", len(got), got, want)
	}
	for p, w := range want {
		if g := got[p]; g.Path != w.Path || g.Type != w.Type || g.Value != w.Value {
			t.Fatalf("%s is %+v; want %+v", p, g, w)
		}
	}
	if literalLeaves("SELEC name FRM") != nil {
		t.Fatal("a statement vitess cannot parse has literals")
	}
}
