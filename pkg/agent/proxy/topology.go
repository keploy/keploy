package proxy

import (
	"errors"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TopologySink is told, under the proven switch, where the run's calls to
// local listeners went (models.LiveRow, models.MockedRow; design v14 §3.1):
// the agent's wire, which knows the test open at the moment and names it on
// the row. Each is called on a connection's own goroutine, LIVE once for each
// read that brings the caller's bytes, so they must return at once and must
// not call back into the proxy. In a keploy mock record it is also told of
// each dependency that did not accept a connection of the run
// (models.UnreachedRow).
type TopologySink interface {
	NoteLive(row models.LiveRow)
	NoteMocked(row models.MockedRow)
	NoteUnreached(row models.UnreachedRow)
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

// unreachedAnswer is how a destination answered the proxy's dial of it that
// failed with err (models.UnreachedRow.Answer): refused, unreachable, or no
// answer. ok is false for an error that is no answer of the destination's
// (the proxy stopping, its own limits, a cancelled dial).
func unreachedAnswer(err error) (answer string, ok bool) {
	var t interface{ Timeout() bool }
	switch {
	case neterr.IsConnRefused(err):
		return models.UnreachedRefused, true
	case neterr.IsUnreachable(err):
		return models.UnreachedUnreachable, true
	case errors.As(err, &t) && t.Timeout():
		return models.UnreachedNoAnswer, true
	}
	return "", false
}

// noteUnreached tells the sink, in a keploy mock record under the proven
// switch, that the dependency at addr did not accept a connection of the
// run, answering as answer says (models.UnreachedRow): nothing of that
// connection is recorded. inst is the instance of the test worker whose
// process made the connection (DestInfo.Instance). A destination mock.real
// lists is not a dependency the record records: the open test's REAL row
// counts its failed dial instead (realDialFailed).
func (p *Proxy) noteUnreached(addr, answer string, inst uint32) {
	s := p.topologySink()
	if s == nil || !p.mockMode || !provenswitch.On() {
		return
	}
	if rule := p.getSession(); rule == nil || rule.Mode != models.MODE_RECORD {
		return
	}
	s.NoteUnreached(models.UnreachedRow{Addr: addr, Answer: answer, Instance: inst})
}

// noteDialFailed is noteUnreached for the proxy's own dial of a connection's
// destination, which failed with err. An error that is no answer of the
// destination's is not noted (unreachedAnswer).
func (p *Proxy) noteDialFailed(addr string, err error, inst uint32) {
	if answer, ok := unreachedAnswer(err); ok {
		p.noteUnreached(addr, answer, inst)
	}
}

// localListener is the mocked-local row of a call to an address of this
// machine that no process of the run's tree listens at, without its test.
// owner is the listener's process, 0 when nothing listens there (a recorded
// dependency that is not running) or keploy could not tell who does.
func localListener(owner int, dstAddr string, port uint32) *models.MockedRow {
	row := &models.MockedRow{Addr: dstAddr, Port: uint16(port)}
	if owner > 0 {
		row.PID, row.Exe = owner, ProgramOf(owner)
	}
	return row
}

// noteMockedLocal tells the sink that a call keploy records, or answers from
// mocks, went to a local destination: a container, named by its container
// key, or its label while its configuration is unread (ctrKey,
// containerkey.Label, when containerkey named the connection), whether it was
// reached at a published port or at its own address, else an address of
// this machine outside the run's tree (outside, from serveTreeListener). Any
// other destination is not listed. Only keploy mock's runs list them. inst
// is the instance of the test worker whose process made the call
// (DestInfo.Instance).
func (p *Proxy) noteMockedLocal(dstAddr string, port uint32, outside *models.MockedRow, ctrKey string, inst uint32) {
	s := p.topologySink()
	if s == nil || !p.mockMode {
		return
	}
	switch {
	case ctrKey != "":
		s.NoteMocked(models.MockedRow{Addr: dstAddr, Container: ctrKey, Port: uint16(port), Instance: inst})
	case outside != nil:
		row := *outside
		row.Instance = inst
		s.NoteMocked(row)
	}
}

// liveConn tells the sink about a LIVE call: conn, the caller's end of a
// call relayed to a listener another process of the run's tree owns, says so
// each time it reads the caller's bytes, so a connection a client keeps open
// across tests (HTTP keep-alive) is listed for every test that used it. inst
// is the instance of the test worker whose process made it
// (DestInfo.Instance). With no sink, conn is returned as it is.
func (p *Proxy) liveConn(conn net.Conn, owner int, port uint32, inst uint32) net.Conn {
	s := p.topologySink()
	if s == nil {
		return conn
	}
	return &liveReader{Conn: conn, sink: s, row: models.LiveRow{Exe: ProgramOf(owner), Port: uint16(port), Instance: inst}}
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

// NetConn is the connection underneath: what looks for the socket under a
// live call's conn (util.WatchPeerEnd, watching the caller for its hang-up)
// looks through it.
func (c *liveReader) NetConn() net.Conn { return c.Conn }
