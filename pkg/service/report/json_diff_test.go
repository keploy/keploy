package report

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateTableDiff_EmptyContainers(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		actual      string
		expectDiff  bool
		contains    []string
		exactOutput string
	}{
		{
			name:        "root empty object vs empty array",
			expected:    "{}",
			actual:      "[]",
			expectDiff:  true,
			exactOutput: "Path: $\n  Expected: {}\n  Actual: []",
		},
		{
			name:        "nested empty array vs empty object",
			expected:    `{"items": []}`,
			actual:      `{"items": {}}`,
			expectDiff:  true,
			exactOutput: "Path: items\n  Expected: []\n  Actual: {}",
		},
		{
			name:       "added empty object",
			expected:   `{"status": "ok"}`,
			actual:     `{"status": "ok", "added": {}}`,
			expectDiff: true,
			contains: []string{
				"Path: added",
				"Expected: <added>",
				"Actual: {}",
			},
		},
		{
			name:       "removed empty array",
			expected:   `{"status": "ok", "removed": []}`,
			actual:     `{"status": "ok"}`,
			expectDiff: true,
			contains: []string{
				"Path: removed",
				"Expected: []",
				"Actual: <removed>",
			},
		},
		{
			name:       "empty vs non-empty container",
			expected:   `{"items": []}`,
			actual:     `{"items": [1]}`,
			expectDiff: true,
			contains: []string{
				"Path: items",
				"Expected: []",
				"Actual: <removed>",
				"Path: items[0]",
				"Expected: <added>",
				"Actual: 1",
			},
		},
		{
			name:        "identical empty objects",
			expected:    "{}",
			actual:      "{}",
			expectDiff:  false,
			exactOutput: "No differences found in JSON body after flattening.",
		},
		{
			name:        "identical empty arrays",
			expected:    "[]",
			actual:      "[]",
			expectDiff:  false,
			exactOutput: "No differences found in JSON body after flattening.",
		},
		{
			name:        "identical nested empty containers",
			expected:    `{"a": {}, "b": []}`,
			actual:      `{"a": {}, "b": []}`,
			expectDiff:  false,
			exactOutput: "No differences found in JSON body after flattening.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diff, err := GenerateTableDiff(tc.expected, tc.actual)
			require.NoError(t, err)

			if !tc.expectDiff {
				assert.Equal(t, "No differences found in JSON body after flattening.", diff)
				if tc.exactOutput != "" {
					assert.Equal(t, tc.exactOutput, diff)
				}
				return
			}

			if tc.exactOutput != "" {
				assert.Equal(t, tc.exactOutput, diff)
			}
			for _, sub := range tc.contains {
				assert.Contains(t, diff, sub)
			}
		})
	}
}
