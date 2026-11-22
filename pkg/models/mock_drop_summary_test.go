package models

import "testing"

// A drop is a mock dropped for memory pressure, or for the output; a summary
// of neither has none.
func TestAMockDropSummaryCountsEitherKind(t *testing.T) {
	for _, c := range []struct {
		d    MockDropSummary
		want bool
	}{
		{MockDropSummary{}, false},
		{MockDropSummary{Named: []string{"named, not counted"}}, false},
		{MockDropSummary{ByPressure: 1}, true},
		{MockDropSummary{Capacity: 1}, true},
	} {
		if got := c.d.Dropped(); got != c.want {
			t.Errorf("%+v.Dropped() = %v, want %v", c.d, got, c.want)
		}
	}
}
