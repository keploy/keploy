package replayer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestFirstPassesServeWhatTheFullScanServes runs one replay twice against the
// real mock manager, each command matched with matchCommand's first passes on
// one side and with the full scan of the whole pool on the other, and requires
// the two to serve, consume and report the same at every command.
//
// A first pass may only change how much of the pool a command reads. The
// replay is stateful, so a difference compounds: the windows are staged the way
// the agent stages them (fresh copies, lax promotion, consumed per-test mocks
// left out), matched session mocks are re-stamped to the back of the pool,
// per-test ones consumed, the startup tier holds the bootstrap traffic, and
// commands arrive on connections whose recorded prepared statements make up a
// connection tier. The pools mix query and prepared-statement traffic,
// statement IDs reused for another query, between-window and undated mocks,
// and commands that arrive between tests.
func TestFirstPassesServeWhatTheFullScanServes(t *testing.T) {
	seeds := int64(60)
	if testing.Short() {
		seeds = 10
	}
	commands := 0
	for seed := int64(1); seed <= seeds; seed++ {
		for _, strict := range []bool{false, true} {
			r := rand.New(rand.NewSource(seed))
			windows := 4 + r.Intn(5)
			pool := diffPool(r, windows)
			ref, fast := newDiffSide(pool), newDiffSide(pool)
			eng := func() *schemanoise.Engine {
				if strict {
					return replayer.StrictEngineForTest()
				}
				return nil
			}
			for _, s := range []*diffSide{ref, fast} {
				s.stage(models.BaseTime, time.Now())
			}
			for w := 0; w < windows; w++ {
				start, end := diffWindow(w)
				for _, s := range []*diffSide{ref, fast} {
					s.stage(start, end)
				}
				for c := 0; c < 5+r.Intn(25); c++ {
					between := r.Intn(8) == 0
					if between {
						ref.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
						fast.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
					}
					req, desc := diffCommand(r)
					ctx := context.Background()
					if conn := []string{"", "c1", "c2"}[r.Intn(3)]; conn != "" {
						ctx = context.WithValue(ctx, models.ClientConnectionIDKey, conn)
					}
					commands++
					want := ref.match(ctx, req, eng(), true)
					got := fast.match(ctx, req, eng(), false)
					if want != got {
						t.Fatalf("seed %d strict=%v window %d command %d (%s), between tests=%v:\n full scan:   %s\n first pass:  %s",
							seed, strict, w, c, desc, between, want, got)
					}
					if between {
						ref.mm.SetCurrentTestWindow(start, end)
						fast.mm.SetCurrentTestWindow(start, end)
					}
				}
			}
			ref.mm.Close()
			fast.mm.Close()
		}
	}
	t.Logf("%d commands", commands)
}

var diffBase = time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)

// diffWindow is test i's window: 10ms apart, 4ms long, so a mock at start+7ms
// falls between two tests.
func diffWindow(i int) (time.Time, time.Time) {
	s := diffBase.Add(time.Duration(i) * 10 * time.Millisecond)
	return s, s.Add(4 * time.Millisecond)
}

const (
	diffQueryA = "SELECT v FROM kv WHERE id = ?"
	diffQueryB = "SELECT name FROM users WHERE id = ?"
	diffQueryC = "UPDATE kv SET v = ? WHERE id = ?"
)

func queryReq(sql string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1)}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: sql},
	}}
}

func prepareReq(sql string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1)}, Type: "COM_STMT_PREPARE"},
		Message: &mysql.StmtPreparePacket{Command: 0x16, Query: sql},
	}}
}

func executeReq(stmtID uint32, param string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 16}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: stmtID, IterationCount: 1, ParameterCount: 1,
			Parameters: []mysql.Parameter{{Type: 254, Value: param}}},
	}}
}

func closeReq(stmtID uint32) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 5}, Type: "COM_STMT_CLOSE"},
		Message: &mysql.StmtClosePacket{Status: 0x19, StatementID: stmtID},
	}}
}

