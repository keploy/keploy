//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches k_app_conn_release at the cgroup root.

package linux

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

const heldConnHelperEnv = "KEPLOY_HELD_CONN_TEST_HELPER"

// runHeldConnHelper is the application of
// TestTheHooksSayHowTheApplicationReleasedItsConnection: it opens a TCP socket
// and prints its cookie; once a line on stdin says keploy redirected it, it
// connects it to 127.0.0.1:port and prints its local port; then it holds the
// connection open until a line "exit N" (it exits with status N, the socket
// still open, which its exit releases) or a signal kills it. Before that, a
// line "shutdown" shuts its end down (it prints "shut"), and a line "trap"
// makes it handle SIGTERM by exiting with status 143, as a JVM does (it
// prints "trapped").
func runHeldConnHelper(port string) int {
	p, err := strconv.Atoi(port)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	in := bufio.NewReader(os.Stdin)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println(cookie)
	if _, err := in.ReadString('\n'); err != nil {
		return 3
	}
	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println(sa.(*unix.SockaddrInet4).Port)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return 3
		}
		switch line = strings.TrimSpace(line); line {
		case "shutdown":
			if err := unix.Shutdown(fd, unix.SHUT_WR); err != nil {
				fmt.Println("helper:", err)
				return 3
			}
			fmt.Println("shut")
		case "trap":
			term := make(chan os.Signal, 1)
			signal.Notify(term, syscall.SIGTERM)
			go func() {
				<-term
				os.Exit(143)
			}()
			fmt.Println("trapped")
		default:
			code, err := strconv.Atoi(strings.TrimPrefix(line, "exit "))
			if err != nil {
				fmt.Println("helper:", err)
				return 3
			}
			return code
		}
	}
}

