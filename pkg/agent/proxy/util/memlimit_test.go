package util

import (
	"sync"
	"testing"

	"go.uber.org/zap"
)

func TestNewMemoryLimiterZeroIsNil(t *testing.T) {
	ml := NewMemoryLimiter(0, zap.NewNop())
	if ml != nil {
		t.Fatal("limit=0 should return nil (unlimited)")
	}
}

func TestNewMemoryLimiterNegativeIsNil(t *testing.T) {
	ml := NewMemoryLimiter(-100, zap.NewNop())
	if ml != nil {
		t.Fatal("negative limit should return nil (unlimited)")
	}
}

func TestNilMemoryLimiterMethodsAreSafe(t *testing.T) {
	var ml *MemoryLimiter

	// All methods should be safe to call on nil.
	if !ml.TryAcquire(1000) {
		t.Fatal("nil TryAcquire should return true")
	}
	ml.Release(1000) // should not panic
	if ml.IsExceeded() {
		t.Fatal("nil IsExceeded should return false")
	}
	if ml.Usage() != 0 {
		t.Fatal("nil Usage should return 0")
	}
	if ml.Limit() != 0 {
		t.Fatal("nil Limit should return 0")
	}
}

func TestTryAcquireAndRelease(t *testing.T) {
	ml := NewMemoryLimiter(100, zap.NewNop())

	if !ml.TryAcquire(60) {
		t.Fatal("should acquire 60/100")
	}
	if ml.Usage() != 60 {
		t.Fatalf("expected usage=60, got %d", ml.Usage())
	}

	if !ml.TryAcquire(30) {
		t.Fatal("should acquire 30 more (90/100)")
	}

	// This should fail — 90 + 20 > 100.
	if ml.TryAcquire(20) {
		t.Fatal("should NOT acquire 20 more (would be 110/100)")
	}
	// Usage should still be 90 (rolled back).
	if ml.Usage() != 90 {
		t.Fatalf("expected usage=90 after failed acquire, got %d", ml.Usage())
	}

	if !ml.IsExceeded() {
		t.Fatal("should be exceeded after failed TryAcquire")
	}

	// Release enough to go below 90% threshold (90).
	ml.Release(10) // 80 < 90 → exceeded should clear
	if ml.IsExceeded() {
		t.Fatal("should no longer be exceeded after release below 90%")
	}
}

func TestHysteresis(t *testing.T) {
	ml := NewMemoryLimiter(100, zap.NewNop())

	// Fill to limit.
	ml.TryAcquire(100)
	// Try one more — sets exceeded.
	ml.TryAcquire(1)
	if !ml.IsExceeded() {
		t.Fatal("should be exceeded")
	}

	// Release just 1 byte — still at 99, above 90% threshold (90).
	ml.Release(1)
	if !ml.IsExceeded() {
		t.Fatal("should still be exceeded at 99 (above 90% of 100)")
	}

	// Release 10 more — now at 89, below 90%.
	ml.Release(10)
	if ml.IsExceeded() {
		t.Fatal("should clear exceeded at 89 (below 90% of 100)")
	}
}

func TestConcurrentAccess(t *testing.T) {
	ml := NewMemoryLimiter(10000, zap.NewNop())
	var wg sync.WaitGroup

	// 100 goroutines each acquire and release 50 bytes.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ml.TryAcquire(50) {
				ml.Release(50)
			}
		}()
	}
	wg.Wait()

	if ml.Usage() != 0 {
		t.Fatalf("expected usage=0 after all releases, got %d", ml.Usage())
	}
}

func TestReleaseUnderflowGuard(t *testing.T) {
	ml := NewMemoryLimiter(100, zap.NewNop())
	ml.TryAcquire(10)
	ml.Release(20) // release more than acquired

	if ml.Usage() != 0 {
		t.Fatalf("expected usage=0 after underflow guard, got %d", ml.Usage())
	}
}
