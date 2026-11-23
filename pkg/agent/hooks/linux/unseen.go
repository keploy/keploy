//go:build linux

package linux

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// Connections keploy does not see: what the application opened while no
// keploy agent ran.
//
// keploy takes a connection when it begins: the connect hooks redirect the
// application's connect() to the proxy, and the ingress lookup steers a
// caller's SYN to the park. A connection begun while no agent ran (the agent
// died and its container is restarting) is the application's and its peer's
// alone for its whole life: a pooled connection to a dependency, a caller's
// keep-alive or HTTP/2 connection. Every call on it passes keploy by:
//   - on a connection to a dependency, a test the application serves after
//     the restart calls it unseen, so the test is recorded without that
//     call's mock, and a replay of it cannot be judged;
//   - on a caller's connection to the application, the call is not
//     recorded at all.
// Only a connection that ends can be begun again through keploy, and keploy
// never ends the application's connections (resetting one fails the calls on
// it). So the restarted agent counts them instead (censusUnseen), once it
// records again: each established TCP socket of the application's
// (unseenApp: the processes the hooks take as the application's) whose peer
// is not one of keploy's own sockets, and is not a pass-through port's. It
// says how many at WARN, and watches their data from then on (the count of
// segments each carried data in: util.TCPSocket.DataSegs): each
// test in which a connection to a dependency carried data is charged
// models.CaptureLossUnseenConnection, which leaves it without a verdict, and
// the calls on callers' connections are counted for the log. A connection
// that closes is read by its end (what it carried up to its close, as the
// kernel sends it when it destroys the socket), then watched no more; the
// application opens the next one through keploy.

// unseenConn is one connection keploy does not see, as last read.
type unseenConn struct {
	local, remote netip.AddrPort
	dataSegs      uint64
}

// unseenState is the census and what it watches, under its mutex.
type unseenState struct {
	once sync.Once
	mu   sync.Mutex
	// egress and inbound are the application's connections to its
	// dependencies and its callers' to it, by cookie.
	egress, inbound map[uint64]unseenConn
	// used counts the reads at which a connection to a dependency had
	// carried data since the read before (the capture loss), latest the
	// latest one; callsIn the same for callers' connections.
	used, callsIn uint64
	latest        string
	// census is what censusUnseen read, kept for unseenLosses' reads.
	census unseenSource
	// ends watches the watched connections end (nil where it cannot), so a
	// connection that closes between two reads is charged by what it
	// carried up to its close.
	ends unseenEnds
	// testSource, set by a test before any call, stands for this agent's.
	testSource *unseenSource
}

// unseenSource is what the census reads: the namespace's TCP sockets, the
// inodes of the application's sockets and of keploy's own, and whether a
// destination port is one keploy passes through.
type unseenSource struct {
	sockets     func(states uint32) ([]util.TCPSocket, error)
	app, own    func() (map[uint32]bool, error)
	passThrough func(port uint16) bool
	// ends starts a watch on the sockets' ends (util.WatchTCPEnds); nil:
	// none.
	ends func() (unseenEnds, error)
}

// unseenEnds is a watch on the TCP sockets' ends (util.TCPEnds).
type unseenEnds interface {
	Only(cookies []uint64)
	Ended(cookies []uint64, wait time.Duration) map[uint64]util.TCPSocket
	Close()
}

func watchTCPEnds() (unseenEnds, error) {
	w, err := util.WatchTCPEnds()
	if err != nil {
		return nil, err
	}
	return w, nil
}

// unseenEndWait is how long a read waits for the end of a watched connection
// gone from the socket table: the kernel sends it shortly after it destroys
// the socket.
const unseenEndWait = 200 * time.Millisecond

// unseenSplit is the census's rule: of socks (established and listening),
// the application's established connections whose peer is none of keploy's
// sockets, those on a port the application listens on (a caller's to it:
// inbound) and the others (its own to a dependency: egress), but those to a
// port keploy passes through.
func unseenSplit(socks []util.TCPSocket, app, own map[uint32]bool, passThrough func(uint16) bool) (inbound, egress []util.TCPSocket) {
	type pair struct{ local, remote netip.AddrPort }
	listens := map[uint16]bool{}
	byEnds := map[pair]uint32{}
	for _, s := range socks {
		switch s.State {
		case util.TCPListen:
			if app[s.Inode] {
				listens[s.Local.Port()] = true
			}
		case util.TCPEstablished:
			byEnds[pair{s.Local, s.Remote}] = s.Inode
		}
	}
	for _, s := range socks {
		if s.State != util.TCPEstablished || s.Inode == 0 || !app[s.Inode] {
			continue
		}
		if peer, ok := byEnds[pair{s.Remote, s.Local}]; ok && own[peer] {
			continue // keploy's: the proxy's, the ingress's forward
		}
		switch {
		case listens[s.Local.Port()]:
			inbound = append(inbound, s)
		case passThrough != nil && passThrough(s.Remote.Port()):
		default:
			egress = append(egress, s)
		}
	}
	return inbound, egress
}

