//go:build linux

package linux

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

func tcpSock(state uint8, inode uint32, cookie uint64, local, remote string, dataSegs uint64) util.TCPSocket {
	s := util.TCPSocket{State: state, Inode: inode, Cookie: cookie, Local: netip.MustParseAddrPort(local), DataSegs: dataSegs, DataKnown: true}
	if remote != "" {
		s.Remote = netip.MustParseAddrPort(remote)
	}
	return s
}

// The census counts the application's established connections whose peer
// is not keploy's: a caller's to a port the application listens on, and its
// own to a dependency, but not one to a port keploy passes through, not
// keploy's forward to it or its connection to keploy's proxy, and no other
// process's.
func TestTheCensusCountsTheApplicationsConnectionsKeployDoesNotSee(t *testing.T) {
	const est, lis = util.TCPEstablished, util.TCPListen
	socks := []util.TCPSocket{
		tcpSock(lis, 10, 1, "0.0.0.0:8080", "", 0),                               // the app listens
		tcpSock(est, 11, 2, "10.0.0.5:8080", "10.0.0.7:40000", 0),                // a caller's, opened in the outage
		tcpSock(est, 12, 3, "127.0.0.1:8080", "127.0.0.1:41000", 0),              // keploy's forward: the app's end
		tcpSock(est, 90, 4, "127.0.0.1:41000", "127.0.0.1:8080", 0),              // keploy's forward: keploy's end
		tcpSock(est, 13, 5, "10.0.0.5:42000", "10.0.0.9:5432", 0),                // to a dependency, opened in the outage
		tcpSock(est, 14, 6, "127.0.0.1:43000", "127.0.0.1:16789", 0),             // to keploy's proxy
		tcpSock(est, 91, 7, "127.0.0.1:16789", "127.0.0.1:43000", 0),             // keploy's proxy's end
		tcpSock(est, 15, 8, "10.0.0.5:44000", "10.0.0.9:9200", 0),                // to a pass-through port
		tcpSock(est, 50, 9, "10.0.0.5:45000", "10.0.0.9:5432", 0),                // another process's
		tcpSock(est, 0, 10, "10.0.0.5:46000", "10.0.0.9:5432", 0),                // no file holds it
		tcpSock(est, 16, 11, "[fd00::5]:8080", "[fd00::7]:40001", 0),             // a caller's over IPv6
		tcpSock(est, 17, 12, "[fd00::5]:47000", "[fd00::9]:5432", 0),             // to a dependency over IPv6
		tcpSock(lis, 51, 13, "0.0.0.0:9000", "", 0),                              // another process listens
		tcpSock(est, 18, 14, "10.0.0.5:48000", "10.0.0.7:9000", 0),               // the app to that one
		tcpSock(est, 52, 15, "10.0.0.7:9000", "10.0.0.5:48000", 0),               // its end
		tcpSock(est, 19, 16, "127.0.0.1:49000", "127.0.0.1:8080", 0),             // the app to itself
		tcpSock(est, 20, 17, "127.0.0.1:8080", "127.0.0.1:49000", 0),             // and its end of that
		tcpSock(est, 21, 18, "[::ffff:10.0.0.5]:8080", "[::ffff:10.0.0.8]:1", 0), // a caller's, on a dual-stack socket
	}
	for i := range socks {
		socks[i].Local = netip.AddrPortFrom(socks[i].Local.Addr().Unmap(), socks[i].Local.Port())
		socks[i].Remote = netip.AddrPortFrom(socks[i].Remote.Addr().Unmap(), socks[i].Remote.Port())
	}
	app := map[uint32]bool{10: true, 11: true, 12: true, 13: true, 14: true, 15: true, 16: true, 17: true, 18: true, 19: true, 20: true, 21: true}
	own := map[uint32]bool{90: true, 91: true}
	inbound, egress := unseenSplit(socks, app, own, func(p uint16) bool { return p == 9200 })
	cookies := func(ss []util.TCPSocket) []uint64 {
		var out []uint64
		for _, s := range ss {
			out = append(out, s.Cookie)
		}
		return out
	}
	if got, want := fmt.Sprint(cookies(inbound)), fmt.Sprint([]uint64{2, 11, 17, 18}); got != want {
		t.Fatalf("callers' connections %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(cookies(egress)), fmt.Sprint([]uint64{5, 12, 14, 16}); got != want {
		t.Fatalf("connections to dependencies %s, want %s", got, want)
	}
}

// Each read charges once every watched connection to a dependency that
// carried data since the read before, one that closed since by what it
// carried up to its close, and names one of them; one that closed is watched
// no more, and calls on callers' connections are counted apart, for the log.
// With nothing left to watch, nothing is read and the ends are not watched.
func TestAConnectionKeployDoesNotSeeIsChargedToEachReadItCarriedDataBefore(t *testing.T) {
	const est = util.TCPEstablished
	var table []util.TCPSocket
	reads := 0
	h := NewHooks(zap.NewNop(), &config.Config{})
	ends := &fakeEnds{}
	h.unseen.testSource = &unseenSource{
		sockets: func(uint32) ([]util.TCPSocket, error) { reads++; return table, nil },
		app:     func() (map[uint32]bool, error) { return map[uint32]bool{11: true, 13: true}, nil },
		own:     func() (map[uint32]bool, error) { return map[uint32]bool{}, nil },
		ends:    func() (unseenEnds, error) { return ends, nil },
	}
	table = []util.TCPSocket{
		tcpSock(util.TCPListen, 11, 1, "0.0.0.0:8080", "", 0),
		tcpSock(est, 11, 2, "10.0.0.5:8080", "10.0.0.7:40000", 100),
		tcpSock(est, 13, 5, "10.0.0.5:42000", "10.0.0.9:5432", 500),
	}
	if l := h.unseenLosses(); len(l) != 0 {
		t.Fatalf("losses %+v at the census, want none: nothing carried data since", l)
	}
	if fmt.Sprint(ends.only) != fmt.Sprint([]uint64{2, 5}) {
		t.Fatalf("the ends watched %v, want the census's connections [2 5]", ends.only)
	}
	table[1].DataSegs, table[2].DataSegs = 150, 600
	want := models.CaptureLosses{{Kind: models.CaptureLossUnseenConnection, Count: 1, What: "a connection from local port 42000 to 10.0.0.9:5432"}}
	if l := h.CaptureLosses(); fmt.Sprint(l) != fmt.Sprint(want) {
		t.Fatalf("the hooks' capture losses %+v, want %+v", l, want)
	}
	if l := h.unseenLosses(); fmt.Sprint(l) != fmt.Sprint(want) {
		t.Fatalf("losses %+v after a read with no data, want the count unchanged %+v", l, want)
	}
	if h.unseen.callsIn != 1 {
		t.Fatalf("callers' reads with data %d, want 1", h.unseen.callsIn)
	}
	// The dependency's connection carries a call and closes before the next
	// read, its caller's closes with nothing more: the dependency's is
	// charged by its end, the caller's is not counted.
	table = nil
	ends.ended = map[uint64]util.TCPSocket{2: {Cookie: 2, DataSegs: 150, DataKnown: true}, 5: {Cookie: 5, DataSegs: 640, DataKnown: true}}
	closed := models.CaptureLosses{{Kind: models.CaptureLossUnseenConnection, Count: 2, What: "a connection from local port 42000 to 10.0.0.9:5432, closed since"}}
	if l := h.unseenLosses(); fmt.Sprint(l) != fmt.Sprint(closed) {
		t.Fatalf("losses %+v after the connection closed, want %+v", l, closed)
	}
	if h.unseen.callsIn != 1 {
		t.Fatalf("callers' reads with data %d after its connection closed with nothing more, want 1", h.unseen.callsIn)
	}
	if !ends.closed {
		t.Fatal("the ends were still watched with no connection left to watch")
	}
	n := reads
	h.unseenLosses()
	if reads != n {
		t.Fatal("the table was read again with no connection left to watch")
	}
	if len(h.unseen.egress)+len(h.unseen.inbound) != 0 {
		t.Fatalf("closed connections still watched: %+v %+v", h.unseen.egress, h.unseen.inbound)
	}
}

// A census that cannot read the tables charges nothing and is not taken
// again.
func TestACensusThatCannotReadTheTablesChargesNothing(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	calls := 0
	h.unseen.testSource = &unseenSource{
		sockets: func(uint32) ([]util.TCPSocket, error) { calls++; return nil, errors.New("no sock_diag") },
		app:     func() (map[uint32]bool, error) { return nil, nil },
		own:     func() (map[uint32]bool, error) { return nil, nil },
	}
	if l := h.unseenLosses(); len(l) != 0 {
		t.Fatalf("losses %+v, want none", l)
	}
	h.unseenLosses()
	if calls != 1 {
		t.Fatalf("the census read the table %d times, want once", calls)
	}
}

// The application's sockets are those of the processes the hooks take as
// its: started at or after the recording's start, or the PID namespace's
// first; never keploy's own process or one running keploy's own file.
func TestTheApplicationsSocketsAreThoseOfItsProcesses(t *testing.T) {
	proc := t.TempDir()
	self := filepath.Join(proc, "self")
	mustMkdir(t, self)
	exe := filepath.Join(self, "exe")
	if err := os.WriteFile(exe, []byte("keploy"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(proc, "otherexe")
	if err := os.WriteFile(other, []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	// start: 5 s after boot; a tick is 10 ms.
	const start = 5_000_000_000
	proc1 := func(pid int, ticks uint64, exeOf string, inodes ...uint32) {
		dir := filepath.Join(proc, fmt.Sprint(pid))
		mustMkdir(t, filepath.Join(dir, "fd"))
		stat := fmt.Sprintf("%d (a (b) c) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0", pid, ticks)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(exeOf, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		for i, ino := range inodes {
			if err := os.Symlink(fmt.Sprintf("socket:[%d]", ino), filepath.Join(dir, "fd", fmt.Sprint(i+3))); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/dev/null", filepath.Join(dir, "fd", "0")); err != nil {
			t.Fatal(err)
		}
	}
	proc1(1, 10, other, 1)             // the namespace's first: the app's, whenever it started
	proc1(40, 499, other, 40)          // started before the recording: not the app's
	proc1(41, 500, other, 41, 42)      // started as it did
	proc1(42, 900, exe, 43)            // runs keploy's own file
	proc1(os.Getpid(), 900, other, 44) // keploy's own process
	got, err := appSocketInodes(proc, start)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[uint32]bool{1: true, 41: true, 42: true}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the application's sockets %v, want %v", got, want)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// fakeEnds stands for util.TCPEnds: ended is what its sockets' ends say.
type fakeEnds struct {
	only   []uint64
	ended  map[uint64]util.TCPSocket
	closed bool
}

func (f *fakeEnds) Only(cookies []uint64) {
	f.only = append([]uint64(nil), cookies...)
	sort.Slice(f.only, func(i, j int) bool { return f.only[i] < f.only[j] })
}

func (f *fakeEnds) Ended(cookies []uint64, _ time.Duration) map[uint64]util.TCPSocket {
	out := map[uint64]util.TCPSocket{}
	for _, c := range cookies {
		if s, ok := f.ended[c]; ok {
			out[c] = s
		}
	}
	return out
}

func (f *fakeEnds) Close() { f.closed = true }

// A watched connection gone from the table is charged by its end: when it
// carried data since the last read, not when it carried none; and when its end
// is not known (lost, or no watch), as one that carried data, so the call it
// may have carried is never left uncharged.
func TestAClosedConnectionIsChargedByItsEnd(t *testing.T) {
	conn := func() map[uint64]unseenConn {
		return map[uint64]unseenConn{7: {local: netip.MustParseAddrPort("10.0.0.5:42000"), remote: netip.MustParseAddrPort("10.0.0.9:5432"), dataSegs: 600}}
	}
	for _, c := range []struct {
		name  string
		ended map[uint64]util.TCPSocket
		n     uint64
		what  string
	}{
		{"carried data", map[uint64]util.TCPSocket{7: {Cookie: 7, DataSegs: 700, DataKnown: true}}, 1, "a connection from local port 42000 to 10.0.0.9:5432, closed since"},
		{"carried none", map[uint64]util.TCPSocket{7: {Cookie: 7, DataSegs: 600, DataKnown: true}}, 0, ""},
		{"end not known", nil, 1, "a connection from local port 42000 to 10.0.0.9:5432, which closed before keploy could read what it carried"},
		{"end with no counts", map[uint64]util.TCPSocket{7: {Cookie: 7}}, 1, "a connection from local port 42000 to 10.0.0.9:5432, which closed before keploy could read what it carried"},
	} {
		w := conn()
		n, what := advanceUnseen(w, nil, c.ended)
		if n != c.n || what != c.what || len(w) != 0 {
			t.Errorf("%s: charged %d (%q), %d still watched; want %d (%q), none watched", c.name, n, what, len(w), c.n, c.what)
		}
	}
}

// The agent's own census source watches the connections' ends (so a
// connection that closes between two reads is charged by what it carried).
func TestTheAgentsCensusWatchesTheConnectionsEnds(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	h.agentInfoSent = true
	h.agentInfo.RecordingStartTime = 1
	src, ok := h.unseenSource()
	if !ok || src.ends == nil {
		t.Fatalf("the census source (taken %v) watches no ends", ok)
	}
}

// The census watches the ends before it reads the table: a connection that
// closes in between is not lost to either.
func TestTheCensusWatchesTheEndsBeforeItReadsTheTable(t *testing.T) {
	var order []string
	h := NewHooks(zap.NewNop(), &config.Config{})
	h.unseen.testSource = &unseenSource{
		sockets: func(uint32) ([]util.TCPSocket, error) {
			order = append(order, "table")
			return []util.TCPSocket{tcpSock(util.TCPEstablished, 13, 5, "10.0.0.5:42000", "10.0.0.9:5432", 500)}, nil
		},
		app:  func() (map[uint32]bool, error) { return map[uint32]bool{13: true}, nil },
		own:  func() (map[uint32]bool, error) { return map[uint32]bool{}, nil },
		ends: func() (unseenEnds, error) { order = append(order, "ends"); return &fakeEnds{}, nil },
	}
	h.censusUnseen()
	if len(order) < 2 || order[0] != "ends" || order[1] != "table" {
		t.Fatalf("the census did %v, want the ends watched, then the table read", order)
	}
}

// A watched connection that is closing (either end closed: FIN_WAIT1,
// FIN_WAIT2, CLOSE_WAIT, LAST_ACK, CLOSING) is still in the table with its
// counts, and read there: not charged while it carries nothing, charged when
// it does, and watched on.
func TestAClosingConnectionIsReadInTheTableNotByItsEnd(t *testing.T) {
	for _, state := range []uint8{util.TCPFinWait1, util.TCPFinWait2, util.TCPCloseWait, util.TCPLastAck, util.TCPClosing} {
		table := []util.TCPSocket{
			tcpSock(util.TCPListen, 11, 1, "0.0.0.0:8080", "", 0),
			tcpSock(util.TCPEstablished, 13, 5, "10.0.0.5:42000", "10.0.0.9:5432", 500),
		}
		h := NewHooks(zap.NewNop(), &config.Config{})
		h.unseen.testSource = &unseenSource{
			sockets: func(states uint32) ([]util.TCPSocket, error) {
				var out []util.TCPSocket
				for _, s := range table {
					if states&(1<<s.State) != 0 {
						out = append(out, s)
					}
				}
				return out, nil
			},
			app:  func() (map[uint32]bool, error) { return map[uint32]bool{11: true, 13: true}, nil },
			own:  func() (map[uint32]bool, error) { return map[uint32]bool{}, nil },
			ends: func() (unseenEnds, error) { return &fakeEnds{}, nil },
		}
		h.unseenLosses()
		table[1].State = state
		if l := h.unseenLosses(); len(l) != 0 || len(h.unseen.egress) != 1 {
			t.Fatalf("state %d: losses %+v, %d watched, with nothing more carried; want none, 1", state, l, len(h.unseen.egress))
		}
		table[1].DataSegs = 501
		if l := h.unseenLosses(); len(l) != 1 || l[0].Count != 1 || len(h.unseen.egress) != 1 {
			t.Fatalf("state %d: losses %+v, %d watched, after it carried data; want one charged, 1", state, l, len(h.unseen.egress))
		}
	}
}
