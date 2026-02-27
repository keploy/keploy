package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestIsMockSubsetWithConfig(t *testing.T) {
	tests := []struct {
		name          string
		consumedMocks []models.MockState
		expectedMocks []string
		want          bool
	}{
		{
			name: "Exact match",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
				{Name: "mock-2", Type: "test"},
			},
			expectedMocks: []string{"mock-1", "mock-2"},
			want:          true,
		},
		{
			name: "User example: extra config mocks",
			consumedMocks: []models.MockState{
				{Name: "mock-22", Type: "config"},
				{Name: "mock-23", Type: "config"},
				{Name: "mock-56", Type: "test"},
				{Name: "mock-57", Type: "test"},
				{Name: "mock-58", Type: "test"},
			},
			expectedMocks: []string{"mock-56", "mock-57", "mock-58"},
			want:          true,
		},
		{
			name: "Extra non-config mock (mismatch)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
				{Name: "mock-2", Type: "test"},
			},
			expectedMocks: []string{"mock-1"},
			want:          false,
		},
		{
			name: "Missing expected mocks (allowed)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
			},
			expectedMocks: []string{"mock-1", "mock-2"},
			want:          true,
		},
		{
			name: "Extra config mock only",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "config"},
			},
			expectedMocks: []string{},
			want:          true,
		},
		{
			name: "Extra non-config mock only (mismatch)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
			},
			expectedMocks: []string{},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMockSubsetWithConfig(tt.consumedMocks, tt.expectedMocks); got != tt.want {
				t.Errorf("isMockSubsetWithConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}