// censusUnseen counts, once, the application's connections keploy does not
// see, and says how many at WARN. It runs where keploy begins to record again
// (the ingress's park armed, or the recording's first read of the capture
// losses), and only where the hooks tell the application's processes by
// their start (a container: recording_start_time). A census that cannot read
// the tables says so at WARN: keploy then cannot tell whether such
// connections exist.
func (h *Hooks) censusUnseen() {
	src, ok := h.unseenSource()
	if !ok {
		return // not yet (the agent's info not sent), or never here
	}
	h.unseen.once.Do(func() {
		// The ends are watched before the table is read, so none of the
		// connections the census finds ends unseen in between.
		var ends unseenEnds
		endsErr := errors.New("not watched here")
		if src.ends != nil {
			ends, endsErr = src.ends()
		}
		socks, err := src.sockets(1<<util.TCPEstablished | 1<<util.TCPListen)
		var app, own map[uint32]bool
		if err == nil {
			app, err = src.app()
		}
		if err == nil {
			own, err = src.own()
		}
		if err != nil {
			if ends != nil {
				ends.Close()
			}
			h.logger.Warn("keploy could not tell whether the application has connections it opened while no keploy agent ran, which keploy does not see: calls on such a connection would be neither recorded nor mocked", zap.Error(err))
			return
		}
		inbound, egress := unseenSplit(socks, app, own, src.passThrough)
		if ends != nil {
			if len(inbound)+len(egress) == 0 {
				ends.Close()
				ends = nil
			} else {
				cookies := make([]uint64, 0, len(inbound)+len(egress))
				for _, socks := range [][]util.TCPSocket{inbound, egress} {
					for _, c := range socks {
						cookies = append(cookies, c.Cookie)
					}
				}
				ends.Only(cookies)
			}
		}
		h.unseen.mu.Lock()
		h.unseen.census = src
		h.unseen.inbound = unseenConns(inbound)
		h.unseen.egress = unseenConns(egress)
		h.unseen.ends = ends
		h.unseen.mu.Unlock()
		if len(inbound)+len(egress) == 0 {
			return
		}
		closing := "charged by what each carried up to its close"
		if ends == nil {
			closing = "charged as if it carried data: keploy cannot read the kernel's socket ends here (" + endsErr.Error() + ")"
		}
		h.logger.Warn("the application has connections it opened while no keploy agent ran (keploy's agent was restarted, or began recording after them), which keploy does not see; a connection it opens from now on is recorded",
			zap.Int("to_its_ports", len(inbound)),
			zap.String("calls_on_those", "not recorded"),
			zap.Int("to_its_dependencies", len(egress)),
			zap.String("calls_on_those_in_a_test", "neither recorded nor mocked: the test is left without a verdict ("+string(models.CaptureLossUnseenConnection)+")"),
			zap.String("one_that_closes_between_two_tests", closing),
			zap.String("next_step", models.UnseenConnectionRemedy))
	})
}

func unseenConns(socks []util.TCPSocket) map[uint64]unseenConn {
	m := make(map[uint64]unseenConn, len(socks))
	for _, s := range socks {
		m[s.Cookie] = unseenConn{local: s.Local, remote: s.Remote, dataSegs: s.DataSegs}
	}
	return m
}

// unseenSource is the census's source for this agent, and whether it takes
// one: only where the hooks tell the application's processes by their start
// (recording_start_time, in a container), and not while they observe only
// (no proxy then).
func (h *Hooks) unseenSource() (unseenSource, bool) {
	if h.unseen.testSource != nil {
		return *h.unseen.testSource, true
	}
	h.agentInfoMu.Lock()
	start, flags, sent := h.agentInfo.RecordingStartTime, h.agentInfo.Flags, h.agentInfoSent
	h.agentInfoMu.Unlock()
	if !sent || start == 0 || flags&structs.FlagObserveOnly != 0 {
		return unseenSource{}, false
	}
	h.clientMu.Lock()
	pass := h.passThroughPorts
	h.clientMu.Unlock()
	return unseenSource{
		sockets: util.TCPSockets,
		app:     func() (map[uint32]bool, error) { return appSocketInodes("/proc", start) },
		own:     func() (map[uint32]bool, error) { return fdSocketInodes("/proc/self/fd") },
		ends:    watchTCPEnds,
		passThrough: func(port uint16) bool {
			for _, p := range pass {
				if p < 0 {
					break
				}
				if uint16(p) == port {
					return true
				}
			}
			return false
		},
	}, true
}

