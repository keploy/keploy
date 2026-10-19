package testdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
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

// TestGetTestCases_KeepsTestCasesNamedAfterMocks guards against the mocks-file
// skip matching by substring: a test case recorded for a /mocks/* route (named
// e.g. get-x-mocks-1) must be read back, while a mocks.yaml in tests/ is still
// skipped.
func TestGetTestCases_KeepsTestCasesNamedAfterMocks(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), t.TempDir(), NamingDescriptive)
	testSetID := "test-set-0"

	for _, name := range []string{"get-x-mocks-1", "get-users-1"} {
		tc := httpTC("GET", "http://api.test/"+name)
		tc.Name = name
		if err := ts.InsertTestCase(t.Context(), tc, testSetID, false); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	// A mocks.yaml inside tests/ is skipped by its exact name; were it read,
	// it would load as a third test case.
	testsDir := filepath.Join(ts.TcsPath, testSetID, "tests")
	data, err := os.ReadFile(filepath.Join(testsDir, "get-users-1.yaml"))
	if err != nil {
		t.Fatalf("read get-users-1.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "mocks.yaml"), data, 0o644); err != nil {
		t.Fatalf("write mocks.yaml: %v", err)
	}

	tcs, err := ts.GetTestCases(t.Context(), testSetID)
	if err != nil {
		t.Fatalf("GetTestCases: %v", err)
	}
	var got []string
	for _, tc := range tcs {
		got = append(got, tc.Name)
	}
	sort.Strings(got)
	want := []string{"get-users-1", "get-x-mocks-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("GetTestCases names = %v, want %v", got, want)
	}
}

// TestInsertTestCase_WritesFilePartCarriedInTheTestCase covers a test case
// persisted by another process than the one that captured it (dsmon F19: the
// DaemonSet agent streams it to k8s-proxy's control plane): its file part
// arrives as bytes in FileContents, not as a temp path. The asset is written
// from them byte for byte, the stored test case points at it, and the bytes
// themselves are stored in neither format.
func TestInsertTestCase_WritesFilePartCarriedInTheTestCase(t *testing.T) {
	file := []byte{'P', 'K', 0x03, 0x04, 0xff, 0xfe, 0x00, 0x80} // not valid UTF-8
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			keployPath := t.TempDir()
			db := NewWithFormat(zap.NewNop(), keployPath, format)
			tc := &models.TestCase{
				Kind: models.HTTP,
				HTTPReq: models.HTTPReq{
					Method: "POST",
					URL:    "http://localhost:8080/dedup/bytecode",
					Header: map[string]string{"Content-Type": "multipart/form-data"},
					Form: []models.FormData{
						{Key: "namespace", Values: []string{"ns"}},
						{Key: "classes", Paths: []string{""}, FileNames: []string{"classes.zip"}, FileContents: [][]byte{file}},
					},
				},
				HTTPResp: models.HTTPResp{StatusCode: 200},
			}
			if err := db.InsertTestCase(context.Background(), tc, "test-set-0", false); err != nil {
				t.Fatalf("insert test case: %v", err)
			}

			form := tc.HTTPReq.Form[1]
			if len(form.Paths) != 1 || form.FileContents != nil {
				t.Fatalf("file part after insert: paths %q, contents kept %v", form.Paths, form.FileContents != nil)
			}
			onDisk, err := os.ReadFile(filepath.Join(keployPath, form.Paths[0]))
			if err != nil || !bytes.Equal(onDisk, file) {
				t.Fatalf("asset %s = %q (err %v), want %q", form.Paths[0], onDisk, err, file)
			}

			stored, err := db.GetTestCases(context.Background(), "test-set-0")
			if err != nil || len(stored) != 1 {
				t.Fatalf("stored test cases = %d (err %v), want 1", len(stored), err)
			}
			if got := stored[0].HTTPReq.Form[1].Paths; len(got) != 1 || got[0] != form.Paths[0] {
				t.Fatalf("stored paths = %q, want [%q]", got, form.Paths[0])
			}
			files, err := filepath.Glob(filepath.Join(keployPath, "test-set-0", "tests", "*"))
			if err != nil || len(files) != 1 {
				t.Fatalf("test case files = %v (err %v), want 1", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("file_contents")) {
				t.Fatalf("the stored test case carries the file bytes:\n%s", raw)
			}
		})
	}
}
