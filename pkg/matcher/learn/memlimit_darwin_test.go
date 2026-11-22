//go:build darwin

package learn

import (
	"errors"
	"math"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

func TestDarwinHostMemoryQueryAndBudget(t *testing.T) {
	// Unit checks on darwinHostMemory: error, zero, normal sizes, overflow.
	var queried string
	if got := darwinHostMemory(func(name string) (uint64, error) {
		queried = name
		return 16 << 30, nil
	}); got != 16<<30 || queried != "hw.memsize" {
		t.Fatalf("darwinHostMemory(16 GiB) = %d (key %q), want %d (key hw.memsize)", got, queried, int64(16<<30))
	}
	if got := darwinHostMemory(func(string) (uint64, error) { return 0, errors.New("boom") }); got != 0 {
		t.Errorf("darwinHostMemory(error) = %d, want 0", got)
	}
	if got := darwinHostMemory(func(string) (uint64, error) { return 0, nil }); got != 0 {
		t.Errorf("darwinHostMemory(0) = %d, want 0", got)
	}
	if got := darwinHostMemory(func(string) (uint64, error) { return math.MaxUint64, nil }); got != math.MaxInt64 {
		t.Errorf("darwinHostMemory(MaxUint64) = %d, want MaxInt64", got)
	}

	// Live sysctl hw.memsize check on macOS.
	got := hostMemory()
	if got <= 0 {
		t.Fatalf("hostMemory() = %d, want > 0 on macOS", got)
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		t.Fatalf("sysctl -n hw.memsize: %v", err)
	}
	want, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse sysctl hw.memsize %q: %v", string(out), err)
	}
	if got != want {
		t.Fatalf("hostMemory() = %d, want sysctl hw.memsize = %d", got, want)
	}

	// With GOMEMLIMIT unset, memoryLimit must read real RAM and setBudget must
	// not fall to the 64 MiB floor.
	prev := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 0
	if l := memoryLimit(); l != want {
		t.Errorf("memoryLimit() with GOMEMLIMIT unset = %d, want %d", l, want)
	}
	if b := setBudget(); b != budgetOf(want) || b <= minSetBudget {
		t.Errorf("setBudget() = %d, want budgetOf(%d) = %d (> 64 MiB floor %d)", b, want, budgetOf(want), minSetBudget)
	}
	// On native macOS, a >64 MiB Mongo set must fit inside the default budget
	// without hitting HeldFull or FormsFull during WarmSet.
	mocks := floorMongoMocks(150)
	tr := NewTracker()
	tr.NewSet()
	use := <-tr.WarmSet(srcOf(mocks))
	if use.Budget != budgetOf(want) || use.Budget <= minSetBudget || use.Staged != len(mocks) || use.HeldFull || use.FormsFull {
		t.Errorf("native macOS WarmSet of Mongo set reported %+v; want budget=%d (>64 MiB), Staged=%d, neither full", use, budgetOf(want), len(mocks))
	}
	t.Logf("macOS hw.memsize=%d MiB, setBudget=%d MiB, WarmSet=%+v", want>>20, use.Budget>>20, use)
}
