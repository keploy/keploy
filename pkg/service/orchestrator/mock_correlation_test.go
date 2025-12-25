package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMockCorrelationManager_DoubleCloseRaceCondition verifies that concurrent calls
// to UnregisterTest and closeAllChannels do not cause double-close panics.
func TestMockCorrelationManager_DoubleCloseRaceCondition(t *testing.T) {
	const iterations = 1000

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := zap.NewNop()
		globalMockCh := make(chan *models.Mock, 1000)
		mcm := NewMockCorrelationManager(ctx, globalMockCh, logger)

		testIDs := []string{"test-1", "test-2", "test-3"}
		for _, testID := range testIDs {
			testCtx := TestContext{
				TestID:  testID,
				TestSet: "test-set-1",
			}
			mcm.RegisterTest(testCtx)
		}

		if count := mcm.GetActiveTestCount(); count != len(testIDs) {
			t.Fatalf("Expected %d active tests, got %d", len(testIDs), count)
		}

		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PANIC in UnregisterTest(test-1) (iteration %d): %v", i, r)
					panic(r)
				}
			}()
			mcm.UnregisterTest("test-1")
		}()

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PANIC in UnregisterTest(test-2) (iteration %d): %v", i, r)
					panic(r)
				}
			}()
			mcm.UnregisterTest("test-2")
		}()

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PANIC in closeAllChannels (iteration %d): %v", i, r)
					panic(r)
				}
			}()
			mcm.closeAllChannels()
		}()

		wg.Wait()

		if (i+1)%100 == 0 {
			t.Logf("Completed %d/%d iterations", i+1, iterations)
		}
	}
}

// TestMockCorrelationManager_DoubleCloseRaceCondition_Exposed demonstrates
// that closing a channel twice causes a panic.
func TestMockCorrelationManager_DoubleCloseRaceCondition_Exposed(t *testing.T) {
	doneCh := make(chan struct{})
	mockCh := make(chan *models.Mock, 100)

	close(doneCh)
	close(mockCh)

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				t.Logf("Double-close panic: %v", r)
			}
		}()
		close(doneCh)
		close(mockCh)
	}()

	if !panicked {
		t.Fatal("Expected panic from double-close, but none occurred")
	}
}

// TestMockCorrelationManager_CloseAfterUnregister verifies that calling
// closeAllChannels after UnregisterTest does not panic.
func TestMockCorrelationManager_CloseAfterUnregister(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zap.NewNop()
	globalMockCh := make(chan *models.Mock, 1000)
	mcm := NewMockCorrelationManager(ctx, globalMockCh, logger)

	testID := "test-1"
	testCtx := TestContext{
		TestID:  testID,
		TestSet: "test-set-1",
	}
	mcm.RegisterTest(testCtx)
	mcm.UnregisterTest(testID)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Unexpected panic when closing after unregister: %v", r)
		}
	}()

	mcm.closeAllChannels()
}

// TestMockCorrelationManager_DoubleCloseRaceCondition_MultipleTests verifies
// concurrent UnregisterTest and closeAllChannels with multiple registered tests.
func TestMockCorrelationManager_DoubleCloseRaceCondition_MultipleTests(t *testing.T) {
	const iterations = 50
	const numTests = 5

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := zap.NewNop()
		globalMockCh := make(chan *models.Mock, 1000)
		mcm := NewMockCorrelationManager(ctx, globalMockCh, logger)

		testIDs := make([]string, numTests)
		for j := 0; j < numTests; j++ {
			testID := fmt.Sprintf("test-%d", j)
			testIDs[j] = testID
			testCtx := TestContext{
				TestID:  testID,
				TestSet: "test-set-1",
			}
			mcm.RegisterTest(testCtx)
		}

		if count := mcm.GetActiveTestCount(); count != numTests {
			t.Fatalf("Expected %d active tests, got %d", numTests, count)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PANIC in UnregisterTest loop (iteration %d): %v", i, r)
					panic(r)
				}
			}()
			for _, testID := range testIDs {
				mcm.UnregisterTest(testID)
			}
		}()

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("PANIC in closeAllChannels (iteration %d): %v", i, r)
					panic(r)
				}
			}()
			mcm.closeAllChannels()
		}()

		wg.Wait()

		if (i+1)%10 == 0 {
			t.Logf("Completed %d/%d iterations", i+1, iterations)
		}
	}
}

// TestMockCorrelationManager_DoubleCloseRaceCondition_Stress runs a stress test
// with many concurrent operations.
func TestMockCorrelationManager_DoubleCloseRaceCondition_Stress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	const iterations = 200
	const concurrentGoroutines = 10

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := zap.NewNop()
		globalMockCh := make(chan *models.Mock, 1000)
		mcm := NewMockCorrelationManager(ctx, globalMockCh, logger)

		testID := "test-1"
		testCtx := TestContext{
			TestID:  testID,
			TestSet: "test-set-1",
		}
		mcm.RegisterTest(testCtx)

		var wg sync.WaitGroup
		wg.Add(concurrentGoroutines)

		for j := 0; j < concurrentGoroutines; j++ {
			go func(routineID int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						t.Logf("PANIC in goroutine %d (iteration %d): %v", routineID, i, r)
						panic(r)
					}
				}()
				if routineID%2 == 0 {
					mcm.UnregisterTest(testID)
				} else {
					mcm.closeAllChannels()
				}
			}(j)
		}

		wg.Wait()

		if (i+1)%20 == 0 {
			t.Logf("Completed %d/%d iterations", i+1, iterations)
		}
	}
}

