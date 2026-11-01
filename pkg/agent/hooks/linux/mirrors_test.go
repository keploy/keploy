//go:build linux

package linux

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// btfStruct is the struct named name in spec's BTF.
func btfStruct(t *testing.T, spec *ebpf.CollectionSpec, name string) *btf.Struct {
	t.Helper()
	var s *btf.Struct
	if err := spec.Types.TypeByName(name, &s); err != nil {
		t.Fatalf("struct %s in the object's BTF: %v", name, err)
	}
	return s
}

// sameLayout fails unless the Go mirror m has the C struct c's size and, field
// for field in order, its offsets (a Go `_` field stands for a C pad).
func sameLayout(t *testing.T, c *btf.Struct, m any) {
	t.Helper()
	typ := reflect.TypeOf(m)
	if got := binary.Size(m); got != int(c.Size) {
		t.Errorf("%s: %d bytes, want struct %s's %d", typ, got, c.Name, c.Size)
	}
	if typ.NumField() != len(c.Members) {
		t.Errorf("%s: %d fields, want struct %s's %d members", typ, typ.NumField(), c.Name, len(c.Members))
		return
	}
	for i, mem := range c.Members {
		f := typ.Field(i)
		if want := uint32(mem.Offset) / 8; uint32(f.Offset) != want {
			t.Errorf("%s.%s at byte %d, want struct %s's %s at %d", typ, f.Name, f.Offset, c.Name, mem.Name, want)
		}
	}
}

// keploy's Go mirrors of the BPF structs that hold a redirected connection's
// original destination (dest_info_t, with the connecting process's number in
// the agent's PID namespace) have the objects' layouts, and the maps keploy
// reads them from hold values of their size: a mirror that drifts from its
// object reads every lookup as an error, or the process's numbers from the
// wrong bytes.
func TestTheDestinationMirrorsMatchTheObjects(t *testing.T) {
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	dst := btfStruct(t, spec, "dest_info_t")
	sameLayout(t, dst, structs.DestInfo{})
	sameLayout(t, dst, udpDest{})
	var names []string
	for _, m := range dst.Members {
		names = append(names, m.Name)
	}
	if want := []string{"ip_version", "dest_ip4", "dest_ip6", "dest_port", "kernelPid", "agentNsPid", "_pad"}; !reflect.DeepEqual(names, want) {
		t.Errorf("dest_info_t's members are %q, want %q (structs.DestInfo mirrors them in order)", names, want)
	}
	sameLayout(t, btfStruct(t, spec, "proxy_conn_mark_t"), proxyConnMark{})
	// struct udp_reply_event is a ring buffer's record, not in the object's
	// BTF: u32 tgid, u32 src_port, a dest_info_t, u32 epoch, u32 flags (k_udp.h).
	if want := 4 + 4 + int(dst.Size) + 4 + 4; udpReplyEventSize != want || binary.Size(udpReplyEvent{}) != want {
		t.Errorf("udp_reply_event is %d bytes with this dest_info_t; udpReplyEventSize says %d, its Go mirror is %d", want, udpReplyEventSize, binary.Size(udpReplyEvent{}))
	}
	if off := reflect.TypeOf(udpReplyEvent{}).Field(3).Offset; off != uintptr(8+dst.Size) {
		t.Errorf("udpReplyEvent.Epoch at byte %d, want %d, after the dest_info_t", off, 8+dst.Size)
	}

	for name, want := range map[string]int{
		"redirect_proxy_map":  binary.Size(structs.DestInfo{}),
		"orig_dst_by_cookie":  binary.Size(structs.DestInfo{}),
		"proxy_listener_mark": binary.Size(proxyConnMark{}),
	} {
		m, ok := spec.Maps[name]
		if !ok {
			t.Errorf("the object has no map %s", name)
			continue
		}
		if int(m.ValueSize) != want {
			t.Errorf("%s holds %d-byte values, its Go mirror is %d bytes", name, m.ValueSize, want)
		}
	}
}
