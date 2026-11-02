package proxy

import (
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
)

// In a run with the suite and app-start marks, a connection the app opened
// outside a test (its pool at boot, before the first begin, or one opened
// between two tests) is served only the open test's mocks: its view is
// resolved from the app starts on every read, whenever it opened. (Without
// the marks a pid begin is refused: service/agent pidCannotScope.)
func TestAConnectionOpenedOutsideATestIsServedOnlyTheOpenTestsMocksWithTheMarks(t *testing.T) {
	mgr := livePool(t)
	pid := uint32(os.Getpid())
	p := &Proxy{}
	boot := p.scopedFor(pid, mgr)
	if boot == integrations.MockMemDb(mgr) {
		t.Fatal("a connection opened at boot kept the bare manager")
	}
	starts.Default.Begin(pid, "t1", "", false, time.Now())
	if got, want := readCounter(t, boot, 2), "a1,a2"; got != want {
		t.Fatalf("during t1 the boot connection served %s, want %s", got, want)
	}
	starts.Default.End(pid, "t1", false, time.Now())
	between := p.scopedFor(pid, mgr)
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	if got, want := readCounter(t, between, 3), "b1,b2,b2"; got != want {
		t.Fatalf("during t2 the connection opened between the tests served %s, want %s", got, want)
	}
	if got, want := readCounter(t, boot, 1), "b2"; got != want {
		t.Fatalf("during t2 the boot connection served %s, want %s", got, want)
	}
}