// unseenLosses reads, for the capture losses, the data counts of the
// connections the census found: a connection to a dependency that carried
// data since the read before counts once (models.CaptureLossUnseenConnection),
// a caller's for the log. A connection that has closed since is read by its
// end, what it carried up to its close, and is watched no more; once none is
// left nothing is read. Counts from the census on: a recording's first read
// of the losses runs the census itself.
func (h *Hooks) unseenLosses() models.CaptureLosses {
	h.censusUnseen()
	h.unseen.mu.Lock()
	defer h.unseen.mu.Unlock()
	u := &h.unseen
	if len(u.egress)+len(u.inbound) > 0 {
		if socks, err := u.census.sockets(unseenLiveStates); err == nil {
			now := make(map[uint64]util.TCPSocket, len(socks))
			for _, s := range socks {
				now[s.Cookie] = s
			}
			var ended map[uint64]util.TCPSocket
			if u.ends != nil {
				var gone []uint64
				for _, w := range []map[uint64]unseenConn{u.egress, u.inbound} {
					for ck := range w {
						if _, ok := now[ck]; !ok {
							gone = append(gone, ck)
						}
					}
				}
				if len(gone) > 0 {
					ended = u.ends.Ended(gone, unseenEndWait)
				}
			}
			if n, latest := advanceUnseen(u.egress, now, ended); n > 0 {
				u.used += n
				u.latest = latest
			}
			n, _ := advanceUnseen(u.inbound, now, ended)
			u.callsIn += n
			if len(u.egress)+len(u.inbound) == 0 && u.ends != nil {
				u.ends.Close()
				u.ends = nil
			}
		}
	}
	if u.used == 0 {
		return nil
	}
	return models.CaptureLosses{{Kind: models.CaptureLossUnseenConnection, Count: u.used, What: u.latest}}
}

// unseenLiveStates are the TCP states of a watched connection whose socket is
// still in the table with its counts: established, or closing (not
// TIME_WAIT, which keeps no counts: the socket's end has them).
const unseenLiveStates = 1<<util.TCPEstablished | 1<<util.TCPFinWait1 | 1<<util.TCPFinWait2 |
	1<<util.TCPCloseWait | 1<<util.TCPLastAck | 1<<util.TCPClosing

// advanceUnseen moves each watched connection to its data count in now, and
// returns how many carried data since, and one of those by its local port and
// its peer's address and port. One not in now has closed: its end in ended
// says what it carried up to its close, and it is watched no more. One with
// no end there (its end was lost, or there is no watch) cannot be read any
// more, so it counts as one that carried data: a connection that carried a
// test's call and closed before the test's end read is never left uncharged.
func advanceUnseen(watched map[uint64]unseenConn, now, ended map[uint64]util.TCPSocket) (uint64, string) {
	var (
		n      uint64
		latest string
	)
	for ck, c := range watched {
		s, ok := now[ck]
		if !ok {
			delete(watched, ck)
			if e, ok := ended[ck]; ok && e.DataKnown {
				if e.DataSegs != c.dataSegs {
					n++
					latest = fmt.Sprintf("a connection from local port %d to %s, closed since", c.local.Port(), c.remote)
				}
				continue
			}
			n++
			latest = fmt.Sprintf("a connection from local port %d to %s, which closed before keploy could read what it carried", c.local.Port(), c.remote)
			continue
		}
		if !s.DataKnown || s.DataSegs == c.dataSegs {
			continue
		}
		n++
		latest = fmt.Sprintf("a connection from local port %d to %s", c.local.Port(), c.remote)
		c.dataSegs = s.DataSegs
		watched[ck] = c
	}
	return n, latest
}

// unseenEnd says, as the hooks unload, how many calls on callers'
// connections keploy did not see passed it by.
func (h *Hooks) unseenEnd() {
	h.unseen.mu.Lock()
	in := h.unseen.callsIn
	if h.unseen.ends != nil {
		h.unseen.ends.Close()
		h.unseen.ends = nil
	}
	h.unseen.mu.Unlock()
	if in > 0 {
		h.logger.Warn("callers sent the application data keploy did not record, on connections they opened while no keploy agent ran", zap.Uint64("reads_with_data", in))
	}
}

// appSocketInodes is the inodes of the sockets the application's processes
// hold: each process under proc that the hooks take as the application's
// (classify_process: it started at or after the recording's start, start, a
// time since boot in nanoseconds, or it is its PID namespace's first) and
// that does not run keploy's own file. A process that goes meanwhile is
// skipped.
func appSocketInodes(proc string, start uint64) (map[uint32]bool, error) {
	self, err := os.Stat(filepath.Join(proc, "self", "exe"))
	if err != nil {
		return nil, fmt.Errorf("read keploy's own file: %w", err)
	}
	ents, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	me := os.Getpid()
	tick := uint64(clockTick.Nanoseconds())
	out := map[uint32]bool{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == me {
			continue
		}
		dir := filepath.Join(proc, e.Name())
		started, err := procStartTicks(filepath.Join(dir, "stat"))
		if err != nil || (pid != 1 && started < start/tick) {
			continue
		}
		if exe, err := os.Stat(filepath.Join(dir, "exe")); err == nil && os.SameFile(exe, self) {
			continue
		}
		inodes, err := fdSocketInodes(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		for ino := range inodes {
			out[ino] = true
		}
	}
	return out, nil
}

// fdSocketInodes is the inodes of the sockets the fds in dir link to.
func fdSocketInodes(dir string) (map[uint32]bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[uint32]bool{}
	for _, e := range ents {
		l, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || !strings.HasPrefix(l, "socket:[") || !strings.HasSuffix(l, "]") {
			continue
		}
		if n, err := strconv.ParseUint(l[len("socket:["):len(l)-1], 10, 32); err == nil {
			out[uint32(n)] = true
		}
	}
	return out, nil
}
