package replayer

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"gopkg.in/yaml.v3"
)

// yamlRoundTrip puts a value through the same serializer the mock file goes
// through, and hands back whatever concrete type comes out the other side.
// The point is to prove the type erasure rather than hard-code the constant
// it produces.
func yamlRoundTrip(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	var out interface{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return out
}

// A FLOAT parameter read off the wire is a float32. The same parameter read
// back out of the mock YAML is a float64, because yaml.v3 resolves an
// untagged scalar by its shape and never yields float32. paramValueEqual sees
// that mixed pair on every FLOAT comparison, so a recorded FLOAT param has to
// match itself across it.
func TestParamValueEqual_FloatParamMatchesItselfAcrossYAML(t *testing.T) {
	nc := util.NewNoiseChecker(nil)

	// Fractional values only. A whole-numbered float32 comes back as an int
	// and takes a different arm — covered separately below.
	for _, want := range []float32{9.99, -0.1, 1.5, 3.4e38, 1e-9} {
		restored := yamlRoundTrip(t, want)

		if _, ok := restored.(float64); !ok {
			t.Fatalf("%v: expected YAML to restore a float64, got %T — "+
				"this test is asserting the wrong thing", want, restored)
		}

		// Both argument orders: which side is the live param and which is
		// the recorded one is not fixed at the call sites.
		if !paramValueEqual(want, restored, nc) {
			t.Errorf("live float32 %v vs recorded %v (%T): no match, want match",
				want, restored, restored)
		}
		if !paramValueEqual(restored, want, nc) {
			t.Errorf("recorded %v (%T) vs live float32 %v: no match, want match",
				restored, restored, want)
		}
	}
}

// A whole-numbered FLOAT param is erased harder: yaml.v3 writes float32(2)
// as "2", which resolves back to int, not float64. That pair goes through the
// float32/int arm, which was already correct — this pins it so the erasure
// stays covered.
func TestParamValueEqual_WholeFloatParamRestoresAsInt(t *testing.T) {
	nc := util.NewNoiseChecker(nil)

	for _, want := range []float32{0, 2, -7, 1000} {
		restored := yamlRoundTrip(t, want)

		if _, ok := restored.(int); !ok {
			t.Fatalf("%v: expected YAML to restore an int, got %T", want, restored)
		}
		if !paramValueEqual(want, restored, nc) {
			t.Errorf("live float32 %v vs recorded %v (%T): no match, want match",
				want, restored, restored)
		}
		if !paramValueEqual(restored, want, nc) {
			t.Errorf("recorded %v (%T) vs live float32 %v: no match, want match",
				restored, restored, want)
		}
	}
}

// Narrowing must not make the comparison blind: values that are genuinely
// different at float32 precision still have to compare unequal.
func TestParamValueEqual_DistinctFloatsStillDiffer(t *testing.T) {
	nc := util.NewNoiseChecker(nil)

	cases := []struct {
		a float32
		b float64
	}{
		{9.99, 10.5},
		{9.99, -9.99},
		{0, 1e-30},
		{1.5, 1.6},
	}
	for _, c := range cases {
		if paramValueEqual(c.a, c.b, nc) {
			t.Errorf("float32(%v) vs float64(%v): matched, want no match", c.a, c.b)
		}
		if paramValueEqual(c.b, c.a, nc) {
			t.Errorf("float64(%v) vs float32(%v): matched, want no match", c.b, c.a)
		}
	}
}

// A DOUBLE parameter is float64 on both sides and must keep full precision —
// the fix narrows only the mixed float32/float64 arms, not this one.
func TestParamValueEqual_DoubleKeepsFullPrecision(t *testing.T) {
	nc := util.NewNoiseChecker(nil)

	// These two are equal as float32 and distinct as float64. If DOUBLE
	// comparison ever started narrowing, this would match.
	a := 9.99
	b := 9.989999771118164
	if a == b {
		t.Fatal("test constants are equal as float64; pick different ones")
	}
	if paramValueEqual(a, b, nc) {
		t.Error("two distinct float64 values matched: DOUBLE comparison lost precision")
	}
	if !paramValueEqual(a, yamlRoundTrip(t, a), nc) {
		t.Error("float64 did not match itself across YAML")
	}
}
