package agent

import "testing"

// An instance value is its id in the low 10 bits and the claim's generation
// above them; the id reads back from any generation, and a message names a
// later claim of an id apart from its first.
func TestAnInstanceValueCarriesItsIdAndGeneration(t *testing.T) {
	for _, c := range []struct {
		id, gen, value uint32
		label          string
	}{
		{7, 0, 7, "7"},
		{7, 2, 2<<10 | 7, "7 (claim 3 of that id)"},
		{MaxInstanceID, MaxInstanceGeneration, 0xffffffff, "1023 (claim 4194304 of that id)"},
	} {
		if got := InstanceValue(c.id, c.gen); got != c.value {
			t.Errorf("InstanceValue(%d, %d) = %d, want %d", c.id, c.gen, got, c.value)
		}
		if got := InstanceIDOf(c.value); got != c.id {
			t.Errorf("InstanceIDOf(%d) = %d, want %d", c.value, got, c.id)
		}
		if got := InstanceLabel(c.value); got != c.label {
			t.Errorf("InstanceLabel(%d) = %q, want %q", c.value, got, c.label)
		}
	}
}
