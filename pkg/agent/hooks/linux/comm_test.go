//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
)

// The key is the application's end of the connection as the kernel writes
// it (redirect_key_t): IPv4 as v4-mapped, each word the address's bytes in
// order, so an IPv4 and an IPv6 connection on one port are two keys.
func TestRedirectKeyIsTheApplicationsEndAsTheKernelWritesIt(t *testing.T) {
	ne := func(b0, b1, b2, b3 byte) uint32 { // a word as its bytes lie in memory
		k := redirectKeyFor(netip.AddrPortFrom(netip.AddrFrom4([4]byte{b0, b1, b2, b3}), 0))
		return k.IP[3]
	}
	v4 := redirectKeyFor(netip.MustParseAddrPort("127.0.0.1:40000"))
	if v4.IP[0] != 0 || v4.IP[1] != 0 || v4.IP[2] != ne(0, 0, 0xff, 0xff) || v4.IP[3] != ne(127, 0, 0, 1) || v4.Port != 40000 {
		t.Fatalf("127.0.0.1:40000 keys as %+v, want ::ffff:127.0.0.1 port 40000", v4)
	}
	if mapped := redirectKeyFor(netip.MustParseAddrPort("[::ffff:127.0.0.1]:40000")); mapped != v4 {
		t.Fatalf("::ffff:127.0.0.1 keys as %+v, want the same as 127.0.0.1 (%+v)", mapped, v4)
	}
	v6 := redirectKeyFor(netip.MustParseAddrPort("[::1]:40000"))
	if v6 == v4 || v6.IP[3] != ne(0, 0, 0, 1) || v6.Port != 40000 {
		t.Fatalf("[::1]:40000 keys as %+v, want ::1, apart from the IPv4 connection on the same port", v6)
	}
}

// By source port alone the Linux hooks name nothing: a port can be two
// connections' at once.
func TestTheLinuxHooksNameNoDestinationByPortAlone(t *testing.T) {
	h := &Hooks{}
	if _, err := h.Get(context.Background(), 40000); !errors.Is(err, errRedirectByPort) {
		t.Fatalf("Get by port: %v, want errRedirectByPort", err)
	}
	if err := h.Delete(context.Background(), 40000); !errors.Is(err, errRedirectByPort) {
		t.Fatalf("Delete by port: %v, want errRedirectByPort", err)
	}
}

// The key's bytes are struct redirect_key_t's as k_sockops writes it: the
// application's address, its 16 bytes in order (IPv4 as v4-mapped), then its
// port as a host-order __u32. Checked against the bytes themselves, not
// against redirectKeyFor's own arithmetic.
func TestRedirectKeyBytesAreTheKernels(t *testing.T) {
	for _, c := range []struct {
		app  string
		addr [16]byte
	}{
		{"127.0.0.1:40000", [16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}},
		{"[::ffff:127.0.0.1]:40000", [16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}},
		{"[::1]:40000", [16]byte{15: 1}},
		{"[2001:db8::7]:40000", [16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 7}},
	} {
		var got bytes.Buffer
		if err := binary.Write(&got, binary.NativeEndian, redirectKeyFor(netip.MustParseAddrPort(c.app))); err != nil {
			t.Fatal(err)
		}
		want := binary.NativeEndian.AppendUint32(c.addr[:], 40000)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s keys as % x, want % x", c.app, got.Bytes(), want)
		}
	}
}
