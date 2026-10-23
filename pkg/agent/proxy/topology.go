package proxy

import (
	"fmt"
	"net"
	"os"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// TopologySink is told, under the proven switch, where the run's calls to
// local listeners went (models.LiveRow, models.MockedRow; design v14 §3.1):
// the agent's wire, which knows the test open at the moment and names it on
// the row. Both are called on a connection's own goroutine, NoteLive once for
// each read that brings the caller's bytes (also on the relay's forwarder, for
// a connection whose parser was retired: mockedNote.retired), so they must
// return at once and must not call back into the proxy.
//
// NoteMocked returns how to take that listing back (unnote, called at most
// once, on the connection's goroutine too): a record's parser that declines
// the connection after it was listed, or is retired before it recorded
// anything of it, records nothing of it, and the connection is listed live
// instead (mockedNote.declined, .retired). unnote takes asLive, that LIVE row,
// and lists it for the test the mocked row named, so that test keeps a row
// however much later the take-back comes. A mocked row another connection
// listed as well stays.
type TopologySink interface {
	NoteLive(row models.LiveRow)
	NoteMocked(row models.MockedRow) (unnote func(asLive models.LiveRow))
}

// topologyBox holds a TopologySink in an atomic.Pointer.
type topologyBox struct{ sink TopologySink }

// SetTopologySink installs the sink the proxy tells where the run's calls to
// local listeners went. nil removes it.
func (p *Proxy) SetTopologySink(s TopologySink) {
	if s == nil {
		p.topology.Store(nil)
		return
	}
	p.topology.Store(&topologyBox{sink: s})
}

func (p *Proxy) topologySink() TopologySink {
	if b := p.topology.Load(); b != nil {
		return b.sink
	}
	return nil
}

// localListener is the mocked-local row of a call to an address of this
// machine that no process of the run's tree listens at, without its test.
// owner is the listener's process, 0 when nothing listens there (a recorded
// dependency that is not running) or keploy could not tell who does.
func localListener(owner int, dstAddr string, port uint32) *models.MockedRow {
	row := &models.MockedRow{Addr: dstAddr, Port: uint16(port)}
	if owner > 0 {
		row.PID, row.Exe = owner, programOf(owner)
	}
	return row
}

// noteMockedLocal tells the sink that a call keploy records, or answers from
// mocks, went to a local destination: a container, named by its container
// key, or its label while its configuration is unread (ctrKey,
// containerkey.Label, when containerkey named the connection), whether it was
// reached at a published port or at its own address, else an address of
// this machine outside the run's tree (outside, from serveTreeListener). Any
// other destination is not listed. Only keploy mock's runs list them. It
// returns how to take the listing back (TopologySink.NoteMocked), nil when
// nothing was listed.
func (p *Proxy) noteMockedLocal(dstAddr string, port uint32, outside *models.MockedRow, ctrKey string) (unnote func(asLive models.LiveRow)) {
	s := p.topologySink()
	if s == nil || !p.mockMode {
		return nil
	}
	switch {
	case ctrKey != "":
		return s.NoteMocked(models.MockedRow{Addr: dstAddr, Container: ctrKey, Port: uint16(port)})
	case outside != nil:
		return s.NoteMocked(*outside)
	}
	return nil
}

// mockedNote is a connection's mocked-local row (noteMockedLocal), listed once
// keploy knows it records the connection or answers it from mocks: when the
// MySQL path or a parser takes it, or as the connection ends before either
// does (a failure: the destination is still a local one keploy would have
// recorded or mocked). A connection the Redis skip, or a record's parsing
// kill switch, relays to its destination is reached for real and nothing of
// it is recorded, so it is listed live instead (relayed); so is a record's
// connection whose parser takes it and then declines it (declined), or that
// the supervisor retires before it recorded anything of it (retired), its
// mocked row taken back. A connection that sends nothing keploy can choose
// a parser by (under five bytes) is listed only as it ends. A nil note lists
// nothing: without the proven switch, or for a record's connection keploy
// relays as server-first; outside keploy mock's runs nothing is listed either
// (noteMockedLocal, relayed).
type mockedNote struct {
	p       *Proxy
	dstAddr string
	port    uint32
	outside *models.MockedRow
	ctrKey  string
	// listed is how the connection is listed so far: not yet, mocked, or
	// live.
	listed noteListed
	// unnote takes the mocked row back (noteMockedLocal); nil when none was
	// listed.
	unnote func(asLive models.LiveRow)
}

// noteListed is how a mockedNote's connection is listed.
type noteListed int

const (
	notListed noteListed = iota
	listedMocked
	listedLive
)

// note lists n's row, once.
func (n *mockedNote) note() {
	if n == nil || n.listed != notListed {
		return
	}
	n.listed = listedMocked
	n.unnote = n.p.noteMockedLocal(n.dstAddr, n.port, n.outside, n.ctrKey)
}

// relayedParsingDisabled is LiveRow.Relayed for a record's connection the
// parsing kill switch (util.DefaultKillSwitch) relays.
const relayedParsingDisabled = "parsing_disabled"

// relayedParserDeclined is LiveRow.Relayed for a record's connection whose
// parser took it and then declined it (integrations.ErrParserDeclined), which
// relayDeclinedConn relays raw.
const relayedParserDeclined = "parser_declined"

// relayedParserRetired is LiveRow.Relayed for a record's connection whose
// parser the supervisor retired (a panic, its memory cap, a hang, an error)
// before the parser recorded anything of it: the relay forwards the rest of
// it raw.
const relayedParserRetired = "parser_retired"

// relayed is conn, the connection n is the note of, as it is relayed live
// to its destination (why: what relays it, LiveRow.Relayed) instead of
// recorded or mocked: its destination is never listed mocked, and is listed
// live each time the caller's bytes are read (liveReader), so a connection a
// client keeps open across tests (a pool's) is listed for every test that
// used it. The destination is named as its mocked row would name it: a
// container by its key, a listener outside the run's tree by its program;
// any other is not listed, and conn is returned as it is. A nil note lists
// nothing.
func (n *mockedNote) relayed(conn net.Conn, why string) net.Conn {
	if n == nil || n.listed != notListed {
		return conn
	}
	return n.live(conn, why)
}

// declined is conn, the caller's end of a record's connection n is the note
// of, as relayDeclinedConn relays it raw once its parser declined it: the
// parser recorded nothing of it, so the mocked row listed when the parser
// took it is taken back, and the destination is listed live instead
// (relayedParserDeclined), as relayed lists it. A connection already listed
// live is returned as it is; a nil note lists nothing.
func (n *mockedNote) declined(conn net.Conn) net.Conn {
	if n == nil || n.listed == listedLive {
		return conn
	}
	n.takeBack(relayedParserDeclined)
	return n.live(conn, relayedParserDeclined)
}

// takeBack takes back the mocked row n listed, if it listed one, listing in
// its place, for the test that row named, the LIVE row n's destination is
// listed by, relayed for why (TopologySink.NoteMocked).
func (n *mockedNote) takeBack(why string) {
	if n.unnote == nil {
		return
	}
	_, row, ok := n.liveRow(why)
	if !ok {
		row = models.LiveRow{}
	}
	n.unnote(row)
	n.unnote = nil
}

// retired is, for a record's connection n is the note of whose parser the
// supervisor retired before the parser recorded anything of it, how to list
// it live (relayedParserRetired) from then on, as relayed lists a connection
// live: the mocked row listed when the parser took it is taken back, and the
// destination is listed live once for each call of the func it returns,
// which recordViaSupervisor has called at once (for the test open as the
// parser is retired) and at each read of the caller's bytes the relay carries
// from then on (parserReads.listLive). The mocked row's own test, the one its
// first bytes are of, is listed live by the take-back. A test between the
// two gets no row of the connection, as no test after the first gets a
// mocked row of it (note lists once); and a parser retired as hung is retired
// a hang budget after its last progress, so the test open then is listed,
// which may be one that did not use the connection: a row too many, never
// one too few for the tests from then on. retired runs on the connection's
// goroutine; the func it returns runs on the relay's forwarder too, and reads
// only what it was made with. nil when nothing is to be listed: a nil note, a
// connection already listed live, outside keploy mock's runs, or a
// destination that is not listed.
func (n *mockedNote) retired() func() {
	if n == nil || n.listed == listedLive {
		return nil
	}
	n.takeBack(relayedParserRetired)
	n.listed = listedLive
	s, row, ok := n.liveRow(relayedParserRetired)
	if !ok {
		return nil
	}
	return func() { s.NoteLive(row) }
}

// live lists n's destination live, as relayed and declined say, from conn's
// reads.
func (n *mockedNote) live(conn net.Conn, why string) net.Conn {
	n.listed = listedLive
	s, row, ok := n.liveRow(why)
	if !ok {
		return conn
	}
	return &liveReader{Conn: conn, sink: s, row: row}
}

// liveRow is the LIVE row n's destination is listed by, relayed for why, and
// the sink it goes to: a container by its key, a listener outside the run's
// tree by its program, as its mocked row names it. ok is false when nothing
// is listed: outside keploy mock's runs, or another destination.
func (n *mockedNote) liveRow(why string) (TopologySink, models.LiveRow, bool) {
	s := n.p.topologySink()
	if s == nil || !n.p.mockMode {
		return nil, models.LiveRow{}, false
	}
	row := models.LiveRow{Port: uint16(n.port), Relayed: why}
	switch {
	case n.ctrKey != "":
		row.Container = n.ctrKey
	case n.outside != nil:
		row.Exe = n.outside.Exe
	default:
		return nil, models.LiveRow{}, false
	}
	return s, row, true
}

// liveConn tells the sink about a LIVE call: conn, the caller's end of a
// call relayed to a listener another process of the run's tree owns, says so
// each time it reads the caller's bytes, so a connection a client keeps open
// across tests (HTTP keep-alive) is listed for every test that used it. With
// no sink, conn is returned as it is.
func (p *Proxy) liveConn(conn net.Conn, owner int, port uint32) net.Conn {
	s := p.topologySink()
	if s == nil {
		return conn
	}
	return &liveReader{Conn: conn, sink: s, row: models.LiveRow{Exe: programOf(owner), Port: uint16(port)}}
}

// liveReader is a caller's connection that reports each read of its bytes as
// a LIVE call (liveConn).
type liveReader struct {
	net.Conn
	sink TopologySink
	row  models.LiveRow
}

func (c *liveReader) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.sink.NoteLive(c.row)
	}
	return n, err
}

// CloseWrite forwards a half-close to the connection underneath: an embedded
// net.Conn does not promote it, and the relay half-closes the caller's end
// when the listener finishes sending.
func (c *liveReader) CloseWrite() error {
	return util.CloseWriteIfPossible(c.Conn)
}

// programOf is the program process pid runs: its executable, else its
// command name; "" when neither can be read.
func programOf(pid int) string {
	if p, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.TrimSuffix(p, " (deleted)")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
