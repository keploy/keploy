//go:build linux

package proxy

import (
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// A segment dropped on its way out (a full qdisc: NET_XMIT_DROP; a netfilter
// rule: -EPERM) is a local transmit failure, which __tcp_transmit_skb has
// already counted in tcpi_bytes_sent; TCP sends it again as new data, counted
// again, never in tcpi_bytes_retrans. The wait's target counts each byte the
// application wrote once all the same (appWritten), so once every byte has
// arrived the target is reached.
//
// The drop is deterministic: a netfilter rule drops every 20th segment the
// application sends after its SYN, from the 11th on (statistic nth: a
// counter, not chance; past the handshake's ACK, so what it drops is data).
// By order, not size: a GSO segment on lo can be large enough (BIG TCP) that
// its IPv4 length reads 0. A transfer of 64 MiB is 128 segments or more (at
// most 512 KiB each), so it always has some. Root, in a network namespace of
// its own (KEPLOY_LOCAL_DROP_TEST=1, set up by the caller: the
// proven_fire_and_forget_linux job in mock_linux.yml), with iptables.
func TestTheTargetCountsALocallyDroppedSegmentOnce(t *testing.T) {
	if os.Getenv("KEPLOY_LOCAL_DROP_TEST") == "" {
		t.Skip("needs KEPLOY_LOCAL_DROP_TEST=1, root in a network namespace of its own, and iptables")
	}
	const total = 64 << 20
	sent, want, received, dropped := locallyDroppedTransfer(t, total)
	t.Logf("written %d, tcpi_bytes_sent %d, want %d, the proxy's side received %d, segments dropped %d", total, sent, want, received, dropped)
	if dropped == 0 {
		t.Fatalf("the rule dropped no segment (tcpi_bytes_sent %d of %d written): the case is not exercised", sent, total)
	}
	if sent <= total {
		t.Fatalf("the rule dropped %d segments but tcpi_bytes_sent %d is not over the %d written: this kernel no longer counts a failed transmit, the case is not exercised", dropped, sent, total)
	}
	if want != total {
		t.Fatalf("the wait's target %d is not the %d bytes the application wrote (tcpi_bytes_sent %d, all %d received)", want, total, sent, received)
	}
}

// dropEveryNth installs a netfilter rule dropping every nth segment sent to
// port after the SYN, from the (nth/2+1)th on, and returns a func that reads
// how many it dropped.
func dropEveryNth(t *testing.T, port uint16, nth int) func() uint64 {
	t.Helper()
	rule := []string{"OUTPUT", "-p", "tcp", "--dport", strconv.Itoa(int(port)), "--tcp-flags", "SYN", "NONE",
		"-m", "statistic", "--mode", "nth", "--every", strconv.Itoa(nth), "--packet", strconv.Itoa(nth / 2),
		"-j", "DROP"}
	if out, err := exec.Command("iptables", append([]string{"-w", "-A"}, rule...)...).CombinedOutput(); err != nil {
		t.Fatalf("install the drop rule: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("iptables", append([]string{"-w", "-D"}, rule...)...).CombinedOutput(); err != nil {
			t.Errorf("remove the drop rule: %v: %s", err, out)
		}
	})
	return func() uint64 {
		out, err := exec.Command("iptables", "-w", "-L", "OUTPUT", "-v", "-x", "-n").CombinedOutput()
		if err != nil {
			t.Fatalf("read the drop rule: %v: %s", err, out)
		}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) > 2 && f[2] == "DROP" && strings.Contains(l, " dpt:"+strconv.Itoa(int(port))+" ") {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				return n
			}
		}
		t.Fatalf("the drop rule is gone:\n%s", out)
		return 0
	}
}

// locallyDroppedTransfer writes total bytes over a fresh loopback connection
// kept as the proxy keeps a paired one, every 20th of its segments dropped on
// the way out (dropEveryNth), and returns, once every byte has arrived and been
// acknowledged, the writer's tcpi_bytes_sent, the wait's target, the proxy's
// side's tcpi_bytes_received and how many segments were dropped.
func locallyDroppedTransfer(t *testing.T, total int) (sent, want, received, dropped uint64) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	drops := dropEveryNth(t, mustAddrPort(t, ln.Addr()).Port(), 20)
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app := c.(*net.TCPConn)
	defer app.Close()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide := s.(*net.TCPConn)
	defer proxySide.Close()
	p := &Proxy{logger: zap.NewNop()}
	p.paired.add(proxySide, 1)
	got := make(chan int64, 1)
	go func() {
		n, _ := io.CopyN(io.Discard, proxySide, int64(total))
		got <- n
	}()
	if n, err := app.Write(make([]byte, total)); err != nil || n != total {
		t.Fatalf("write: %d, %v (the drops killed the connection)", n, err)
	}
	if n := <-got; n != int64(total) {
		t.Fatalf("the proxy's side read %d of %d", n, total)
	}
	// Every byte has arrived and been read; wait for the last ACK to land
	// at the application's side (an event-free poll is fine in a probe).
	for {
		d, err := lookupTCP(mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr()))
		if err != nil {
			t.Fatal(err)
		}
		if d.wqueue == 0 {
			targets, err := p.pairTargets(1)
			if err != nil || len(targets) != 1 {
				t.Fatalf("pairTargets = %v, %v", targets, err)
			}
			sent, want = d.info.Bytes_sent, targets[0].want
			break
		}
		runtime.Gosched()
	}
	var ti *unix.TCPInfo
	rc, _ := proxySide.SyscallConn()
	_ = rc.Control(func(fd uintptr) { ti, _ = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) })
	return sent, want, ti.Bytes_received, drops()
}

func mustAddrPort(t *testing.T, a net.Addr) netip.AddrPort {
	t.Helper()
	ap, ok := addrPortOf(a)
	if !ok {
		t.Fatalf("not TCP: %v", a)
	}
	return ap
}
