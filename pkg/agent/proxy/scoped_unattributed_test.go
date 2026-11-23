package proxy

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// With the marks, a connection whose process began no test and descends from
// none that did (an app server the tests did not start, a call from a test
// process whose pids were sent from another pid namespace, or one whose pid
// the proxy could not read, 0) is never served the whole pool while a test is
// open: while exactly one is, it is served that test's mocks, as a begin
// without a pid serves every connection; while more than one is, keploy
// cannot tell which is its test, and its calls are refused, naming the pid.
func TestAConnectionOfNoTestProcessIsServedTheOneOpenTestOrRefused(t *testing.T) {
	// The test processes sent their pids from another pid namespace: neither
	// is this process or one of its ancestors.
	const worker, other = 3999991, 3999992
	for _, pid := range []uint32{uint32(os.Getpid()), 0} {
		t.Run(fmt.Sprint("pid ", pid), func(t *testing.T) {
			mgr := livePool(t)
			p := &Proxy{}
			conn := p.scopedFor(pid, mgr)
			if conn == integrations.MockMemDb(mgr) {
				t.Fatal("the connection kept the bare manager, which serves every test's mocks")
			}
			starts.Default.Begin(worker, "t2", "", false, time.Now())
			if got, want := liveKeyedNames(t, conn), "b1,b2"; got != want {
				t.Fatalf("with only t2 open a keyed walk showed %s, want %s", got, want)
			}
			if got, want := readCounter(t, conn, 3), "b1,b2,b2"; got != want {
				t.Fatalf("with only t2 open the connection served %s, want %s", got, want)
			}

			starts.Default.Begin(other, "t1", "", false, time.Now())
			for read, err := range map[string]error{
				"GetPerTestMocksInWindow":  errOfRead(conn.GetPerTestMocksInWindow()),
				"GetSessionScopedMocks":    errOfRead(conn.GetSessionScopedMocks()),
				"GetStartupMocks":          errOfRead(conn.GetStartupMocks()),
				"RangeSessionMocksWithKey": liveKeyedErr(conn),
			} {
				if err == nil {
					t.Fatalf("with t1 and t2 open, %s served the connection", read)
				}
				if !strings.Contains(err.Error(), "cannot tell which test") || !strings.Contains(err.Error(), "2 tests are open") {
					t.Fatalf("%s refused it with %q, which does not say keploy cannot tell its test", read, err)
				}
				if pid != 0 && !strings.Contains(err.Error(), fmt.Sprintf("pid %d", pid)) {
					t.Fatalf("%s refused it with %q, which does not name the pid", read, err)
				}
			}
			if resp, body, _ := serveOne(t, httpint.New(zap.NewNop()), conn, "GET /counter HTTP/1.1\r\nHost: api\r\n\r\n"); resp != nil {
				t.Fatalf("with t1 and t2 open the call was served %s", body)
			}

			starts.Default.End(other, "t1", false, time.Now())
			if got, want := readCounter(t, conn, 1), "b2"; got != want {
				t.Fatalf("with only t2 open again the connection served %s, want %s", got, want)
			}
			starts.Default.End(worker, "t2", false, time.Now())
		})
	}
}

// With the marks, a keyed walk of the session tier (MySQL's lookups by
// statement) shows a connection what its other reads show: the open test's
// recordings, not every test's.
func TestALiveKeyedSessionWalkShowsOnlyTheOpenTestsMocks(t *testing.T) {
	mgr := livePool(t)
	pid := uint32(os.Getpid())
	conn := (&Proxy{}).scopedFor(pid, mgr)
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	if got, want := liveKeyedNames(t, conn), "b1,b2"; got != want {
		t.Fatalf("during t2 a keyed walk showed %s, want %s", got, want)
	}
}

var liveCounterIndex = &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"counter"} }}

func liveKeyedNames(t *testing.T, db integrations.MockMemDb) string {
	t.Helper()
	var names []string
	if err := db.(integrations.SessionKeyReader).RangeSessionMocksWithKey(liveCounterIndex, "counter", func(mk *models.Mock) bool {
		names = append(names, mk.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ",")
}

func liveKeyedErr(db integrations.MockMemDb) error {
	return db.(integrations.SessionKeyReader).RangeSessionMocksWithKey(liveCounterIndex, "counter", func(*models.Mock) bool { return true })
}

func errOfRead(_ []*models.Mock, err error) error { return err }
