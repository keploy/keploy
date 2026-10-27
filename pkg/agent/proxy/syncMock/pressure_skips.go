package manager

import (
	"context"
	"sort"
	"strconv"
	"sync"
)

// maxPressureSkipConns bounds how many connections the count of requests not
// recorded under pressure keeps apart (notRecordedUnderPressure). Past it, the
// requests of a connection not seen yet are counted in the total and in
// PressureSkips.OtherConnections only: a load that opens a connection per
// request would otherwise grow the map for as long as the pressure lasts.
const maxPressureSkipConns = 1024

// notRecordedUnderPressure counts the requests an ingress read while the memory
// guard had paused recording, and did not record for that reason: in all, and
// by connection. Zero value ready.
type notRecordedUnderPressure struct {
	mu     sync.Mutex
	total  uint64
	byConn map[string]uint64
	// other counts the requests of connections past maxPressureSkipConns.
	other uint64
}

// NoteNotRecordedUnderPressure counts a request its ingress read and did not
// record because the memory guard had paused recording when it came (or when
// its capture was decided), on the connection conn names ("" when the caller
// cannot name it). A pause costs the requests that come while it lasts and no
// more: the connection's next request after it is recorded. The recording's
// summary reports the count and the connections (routes/record.go), so a
// recording thinner than its traffic says why. Nil-safe.
//
// It counts what no other count does: a request whose window the pause gave up
// and that its capture then kept is told as left out (Window.Keep), so a
// caller notes here only a request it never brought to Keep.
func (m *SyncMockManager) NoteNotRecordedUnderPressure(conn string) {
	if m == nil {
		return
	}
	s := &m.notRecorded
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if _, seen := s.byConn[conn]; !seen && len(s.byConn) >= maxPressureSkipConns {
		s.other++
		return
	}
	if s.byConn == nil {
		s.byConn = make(map[string]uint64)
	}
	s.byConn[conn]++
}

// PressureSkips is what NoteNotRecordedUnderPressure has counted
// (SyncMockManager.NotRecordedUnderPressure), or what it counted between two
// such reads (Since).
type PressureSkips struct {
	// Total is the requests counted.
	Total uint64
	// ByConn is the requests counted on each connection kept apart.
	ByConn map[string]uint64
	// Other is the requests counted on connections past the bound of those
	// kept apart (maxPressureSkipConns).
	Other uint64
}

// NotRecordedUnderPressure returns a copy of what NoteNotRecordedUnderPressure
// has counted over the manager's life. The manager serves every session of the
// agent's life: a session reads it as it opens and as it ends, and reports what
// it grew by (Since). Nil-safe.
func (m *SyncMockManager) NotRecordedUnderPressure() PressureSkips {
	if m == nil {
		return PressureSkips{}
	}
	s := &m.notRecorded
	s.mu.Lock()
	defer s.mu.Unlock()
	out := PressureSkips{Total: s.total, Other: s.other}
	if len(s.byConn) > 0 {
		out.ByConn = make(map[string]uint64, len(s.byConn))
		for c, n := range s.byConn {
			out.ByConn[c] = n
		}
	}
	return out
}

// Since returns what was counted after before was read, before being an
// earlier read of the same manager.
func (p PressureSkips) Since(before PressureSkips) PressureSkips {
	out := PressureSkips{Total: p.Total - before.Total, Other: p.Other - before.Other}
	for c, n := range p.ByConn {
		if d := n - before.ByConn[c]; d > 0 {
			if out.ByConn == nil {
				out.ByConn = make(map[string]uint64)
			}
			out.ByConn[c] = d
		}
	}
	return out
}

// Connections is how many connections the requests were counted on: those kept
// apart, and at least one more when some were counted past the bound.
func (p PressureSkips) Connections() int {
	n := len(p.ByConn)
	if p.Other > 0 {
		n++
	}
	return n
}

// Most names the connections with the most requests counted, at most n of
// them, most first, each as "<connection> (<requests>)"; a connection the
// caller could not name reads "unnamed". Ties go by name, so the list is the
// same for the same counts.
func (p PressureSkips) Most(n int) []string {
	type conn struct {
		name string
		reqs uint64
	}
	conns := make([]conn, 0, len(p.ByConn))
	for c, r := range p.ByConn {
		conns = append(conns, conn{c, r})
	}
	sort.Slice(conns, func(i, j int) bool {
		if conns[i].reqs != conns[j].reqs {
			return conns[i].reqs > conns[j].reqs
		}
		return conns[i].name < conns[j].name
	})
	if len(conns) > n {
		conns = conns[:n]
	}
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		name := c.name
		if name == "" {
			name = "unnamed"
		}
		out = append(out, name+" ("+strconv.FormatUint(c.reqs, 10)+")")
	}
	return out
}

// ingressConnKey is the context key under which an ingress names the connection
// a request came on (WithIngressConn).
type ingressConnKey struct{}

// WithIngressConn returns ctx naming the ingress connection its request came on,
// for a capture that decides not to record it to say where
// (NoteNotRecordedUnderPressure). An empty conn leaves ctx as it is.
func WithIngressConn(ctx context.Context, conn string) context.Context {
	if conn == "" {
		return ctx
	}
	return context.WithValue(ctx, ingressConnKey{}, conn)
}

// IngressConn returns the connection ctx names (WithIngressConn), or "".
func IngressConn(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	c, _ := ctx.Value(ingressConnKey{}).(string)
	return c
}
