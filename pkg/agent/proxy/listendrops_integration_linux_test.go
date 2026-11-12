//go:build linux && listendrops_integration

// Root-only: each row runs in a network namespace of its own (the test binary
// runs itself again there), so that the namespace's ListenDrops are its
// listener's alone, and it sends a SYN through a raw socket. It lives behind
// the listendrops_integration build tag and runs as root in its own CI job
// (.github/workflows/listener-drops-linux.yml), never in `go test ./...`.

package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// inOwnNetnsEnv names, in the test binary run again, the test it runs in its
// own network namespace.
const inOwnNetnsEnv = "KEPLOY_LISTENDROPS_IN_OWN_NETNS"

// inOwnNetns runs the calling test again, alone, in a new network namespace
// with its loopback up, and fails it if it did not pass there: it returns
// true in that run, which does the test's work, and false in the first.
func inOwnNetns(t *testing.T) bool {
	t.Helper()
	if os.Getenv(inOwnNetnsEnv) == t.Name() {
		upLoopback(t)
		return true
	}
	if os.Geteuid() != 0 {
		t.Fatal("this row needs root: it makes a network namespace and sends a SYN through a raw socket")
	}
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), inOwnNetnsEnv+"="+t.Name())
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET, Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("run the row in a network namespace of its own: %v", err)
	}
	// A row that dies early leaves nothing behind.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err := cmd.Wait()
	t.Logf("in its own network namespace:\n%s", out.String())
	if err != nil || !bytes.Contains(out.Bytes(), []byte("--- PASS: "+t.Name())) {
		t.Fatalf("the row did not pass in its own network namespace: %v", err)
	}
	return false
}

// upLoopback brings the namespace's loopback up.
func upLoopback(t *testing.T) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		t.Fatalf("read lo's flags: %v", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		t.Fatalf("bring lo up: %v", err)
	}
}

