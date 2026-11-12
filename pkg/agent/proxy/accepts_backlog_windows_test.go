//go:build windows

package proxy

import (
	"fmt"
	"net"
	"sync"
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

// On Windows a full backlog loses no connection the client saw complete,
// which is why listenerDrops is 0 there (accepts_windows.go): connects to a
// listener of backlog 1 that no one accepts on, many at once, until some
// fail; then every connect that completed at the client is accepted. A
// connection the stack completed and then dropped from the backlog would be
// one the application had written to and the proxy never saw.
func TestOnWindowsAFullBacklogLosesNoConnectionTheClientCompleted(t *testing.T) {
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
	t.Logf("%d of %d connects completed, %d failed (the first with %v)", len(connected), attempts, failed, firstErr)

	// Each accept is waited for on its own: one the client completed that is
	// not in the backlog blocks for good, and the cleanup's close ends it.
	accepted := make(chan error, 1)
	for i := range connected {
		go func() {
			r, _, err := wsAccept.Call(uintptr(ln), 0, 0)
			if windows.Handle(r) == windows.InvalidHandle {
				accepted <- fmt.Errorf("accept: %w", err)
				return
			}
			_ = windows.Closesocket(windows.Handle(r))
			accepted <- nil
		}()
		select {
		case err := <-accepted:
			if err != nil {
				t.Fatalf("accept %d of the %d connections the client completed: %v", i+1, len(connected), err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the client completed %d connections, and the listener holds only %d of them: Windows dropped one the client saw complete, so listenerDrops' 0 is wrong there", len(connected), i)
		}
	}
}
