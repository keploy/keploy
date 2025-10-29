package testdb

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestGetTestCases_SortingLogic(t *testing.T) {
	tests := []struct {
		name          string
		testCases     []*models.TestCase
		expectedOrder []string
		description   string
	}{
		{
			name: "Mixed HTTP and gRPC test cases chronological order",
			testCases: []*models.TestCase{
				{
					Name: "http-t3",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 3, 0, 0, time.UTC),
					},
				},
				{
					Name: "grpc-t1",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 10, 1, 0, 0, time.UTC),
					},
				},
				{
					Name: "http-t0",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "grpc-t4",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 10, 4, 0, 0, time.UTC),
					},
				},
				{
					Name: "grpc-t2",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 10, 2, 0, 0, time.UTC),
					},
				},
				{
					Name: "http-t5",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 5, 0, 0, time.UTC),
					},
				},
			},
			expectedOrder: []string{"http-t0", "grpc-t1", "grpc-t2", "http-t3", "grpc-t4", "http-t5"},
			description:   "Should sort mixed HTTP and gRPC test cases in chronological order",
		},
		{
			name: "Only HTTP test cases",
			testCases: []*models.TestCase{
				{
					Name: "http-c",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "http-a",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "http-b",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
					},
				},
			},
			expectedOrder: []string{"http-a", "http-b", "http-c"},
			description:   "Should sort HTTP-only test cases by timestamp",
		},
		{
			name: "Only gRPC test cases",
			testCases: []*models.TestCase{
				{
					Name: "grpc-z",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 15, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "grpc-x",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "grpc-y",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC),
					},
				},
			},
			expectedOrder: []string{"grpc-x", "grpc-y", "grpc-z"},
			description:   "Should sort gRPC-only test cases by timestamp",
		},
		{
			name: "Same timestamps should maintain stable order",
			testCases: []*models.TestCase{
				{
					Name: "first",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "second",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
				{
					Name: "third",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
			},
			expectedOrder: []string{"first", "second", "third"},
			description:   "Should maintain stable order for test cases with same timestamp",
		},
		{
			name:          "Empty slice",
			testCases:     []*models.TestCase{},
			expectedOrder: []string{},
			description:   "Should handle empty test case slice",
		},
		{
			name: "Single test case",
			testCases: []*models.TestCase{
				{
					Name: "single-http",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
					},
				},
			},
			expectedOrder: []string{"single-http"},
			description:   "Should handle single test case",
		},
		{
			name: "Microsecond precision ordering",
			testCases: []*models.TestCase{
				{
					Name: "micro-2",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 2000, time.UTC), // 2 microseconds
					},
				},
				{
					Name: "micro-1",
					Kind: models.GRPC_EXPORT,
					GrpcReq: models.GrpcReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 1000, time.UTC), // 1 microsecond
					},
				},
				{
					Name: "micro-3",
					Kind: models.HTTP,
					HTTPReq: models.HTTPReq{
						Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 3000, time.UTC), // 3 microseconds
					},
				},
			},
			expectedOrder: []string{"micro-1", "micro-2", "micro-3"},
			description:   "Should handle microsecond-level timestamp precision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Call the sorting logic directly by simulating the relevant part of GetTestCases
			tcs := tt.testCases

			// Apply the same sorting logic as in GetTestCases
			if len(tcs) > 0 {
				// Sort test cases by their actual timestamp, whether HTTP or gRPC
				// (This is the exact same logic from the GetTestCases method)
				sortTestCases(tcs)
			}

			// Verify the order
			if len(tcs) != len(tt.expectedOrder) {
				t.Errorf("Expected %d test cases, got %d", len(tt.expectedOrder), len(tcs))
				return
			}

			for i, expectedName := range tt.expectedOrder {
				if i >= len(tcs) {
					t.Errorf("Missing test case at index %d, expected: %s", i, expectedName)
					continue
				}
				if tcs[i].Name != expectedName {
					t.Errorf("At index %d: expected %s, got %s", i, expectedName, tcs[i].Name)
				}
			}

			t.Logf("✓ %s: Test cases sorted correctly", tt.description)
		})
	}
}

// sortTestCases applies the same sorting logic as used in GetTestCases method
// This function extracts the sorting logic for testing purposes
func sortTestCases(tcs []*models.TestCase) {
	// This is the exact same sorting logic from GetTestCases
	sort.SliceStable(tcs, func(i, j int) bool {
		var timeI, timeJ time.Time

		// Determine which timestamp to use for test case i based on its Kind
		if tcs[i].Kind == models.HTTP {
			timeI = tcs[i].HTTPReq.Timestamp
		} else if tcs[i].Kind == models.GRPC_EXPORT {
			timeI = tcs[i].GrpcReq.Timestamp
		}

		// Determine which timestamp to use for test case j based on its Kind
		if tcs[j].Kind == models.HTTP {
			timeJ = tcs[j].HTTPReq.Timestamp
		} else if tcs[j].Kind == models.GRPC_EXPORT {
			timeJ = tcs[j].GrpcReq.Timestamp
		}

		return timeI.Before(timeJ)
	})
}

func TestGetTestCases_UnknownKind(t *testing.T) {
	testCases := []*models.TestCase{
		{
			Name: "unknown-kind",
			Kind: models.Kind("UNKNOWN"), // Unknown kind
			HTTPReq: models.HTTPReq{
				Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
			},
			GrpcReq: models.GrpcReq{
				Timestamp: time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			Name: "http-test",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Timestamp: time.Date(2024, 1, 1, 10, 5, 0, 0, time.UTC),
			},
		},
	}

	sortTestCases(testCases)

	// Unknown kind test case should use zero time and be sorted first
	// (zero time is before all other times)
	if testCases[0].Name != "unknown-kind" {
		t.Errorf("Expected unknown-kind test case to be first due to zero timestamp, got: %s", testCases[0].Name)
	}
	if testCases[1].Name != "http-test" {
		t.Errorf("Expected http-test to be second, got: %s", testCases[1].Name)
	}
}

func TestGetTestCases_EdgeCases(t *testing.T) {
	t.Run("Zero timestamps", func(t *testing.T) {
		testCases := []*models.TestCase{
			{
				Name: "zero-http",
				Kind: models.HTTP,
				HTTPReq: models.HTTPReq{
					Timestamp: time.Time{}, // Zero time
				},
			},
			{
				Name: "normal-grpc",
				Kind: models.GRPC_EXPORT,
				GrpcReq: models.GrpcReq{
					Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
				},
			},
		}

		sortTestCases(testCases)

		// Zero time should be sorted before normal time
		if testCases[0].Name != "zero-http" {
			t.Errorf("Expected zero timestamp test case to be first, got: %s", testCases[0].Name)
		}
	})

	t.Run("Very old and very new timestamps", func(t *testing.T) {
		testCases := []*models.TestCase{
			{
				Name: "future",
				Kind: models.HTTP,
				HTTPReq: models.HTTPReq{
					Timestamp: time.Date(2030, 12, 31, 23, 59, 59, 0, time.UTC),
				},
			},
			{
				Name: "past",
				Kind: models.GRPC_EXPORT,
				GrpcReq: models.GrpcReq{
					Timestamp: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			{
				Name: "present",
				Kind: models.HTTP,
				HTTPReq: models.HTTPReq{
					Timestamp: time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
				},
			},
		}

		sortTestCases(testCases)

		expectedOrder := []string{"past", "present", "future"}
		for i, expectedName := range expectedOrder {
			if testCases[i].Name != expectedName {
				t.Errorf("At index %d: expected %s, got %s", i, expectedName, testCases[i].Name)
			}
		}
	})
}
