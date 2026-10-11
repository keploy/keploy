//go:build linux

package synhold

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestEndpointsReadsTheSYN: a held SYN is named by its source (the
// application's end) and destination (the proxy's), for IPv4 with and
// without IP options and for IPv6.
func TestEndpointsReadsTheSYN(t *testing.T) {
	v4 := func(ihl int) []byte {
		p := make([]byte, ihl*4+20)
		p[0] = 0x40 | byte(ihl)
		p[9] = 6 // TCP
		copy(p[12:], []byte{127, 0, 0, 1})
		copy(p[16:], []byte{172, 17, 0, 2})
		tcp := p[ihl*4:]
		tcp[0], tcp[1] = 0xd4, 0x31 // 54321
		tcp[2], tcp[3] = 0x41, 0x95 // 16789
		tcp[4], tcp[5], tcp[6], tcp[7] = 0xde, 0xad, 0xbe, 0xef
		return p
	}
	v6 := func(next byte) []byte {
		p := make([]byte, 60)
		p[0] = 0x60
		p[6] = next
		p[23] = 1 // ::1
		p[39] = 1 // ::1
		p[40], p[41] = 0xd4, 0x31
		p[42], p[43] = 0x41, 0x95
		p[44], p[45], p[46], p[47] = 0xde, 0xad, 0xbe, 0xef
		return p
	}
	for _, tc := range []struct {
		name          string
		pkt           []byte
		client, proxy string
		ok            bool
	}{
		{"ipv4", v4(5), "127.0.0.1:54321", "172.17.0.2:16789", true},
		{"ipv4 with options", v4(7), "127.0.0.1:54321", "172.17.0.2:16789", true},
		{"ipv6", v6(6), "[::1]:54321", "[::1]:16789", true},
		{"ipv6 with an extension header", v6(0), "", "", false},
		{"ipv4 not tcp", func() []byte { p := v4(5); p[9] = 17; return p }(), "", "", false},
		{"truncated", v4(5)[:26], "", "", false},
		{"bad ihl", func() []byte { p := v4(5); p[0] = 0x43; return p }(), "", "", false},
		{"empty", nil, "", "", false},
		{"not ip", []byte{0x20, 0, 0}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, isn, ok := endpoints(tc.pkt)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if c != netip.MustParseAddrPort(tc.client) || p != netip.MustParseAddrPort(tc.proxy) {
				t.Fatalf("got %s -> %s, want %s -> %s", c, p, tc.client, tc.proxy)
			}
			if isn != 0xdeadbeef {
				t.Fatalf("initial sequence number %#x, want 0xdeadbeef", isn)
			}
		})
	}
}

// TestMarksAreDistinctAndNonZero: a zero mark is every unmarked packet's, and
// two equal marks would answer one outcome with the other's reject.
func TestMarksAreDistinctAndNonZero(t *testing.T) {
	for i := 0; i < 1000; i++ {
		m, err := newMarks()
		if err != nil {
			t.Fatal(err)
		}
		all := []uint32{m.refuse, m.host, m.net}
		for i := range all {
			if all[i] == 0 {
				t.Fatalf("marks %+v", m)
			}
			for j := i + 1; j < len(all); j++ {
				if all[i] == all[j] {
					t.Fatalf("marks %+v", m)
				}
			}
		}
	}
}

func TestIfnameIsPaddedLikeTheKernelsCompare(t *testing.T) {
	b := ifname("lo")
	if len(b) != 16 || b[0] != 'l' || b[1] != 'o' || b[2] != 0 || b[15] != 0 {
		t.Fatalf("ifname(lo) = %v", b)
	}
}