// markedProxyListener is the proxy's own marked listener (dual-stack, as the
// proxy opens it) and its port.
func markedProxyListener(t *testing.T) (*Proxy, net.Listener, int) {
	t.Helper()
	p := New(zap.NewNop(), plainDestInfo{}, config.New())
	p.SetListenerCounter(&fakeCounter{})
	ln, err := p.listen(context.Background(), ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return p, ln, ln.Addr().(*net.TCPAddr).Port
}

func dropsOf(t *testing.T, p *Proxy) models.ListenDrops {
	t.Helper()
	d, err := p.ListenDrops()
	if err != nil {
		t.Fatal(err)
	}
	if !d.NetKnown {
		t.Fatalf("the namespace's ListenDrops were not read: %s", d.NetUnread)
	}
	return d
}

// sendSYN sends one SYN to 127.0.0.1:dst from port src, where no socket is:
// the kernel answers the listener's SYN-ACK to it with a reset, as it does
// for a client whose process exited, or that closed its socket, before its
// handshake finished.
func sendSYN(t *testing.T, src, dst uint16, seq uint32) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("open a raw socket: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	h := make([]byte, 20)
	binary.BigEndian.PutUint16(h[0:], src)
	binary.BigEndian.PutUint16(h[2:], dst)
	binary.BigEndian.PutUint32(h[4:], seq)
	h[12] = 5 << 4 // a 20-byte header
	h[13] = 0x02   // SYN
	binary.BigEndian.PutUint16(h[14:], 65535)
	pseudo := []byte{127, 0, 0, 1, 127, 0, 0, 1, 0, unix.IPPROTO_TCP, 0, byte(len(h))}
	binary.BigEndian.PutUint16(h[16:], checksum(append(pseudo, h...)))
	if err := unix.Sendto(fd, h, 0, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("send the SYN: %v", err)
	}
}

// checksum is the internet checksum of b.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// A client that closed its connect before the handshake finished resets the
// half-open connection: the kernel raises the listener's drops for it, and
// not its namespace's ListenDrops, so keploy charges no test for it (the
// agent's chargeDrops). On an idle loopback a real connect finishes its
// handshake inside connect(); a loaded host (or keploy's own hold of a
// handshake) leaves it half-open, and the raw SYN, whose answer only a reset
// meets, makes that so here, every time.
func TestAConnectAbandonedMidHandshakeRaisesTheListenersDropsAlone(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p, _, port := markedProxyListener(t)
	was := dropsOf(t, p)
	const abandoned = 5
	for i := 0; i < abandoned; i++ {
		sendSYN(t, uint16(40001+i), uint16(port), uint32(1000*(i+1)))
	}
	// The resets are processed as they are sent on loopback; no deadline but
	// the test binary's -timeout.
	now := dropsOf(t, p)
	for now.Listener < was.Listener+abandoned {
		time.Sleep(time.Millisecond)
		now = dropsOf(t, p)
	}
	if now.Net != was.Net {
		t.Fatalf("%d abandoned connects raised the namespace's ListenDrops %d to %d: an abandoned connect would be charged as a drop", abandoned, was.Net, now.Net)
	}
	t.Logf("%d abandoned connects: the listener's drops %d to %d, the namespace's ListenDrops %d to %d", abandoned, was.Listener, now.Listener, was.Net, now.Net)
}

// A connection the listener's full accept queue has no room for is a drop
// that can lose a connection: the kernel raises both counts, so keploy
// charges the test it fell in.
func TestAFullAcceptQueueRaisesBothDropCounts(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p, ln, port := markedProxyListener(t)
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var lerr error
	if err := rc.Control(func(fd uintptr) { lerr = unix.Listen(int(fd), 0) }); err != nil || lerr != nil {
		t.Fatalf("shrink the accept queue: %v %v", err, lerr)
	}
	was := dropsOf(t, p)
	for i := 0; i < 4; i++ {
		connectNoWait(t, port)
	}
	// Both rise as the connects' packets are processed; no deadline but the
	// test binary's -timeout.
	now := dropsOf(t, p)
	for now.Listener == was.Listener || now.Net == was.Net {
		time.Sleep(time.Millisecond)
		now = dropsOf(t, p)
	}
	t.Logf("a full accept queue: the listener's drops %d to %d, the namespace's ListenDrops %d to %d", was.Listener, now.Listener, was.Net, now.Net)
}

// A listener keploy opened in another network namespace than its process's
// has no namespace count keploy can read: /proc/self/net/netstat is its own
// namespace's, whose ListenDrops another listener raises, never this one. It
// is said unread, never read: a count of the wrong namespace would charge
// every real drop min(own, 0), nothing.
func TestAListenerInAnotherNamespaceHasItsNamespaceCountUnread(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := New(zap.NewNop(), plainDestInfo{}, config.New())
	p.SetListenerCounter(&fakeCounter{})
	type opened struct {
		ln  net.Listener
		err error
	}
	got := make(chan opened, 1)
	go func() {
		// The listener is opened on a thread moved into a namespace of its
		// own, then moved back: a socket stays in the namespace it was
		// made in.
		runtime.LockOSThread()
		back, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			got <- opened{err: err}
			return // the thread ends with its goroutine, still locked
		}
		defer func() { _ = unix.Close(back) }()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			got <- opened{err: err}
			return
		}
		ln, lerr := p.listen(context.Background(), ":0")
		if err := unix.Setns(back, unix.CLONE_NEWNET); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			got <- opened{err: err}
			return // the thread is in the other namespace: it ends locked
		}
		runtime.UnlockOSThread()
		got <- opened{ln: ln, err: lerr}
	}()
	o := <-got
	if o.err != nil {
		t.Fatalf("open the listener in another network namespace: %v", o.err)
	}
	t.Cleanup(func() { _ = o.ln.Close() })
	d, err := p.ListenDrops()
	if err != nil {
		t.Fatalf("the listener's own count is read wherever it is: %v", err)
	}
	if d.NetKnown || d.Net != 0 || !strings.Contains(d.NetUnread, "another network namespace") {
		t.Fatalf("a listener in another namespace: %+v; want its namespace count unread, naming the other namespace", d)
	}
	t.Logf("a listener in another namespace: %+v", d)
}
