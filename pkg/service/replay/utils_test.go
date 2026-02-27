package replay

import (
	"testing"
)

func TestIsMockSubset(t *testing.T) {
	tests := []struct {
		name     string
		subset   []string
		superset []string
		want     bool
	}{
		{
			name:     "Exact match",
			subset:   []string{"mock-1", "mock-2"},
			superset: []string{"mock-1", "mock-2"},
			want:     true,
		},
		{
			name:     "Subset is smaller than superset",
			subset:   []string{"mock-1"},
			superset: []string{"mock-1", "mock-2"},
			want:     true,
		},
		{
			name:     "Subset has element not in superset",
			subset:   []string{"mock-3"},
			superset: []string{"mock-1", "mock-2"},
			want:     false,
		},
		{
			name:     "Subset is larger than superset (not a subset)",
			subset:   []string{"mock-1", "mock-2", "mock-3"},
			superset: []string{"mock-1", "mock-2"},
			want:     false,
		},
		{
			name:     "Empty subset",
			subset:   []string{},
			superset: []string{"mock-1"},
			want:     true,
		},
		{
			name:     "Duplicate mocks in subset matching superset",
			subset:   []string{"mock-1", "mock-1"},
			superset: []string{"mock-1", "mock-1", "mock-2"},
			want:     true,
		},
		{
			name:     "Duplicate mocks in subset exceeding superset",
			subset:   []string{"mock-1", "mock-1", "mock-1"},
			superset: []string{"mock-1", "mock-1"},
			want:     false,
		},
		{
			name:     "User example: expected is subset of actual",
			subset:   []string{"mock-56", "mock-57", "mock-58"},
			superset: []string{"mock-22", "mock-23", "mock-56", "mock-57", "mock-58"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMockSubset(tt.subset, tt.superset); got != tt.want {
				t.Errorf("isMockSubset() = %v, want %v", got, tt.want)
			}
		})
	}
}
