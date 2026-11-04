package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A learned value (models.MockVariable.Of) goes with its bind when two
// declarations of one variable in one call merge: taken with a bind the
// first lacks, kept where both bind naming one value, dropped where they
// name two or one names none.
func TestMergeVariable_OfGoesWithItsBind(t *testing.T) {
	const of, other = "6f9619ff-8b86-4011-b42d-00c04fc964ff", "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
	bound := models.MockVariable{Recorded: "4011", Bind: "body.id#2", Of: of}
	use := models.MockVariable{Recorded: "4011", Use: []string{"body.x"}}
	for _, c := range []struct {
		name string
		a, b models.MockVariable
		want string
	}{
		{"a bind, then a use", bound, use, of},
		{"a use, then a bind", use, bound, of},
		{"two binds naming one value", bound, bound, of},
		{"two binds naming two values", bound, models.MockVariable{Recorded: "4011", Bind: "body.id#2", Of: other}, ""},
		{"a bind, then one naming none", bound, models.MockVariable{Recorded: "4011", Bind: "body.id#2"}, ""},
	} {
		if got := MergeVariable(c.a, c.b).Of; got != c.want {
			t.Errorf("%s: Of %q, want %q", c.name, got, c.want)
		}
	}
}
