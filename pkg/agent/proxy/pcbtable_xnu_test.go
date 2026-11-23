package proxy

import (
	"encoding/binary"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
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
	binary.LittleEndian.PutUint16(r[xnuOffSoState:], b.soState)
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
	want := xnuPCB{state: xnuTCPEstablished, soState: xnuSSIsConnected, sndUna: 0x101, sndCC: 572, iss: 0xFFFFFF00, irs: 7, rcvNxt: 8, sndWnd: 0}
	want6 := xnuPCB{state: xnuTCPLastAck, soState: xnuSSNoFDRef | xnuSSIsConnected, sndUna: 11, sndCC: 3, iss: 10, irs: 20, rcvNxt: 22, sndWnd: 65535}
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

// checkXNUAppClosed passes only when the live loopback socket shows
// SS_ISCONNECTED with SS_NOFDREF/SS_CANTSENDMORE/SS_CANTRCVMORE clear and the
// closed-whole socket shows SS_NOFDREF set.
func TestCheckXNUAppClosedProvesLiveAndOrphanSSNoFDRef(t *testing.T) {
	a := netip.MustParseAddrPort("127.0.0.1:51000")
	r := netip.MustParseAddrPort("127.0.0.1:41000")
	goodLive := []xnuPCB{{local: a, remote: r, state: xnuTCPEstablished, soState: xnuSSIsConnected}}
	goodClosed := []xnuPCB{{local: a, remote: r, state: xnuTCPFinWait2, soState: xnuSSNoFDRef | xnuSSCantSend}}
	if err := checkXNUAppClosed(goodLive, goodClosed, a, r); err != nil {
		t.Fatalf("valid live+closed tables refused: %v", err)
	}
	for _, c := range []struct {
		name         string
		live, closed []xnuPCB
		says         string
	}{
		{"live socket missing", nil, goodClosed, "open loopback"},
		{"live missing SS_ISCONNECTED", []xnuPCB{{local: a, remote: r, soState: 0}}, goodClosed, "so_state"},
		{"live has SS_NOFDREF", []xnuPCB{{local: a, remote: r, soState: xnuSSIsConnected | xnuSSNoFDRef}}, goodClosed, "so_state"},
		{"live has SS_CANTSENDMORE", []xnuPCB{{local: a, remote: r, soState: xnuSSIsConnected | xnuSSCantSend}}, goodClosed, "so_state"},
		{"live has SS_CANTRCVMORE", []xnuPCB{{local: a, remote: r, soState: xnuSSIsConnected | xnuSSCantRecv}}, goodClosed, "so_state"},
		{"closed socket missing", goodLive, nil, "closed loopback"},
		{"closed lacks SS_NOFDREF", goodLive, []xnuPCB{{local: a, remote: r, state: xnuTCPFinWait2, soState: xnuSSIsConnected | xnuSSCantSend}}, "without SS_NOFDREF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkXNUAppClosed(c.live, c.closed, a, r)
			if err == nil {
				t.Fatalf("checkXNUAppClosed passed %s", c.name)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("checkXNUAppClosed(%s) = %q, want %q", c.name, err, c.says)
			}
		})
	}
}

// xnuAppSocketClosed decides from macOS's pcblist64 table — or, when the table
// hides application sockets (!visible), from the process-descriptor lookup —
// whether no process holds the application socket any more: SS_NOFDREF,
// CLOSED/TIME_WAIT, vanished once visible, or absent from readable holders
// once held has been proven. Half-closed sockets, hidden sockets with neither
// visible nor held proven, and lookup errors return false (fail closed).
func TestMacOSsAppSocketClosedUsesSSNoFDRefAndVisibility(t *testing.T) {
	app := netip.MustParseAddrPort("127.0.0.1:51234")
	proxy := netip.MustParseAddrPort("127.0.0.1:16789")
	for _, c := range []struct {
		name           string
		rec            *xnuPCB
		visible, held  bool
		noFDRefProven  bool
		forbidLookup   bool
		lookup         func() (uint8, bool, error)
		wantClosed     bool
		wantSawVisible bool
		wantSawHeld    bool
	}{
		{"live socket held by process", &xnuPCB{state: xnuTCPEstablished, soState: xnuSSIsConnected}, true, false, true, true, nil, false, true, false},
		{"half-closed FIN_WAIT_2 still held by process", &xnuPCB{state: xnuTCPFinWait2, soState: xnuSSIsConnected}, true, false, true, true, nil, false, true, false},
		{"FIN_WAIT_1 orphan with SS_NOFDREF", &xnuPCB{state: xnuTCPFinWait1, soState: xnuSSNoFDRef | xnuSSIsConnected}, false, false, true, true, nil, true, true, false},
		{"FIN_WAIT_2 orphan with SS_NOFDREF", &xnuPCB{state: xnuTCPFinWait2, soState: xnuSSNoFDRef}, false, false, true, true, nil, true, true, false},
		{"FIN_WAIT_2 with SS_NOFDREF but proof failed stays held", &xnuPCB{state: xnuTCPFinWait2, soState: xnuSSNoFDRef}, false, false, false, true, nil, false, true, false},
		{"CLOSED socket", &xnuPCB{state: xnuTCPClosed}, false, false, true, true, nil, true, true, false},
		{"TIME_WAIT socket stays closed even if SS_NOFDREF proof failed", &xnuPCB{state: xnuTCPTimeWait}, false, false, false, true, nil, true, true, false},
		{"vanished while app sockets never seen and no lookup", nil, false, false, true, false, nil, false, false, false},
		{"vanished once app sockets are visible", nil, true, false, true, true, nil, true, false, false},
		{"hidden table, holder still open (ESTABLISHED)", nil, false, false, true, false, func() (uint8, bool, error) { return xnuTCPEstablished, true, nil }, false, false, true},
		{"hidden table, holder half-closed (FIN_WAIT_2)", nil, false, true, true, false, func() (uint8, bool, error) { return xnuTCPFinWait2, true, nil }, false, false, true},
		{"hidden table, holder reports CLOSED", nil, false, false, true, false, func() (uint8, bool, error) { return xnuTCPClosed, true, nil }, true, false, true},
		{"hidden table, holder reports TIME_WAIT", nil, false, false, true, false, func() (uint8, bool, error) { return xnuTCPTimeWait, true, nil }, true, false, true},
		{"hidden table, holder closed whole and held proven (proof failed still works)", nil, false, true, false, false, func() (uint8, bool, error) { return 0, false, nil }, true, false, false},
		{"hidden table, holder absent and held never proven", nil, false, false, true, false, func() (uint8, bool, error) { return 0, false, nil }, false, false, false},
		{"hidden table, holder lookup error fails closed", nil, false, true, true, false, func() (uint8, bool, error) { return 0, false, errors.New("proc_pidinfo failed") }, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var raw []byte
			if c.rec != nil {
				raw = xnuTable(xnuRecord(app, proxy, *c.rec))
			} else {
				raw = xnuTable()
			}
			table, err := parseXNUPCBs(raw)
			if err != nil {
				t.Fatalf("parseXNUPCBs: %v", err)
			}
			lookup := c.lookup
			if c.forbidLookup {
				lookup = func() (uint8, bool, error) {
					t.Fatal("lookupHeld must not be called when the socket is listed or visible")
					return 0, false, nil
				}
			}
			closed, sawVis, sawHeld := xnuAppSocketClosed(table, app, proxy, c.visible, c.held, c.noFDRefProven, lookup)
			if closed != c.wantClosed || sawVis != c.wantSawVisible || sawHeld != c.wantSawHeld {
				t.Fatalf("xnuAppSocketClosed = (closed=%v, sawVisible=%v, sawHeld=%v); want (%v, %v, %v)",
					closed, sawVis, sawHeld, c.wantClosed, c.wantSawVisible, c.wantSawHeld)
			}
		})
	}
}

