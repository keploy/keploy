//go:build linux

package util

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The sock_diag destroy groups (include/uapi/linux/sock_diag.h, enum
// sknetlink_groups) of TCP over IPv4 and IPv6, as sockaddr_nl's group mask.
const (
	sknlgrpInetTCPDestroy  = 1
	sknlgrpInet6TCPDestroy = 3
	tcpEndsGroups          = 1<<(sknlgrpInetTCPDestroy-1) | 1<<(sknlgrpInet6TCPDestroy-1)

	// tcpEndsCookieAt is the offset of idiag_cookie in a destroy
	// message: the netlink header, then inet_diag_msg's sockid.
	tcpEndsCookieAt = unix.NLMSG_HDRLEN + listenDiagSockid + listenDiagCookie
	// tcpEndsMaxFilter is the most cookies the socket filter names (5
	// instructions each, of BPF_MAXINSNS); more are told apart here.
	tcpEndsMaxFilter = 800
	tcpEndsRcvBuf    = 4 << 20
)

// TCPEnds watches the TCP sockets of this process's network namespace end: as
// the kernel destroys each one, sock_diag sends its last state, its byte
// counts among it, to its destroy groups. A connection that closes is
// destroyed as it leaves its last full state (into TIME_WAIT, or closed), so
// what it carried up to its close can be read after it has gone from the
// socket table. Only the sockets Only names are kept.
type TCPEnds struct {
	fd    int
	mu    sync.Mutex
	want  map[uint64]bool // nil: every socket's
	ended map[uint64]TCPSocket
	stop  bool
	done  chan struct{}
}

// WatchTCPEnds starts watching the TCP sockets of this process's network
// namespace end. Close stops it.
func WatchTCPEnds() (*TCPEnds, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	// Room for a burst of ends (the application's own connections end too):
	// past the system's limit where this process may.
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, tcpEndsRcvBuf) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, tcpEndsRcvBuf)
	}
	// A receive that waits at most this long, so that Close is seen.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 100_000}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("sock_diag socket's receive timeout: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: tcpEndsGroups}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("join the sock_diag TCP destroy groups: %w", err)
	}
	w := &TCPEnds{fd: fd, ended: map[uint64]TCPSocket{}, done: make(chan struct{})}
	go w.read()
	return w, nil
}

func (w *TCPEnds) read() {
	defer close(w.done)
	buf := make([]byte, 64<<10)
	for {
		n, _, err := unix.Recvfrom(w.fd, buf, 0)
		w.mu.Lock()
		stop := w.stop
		w.mu.Unlock()
		if stop {
			return
		}
		if err != nil {
			// EAGAIN: the receive timeout; ENOBUFS: ends overran the
			// socket and are lost (a socket whose end is lost is not
			// in Ended's answer); EINTR.
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOBUFS) || errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			continue
		}
		for _, m := range msgs {
			if m.Header.Type != unix.SOCK_DIAG_BY_FAMILY || len(m.Data) == 0 {
				continue
			}
			s, ok := parseTCPSocket(m.Data, m.Data[0])
			if !ok {
				continue
			}
			w.mu.Lock()
			if w.want == nil || w.want[s.Cookie] {
				w.ended[s.Cookie] = s
			}
			w.mu.Unlock()
		}
	}
}

// Only keeps the ends of the sockets of cookies alone from now on, and drops
// the others' kept so far: by a filter on the socket where they fit in one,
// so the others do not reach this process at all.
func (w *TCPEnds) Only(cookies []uint64) {
	want := make(map[uint64]bool, len(cookies))
	for _, c := range cookies {
		want[c] = true
	}
	if len(cookies) <= tcpEndsMaxFilter {
		prog := tcpEndsFilter(cookies)
		_ = unix.SetsockoptSockFprog(w.fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.want = want
	for c := range w.ended {
		if !want[c] {
			delete(w.ended, c)
		}
	}
}

// tcpEndsFilter is a classic BPF socket filter that passes the destroy
// messages of the sockets of cookies, and drops every other one. A word load
// reads in network order, so each half of a cookie (which the message has in
// this host's order) is compared as such.
func tcpEndsFilter(cookies []uint64) []unix.SockFilter {
	be := func(v uint32) uint32 {
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], v)
		return binary.BigEndian.Uint32(b[:])
	}
	const (
		ldAbsW = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeqK   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		retK   = unix.BPF_RET | unix.BPF_K
	)
	prog := make([]unix.SockFilter, 0, 5*len(cookies)+1)
	for _, c := range cookies {
		prog = append(prog,
			unix.SockFilter{Code: ldAbsW, K: tcpEndsCookieAt},
			unix.SockFilter{Code: jeqK, K: be(uint32(c)), Jt: 0, Jf: 3},
			unix.SockFilter{Code: ldAbsW, K: tcpEndsCookieAt + 4},
			unix.SockFilter{Code: jeqK, K: be(uint32(c >> 32)), Jt: 0, Jf: 1},
			unix.SockFilter{Code: retK, K: 0xffffffff},
		)
	}
	return append(prog, unix.SockFilter{Code: retK, K: 0})
}

// Ended is the last state of each of the sockets of cookies the kernel has
// destroyed, waiting up to wait for those not yet in: the kernel sends a
// socket's end shortly after it destroys the socket. A socket not in the
// answer has not ended, or its end was lost. Each is answered once.
func (w *TCPEnds) Ended(cookies []uint64, wait time.Duration) map[uint64]TCPSocket {
	deadline := time.Now().Add(wait)
	for {
		w.mu.Lock()
		missing := 0
		for _, c := range cookies {
			if _, ok := w.ended[c]; !ok {
				missing++
			}
		}
		if missing == 0 || w.stop || !time.Now().Before(deadline) {
			out := make(map[uint64]TCPSocket, len(cookies)-missing)
			for _, c := range cookies {
				if s, ok := w.ended[c]; ok {
					out[c] = s
					delete(w.ended, c)
				}
				if w.want != nil {
					delete(w.want, c)
				}
			}
			w.mu.Unlock()
			return out
		}
		w.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

// Close stops the watch.
func (w *TCPEnds) Close() {
	w.mu.Lock()
	if w.stop {
		w.mu.Unlock()
		return
	}
	w.stop = true
	w.mu.Unlock()
	<-w.done
	_ = unix.Close(w.fd)
}
