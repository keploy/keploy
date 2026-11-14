//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// wsAccept is Winsock's accept, for a listener the net package did not make:
// net.Listen gives no way to choose the backlog.
var wsAccept = windows.NewLazySystemDLL("ws2_32.dll").NewProc("accept")

// rawListener listens on loopback with the given backlog and returns the
// socket and its address. The test's cleanup closes it, which also ends any
// accept still waiting on it.
func rawListener(t *testing.T, backlog int) (windows.Handle, string) {
	t.Helper()
	h, err := windows.Socket(windows.AF_INET, windows.SOCK_STREAM, windows.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = windows.Closesocket(h) })
	if err := windows.Bind(h, &windows.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := windows.Listen(h, backlog); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := windows.Getsockname(h)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	in4, ok := sa.(*windows.SockaddrInet4)
	if !ok {
		t.Fatalf("getsockname gave %T, not an IPv4 address", sa)
	}
	return h, fmt.Sprintf("127.0.0.1:%d", in4.Port)
}

// accepted is one accept on a raw listener: the client's port, 0 when the
// accepted socket had no peer left to read (the client reset it first).
type accepted struct {
	port int
	err  error
}

// acceptOne accepts one connection on ln, reads its client's port and
// closes it.
func acceptOne(ln windows.Handle) accepted {
	r, _, err := wsAccept.Call(uintptr(ln), 0, 0)
	h := windows.Handle(r)
	if h == windows.InvalidHandle {
		return accepted{err: fmt.Errorf("accept: %w", err)}
	}
	defer func() { _ = windows.Closesocket(h) }()
	sa, err := windows.Getpeername(h)
	if in4, ok := sa.(*windows.SockaddrInet4); err == nil && ok {
		return accepted{port: in4.Port}
	}
	return accepted{}
}

// acceptedPorts accepts on ln until no connection comes for idle, and
// returns the client's port of each one accepted, and how many were accepted
// with no peer left to read. The accept loop runs until the test's cleanup
// closes ln.
func acceptedPorts(t *testing.T, ln windows.Handle, idle time.Duration) (map[int]bool, int) {
	t.Helper()
	got := make(chan accepted)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			a := acceptOne(ln)
			select {
			case got <- a:
			case <-done:
				return
			}
			if a.err != nil {
				return
			}
		}
	}()
	ports, noPeer := map[int]bool{}, 0
	for {
		select {
		case a := <-got:
			switch {
			case a.err != nil:
				t.Fatalf("after %d accepts: %v", len(ports)+noPeer, a.err)
			case a.port == 0:
				noPeer++
			default:
				ports[a.port] = true
			}
		case <-time.After(idle):
			return ports, noPeer
		}
	}
}

// How the client sees a connection it completed that the listener never
// gave to accept, once it has written to it and read from it.
const (
	seenReset  = "reset"
	seenClosed = "closed"
	seenOpen   = "still open"
)

// clientSees writes a byte on c and reads, and says how the connection ended
// at the client: reset by the listener's side, closed, or neither by the
// deadline (still open).
func clientSees(c net.Conn) (string, error) {
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", err
	}
	_, werr := c.Write([]byte{0})
	_, rerr := c.Read(make([]byte, 1))
	for _, err := range []error{werr, rerr} {
		switch {
		case err == nil:
		case errors.Is(err, syscall.WSAECONNRESET), errors.Is(err, syscall.WSAECONNABORTED):
			return seenReset, nil
		case errors.Is(err, io.EOF):
			return seenClosed, nil
		case errors.Is(err, os.ErrDeadlineExceeded):
			return seenOpen, nil
		default:
			return "", err
		}
	}
	return "", errors.New("the listener's side answered a connection no one accepted")
}

// On Windows a full backlog can drop a connection the client saw complete:
// on Windows 11 (10.0.26200) 7 of 300 connects at once to a listener of
// backlog 1 that accepts nothing completed at the client and only 5 reached
// accept. So the proxy's accepts alone cannot tell keploy that every
// connection the application completed reached the proxy; listenerDrops' 0
// rests on the application's side instead (accepts_windows.go): a connection
// the hooks numbered is settled only once it is paired with the proxy's side
// of it, or lost (agent.ProxyConnsReporter's AppConnsSettled). This row
// checks what such a drop leaves the application: every connect that
// completed at the client is either accepted, or reset by the time the client
// has written to it and read from it, so the application learns of it as its
// own error and the connection is gone, a loss the hooks can see. None stays
// open with no one to accept it.
func TestOnWindowsAFullBacklogResetsAConnectionItDropsAfterTheClientCompletedIt(t *testing.T) {
	ln, addr := rawListener(t, 1)

	// More than any backlog Windows keeps (SOMAXCONN is 200), so it fills
	// whatever the stack makes of 1.
	const attempts = 300
	var (
		mu        sync.Mutex
		connected []net.Conn
		failed    int
		firstErr  error
		wg        sync.WaitGroup
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range connected {
			_ = c.Close()
		}
	})
	d := net.Dialer{Timeout: 5 * time.Second}
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.Dial("tcp4", addr)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			connected = append(connected, c)
		}()
	}
	wg.Wait()
	if failed == 0 {
		t.Fatalf("all %d connects to a listener of backlog 1 that accepts nothing completed: the backlog never filled, so this shows nothing", attempts)
	}
	if len(connected) == 0 {
		t.Fatalf("none of %d connects completed (the first failed with %v): the listener took none, so this shows nothing", attempts, firstErr)
	}

	ports, noPeer := acceptedPorts(t, ln, 3*time.Second)
	if n := len(ports) + noPeer; n > len(connected) {
		t.Fatalf("%d accepted, more than the %d connects that completed at the client", n, len(connected))
	}
	dropped := map[string][]int{}
	for _, c := range connected {
		port := c.LocalAddr().(*net.TCPAddr).Port
		if ports[port] {
			continue
		}
		seen, err := clientSees(c)
		if err != nil {
			t.Fatalf("the connection from port %d completed at the client and was never accepted, and writing and reading on it gave: %v", port, err)
		}
		dropped[seen] = append(dropped[seen], port)
	}
	t.Logf("%d of %d connects completed, %d failed (the first with %v); %d accepted, %d of them with no peer left; the rest, by how the client saw them: %v",
		len(connected), attempts, failed, firstErr, len(ports)+noPeer, noPeer, dropped)
	for _, seen := range []string{seenClosed, seenOpen} {
		if ports := dropped[seen]; len(ports) > 0 {
			t.Errorf("the connections from ports %v completed at the client and were never accepted, and the client saw them %s, not reset", ports, seen)
		}
	}
}