// Every TCP-level socket option keploy's macOS proxy reads goes through one
// seam, readTCPOpt (pcbtable_darwin.go). A read of its own (x/sys's
// GetsockoptTCPConnectionInfo, a getsockopt at IPPROTO_TCP, or a call to the
// package's raw getsockopt, which only readTCPOpt may call) is out of reach of
// the rows that hand the counts a socket that received a segment twice, or
// whose tcp_info cannot be read: the byte count the settle once read
// (TCP_CONNECTION_INFO's tcpi_rxbytes, which counts such a segment twice)
// could come back with no row going red. Read from the sources that build for
// darwin/arm64, so it runs on Linux too. Three spellings get past it, and no
// code here uses them: a level or option spelt as a bare number; the raw
// getsockopt taken as a value and called through it (rd := getsockopt;
// rd(...)), since only a call naming getsockopt itself is flagged; and a
// second raw reader declared in readTCPOpt's own var ( ... ) group, since the
// row skips that whole declaration.
func TestTheMacOSProxyReadsTCPOptionsOnlyThroughReadTCPOpt(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "darwin", "arm64", false
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seam := false
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := ctx.MatchFile(".", name); err != nil || !ok {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			// The seam itself, and the raw getsockopt it calls, are where
			// such a read belongs.
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "getsockopt" {
					continue
				}
			case *ast.GenDecl:
				if declares(d, "readTCPOpt") {
					seam = true
					continue
				}
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && isSysPkg(sel.X) {
					switch sel.Sel.Name {
					case "GetsockoptTCPConnectionInfo", "TCP_CONNECTION_INFO", "SYS_GETSOCKOPT":
						t.Errorf("%s: %s.%s outside readTCPOpt: read TCP options through readTCPOpt", fset.Position(sel.Pos()), sel.X, sel.Sel.Name)
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// The raw getsockopt reads any level and option into a
				// buffer: called from anywhere but readTCPOpt, it reads
				// around the seam, whatever its arguments.
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "getsockopt" {
					t.Errorf("%s: getsockopt called outside readTCPOpt: read TCP options through readTCPOpt", fset.Position(call.Pos()))
				}
				if len(call.Args) < 2 {
					return true
				}
				if fn, ok := call.Fun.(*ast.SelectorExpr); ok && isSysPkg(fn.X) && strings.HasPrefix(fn.Sel.Name, "Getsockopt") {
					if lvl, ok := call.Args[1].(*ast.SelectorExpr); ok && isSysPkg(lvl.X) && lvl.Sel.Name == "IPPROTO_TCP" {
						t.Errorf("%s: %s at IPPROTO_TCP outside readTCPOpt: read TCP options through readTCPOpt", fset.Position(call.Pos()), fn.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if !seam {
		t.Fatal("no file that builds for darwin declares readTCPOpt: the rows' seam for TCP options is gone")
	}
}

// declares reports whether d declares name.
func declares(d *ast.GenDecl, name string) bool {
	for _, s := range d.Specs {
		if v, ok := s.(*ast.ValueSpec); ok {
			for _, n := range v.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// isSysPkg reports whether x names x/sys/unix or syscall.
func isSysPkg(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && (id.Name == "unix" || id.Name == "syscall")
}
