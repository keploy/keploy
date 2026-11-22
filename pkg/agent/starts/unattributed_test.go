package starts

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A call whose process began no test and descends from none that did (an app
// server the tests did not start, a call from a test process whose pids were
// sent from another pid namespace, or pid 0, one the proxy could not read) is
// never shown the whole pool while a test is open: while exactly one is, it is
// that test's (its mocks, the runner's, and those mapped to no test; no boot,
// which belongs to an app start), its scope and ownership that test's too;
// while more than one is, it is refused, naming the pid; while none is, it
// is no test's (every recording, no scope, nothing owned), as before the
// first test. A call of a test process keeps its own test.
func TestACallOfNoTestProcessIsTheOneOpenTestsOrRefused(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	r.SetTable("/r", map[string]models.SetTable{"web": {
		Boots:  map[string][]string{"server#1": {"boot"}},
		Tests:  map[string][]models.Owned{"login": {{Name: "auth"}}, "cart": {{Name: "add"}}},
		Runner: []string{"warmup"},
	}})
	// The test processes sent their pids, 7 and 8, from their own pid
	// namespace; the proxy reads the app's in the kernel's: 4321, under
	// 4000, under 1.
	f.spawn(4000, 1, at(0), "node")
	f.spawn(4321, 4000, at(5), "server")
	calls := []uint32{4321, 0}

	r.Begin(7, "web", "/r/web", true, at(1))
	noTestOpen := func(when string, ms int) {
		t.Helper()
		for _, pid := range calls {
			if _, _, ok, err := r.View(pid, at(ms)); ok || err != nil {
				t.Fatalf("pid %d, %s: view ok %v err %v; want no view (every recording), as before", pid, when, ok, err)
			}
			if got := r.Scope(pid); got != "" {
				t.Fatalf("pid %d, %s: scope %q, want none", pid, when, got)
			}
			if owned, known := r.Owns(pid, "auth"); owned || known {
				t.Fatalf("pid %d, %s: Owns(auth) = %v, %v; want false, false", pid, when, owned, known)
			}
		}
	}
	noTestOpen("no test open", 10)

	r.Begin(7, "login", "/r/web", false, at(20))
	for _, pid := range calls {
		rank, _, ok, err := r.View(pid, at(30))
		if err != nil || !ok || rank["auth"] != 1 || rank["warmup"] != 2 || hasAny(rank, "add", "boot") {
			t.Fatalf("pid %d, login open: view %v ok %v err %v; want login's auth and the runner's warmup only", pid, rank, ok, err)
		}
		if got, want := r.Scope(pid), r.Scope(7); got != want || !strings.HasPrefix(got, "t") {
			t.Fatalf("pid %d, login open: scope %q, want login's %q", pid, got, want)
		}
		if owned, known := r.Owns(pid, "auth"); !owned || !known {
			t.Fatalf("pid %d, login open: Owns(auth) = %v, %v; want true, true", pid, owned, known)
		}
		if owned, known := r.Owns(pid, "add"); owned || !known {
			t.Fatalf("pid %d, login open: Owns(add) = %v, %v; want false, true", pid, owned, known)
		}
	}

	r.Begin(8, "cart", "/r/web", false, at(40))
	for _, pid := range calls {
		rank, _, ok, err := r.View(pid, at(50))
		var u *Unattributed
		if ok || rank != nil || !errors.As(err, &u) || u.PID != pid || u.Open != 2 {
			t.Fatalf("pid %d, login and cart open: view %v ok %v err %v; want refused, naming the pid and 2 open tests", pid, rank, ok, err)
		}
		if !strings.Contains(err.Error(), "cannot tell which test") || !strings.Contains(err.Error(), "2 tests are open") {
			t.Fatalf("pid %d: refusal %q does not say keploy cannot tell its test among 2", pid, err)
		}
		who := fmt.Sprintf("the call from pid %d", pid)
		if pid == 0 {
			who = "a call whose pid the proxy could not read"
		}
		if !strings.Contains(err.Error(), who) {
			t.Fatalf("pid %d: refusal %q does not say %q", pid, err, who)
		}
		if got := r.Scope(pid); got != "" {
			t.Fatalf("pid %d, refused: scope %q, want none", pid, got)
		}
		if owned, known := r.Owns(pid, "auth"); owned || known {
			t.Fatalf("pid %d, refused: Owns(auth) = %v, %v; want false, false", pid, owned, known)
		}
	}
	// A test process's own call keeps its test.
	if rank, _, ok, err := r.View(8, at(50)); err != nil || !ok || rank["add"] != 1 || hasAny(rank, "auth") {
		t.Fatalf("cart's own view %v ok %v err %v; want cart's add only", rank, ok, err)
	}

	r.End(7, "login", false, at(60))
	for _, pid := range calls {
		if rank, _, ok, err := r.View(pid, at(70)); err != nil || !ok || rank["add"] != 1 || hasAny(rank, "auth") {
			t.Fatalf("pid %d, only cart open: view %v ok %v err %v; want cart's add", pid, rank, ok, err)
		}
	}
	r.End(8, "cart", false, at(80))
	noTestOpen("no test open again", 90)
}