func pingReq() mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: "COM_PING"},
		Message: &mysql.PingPacket{Command: 0x0e},
	}}
}

// diffMock is a recorded mock serving a result that names it.
func diffMock(name, conn string, reqs []mysql.Request, ts time.Time, lifetime models.Lifetime, typ string) *models.Mock {
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = lifetime
	m.Spec.Metadata = map[string]string{"type": typ, "connID": conn}
	m.Spec.ReqTimestampMock = ts
	if !ts.IsZero() {
		m.Spec.ResTimestampMock = ts.Add(500 * time.Microsecond)
	}
	m.Spec.MySQLRequests = reqs
	m.Spec.MySQLResponses = []mysql.Response{{
		PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 8, SequenceID: 1}, Type: "TextResultSet"}, Message: &mysql.TextResultSet{}},
		Payload:      name,
	}}
	return m
}

func diffPrepareMock(name, conn, sql string, stmtID uint32, ts time.Time) *models.Mock {
	m := diffMock(name, conn, []mysql.Request{prepareReq(sql)}, ts, models.LifetimeConnection, "connection")
	m.Spec.MySQLResponses = []mysql.Response{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
			Message: &mysql.StmtPrepareOkPacket{StatementID: stmtID, NumParams: uint16(strings.Count(sql, "?"))},
		},
		Payload: name,
	}}
	return m
}

// diffTimestamp is a request time for a mock of window w: mostly inside it,
// sometimes on a bound, between windows, before the first or undated.
func diffTimestamp(r *rand.Rand, w int) time.Time {
	start, _ := diffWindow(w)
	switch r.Intn(12) {
	case 0:
		return time.Time{}
	case 1:
		return start.Add(7 * time.Millisecond)
	case 2:
		return diffBase.Add(-time.Duration(1+r.Intn(30)) * time.Millisecond)
	case 3:
		return start
	case 4:
		return start.Add(4 * time.Millisecond)
	default:
		return start.Add(time.Duration(r.Intn(4000)) * time.Microsecond)
	}
}

// diffLifetime is how the recorder classified a data mock.
func diffLifetime(r *rand.Rand) (models.Lifetime, string) {
	switch r.Intn(6) {
	case 0:
		return models.LifetimeSession, "config"
	case 1:
		return models.LifetimeConnection, "connection"
	default:
		return models.LifetimePerTest, "mocks"
	}
}

func diffQueryText(r *rand.Rand) string {
	switch r.Intn(8) {
	case 0:
		return "START TRANSACTION"
	case 1:
		return "COMMIT"
	case 2:
		return fmt.Sprintf("SELECT v FROM kv WHERE id = 'k%d'", r.Intn(4))
	case 3:
		return fmt.Sprintf("INSERT INTO kv (id, v) VALUES ('k%d', %d)", r.Intn(4), r.Intn(3))
	case 4:
		return "SELECT @@session.transaction_isolation"
	case 5:
		return fmt.Sprintf("/* traceparent=00-%02x */ SELECT v FROM kv WHERE id = 'k%d'", r.Intn(3), r.Intn(4))
	case 6:
		return "SET autocommit=0"
	default:
		return fmt.Sprintf("SELECT COUNT(*) FROM kv WHERE v > %d", r.Intn(3))
	}
}

// diffPrepareText is q as a client may PREPARE it: as it is, or behind an inert
// trace comment (sqlcommenter / DBM traceparent) whose value differs between
// the recording and the replay.
func diffPrepareText(r *rand.Rand, q string) string {
	if r.Intn(4) == 0 {
		return fmt.Sprintf("/* traceparent=00-%02x */ %s", r.Intn(3), q)
	}
	return q
}