// TestForgetExpiredDropsOnlyWhatExpired: answers expire oldest first, and a
// SYN answered again after its first answer keeps the newer one.
func TestForgetExpiredDropsOnlyWhatExpired(t *testing.T) {
	h := &Holder{answered: map[synKey]answer{}}
	now := time.Now()
	k := func(port uint16) synKey {
		return synKey{client: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), isn: uint32(port)}
	}
	add := func(key synKey, until time.Time) {
		h.answered[key] = answer{out: Accept, until: until}
		h.expiry = append(h.expiry, answeredAt{key: key, until: until})
	}
	add(k(1), now.Add(-2*time.Second))
	add(k(2), now.Add(-time.Second))
	add(k(3), now.Add(time.Second))
	add(k(2), now.Add(2*time.Second)) // answered again, later
	h.forgetExpired(now)
	if _, ok := h.answered[k(1)]; ok {
		t.Fatal("an expired answer was kept")
	}
	if a, ok := h.answered[k(2)]; !ok || !a.until.After(now) {
		t.Fatal("a re-answered SYN lost its newer answer to its older one's expiry")
	}
	if _, ok := h.answered[k(3)]; !ok {
		t.Fatal("an unexpired answer was dropped")
	}
	if len(h.expiry) != 2 {
		t.Fatalf("%d entries left to expire, want 2", len(h.expiry))
	}
}

// TestConnectWithinWaitsThroughSignals: connectWithin runs on a thread that
// is sent SIGURG each time it sleeps waiting for the connection's answer, and
// returns that answer: refused, accepted, or, when nothing answers, ETIMEDOUT
// once its timeout is up and not long after. The signals stop half a second
// before the timeout, so a wait that started over at the last of them would
// end long after it. connectWithin is handed a blocking socket, which it must
// make non-blocking: a blocking connect waits for as long as the kernel
// retries the SYN, not for the timeout.
func TestConnectWithinWaitsThroughSignals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		// answer is given once the connection has been signalled three
		// times.
		answer func(*unanswering, *testing.T)
		want   error
	}{
		// The answer reaches the connection with a SYN it sends again a
		// second or more after its first; the timeout leaves a loaded
		// machine room to miss some of them.
		{"refused", 20 * time.Second, (*unanswering).refuse, unix.ECONNREFUSED},
		{"accepted", 20 * time.Second, (*unanswering).accept, nil},
		{"never answered", 2 * time.Second, func(*unanswering, *testing.T) {}, unix.ETIMEDOUT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := newUnanswering(t)
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unix.Close(fd) })
			start := time.Now()
			signals, err := signalled(t, fd,
				func() error { return connectWithin(fd, u.addr, tc.timeout) },
				func() { tc.answer(u, t) }, tc.timeout-500*time.Millisecond, tc.timeout+3*time.Second)
			took := time.Since(start)
			if !errors.Is(err, tc.want) {
				t.Fatalf("connectWithin = %v after %v, signalled %d times; want %v", err, took, signals, tc.want)
			}
			if tc.want == unix.ETIMEDOUT && took < tc.timeout {
				t.Fatalf("gave up after %v, before its %v were up", took, tc.timeout)
			}
			if signals < 3 {
				t.Fatalf("the connection was signalled %d times while it waited, not 3: nothing was tested", signals)
			}
			if tc.want == unix.ETIMEDOUT && took > tc.timeout+time.Second {
				t.Fatalf("gave up after %v, long after its %v were up", took, tc.timeout)
			}
		})
	}
}

// unanswering is a loopback port whose accept queue is full, so the kernel
// drops each SYN sent to it: a connection to it waits for its answer, as one
// to a held port does. Its next SYN is refused once the listener is closed,
// or accepted once the queue is emptied.
type unanswering struct {
	fd    int // the listener
	addr  *unix.SockaddrInet4
	close func()
}

