//go:build linux

package linux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/captureloss"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// The proven switch's watch on the application's non-DNS UDP (keploy/ebpf
// k_udp.h, design v14 §3.2 (c)). keploy leaves such UDP where the
// application sends it, so a datagram that answers it was answered live,
// neither recorded nor mocked: counted as a udp_reply capture loss. An answer
// is what a socket that sent first receives, or what a socket that received
// first (a server) receives from a peer it asked first; never a request a
// server answers, a datagram from another member's socket (the application
// talking to itself) or DNS's (keploy serves DNS). A datagram whose source
// cannot be read counts. The kernel reports a socket's answer once per epoch,
// which the watch moves on at each read of its count (snapshot), so a socket
// answered again after a read is counted again. A datagram to UDP port 443
// (QUIC) is refused in the kernel, so the client falls back to TCP; a
// connect() to it never is (glibc's address sort probes addresses with one).
// Only under KEPLOY_PROVEN: the agent then sets FlagProvenUDP and attaches the
// bind, ingress, egress and release programs, which today's users never run.

// udpDest mirrors struct dest_info_t.
type udpDest struct {
	IPVersion  uint32
	IP4        uint32
	IP6        [4]uint32
	Port       uint32
	KernelPid  uint32
	AgentNsPid uint32
	_          uint32
}

// udpReplyEventSize is struct udp_reply_event's size: a report of any other
// size is one keploy cannot read.
const udpReplyEventSize = 56

// udpReplyEvent mirrors struct udp_reply_event (k_udp.h).
type udpReplyEvent struct {
	TGID    uint32
	SrcPort uint32
	Dst     udpDest
	Epoch   uint32 // the watch's epoch the reply came in
	Flags   uint32 // udpSrcUnread
}

// udpSrcUnread (udpReplyEvent.Flags): the datagram's source could not be read
// (an IPv6 extension header first), so whose it is cannot be told: it counts.
const udpSrcUnread = 1

func (d udpDest) String() string {
	var a netip.Addr
	if d.IPVersion == 6 {
		var b [16]byte
		for i, w := range d.IP6 {
			binary.BigEndian.PutUint32(b[4*i:], w)
		}
		a = netip.AddrFrom16(b).Unmap() // a dual-stack socket's IPv4 peer
	} else {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], d.IP4)
		a = netip.AddrFrom4(b)
	}
	return netip.AddrPortFrom(a, uint16(d.Port)).String()
}

type udpWatch struct {
	links  []link.Link
	rd     *ringbuf.Reader
	maps   *bpfObjects
	losses captureloss.Counter
	mu     sync.Mutex
	// epoch is the kernel's (udp_reply_epoch), moved on at each snapshot.
	epoch uint32
	// open are the reports of the epoch the kernel is in: charged by the
	// snapshot that moves it on, never by an earlier one, whose count would
	// then hold a report while its epoch, still open, hides the socket's
	// next replies (a reply that lands between a snapshot's move and its
	// read).
	open []string
	// err is why what the watch counted cannot be told any more: an epoch it
	// could not move on, a count of the kernel's it could not read, a report
	// it could not read (CaptureLossesErr). It stays.
	err error
	// drain lets a read wait for every report before it (DrainCaptureLosses).
	drain *reportDrain
}

// startProvenUDP starts the UDP watch under the proven switch only, and
// returns the agent-info flags that turn the kernel's side on: the programs
// first, then the flag, so the kernel never registers what no reader drains.
// With the switch off nothing is attached and no flag is returned.
func (h *Hooks) startProvenUDP(cg string) (uint32, error) {
	if !provenswitch.On() {
		return 0, nil
	}
	w, err := startUDPWatch(&h.objects, cg)
	if err != nil {
		return 0, err
	}
	h.udpMu.Lock()
	h.udp = w
	h.udpMu.Unlock()
	go func() {
		defer utils.Recover(h.logger)
		w.watch(h.logger)
	}()
	return structs.FlagProvenUDP, nil
}

// startUDPWatch attaches the UDP watch's programs to the cgroup at cg and
// reads its reports. An error leaves nothing attached.
func startUDPWatch(objs *bpfObjects, cg string) (*udpWatch, error) {
	if objs.UdpReplyEpoch == nil || objs.UdpReplyEvents == nil || objs.UdpMemberSocksFailures == nil || objs.UdpReplyEventsLost == nil ||
		objs.K_udpIngress == nil || objs.K_udpEgress == nil || objs.K_udpSockRelease == nil || objs.K_udpBind4 == nil || objs.K_udpBind6 == nil {
		return nil, errors.New("keploy's BPF objects are not loaded: the UDP watch's programs and maps are missing")
	}
	// 2: the kernel reads an epoch never set (0) as 1.
	w := &udpWatch{maps: objs, epoch: 2, drain: newReportDrain()}
	if err := objs.UdpReplyEpoch.Update(uint32(0), w.epoch, ebpf.UpdateAny); err != nil {
		return nil, fmt.Errorf("set the UDP watch's epoch: %w", err)
	}
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
		name   string
	}{
		{objs.K_udpBind4, ebpf.AttachCGroupInet4Bind, "k_udp_bind4"},
		{objs.K_udpBind6, ebpf.AttachCGroupInet6Bind, "k_udp_bind6"},
		{objs.K_udpIngress, ebpf.AttachCGroupInetIngress, "k_udp_ingress"},
		{objs.K_udpEgress, ebpf.AttachCGroupInetEgress, "k_udp_egress"},
		{objs.K_udpSockRelease, ebpf.AttachCgroupInetSockRelease, "k_udp_sock_release"},
	} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: p.attach, Program: p.prog})
		if err != nil {
			w.close()
			return nil, fmt.Errorf("attach %s: %w", p.name, err)
		}
		w.links = append(w.links, l)
	}
	rd, err := ringbuf.NewReader(objs.UdpReplyEvents)
	if err != nil {
		w.close()
		return nil, fmt.Errorf("read udp_reply_events: %w", err)
	}
	w.rd = rd
	return w, nil
}

