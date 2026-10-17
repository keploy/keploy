//go:build linux

package linux

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// With the proven switch off the UDP watch starts nothing and turns nothing
// on in the kernel: no programs (the hooks' objects are not even loaded
// here), no flag, no losses.
func TestTheUDPWatchStaysOffWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := &Hooks{logger: zap.NewNop()}
	flags, err := h.startProvenUDP("/nonexistent-cgroup")
	if err != nil || flags != 0 {
		t.Fatalf("startProvenUDP with the switch off = %#x, %v; want 0, nil", flags, err)
	}
	if flags&structs.FlagProvenUDP != 0 || h.udp != nil {
		t.Fatalf("with the switch off the UDP watch started: flags %#x, watch %v", flags, h.udp)
	}
	if got := h.udpLosses(); len(got) != 0 {
		t.Fatalf("with the switch off the UDP watch counted %+v", got)
	}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("with the switch off, CaptureLossesErr = %v", err)
	}
}

// Under the switch the watch needs the hooks' objects: with none loaded it
// fails, and turns nothing on.
func TestTheUDPWatchFailsWithoutItsObjectsUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &Hooks{logger: zap.NewNop()}
	flags, err := h.startProvenUDP("/nonexistent-cgroup")
	if err == nil || flags != 0 || h.udp != nil {
		t.Fatalf("startProvenUDP with no objects under the switch = %#x, %v (watch %v); want 0 and an error", flags, err, h.udp)
	}
}

// A watch that cannot tell what it counted (an epoch it could not move on, a
// report it could not read) leaves the capture losses unknown, never 0.
func TestAUDPWatchFailureIsUnknownNeverZero(t *testing.T) {
	h := &Hooks{logger: zap.NewNop(), udp: &udpWatch{}}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("a watch that has not failed: CaptureLossesErr = %v", err)
	}
	h.udp.fail(errors.New("a UDP reply report of 40 bytes"))
	if err := h.CaptureLossesErr(); err == nil || !strings.Contains(err.Error(), "40 bytes") {
		t.Fatalf("CaptureLossesErr = %v, want the watch's failure", err)
	}
}

// A report names what the kernel knows: the destination when it has one, a
// bound socket when it has none, and a datagram whose source it could not
// read.
func TestAUDPReportNamesWhatTheKernelKnows(t *testing.T) {
	w := &udpWatch{epoch: 2}
	to := udpDest{IPVersion: 4, IP4: 0x7f000001, Port: 9000}
	for _, c := range []struct {
		e    udpReplyEvent
		want string
	}{
		{udpReplyEvent{SrcPort: 9000, Dst: to, Epoch: 1}, "UDP to 127.0.0.1:9000 answered from port 9000"},
		{udpReplyEvent{SrcPort: 9000, Epoch: 1}, "UDP on a socket the application bound answered from port 9000"},
		{udpReplyEvent{Dst: to, Epoch: 1, Flags: udpSrcUnread}, "UDP to 127.0.0.1:9000 got a datagram from a source keploy could not read"},
		{udpReplyEvent{Epoch: 1, Flags: udpSrcUnread}, "UDP on a socket the application bound got a datagram from a source keploy could not read"},
	} {
		if got := w.handle(c.e); got != c.want {
			t.Errorf("handle(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
	if l := w.losses.Snapshot(); len(l) != 1 || l[0].Count != 4 {
		t.Fatalf("losses %+v, want 4 udp_reply", l)
	}
}

// With the proven switch off the UDP watch's maps, which the kernel allocates
// whole at load, are loaded at their smallest: the watch costs nothing.
// Under the switch they keep the sizes keploy/ebpf gave them.
func TestTheUDPWatchsMapsCostNothingWithoutTheSwitch(t *testing.T) {
	sizes := func(on string) map[string]uint32 {
		t.Helper()
		t.Setenv(provenswitch.Env, on)
		spec, err := hookSpec()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]uint32{}
		for _, name := range unswitchedMaps {
			m, ok := spec.Maps[name]
			if !ok {
				t.Fatalf("the object has no %s", name)
			}
			out[name] = m.MaxEntries
		}
		return out
	}
	off, on := sizes("0"), sizes("1")
	for _, name := range unswitchedMaps {
		if off[name] >= on[name] {
			t.Errorf("%s: %d entries with the switch off, %d with it on: want it at its smallest when off", name, off[name], on[name])
		}
	}
	if off["udp_peer_first"] != 1 || off["udp_member_addrs"] != 1 || off["udp_reply_events"] != uint32(os.Getpagesize()) {
		t.Fatalf("with the switch off: %v, want one entry each and one page of ring", off)
	}
}

// Every map of the proven switch's UDP and readiness watches (udp_*,
// ready_*) the kernel allocates whole at load costs a page at most with the
// switch off: a ring buffer one page, a preallocated hash or array a page's
// worth of entries (64 bytes counted for each one's overhead). A map added
// later, and not listed in unswitchedMaps, fails it.
func TestEverySwitchOnlyMapCostsAPageAtMostWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	spec, err := hookSpec()
	if err != nil {
		t.Fatal(err)
	}
	shrinkUnswitchedMaps(spec)
	page := os.Getpagesize()
	checked := 0
	for name, m := range spec.Maps {
		if !strings.HasPrefix(name, "udp_") && !strings.HasPrefix(name, "ready_") {
			continue
		}
		checked++
		switch {
		case m.Type == ebpf.RingBuf:
			if int(m.MaxEntries) > page {
				t.Errorf("%s: a ring of %d bytes with the switch off, want one page (%d)", name, m.MaxEntries, page)
			}
		case m.Flags&unix.BPF_F_NO_PREALLOC != 0:
			// Allocated as it fills, and nothing fills it with the switch off.
		default:
			if cost := int(m.MaxEntries) * (int(m.KeySize+m.ValueSize) + 64); cost > page {
				t.Errorf("%s: %d entries, about %d bytes allocated at load with the switch off, want a page at most (list it in unswitchedMaps)", name, m.MaxEntries, cost)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the object has no udp_ or ready_ map")
	}
}