// The hooks say how the application released its end of a connection they
// redirected (TakeAppConnEnd), as k_app_conn_release wrote it at the release,
// and the proxy reads it once it has read that end: closed while its process
// ran; released as its process exited on its own, with a status of 0 or not;
// released as a signal killed it, or as it exited from its handler of a
// SIGTERM another process sent (the signal's end, k_stop_signal). Each record
// is read once. A connection the hooks did not redirect leaves none, and so
// does one whose end the proxy read before its release: shut down, then
// released as its process exited.
func TestTheHooksSayHowTheApplicationReleasedItsConnection(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	cg, err := agent.DetectCgroupPath(h.logger)
	if err != nil {
		t.Fatalf("no cgroup v2 path: %v", err)
	}
	rel, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: ebpf.AttachCgroupInetSockRelease, Program: h.objects.K_appConnRelease})
	if err != nil {
		t.Fatalf("attach k_app_conn_release to %s: %v", cg, err)
	}
	t.Cleanup(func() { _ = rel.Close() })
	stop, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "signal_generate", Program: h.objects.K_stopSignal})
	if err != nil {
		t.Fatalf("attach k_stop_signal: %v", err)
	}
	t.Cleanup(func() { _ = stop.Close() })

	// The proxy's side: each connection accepted and read to its end, as the
	// relay reads the application's end before it asks how it ended.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	// readEndHeld reads the application's end and leaves the proxy's open.
	readEndHeld := func(t *testing.T) (netip.AddrPort, net.Conn) {
		t.Helper()
		select {
		case c := <-accepted:
			t.Cleanup(func() { _ = c.Close() })
			_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
			if _, err := io.Copy(io.Discard, c); err != nil && !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("read the application's end: %v", err)
			}
			return c.RemoteAddr().(*net.TCPAddr).AddrPort(), c
		case <-time.After(30 * time.Second):
			t.Fatal("the application's connection never reached the listener")
		}
		return netip.AddrPort{}, nil
	}
	readEnd := func(t *testing.T) netip.AddrPort {
		t.Helper()
		app, c := readEndHeld(t)
		_ = c.Close()
		return app
	}
	lnPort := ln.Addr().(*net.TCPAddr).Port
	take := func(t *testing.T, app netip.AddrPort, want agent.AppConnEnd) {
		t.Helper()
		got, ok := h.TakeAppConnEnd(app)
		if !ok || got != want {
			t.Fatalf("TakeAppConnEnd(%s) = %+v, %v; want %+v, true", app, got, ok, want)
		}
		if again, ok := h.TakeAppConnEnd(app); ok {
			t.Fatalf("a second TakeAppConnEnd(%s) = %+v: the record was not deleted as it was read", app, again)
		}
	}

	t.Run("closed while its process runs", func(t *testing.T) {
		fd := redirectedTCPSocket(t, h)
		if err := unix.Connect(fd, &unix.SockaddrInet4{Port: lnPort, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(fd)
		take(t, readEnd(t), agent.AppConnEnd{})
	})
	t.Run("a connection the hooks did not redirect", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Connect(fd, &unix.SockaddrInet4{Port: lnPort, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(fd)
		app := readEnd(t)
		if got, ok := h.TakeAppConnEnd(app); ok {
			t.Fatalf("TakeAppConnEnd(%s) = %+v for a connection the hooks did not redirect", app, got)
		}
	})
	for _, tc := range []struct {
		name  string
		first string         // a step before the end: "trap" or "shutdown"
		kill  syscall.Signal // 0: the helper exits on its own, with status
		exit  int
		want  *agent.AppConnEnd // nil: no record
	}{
		{"its process exits on its own", "", 0, 0, &agent.AppConnEnd{Exiting: true}},
		{"its process exits on its own with a failure status", "", 0, 3, &agent.AppConnEnd{Exiting: true}},
		{"a SIGKILL kills its process", "", syscall.SIGKILL, 0, &agent.AppConnEnd{Exiting: true, Signal: 9}},
		{"a SIGTERM kills its process", "", syscall.SIGTERM, 0, &agent.AppConnEnd{Exiting: true, Signal: 15}},
		{"its process exits from its handler of a SIGTERM another process sent", "trap", syscall.SIGTERM, 0, &agent.AppConnEnd{Exiting: true, Signal: 15}},
		{"its process shuts its end down, then exits on its own", "shutdown", 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), heldConnHelperEnv+"="+strconv.Itoa(lnPort))
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
			})
			out := bufio.NewReader(stdout)
			line := func() string {
				s, err := out.ReadString('\n')
				if err != nil {
					t.Fatalf("the helper said %q, then: %v", s, err)
				}
				return strings.TrimSpace(s)
			}
			cookie, err := strconv.ParseUint(line(), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.objects.OrigDstByCookie.Update(cookie, structs.DestInfo{}, ebpf.UpdateAny); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, "redirected\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := strconv.Atoi(line()); err != nil {
				t.Fatal(err)
			}
			if tc.first != "" {
				if _, err := io.WriteString(stdin, tc.first+"\n"); err != nil {
					t.Fatal(err)
				}
				if got := line(); got != map[string]string{"trap": "trapped", "shutdown": "shut"}[tc.first] {
					t.Fatalf("the helper answered %q to %q", got, tc.first)
				}
			}
			if tc.want == nil {
				// The proxy reads the shut end first and holds its own end,
				// as the relay does, while the process exits; its release,
				// done once Wait returns, is after the end the proxy read.
				app, _ := readEndHeld(t)
				if _, err := fmt.Fprintf(stdin, "exit %d\n", tc.exit); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatalf("the helper: %v", err)
				}
				if got, ok := h.TakeAppConnEnd(app); ok {
					t.Fatalf("TakeAppConnEnd(%s) = %+v for a connection whose end the proxy read before its release", app, got)
				}
				return
			}
			if tc.kill != 0 {
				if err := cmd.Process.Signal(tc.kill); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fmt.Fprintf(stdin, "exit %d\n", tc.exit); err != nil {
				t.Fatal(err)
			}
			take(t, readEnd(t), *tc.want)
		})
	}
}

// redirectedTCPSocket is a TCP socket of this process's that the hooks
// redirected: an orig_dst_by_cookie entry, as connect4 leaves one.
func redirectedTCPSocket(t *testing.T, h *Hooks) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.objects.OrigDstByCookie.Update(cookie, structs.DestInfo{}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	return fd
}