func newUnanswering(t *testing.T) *unanswering {
	t.Helper()
	socket := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Lets two sockets be bound to one port while only one listens.
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			t.Fatal(err)
		}
		return fd
	}
	// keep stays bound to the port, and never listens: once the listener is
	// closed the kernel gives the port to nothing else, which could listen
	// there and accept the connection that is to be refused.
	keep := socket()
	t.Cleanup(func() { _ = unix.Close(keep) })
	if err := unix.Bind(keep, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(keep)
	if err != nil {
		t.Fatal(err)
	}
	fd := socket()
	u := &unanswering{fd: fd, addr: sa.(*unix.SockaddrInet4), close: sync.OnceFunc(func() { _ = unix.Close(fd) })}
	t.Cleanup(u.close)
	if err := unix.Bind(fd, u.addr); err != nil {
		t.Fatal(err)
	}
	// A backlog of 0 is full with one connection waiting to be accepted.
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(u.addr.Port)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// The connection is in the queue once the listener has its last ACK,
	// which may be a moment after the dial returned.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		ti, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
		if err != nil {
			t.Fatal(err)
		}
		// Of a listener, Unacked counts the connections waiting to be
		// accepted.
		if ti.Unacked > 0 {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener's accept queue did not fill")
		}
	}
}

// refuse closes the listener.
func (u *unanswering) refuse(*testing.T) { u.close() }

// accept empties the queue.
func (u *unanswering) accept(t *testing.T) {
	c, _, err := unix.Accept(u.fd)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(c)
}

// signalled runs call on a thread of its own and, for signalFor, sends that
// thread SIGURG each time it sleeps waiting for fd's connection to be
// answered (in the poll, or in connect itself), running answer once it has
// sent three. It returns how many it sent and what call returned. It fails
// the test when it cannot tell where the thread sleeps, or when call has not
// returned within limit; fd is then shut down first, which ends any wait on
// it, so that call is not left on a descriptor the cleanup closes and another
// test reuses.
func signalled(t *testing.T, fd int, call func() error, answer func(), signalFor, limit time.Duration) (int, error) {
	t.Helper()
	tids := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		// Never unlocked: the thread exits with the goroutine.
		runtime.LockOSThread()
		tids <- unix.Gettid()
		done <- call()
	}()
	tid := <-tids
	signals := 0
	// stop ends the call before the test fails, so it is not left waiting
	// on a descriptor the cleanup closes and another test may be given.
	stop := func() {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	for start := time.Now(); ; time.Sleep(time.Millisecond) {
		select {
		case err := <-done:
			return signals, err
		default:
		}
		if time.Since(start) > limit {
			stop()
			t.Fatalf("the connection was not answered within %v, having been signalled %d times", limit, signals)
		}
		asleep, err := asleepIn(tid, unix.SYS_PPOLL, unix.SYS_CONNECT)
		if err != nil {
			// The thread is gone only once call has returned.
			select {
			case err := <-done:
				return signals, err
			default:
			}
			stop()
			t.Fatalf("cannot tell where the connection waits, so cannot signal it there: %v", err)
		}
		if !asleep || time.Since(start) > signalFor {
			continue
		}
		// tgkill wakes the thread before it returns: asleep again is asleep
		// after the last signal.
		if err := unix.Tgkill(os.Getpid(), tid, unix.SIGURG); err != nil {
			if errors.Is(err, unix.ESRCH) {
				continue // call returned meanwhile, and its thread is gone
			}
			stop()
			t.Fatal(err)
		}
		if signals++; signals == 3 {
			answer()
		}
	}
}

// asleepIn reports whether thread tid sleeps in one of the system calls.
func asleepIn(tid int, sysnos ...uintptr) (bool, error) {
	sc, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/syscall", tid))
	if err != nil {
		return false, err
	}
	// The call's number while the thread is in one; "running" otherwise.
	f := strings.Fields(string(sc))
	if len(f) == 0 {
		return false, nil
	}
	if n, err := strconv.ParseUint(f[0], 10, 64); err != nil || !slices.Contains(sysnos, uintptr(n)) {
		return false, nil
	}
	st, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", tid))
	if err != nil {
		return false, err
	}
	i := bytes.LastIndexByte(st, ')')
	f = strings.Fields(string(st[i+1:]))
	return i >= 0 && len(f) > 0 && f[0] == "S", nil
}