// diffPool is a recording: bootstrap PREPAREs on two connections, then per
// window queries, pings and go-sql-driver style PREPARE / EXECUTE / CLOSE
// triples under fresh statement IDs, some of them reusing an ID.
func diffPool(r *rand.Rand, windows int) []*models.Mock {
	var pool []*models.Mock
	n := 0
	name := func(kind string) string { n++; return fmt.Sprintf("%s-%d", kind, n) }
	queries := []string{diffQueryA, diffQueryB, diffQueryC}
	next := map[string]uint32{}
	for _, c := range []string{"c1", "c2"} {
		for _, q := range queries {
			next[c]++
			pool = append(pool, diffPrepareMock(name("bootprep"), c, q, next[c], diffBase.Add(-time.Duration(30+r.Intn(10))*time.Millisecond)))
		}
	}
	for w := 0; w < windows; w++ {
		for j := 0; j < 1+r.Intn(6); j++ {
			lt, typ := diffLifetime(r)
			reqs := []mysql.Request{queryReq(diffQueryText(r))}
			if r.Intn(10) == 0 {
				reqs = append(reqs, queryReq(diffQueryText(r)))
			}
			pool = append(pool, diffMock(name("q"), "c1", reqs, diffTimestamp(r, w), lt, typ))
		}
		if r.Intn(3) == 0 {
			lt, typ := diffLifetime(r)
			pool = append(pool, diffMock(name("ping"), "c1", []mysql.Request{pingReq()}, diffTimestamp(r, w), lt, typ))
		}
		for j := 0; j < r.Intn(4); j++ {
			c := []string{"c1", "c2"}[r.Intn(2)]
			id := next[c] + 1
			if r.Intn(5) == 0 {
				id = uint32(1 + r.Intn(int(next[c]))) // reused, maybe for another query
			} else {
				next[c] = id
			}
			ts := diffTimestamp(r, w)
			prep := diffPrepareMock(name("prep"), c, diffPrepareText(r, queries[r.Intn(3)]), id, ts)
			if r.Intn(6) == 0 {
				prep.TestModeInfo.Lifetime, prep.Spec.Metadata["type"] = models.LifetimePerTest, "mocks"
			}
			pool = append(pool, prep)
			for k := 0; k < 1+r.Intn(2); k++ {
				lt, typ := diffLifetime(r)
				pool = append(pool, diffMock(name("exec"), c, []mysql.Request{executeReq(id, fmt.Sprintf("k%d", r.Intn(5)))}, ts.Add(time.Duration(k+1)*100*time.Microsecond), lt, typ))
			}
			if r.Intn(3) != 0 {
				cl := closeReq(id)
				if r.Intn(6) == 0 {
					cl.PacketBundle.Header.Header.SequenceID = 1 // a header the live CLOSE does not match
				}
				pool = append(pool, diffMock(name("close"), c, []mysql.Request{cl}, ts.Add(900*time.Microsecond), models.LifetimeConnection, "connection"))
			}
		}
	}
	return pool
}

// diffCommand is a live command: queries recorded and not, PREPAREs of
// recorded and unrecorded statements, EXECUTEs and CLOSEs of live statements
// mapped to them, and pings.
func diffCommand(r *rand.Rand) (mysql.Request, string) {
	switch k := r.Intn(12); {
	case k < 4:
		q := diffQueryText(r)
		if r.Intn(4) == 0 {
			q = []string{"SELECT v FROM kv WHERE id = 'k9'", "INSERT INTO kv (id, v) VALUES ('zz', 7)", "SELECT @@session.transaction_read_only", "SELECT name FROM never_recorded"}[r.Intn(4)]
		}
		return queryReq(q), "QUERY " + q
	case k < 6:
		q := diffPrepareText(r, []string{diffQueryA, diffQueryB, diffQueryC, "SELECT nope FROM t WHERE id = ?"}[r.Intn(4)])
		return prepareReq(q), "PREPARE " + q
	case k < 9:
		id := uint32(7 + r.Intn(5))
		p := fmt.Sprintf("k%d", r.Intn(6))
		return executeReq(id, p), fmt.Sprintf("EXECUTE %d %s", id, p)
	case k < 11:
		id := uint32(7 + r.Intn(5))
		return closeReq(id), fmt.Sprintf("CLOSE %d", id)
	default:
		return pingReq(), "PING"
	}
}

