package conn

import (
	"net/http"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestIsFiltered_FilterPolicy(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name     string
		filters  []models.Filter
		method   string
		path     string
		expected bool
	}{
		{
			name: "Only Exclude - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
		{
			name: "Only Exclude - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/data",
			expected: false,
		},
		{
			name: "Only Include - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // NOT filtered
		},
		{
			name: "Only Include - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true, // Filtered because it's not in the whitelist
		},
		{
			name: "Mixed - Include Match and Exclude Match (Exclude Wins)",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/admin",
			expected: true, // Filtered because Exclude takes priority
		},
		{
			name: "Mixed - Include Match and Exclude No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // Not filtered
		},
		{
			name: "Multiple Includes - One Matches",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/v1/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/v2/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/v2/users",
			expected: false, // Not filtered
		},
		{
			name: "Backward Compatibility - Default to Exclude",
			filters: []models.Filter{
				{
					BypassRule: models.BypassRule{Path: "/health"},
					// FilterPolicy is missing (defaults to empty string, which is Exclude logic)
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, "http://localhost"+tt.path, nil)
			opts := models.IncomingOptions{
				Filters: tt.filters,
			}
			got := IsFiltered(logger, req, opts)
			if got != tt.expected {
				t.Errorf("IsFiltered() = %v, want %v", got, tt.expected)
			}
		})
	}
}
