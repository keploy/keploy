package models

import "testing"

// TestRecordedNumberReadsOnlyTheNamesMockdbWrites: mockdb names a mock
// "mock-<N>" as it writes it, so only that form, with decimal digits that
// fit the counter, carries a place in the recording.
func TestRecordedNumberReadsOnlyTheNamesMockdbWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    uint64
		ok   bool
	}{
		{"mock-0", 0, true},
		{"mock-19", 19, true},
		{"mock-007", 7, true},
		{"mock-9999999999999999999", 9999999999999999999, true},
		{"mock-10000000000000000000", 0, false}, // 20 digits: past any counter
		{"mock-", 0, false},
		{"mock", 0, false},
		{"", 0, false},
		{"task-12", 0, false},
		{"Mock-1", 0, false},
		{"mock-1a", 0, false},
		{"mock-a1", 0, false},
		{"mock-+1", 0, false},
		{"mock--1", 0, false},
		{"mock- 1", 0, false},
	} {
		n, ok := RecordedNumber(tc.name)
		if n != tc.n || ok != tc.ok {
			t.Errorf("RecordedNumber(%q) = %d, %v; want %d, %v", tc.name, n, ok, tc.n, tc.ok)
		}
	}
}

// TestCompareRecordedNumbersIsTheWriteOrder: two numbered names compare by
// number, not as text (mock-2 before mock-10); a numbered name before one
// with no number; a tie when neither has one or both have one number.
func TestCompareRecordedNumbersIsTheWriteOrder(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"mock-2", "mock-10", -1},
		{"mock-10", "mock-2", 1},
		{"mock-0", "mock-1", -1},
		{"mock-3", "mock-3", 0},
		{"mock-007", "mock-7", 0},
		{"mock-1", "custom", -1},
		{"custom", "mock-1", 1},
		{"mock-0", "custom", -1},
		{"custom", "other", 0},
	} {
		if got := CompareRecordedNumbers(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareRecordedNumbers(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
