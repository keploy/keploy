package rowscols

import (
	"math"
	"testing"
)

// coerceToInt64 is handed whatever a serializer produced for an
// integer column. When that is a float it cannot represent exactly,
// converting anyway is implementation-defined in Go and saturates on
// amd64 — 1e300 and 1.8e19 both become MinInt64, which the unsigned
// branch then writes as a real-looking row value. Erroring keeps a
// corrupt mock from replaying as a plausible wrong number.
func TestCoerceToInt64RejectsUnrepresentableFloats(t *testing.T) {
	for _, v := range []interface{}{
		float64(1e300),
		float64(-1e300),
		float64(math.MaxUint64),
		float64(9.5),
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		float32(1e30),
		float32(0.5),
	} {
		if got, err := coerceToInt64(v); err == nil {
			t.Errorf("coerceToInt64(%T %v) = %d, want an error", v, v, got)
		}
	}
}

func TestCoerceToInt64AcceptsRepresentableFloats(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int64
	}{
		{float64(0), 0},
		{float64(42), 42},
		{float64(-42), -42},
		{float64(1 << 53), 1 << 53},
		{float32(2), 2},
		{float32(-7), -7},
	}
	for _, c := range cases {
		got, err := coerceToInt64(c.in)
		if err != nil {
			t.Errorf("coerceToInt64(%T %v): %v", c.in, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceToInt64(%T %v) = %d, want %d", c.in, c.in, got, c.want)
		}
	}
}

// BIGINT UNSIGNED above MaxInt64 arrives as uint64, not as a float,
// and has to keep its bit pattern through the signed intermediate.
func TestCoerceToInt64PreservesUint64BitPattern(t *testing.T) {
	for _, want := range []uint64{math.MaxUint64, math.MaxUint64 - 1, math.MaxInt64 + 1} {
		got, err := coerceToInt64(want)
		if err != nil {
			t.Fatalf("coerceToInt64(uint64 %d): %v", want, err)
		}
		if uint64(got) != want {
			t.Errorf("coerceToInt64(uint64 %d) round-tripped to %d", want, uint64(got))
		}
	}
}

// coerceToFloat64 widens a float32 by plain conversion, NOT by routing
// it through its shortest decimal form. Asserting the narrowing round
// trip alone would pass either way — float32→shortest-decimal→float64
// is exact by construction — so pin the widened value itself.
func TestCoerceToFloat64WidensFloat32ByConversion(t *testing.T) {
	got, err := coerceToFloat64(float32(9.99))
	if err != nil {
		t.Fatalf("coerceToFloat64(float32 9.99): %v", err)
	}
	// float64(float32(9.99)), not the 9.99 a human wrote.
	if want := 9.989999771118164; got != want {
		t.Errorf("coerceToFloat64(float32 9.99) = %v, want %v", got, want)
	}

	// Whatever the widening, narrowing back at the FLOAT encode site
	// must be exact.
	for _, want := range []float32{9.99, -0.1, 0, 3.4e38, 1e-9} {
		got, err := coerceToFloat64(want)
		if err != nil {
			t.Fatalf("coerceToFloat64(float32 %v): %v", want, err)
		}
		if float32(got) != want {
			t.Errorf("coerceToFloat64(float32 %v) = %v, narrows back to %v", want, got, float32(got))
		}
	}
}

// The bound exactInt64 exists to get right: exclusive against 2^63
// rather than inclusive against MaxInt64, because float64(MaxInt64)
// rounds up to 2^63 and so never names MaxInt64 itself. Without these,
// relaxing the check to `f > float64(math.MaxInt64)` keeps the suite
// green while letting an out-of-range value through.
func TestExactInt64Boundaries(t *testing.T) {
	const twoPow63 = 9223372036854775808.0

	accept := []struct {
		in   float64
		want int64
	}{
		{-twoPow63, math.MinInt64},                   // exactly representable
		{9223372036854774784.0, 9223372036854774784}, // largest float64 below 2^63
		{0, 0},
		{-1, -1},
	}
	for _, c := range accept {
		got, err := exactInt64(c.in)
		if err != nil {
			t.Errorf("exactInt64(%v): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("exactInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}

	reject := []float64{
		twoPow63,               // == float64(math.MaxInt64); not representable as int64
		float64(math.MaxInt64), // the same value, spelled the way a reader expects
		-twoPow63 - 2048,       // first float64 step below MinInt64
	}
	for _, f := range reject {
		if got, err := exactInt64(f); err == nil {
			t.Errorf("exactInt64(%v) = %d, want an error", f, got)
		}
	}
}
