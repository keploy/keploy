package proxy

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
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

// The counts' proof (checkXNUCounts) passes what macOS gives keploy's own
// loopback connection once 3 bytes went across, and refuses each thing a
// layout that moved would read instead: a send buffer read at the wrong
// offset, two ends that began from different numbers, a state that is not
// ESTABLISHED, a received count that is not the 3 sent, a tcp_info that
// disagrees with the table, and a table that lists either end nowhere.
// Checked on every platform: the verdict is plain Go.
func TestTheCountsProofRefusesWhatAMovedLayoutReads(t *testing.T) {
	const n = 3
	writer := netip.MustParseAddrPort("127.0.0.1:50000")
	reader := netip.MustParseAddrPort("127.0.0.1:40000")
	const wIss, rIss = 1000, 9000
	good := func() ([]xnuPCB, proxySide) {
		w := xnuPCB{local: writer, remote: reader, state: xnuTCPEstablished, sndUna: wIss + 1 + n, iss: wIss, irs: rIss, rcvNxt: rIss + 1}
		r := xnuPCB{local: reader, remote: writer, state: xnuTCPEstablished, sndUna: rIss + 1, iss: rIss, irs: wIss, rcvNxt: wIss + 1 + n}
		return []xnuPCB{w, r}, proxySide{rcvNxt: wIss + 1 + n, state: xnuTCPEstablished}
	}
	if table, ps := good(); checkXNUCounts(table, writer, reader, ps, n) != nil {
		t.Fatalf("the counts of a layout read where it is refused: %v", checkXNUCounts(table, writer, reader, ps, n))
	}
	for _, c := range []struct {
		name  string
		spoil func(table []xnuPCB, ps *proxySide) []xnuPCB
		// says is what the refusal must say, where a later check would
		// refuse the spoiled read too, for another reason.
		says string
	}{
		{"a moved send buffer (sndCC)", func(tb []xnuPCB, _ *proxySide) []xnuPCB { tb[0].sndCC = 4096; return tb }, ""},
		{"a moved snd_una", func(tb []xnuPCB, _ *proxySide) []xnuPCB { tb[0].sndUna = 0; return tb }, ""},
		// The reader's counts move with its irs, so that only the two
		// ends' first numbers disagree.
		{"two ends from different first numbers (iss/irs)", func(tb []xnuPCB, ps *proxySide) []xnuPCB {
			tb[1].irs += 7
			tb[1].rcvNxt += 7
			ps.rcvNxt += 7
			return tb
		}, ""},
		{"a writer not ESTABLISHED in the table", func(tb []xnuPCB, _ *proxySide) []xnuPCB { tb[0].state = xnuTCPTimeWait; return tb }, ""},
		{"a reader not ESTABLISHED in the table", func(tb []xnuPCB, _ *proxySide) []xnuPCB { tb[1].state = xnuTCPFinWait2; return tb }, ""},
		// The tcp_info agrees with the table, so that only the count is off.
		{"a received count short of what was sent", func(tb []xnuPCB, ps *proxySide) []xnuPCB {
			tb[1].rcvNxt = wIss + 1
			ps.rcvNxt = wIss + 1
			return tb
		}, ""},
		{"a tcp_info rcv_nxt the table disagrees with", func(tb []xnuPCB, ps *proxySide) []xnuPCB { ps.rcvNxt += 2; return tb }, ""},
		{"a tcp_info state not ESTABLISHED", func(tb []xnuPCB, ps *proxySide) []xnuPCB { ps.state = xnuTCPCloseWait; return tb }, ""},
		{"a tcp_info that cannot be read", func(tb []xnuPCB, ps *proxySide) []xnuPCB {
			ps.err = errors.New("getsockopt TCP_INFO: invalid argument")
			return tb
		}, ""},
		{"no writer row", func(tb []xnuPCB, _ *proxySide) []xnuPCB { return tb[1:] }, "does not list"},
		{"no reader row", func(tb []xnuPCB, _ *proxySide) []xnuPCB { return tb[:1] }, "does not list"},
		{"an empty table", func([]xnuPCB, *proxySide) []xnuPCB { return nil }, "does not list"},
	} {
		t.Run(c.name, func(t *testing.T) {
			table, ps := good()
			table = c.spoil(table, &ps)
			err := checkXNUCounts(table, writer, reader, ps, n)
			if err == nil {
				t.Fatalf("the counts' proof passes %s", c.name)
			}
			if c.says != "" && !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the counts' proof refuses %s with %q, which does not say %q", c.name, err, c.says)
			}
		})
	}
}
