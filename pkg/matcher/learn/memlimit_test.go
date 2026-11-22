package learn

import (
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestMemoryLimitFromAndClamp(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct {
		name                     string
		goLimit, cgroup, machine int64
		wantLimit, wantSetBudget int64
	}{
		{"GOMEMLIMIT unset uses machine memory (16 GiB)", math.MaxInt64, 0, 16 * gib, 16 * gib, maxSetBudget},
		{"GOMEMLIMIT unset uses machine memory (8 GiB)", math.MaxInt64, 0, 8 * gib, 8 * gib, 2 * gib},
		{"GOMEMLIMIT unset uses machine memory (1 GiB)", math.MaxInt64, 0, 1 * gib, 1 * gib, 256 << 20},
		{"GOMEMLIMIT below machine memory wins", 2 * gib, 0, 16 * gib, 2 * gib, gib / 2},
		{"GOMEMLIMIT above machine memory uses machine memory", 32 * gib, 0, 8 * gib, 8 * gib, 2 * gib},
		{"cgroup below machine memory wins", math.MaxInt64, 4 * gib, 16 * gib, 4 * gib, 1 * gib},
		{"machine memory below cgroup wins", math.MaxInt64, 32 * gib, 16 * gib, 16 * gib, maxSetBudget},
		{"all unknown falls to 0 and 64 MiB floor", math.MaxInt64, 0, 0, 0, minSetBudget},
		{"small machine memory floors at 64 MiB", math.MaxInt64, 0, 128 << 20, 128 << 20, minSetBudget},
		{"clamped MaxInt64 machine memory caps at 4 GiB maxSetBudget", math.MaxInt64, 0, math.MaxInt64, math.MaxInt64, maxSetBudget},
	} {
		got := memoryLimitFrom(c.goLimit, c.cgroup, c.machine)
		if got != c.wantLimit {
			t.Errorf("%s: memoryLimitFrom = %d, want %d", c.name, got, c.wantLimit)
		}
		if b := budgetOf(got); b != c.wantSetBudget {
			t.Errorf("%s: budgetOf(%d) = %d, want %d", c.name, got, b, c.wantSetBudget)
		}
	}

	if got := clampMemoryBytes(0); got != 0 {
		t.Errorf("clampMemoryBytes(0) = %d, want 0", got)
	}
	if got := clampMemoryBytes(8 << 30); got != 8<<30 {
		t.Errorf("clampMemoryBytes(8 GiB) = %d, want %d", got, int64(8<<30))
	}
	if got := clampMemoryBytes(uint64(math.MaxInt64) + 1); got != math.MaxInt64 {
		t.Errorf("clampMemoryBytes(MaxInt64+1) = %d, want MaxInt64", got)
	}
	if got := clampMemoryBytes(math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("clampMemoryBytes(MaxUint64) = %d, want MaxInt64", got)
	}

	// Overflowing MemTotal in kB must not wrap negative.
	dir := t.TempDir()
	overflow := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(overflow, []byte("MemTotal: 9223372036854775807 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := machineMemory(overflow); got != 0 {
		t.Errorf("machineMemory(overflow) = %d, want 0", got)
	}
}

// On every supported OS (linux, darwin, windows), hostMemory reads the
// machine's real physical RAM and memoryLimit / setBudget derive from it
// instead of falling back to the 64 MiB floor when GOMEMLIMIT is unset.
func TestNativeHostMemoryAndBudgetOnCurrentOS(t *testing.T) {
	hm := hostMemory()
	if hm <= 0 {
		t.Fatalf("hostMemory() = %d, want > 0 on this OS", hm)
	}
	// Restore process memory limit after testing GOMEMLIMIT unset and set.
	prev := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	limit := memoryLimit()
	if limit <= 0 || limit > hm {
		t.Fatalf("memoryLimit() with GOMEMLIMIT unset = %d, hostMemory() = %d; want 0 < limit <= hostMemory", limit, hm)
	}
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 0
	gotBudget := setBudget()
	wantBudget := budgetOf(limit)
	if gotBudget != wantBudget {
		t.Errorf("setBudget() = %d, want budgetOf(%d) = %d", gotBudget, limit, wantBudget)
	}
	// If no cgroup caps this process to <= 256 MiB and host RAM > 256 MiB,
	// the default budget must be strictly above the 64 MiB floor.
	if limit > 4*minSetBudget && gotBudget <= minSetBudget {
		t.Errorf("setBudget() = %d fell to 64 MiB floor (%d) despite limit = %d", gotBudget, minSetBudget, limit)
	}

	// Explicit GOMEMLIMIT below limit takes precedence with the same 1/4 fraction.
	tight := int64(512 << 20)
	if limit > tight {
		debug.SetMemoryLimit(tight)
		if l := memoryLimit(); l != tight {
			t.Errorf("memoryLimit() with GOMEMLIMIT=%d = %d, want %d", tight, l, tight)
		}
		if b := setBudget(); b != budgetOf(tight) {
			t.Errorf("setBudget() with GOMEMLIMIT=%d = %d, want %d", tight, b, budgetOf(tight))
		}
	}
	t.Logf("hostMemory=%d MiB, memoryLimit=%d MiB, setBudget=%d MiB", hm>>20, limit>>20, gotBudget>>20)
}
