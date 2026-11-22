package models

import "testing"

// A mark is before a round when it was made in an earlier staging, whatever
// its boundary count, or earlier in the same staging; never in a later
// staging, whatever its count, nor at the round itself. A mark not known is
// before no round: such a miss is placed by when it was filed.
func TestAMissMarkIsBeforeOnlyAnEarlierRound(t *testing.T) {
	for _, c := range []struct {
		name string
		mark MissMark
		want bool
	}{
		{"an earlier staging, a higher count", MissMark{Epoch: 2, Bounds: 40, Known: true}, true},
		{"the same staging, a lower count", MissMark{Epoch: 3, Bounds: 9, Known: true}, true},
		{"the same staging, the same count", MissMark{Epoch: 3, Bounds: 10, Known: true}, false},
		{"the same staging, a higher count", MissMark{Epoch: 3, Bounds: 11, Known: true}, false},
		{"a later staging, a lower count", MissMark{Epoch: 4, Bounds: 5, Known: true}, false},
		{"not known", MissMark{}, false},
	} {
		if got := c.mark.Before(3, 10); got != c.want {
			t.Errorf("%s: %+v before staging 3, count 10: %v; want %v", c.name, c.mark, got, c.want)
		}
	}
}
