//go:build windows

package learn

import (
	"math"
	"runtime/debug"
	"testing"
	"unsafe"
)

func TestWindowsHostMemoryQueryAndBudget(t *testing.T) {
	if sz := unsafe.Sizeof(memoryStatusEx{}); sz != 64 {
		t.Fatalf("unsafe.Sizeof(memoryStatusEx{}) = %d, want 64", sz)
	}

	// Unit checks on windowsHostMemory: dwLength init, failure, zero, normal, overflow.
	var gotLen uint32
	if got := windowsHostMemory(func(ms *memoryStatusEx) bool {
		gotLen = ms.length
		ms.totalPhys = 16 << 30
		return true
	}); got != 16<<30 || gotLen != 64 {
		t.Fatalf("windowsHostMemory(16 GiB) = %d (dwLength=%d), want %d (dwLength=64)", got, gotLen, int64(16<<30))
	}
	if got := windowsHostMemory(func(*memoryStatusEx) bool { return false }); got != 0 {
		t.Errorf("windowsHostMemory(false) = %d, want 0", got)
	}
	if got := windowsHostMemory(func(ms *memoryStatusEx) bool { ms.totalPhys = 0; return true }); got != 0 {
		t.Errorf("windowsHostMemory(0) = %d, want 0", got)
	}
	if got := windowsHostMemory(func(ms *memoryStatusEx) bool { ms.totalPhys = math.MaxUint64; return true }); got != math.MaxInt64 {
		t.Errorf("windowsHostMemory(MaxUint64) = %d, want MaxInt64", got)
	}

	// Live GlobalMemoryStatusEx check on Windows.
	got := hostMemory()
	if got <= 4*minSetBudget {
		t.Fatalf("hostMemory() = %d, want > %d (256 MiB) on native Windows", got, 4*minSetBudget)
	}

	// With GOMEMLIMIT unset, memoryLimit must read real RAM and setBudget must
	// not fall to the 64 MiB floor.
	prev := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 0
	if l := memoryLimit(); l != got {
		t.Errorf("memoryLimit() with GOMEMLIMIT unset = %d, want %d", l, got)
	}
	if b := setBudget(); b != budgetOf(got) || b <= minSetBudget {
		t.Errorf("setBudget() = %d, want budgetOf(%d) = %d (> 64 MiB floor %d)", b, got, budgetOf(got), minSetBudget)
	}
	// On native Windows, a >64 MiB Mongo set must fit inside the default budget
	// without hitting HeldFull or FormsFull during WarmSet.
	mocks := floorMongoMocks(150)
	tr := NewTracker()
	tr.NewSet()
	use := <-tr.WarmSet(srcOf(mocks))
	if use.Budget != budgetOf(got) || use.Budget <= minSetBudget || use.Staged != len(mocks) || use.HeldFull || use.FormsFull {
		t.Errorf("native Windows WarmSet of Mongo set reported %+v; want budget=%d (>64 MiB), Staged=%d, neither full", use, budgetOf(got), len(mocks))
	}
	t.Logf("Windows totalPhys=%d MiB, setBudget=%d MiB, WarmSet=%+v", got>>20, use.Budget>>20, use)
}
