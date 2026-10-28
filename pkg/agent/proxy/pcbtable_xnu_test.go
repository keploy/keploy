package proxy

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
)

// xnuRecord is one struct xtcpcb64 as macOS lays it out (pcbtable_xnu.go).
func xnuRecord(local, remote netip.AddrPort, b xnuPCB) []byte {
	r := make([]byte, xnuXtcpcb64Len)
	binary.LittleEndian.PutUint32(r, xnuXtcpcb64Len)
	binary.BigEndian.PutUint16(r[xnuOffLport:], local.Port())
	binary.BigEndian.PutUint16(r[xnuOffFport:], remote.Port())
	put := func(off int, a netip.Addr) {
		if a.Is4() {
			a4 := a.As4()
			copy(r[off+xnuIn4in6Offset:], a4[:])
			return
		}
		a16 := a.As16()
		copy(r[off:], a16[:])
	}
	if local.Addr().Is4() {
		r[xnuOffVflag] = xnuInpIPv4
	} else {
		r[xnuOffVflag] = xnuInpIPv6
	}
	put(xnuOffLaddr, local.Addr())
	put(xnuOffFaddr, remote.Addr())
	binary.LittleEndian.PutUint32(r[xnuOffState:], uint32(b.state))
	binary.LittleEndian.PutUint32(r[xnuOffSndUna:], b.sndUna)
	binary.LittleEndian.PutUint32(r[xnuOffSndCC:], b.sndCC)
	binary.LittleEndian.PutUint32(r[xnuOffIss:], b.iss)
	binary.LittleEndian.PutUint32(r[xnuOffIrs:], b.irs)
	binary.LittleEndian.PutUint32(r[xnuOffRcvNxt:], b.rcvNxt)
	binary.LittleEndian.PutUint32(r[xnuOffSndWnd:], b.sndWnd)
	return r
}

func xnuGen() []byte {
	g := make([]byte, xnuXinpgenLen)
	binary.LittleEndian.PutUint32(g, xnuXinpgenLen)
	return g
}

func xnuTable(records ...[]byte) []byte {
	buf := xnuGen()
	for _, r := range records {
		buf = append(buf, r...)
	}
	return append(buf, xnuGen()...)
}

// The table is read at the layout's offsets, for IPv4 and IPv6 sockets, and
// a socket is found by its ends however they are written.
func TestMacOSsTCPTableIsReadAtItsLayout(t *testing.T) {
	app := netip.MustParseAddrPort("127.0.0.1:50123")
	proxy := netip.MustParseAddrPort("127.0.0.1:16789")
	app6 := netip.MustParseAddrPort("[::1]:50124")
	proxy6 := netip.MustParseAddrPort("[::1]:16790")
	want := xnuPCB{state: xnuTCPEstablished, sndUna: 0x101, sndCC: 572, iss: 0xFFFFFF00, irs: 7, rcvNxt: 8, sndWnd: 0}
	want6 := xnuPCB{state: xnuTCPLastAck, sndUna: 11, sndCC: 3, iss: 10, irs: 20, rcvNxt: 22, sndWnd: 65535}
	table, err := parseXNUPCBs(xnuTable(xnuRecord(app, proxy, want), xnuRecord(app6, proxy6, want6)))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := findXNUPCB(table, netip.MustParseAddrPort("[::ffff:127.0.0.1]:50123"), proxy)
	want.local, want.remote = app, proxy
	if !ok || got != want {
		t.Fatalf("IPv4 socket = %+v, %v; want %+v", got, ok, want)
	}
	got6, ok := findXNUPCB(table, app6, proxy6)
	want6.local, want6.remote = app6, proxy6
	if !ok || got6 != want6 {
		t.Fatalf("IPv6 socket = %+v, %v; want %+v", got6, ok, want6)
	}
	if _, ok := findXNUPCB(table, proxy, app); ok {
		t.Fatal("found a socket by its ends swapped")
	}
	// writeSeq: what the send buffer holds counts, across the 2^32 wrap; a
	// FIN not yet acknowledged counts one more.
	if s := got.writeSeq() - got.iss - 1; s != 0x200+572 {
		t.Fatalf("IPv4 written = %d, want 0x200 acknowledged and 572 held", s)
	}
	if s := got6.writeSeq() - got6.iss - 1; s != 4 {
		t.Fatalf("IPv6 written in LAST_ACK = %d, want 3 bytes and the FIN", s)
	}
}

// A table in any layout but the one keploy reads is refused, never read at
// the wrong offsets.
func TestMacOSsTCPTableInAnotherLayoutIsRefused(t *testing.T) {
	rec := xnuRecord(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2"), xnuPCB{})
	longer := append(append([]byte{}, rec...), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(longer, xnuXtcpcb64Len+4)
	for name, buf := range map[string][]byte{
		"a longer record":   xnuTable(longer),
		"no closing header": append(xnuGen(), rec...),
		"no header":         rec,
		"cut short":         xnuTable(rec)[:xnuXinpgenLen+100],
	} {
		if _, err := parseXNUPCBs(buf); !errors.Is(err, errXNUTableLayout) {
			t.Errorf("%s: err = %v, want errXNUTableLayout", name, err)
		}
	}
}
