//go:build linux

package proxy

import (
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// A segment the qdisc drops is a local transmit failure (NET_XMIT_DROP),
// which __tcp_transmit_skb has already counted in tcpi_bytes_sent; TCP sends
// it again as new data, counted again, never in tcpi_bytes_retrans. The
// wait's target counts each byte the application wrote once all the same
// (appWritten), so once every byte has arrived the target is reached. Root,
// in a network namespace of its own with a small queue on lo
// (KEPLOY_QDISC_TEST=1, set up by the caller: the proven_fire_and_forget_linux
// job in mock_linux.yml).
func TestTheTargetCountsALocallyDroppedSegmentOnce(t *testing.T) {
	if os.Getenv("KEPLOY_QDISC_TEST") == "" {
		t.Skip("needs KEPLOY_QDISC_TEST=1, root and a small qdisc on lo")
	}
	// A transfer the qdisc happened not to drop from proves nothing: the
	// case is one whose tcpi_bytes_sent counted a dropped segment twice.
	const total = 64 << 20
	const attempts = 20
	for i := 1; i <= attempts; i++ {
		sent, want, received := locallyDroppedTransfer(t, total)
		t.Logf("transfer %d: written %d, tcpi_bytes_sent %d, want %d, the proxy's side received %d", i, total, sent, want, received)
		if want != total {
			t.Fatalf("the wait's target %d is not the %d bytes the application wrote (tcpi_bytes_sent %d, all %d received)", want, total, sent, received)
		}
		if sent > total {
			return
		}
	}
	t.Fatalf("the qdisc dropped no segment in %d transfers of %d bytes: the case is not exercised", attempts, total)
}

// locallyDroppedTransfer writes total bytes over a fresh loopback connection
// kept as the proxy keeps a paired one, and returns, once every byte has
// arrived and been acknowledged, the writer's tcpi_bytes_sent, the wait's
// target and the proxy's side's tcpi_bytes_received.
func locallyDroppedTransfer(t *testing.T, total int) (sent, want, received uint64) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
		t.Fatalf("write: %d, %v (the qdisc killed the connection)", n, err)
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
	return sent, want, ti.Bytes_received
}

func mustAddrPort(t *testing.T, a net.Addr) netip.AddrPort {
	t.Helper()
	ap, ok := addrPortOf(a)
	if !ok {
		t.Fatalf("not TCP: %v", a)
	}
	return ap
}
