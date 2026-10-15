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
// application sends it, so a datagram that gets a reply was answered live,
// neither recorded nor mocked: counted as a udp_reply capture loss. The
// kernel reports a socket's reply once per epoch, which the watch moves on at
// each read of its count (snapshot), so a socket answered again after a read
// is counted again. A datagram to UDP port 443 (QUIC) is refused in the
// kernel, so the client falls back to TCP; a connect() to it never is
// (glibc's address sort probes addresses with one). Only under
// KEPLOY_PROVEN: the agent then sets FlagProvenUDP and attaches the ingress,
// egress and release programs, which today's users never run.

// udpDest mirrors struct dest_info_t.
type udpDest struct {
	IPVersion uint32
	IP4       uint32
	IP6       [4]uint32
	Port      uint32
	KernelPid uint32
}

// udpReplyEventSize is struct udp_reply_event's size: a report of any other
// size is one keploy cannot read.
const udpReplyEventSize = 48

// udpReplyEvent mirrors struct udp_reply_event (k_udp.h).
type udpReplyEvent struct {
	TGID    uint32
	SrcPort uint32
	Dst     udpDest
	Epoch   uint32 // the watch's epoch the reply came in
	_       uint32
}

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
	// err is why what the watch counted cannot be told any more: an epoch it
	// could not move on, a count of the kernel's it could not read, a report
	// it could not read (CaptureLossesErr). It stays.
	err error
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
		objs.K_udpIngress == nil || objs.K_udpEgress == nil || objs.K_udpSockRelease == nil {
		return nil, errors.New("keploy's BPF objects are not loaded: the UDP watch's programs and maps are missing")
	}
	// 2: the kernel reads an epoch never set (0) as 1.
	w := &udpWatch{maps: objs, epoch: 2}
	if err := objs.UdpReplyEpoch.Update(uint32(0), w.epoch, ebpf.UpdateAny); err != nil {
		return nil, fmt.Errorf("set the UDP watch's epoch: %w", err)
	}
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
		name   string
	}{
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
	warned := false
	for {
		rec, err := w.rd.Read()
		if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		if err != nil {
			w.fail(fmt.Errorf("read a UDP reply report: %w", err))
			continue
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
		what := fmt.Sprintf("UDP to %s answered from port %d", e.Dst, e.SrcPort)
		w.losses.Add(models.CaptureLossUDPReply, what)
		if !warned {
			warned = true
			logger.Warn("the application's UDP got a reply from the real destination: keploy neither records nor mocks UDP other than DNS",
				zap.String("what", what), zap.Uint32("tgid", e.TGID),
				zap.String("next_step", "add its port to bypassRules if it is meant to be live, or reach it over TCP"))
		}
	}
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
	w.mu.Lock()
	w.epoch++
	if w.epoch == 0 {
		w.epoch = 1 // 0 is the kernel's "never set"
	}
	if err := w.maps.UdpReplyEpoch.Update(uint32(0), w.epoch, ebpf.UpdateAny); err != nil && w.err == nil {
		w.err = fmt.Errorf("move the UDP watch's epoch on: %w", err)
	}
	w.mu.Unlock()
	out := w.losses.Snapshot()
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