// diffSide is one replay: a mock manager staged from the recording the way
// the agent stages it.
type diffSide struct {
	mm       *proxy.MockManager
	pool     []*models.Mock
	consumed map[string]bool
	dctx     *wire.DecodeContext
}

func newDiffSide(pool []*models.Mock) *diffSide {
	return &diffSide{
		mm:       proxy.NewMockManager(nil, nil, zap.NewNop()),
		pool:     pool,
		consumed: map[string]bool{},
		dctx: &wire.DecodeContext{
			PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
			StmtIDToQuery:      map[uint32]string{7: diffQueryA, 8: diffQueryB, 9: diffQueryC, 10: "SELECT nope FROM t WHERE id = ?"},
			NextStmtID:         100,
		},
	}
}

// stage stages the window [start, end] as the agent does in lax mode: fresh
// copies, per-test mocks inside the window in the per-test tier and the rest
// promoted to the session pool after the reusable mocks, each part in recorded
// order, and per-test mocks already consumed left out.
func (s *diffSide) stage(start, end time.Time) {
	for _, st := range s.mm.GetConsumedMocks() {
		if st.Usage == models.Deleted {
			s.consumed[st.Name] = true
		}
	}
	var perTest, reusable, promoted []*models.Mock
	for _, m := range s.pool {
		c := m.DeepCopy()
		if c.TestModeInfo.Lifetime != models.LifetimePerTest || c.Spec.Metadata["type"] == "config" {
			reusable = append(reusable, c)
			continue
		}
		if s.consumed[c.Name] {
			continue
		}
		at := c.Spec.ReqTimestampMock
		if start.Equal(models.BaseTime) || at.IsZero() || (!at.Before(start) && !at.After(end)) || at.Before(diffBase) {
			perTest = append(perTest, c)
		} else {
			promoted = append(promoted, c)
		}
	}
	for _, part := range [][]*models.Mock{perTest, reusable, promoted} {
		sort.SliceStable(part, func(i, j int) bool { return part[i].Spec.ReqTimestampMock.Before(part[j].Spec.ReqTimestampMock) })
	}
	s.mm.SetMocksWithWindow(perTest, append(reusable, promoted...), start, end)
}

// match matches one command and describes what it served and left behind: the
// response, the verdict and miss report, the mocks it consumed, and the order
// of the pools a later command reads.
func (s *diffSide) match(ctx context.Context, req mysql.Request, eng *schemanoise.Engine, fullScan bool) string {
	resp, ok, miss, err := replayer.MatchCommandForTest(ctx, zap.NewNop(), req, s.mm, s.dctx, eng, fullScan)
	var b strings.Builder
	if resp != nil {
		body, _ := json.Marshal(resp.Message)
		fmt.Fprintf(&b, "served %v %T %s; ", resp.Payload, resp.Message, body)
	}
	fmt.Fprintf(&b, "ok=%v miss=%q err=%v; consumed", ok, miss, err)
	for _, st := range s.mm.GetConsumedMocks() {
		fmt.Fprintf(&b, " %s:%v", st.Name, st.Usage)
		if st.Usage == models.Deleted {
			s.consumed[st.Name] = true
		}
	}
	session, _ := s.mm.GetSessionMocks()
	perTest, _ := s.mm.GetPerTestMocksInWindow()
	fmt.Fprintf(&b, "; session %s; per-test %s", mockNames(session), mockNames(perTest))
	return b.String()
}

func mockNames(ms []*models.Mock) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return strings.Join(out, ",")
}
