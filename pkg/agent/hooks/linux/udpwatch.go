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
	"go.keploy.io/server/v3/pkg/models"
)

// The proven switch's watch on the application's non-DNS UDP (keploy/ebpf
// k_udp.h, design v14 §3.2 (c)). keploy leaves such UDP where the
// application sends it, so a datagram that gets a reply was answered live,
// neither recorded nor mocked: counted as a udp_reply capture loss. A send
// to UDP port 443 (QUIC) is refused in the kernel, so the client falls back
// to TCP. Only under KEPLOY_PROVEN: the agent then sets FlagProvenUDP and
// attaches the ingress and release programs, which today's users never run.

// udpDest mirrors struct dest_info_t.
type udpDest struct {
	IPVersion uint32
	IP4       uint32
	IP6       [4]uint32
	Port      uint32
	KernelPid uint32
}

// udpReplyEvent mirrors struct udp_reply_event (k_udp.h).
type udpReplyEvent struct {
	TGID    uint32
	SrcPort uint32
	Dst     udpDest
}

func (d udpDest) String() string {
	var a netip.Addr
	if d.IPVersion == 6 {
		var b [16]byte
		for i, w := range d.IP6 {
			binary.BigEndian.PutUint32(b[4*i:], w)
		}
		a = netip.AddrFrom16(b)
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
}

// startUDPWatch attaches the UDP watch's programs to the cgroup at cg and
// reads its reports. An error leaves nothing attached.
func startUDPWatch(objs *bpfObjects, cg string) (*udpWatch, error) {
	w := &udpWatch{maps: objs}
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
		name   string
	}{
		{objs.K_udpIngress, ebpf.AttachCGroupInetIngress, "k_udp_ingress"},
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
			logger.Debug("failed to read a UDP reply report", zap.Error(err))
			continue
		}
		var e udpReplyEvent
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			logger.Debug("failed to decode a UDP reply report", zap.Error(err))
			continue
		}
		what := fmt.Sprintf("UDP to %s answered from port %d", e.Dst, e.SrcPort)
		w.losses.Add(models.CaptureLossUDPReply, what)
		if !warned {
			warned = true
			logger.Warn("the application's UDP got a reply from the real destination: keploy neither records nor mocks UDP other than DNS",
				zap.String("what", what), zap.Uint32("tgid", e.TGID),
				zap.String("next_step", "list that destination in mock.real if it is meant to be live, or reach it over TCP"))
		}
	}
}

// snapshot is what the watch counted, with the sockets and reports the
// kernel had no room for: each a reply it could not report.
func (w *udpWatch) snapshot() models.CaptureLosses {
	out := w.losses.Snapshot()
	var missed uint64
	for _, m := range []*ebpf.Map{w.maps.UdpMemberSocksFailures, w.maps.UdpReplyEventsLost} {
		var n uint64
		if m != nil && m.Lookup(uint32(0), &n) == nil {
			missed += n
		}
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