func (w *udpWatch) close() {
	if w == nil {
		return
	}
	if w.rd != nil {
		_ = w.rd.Close()
	}
	for _, l := range w.links {
		_ = l.Close()
	}
}

// watch counts each reported reply until the reader is closed.
func (w *udpWatch) watch(logger *zap.Logger) {
	// However it ends (a panic too): a drain waiting on it ends with it.
	defer w.drain.stop(errors.New("the UDP watch ended"))
	warned := false
	for {
		rec, err := w.rd.Read()
		switch {
		case errors.Is(err, ringbuf.ErrFlushed):
			// Every report before a read's flush is handled (drain).
			w.drain.flushed()
			continue
		case errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed):
			w.drain.stop(errReportClosed)
			return
		case err != nil:
			// The ring cannot be read past here: nothing after can be
			// counted.
			err = fmt.Errorf("read a UDP reply report: %w", err)
			w.fail(err)
			w.drain.stop(err)
			return
		}
		var e udpReplyEvent
		if len(rec.RawSample) != udpReplyEventSize {
			w.fail(fmt.Errorf("a UDP reply report of %d bytes, want %d: keploy's agent and its BPF objects disagree", len(rec.RawSample), udpReplyEventSize))
			continue
		}
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			w.fail(fmt.Errorf("decode a UDP reply report of %d bytes: %w", len(rec.RawSample), err))
			continue
		}
		what := w.handle(e)
		if !warned {
			warned = true
			logger.Warn("the application's UDP got a reply from the real destination: keploy neither records nor mocks UDP other than DNS",
				zap.String("what", what), zap.Uint32("tgid", e.TGID),
				zap.String("next_step", "add a port-only bypassRules entry for it if it is meant to be live, or reach it over TCP"))
		}
	}
}

// handle counts a report by the epoch it came in: one of the epoch the
// kernel is in waits for the snapshot that moves it on.
func (w *udpWatch) handle(e udpReplyEvent) string {
	on := "UDP to " + e.Dst.String()
	if e.Dst.IPVersion == 0 {
		// A socket registered at bind() that has asked no one yet (a
		// server's unreadable first datagram): the kernel has no
		// destination for it (k_udp.h).
		on = "UDP on a socket the application bound"
	}
	what := fmt.Sprintf("%s answered from port %d", on, e.SrcPort)
	if e.Flags&udpSrcUnread != 0 {
		what = on + " got a datagram from a source keploy could not read"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.Epoch == w.epoch {
		w.open = append(w.open, what)
	} else {
		w.losses.Add(models.CaptureLossUDPReply, what)
	}
	return what
}

// fail notes why what the watch counted cannot be told any more; the first
// reason stays.
func (w *udpWatch) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

// failure is why what the watch counted cannot be told, if it cannot.
func (w *udpWatch) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// snapshot moves the kernel's epoch on, then reads what the watch counted,
// with the sockets and reports the kernel had no room for: each a reply it
// could not report. A socket answered after the epoch moved on is reported
// again (k_udp.h), so a test after a long-lived socket's first reply still
// sees its next one.
func (w *udpWatch) snapshot() models.CaptureLosses {
	w.closeEpoch()
	return w.counts()
}

// closeEpoch charges the reports of the epoch the kernel is in, then moves
// the kernel's epoch on.
func (w *udpWatch) closeEpoch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, what := range w.open {
		w.losses.Add(models.CaptureLossUDPReply, what)
	}
	w.open = nil
	if w.err != nil {
		return // failed (its maps may be closed): what it counts is unknown
	}
	w.epoch++
	if w.epoch == 0 {
		w.epoch = 1 // 0 is the kernel's "never set"
	}
	if err := w.maps.UdpReplyEpoch.Update(uint32(0), w.epoch, ebpf.UpdateAny); err != nil && w.err == nil {
		w.err = fmt.Errorf("move the UDP watch's epoch on: %w", err)
	}
}

// counts is what the watch charged, with what the kernel had no room for.
func (w *udpWatch) counts() models.CaptureLosses {
	out := w.losses.Snapshot()
	if w.failure() != nil {
		return out // its maps may be closed; CaptureLossesErr says it is unknown
	}
	var missed uint64
	for _, m := range []*ebpf.Map{w.maps.UdpMemberSocksFailures, w.maps.UdpReplyEventsLost} {
		var n uint64
		if m == nil {
			w.fail(errors.New("the UDP watch's count of what the kernel had no room for is not loaded"))
			continue
		}
		if err := m.Lookup(uint32(0), &n); err != nil {
			w.fail(fmt.Errorf("read the UDP watch's count of what the kernel had no room for: %w", err))
			continue
		}
		missed += n
	}
	if missed > 0 {
		for i := range out {
			if out[i].Kind == models.CaptureLossUDPReply {
				out[i].Count += missed
				return out
			}
		}
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossUDPReply, Count: missed, What: "UDP sockets or replies the kernel had no room to report"})
	}
	return out
}
