package starts

import (
	"maps"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// THE ADDRESS A RECORDED CALL REACHED.
//
// Every recorded mock names the address its connection reached, the one the
// application dialled, as Metadata["destAddr"] ("host:port"): under `keploy
// record` as under `keploy mock record`. A replay reads it to tell what a port
// held. On a built-in default MySQL port (3306, 4000), a recording that reached
// another dependency there is served by its client's bytes, not greeted as
// MySQL (proxy.mysqlPortRegistry.recordedNotMysql); an HTTP mock's Host may
// name no port, or another one (a virtual host), so only destAddr tells. A
// mock whose recorder wrote its own destAddr (MySQL's) keeps it.
//
// The proxy notes each connection's address as it accepts the connection
// (Dest), and forgets it once the connection's parsers are done (ForgetDest).
// A mock is stamped (StampDest) as its parser hands it over (the sync
// manager's AddMock), while its connection is still open, and again as it is
// streamed (Stamp), for one no parser handed over. A connection closed since
// is kept for a late mock, among the last keptClosedDests closed, as its
// process is (keptClosedOrigins): the table is bounded by the connections
// open, not by how long the recording runs.

// keptClosedDests bounds how many closed connections the registry keeps the
// address of, for mocks stamped after their connection closed.
const keptClosedDests = 4096

// destinations is the registry's table of each connection's address.
type destinations struct {
	mu     sync.Mutex
	addrs  map[string]string
	closed []string
}

// Dest notes that connection conn (the connection ID its mocks carry) reached
// addr. A connection ID noted again is the later connection's: no longer among
// the closed (a later close puts it back, last).
func (r *Registry) Dest(conn, addr string) {
	if conn == "" || addr == "" {
		return
	}
	d := &r.dest
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.addrs == nil {
		d.addrs = map[string]string{}
	}
	_, known := d.addrs[conn]
	d.addrs[conn] = addr
	// Only a connection ID known already can be among the closed: a new one
	// costs no scan.
	if known {
		for i, c := range d.closed {
			if c == conn {
				d.closed = append(d.closed[:i], d.closed[i+1:]...)
				break
			}
		}
	}
}

// ForgetDest closes connection conn: its address is kept for a mock stamped
// after, among the last keptClosedDests connections closed.
func (r *Registry) ForgetDest(conn string) {
	d := &r.dest
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.addrs[conn]; !ok {
		return
	}
	d.closed = append(d.closed, conn)
	if len(d.closed) > keptClosedDests {
		delete(d.addrs, d.closed[0])
		d.closed = d.closed[1:]
	}
}

// StampDest gives m, a recorded mock, the address its connection reached
// (Metadata["destAddr"]), unless its recorder wrote one. The metadata is
// replaced by a copy, never edited: a parser may hand several mocks one
// metadata map, and the outgoing stream may be encoding an earlier one.
func (r *Registry) StampDest(m *models.Mock) {
	if m == nil || m.Spec.Metadata["destAddr"] != "" {
		return
	}
	id := m.ConnectionID
	if id == "" {
		id = m.Spec.Metadata["connID"]
	}
	if id == "" {
		return
	}
	d := &r.dest
	d.mu.Lock()
	addr := d.addrs[id]
	d.mu.Unlock()
	if addr == "" {
		return
	}
	md := maps.Clone(m.Spec.Metadata)
	if md == nil {
		md = map[string]string{}
	}
	md["destAddr"] = addr
	m.Spec.Metadata = md
}

// resetDests empties the table: a new session's connections start it.
func (r *Registry) resetDests() {
	d := &r.dest
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs, d.closed = nil, nil
}
