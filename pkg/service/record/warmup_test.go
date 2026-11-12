package record

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var warmupT0 = time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)

// fetch is a recorded HTTP call by process pid, at warmupT0 plus sec
// seconds, answered with body.
func fetch(name string, pid uint32, sec int, method, body string) *models.Mock {
	at := warmupT0.Add(time.Duration(sec) * time.Second)
	return &models.Mock{
		Name: name, Kind: models.HTTP, SourcePID: pid,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: models.Method(method), URL: "http://idp/.well-known/jwks.json",
				// A header that differs on every call does not tell calls apart.
				Header: map[string]string{"Traceparent": name}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
	}
}

// sameAnswer is a copy of m answered as jwksFetch's "keys" are.
func sameAnswer(m *models.Mock) *models.Mock {
	c := *m
	res := *m.Spec.HTTPResp
	res.Body = "keys"
	c.Spec.HTTPResp = &res
	return &c
}

func jwksFetch(name string, pid uint32, sec int, body string) *models.Mock {
	return fetch(name, pid, sec, "GET", body)
}

func sqlRead(name string, pid uint32, query string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.MySQL, SourcePID: pid,
		Spec: models.MockSpec{
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Type: "COM_QUERY"},
				Message: &mysql.QueryPacket{Command: 3, Query: query},
			}}},
			MySQLResponses:   []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: "id,name"}}},
			ReqTimestampMock: warmupT0,
		}}
}

// redisCall is a Redis data command as the enterprise Redis recorder saves
// it: kind Redis, its connection in the metadata, no config tag.
func redisCall(name string, pid uint32, typ, data, answer string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.REDIS, SourcePID: pid,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "7"},
			GenericRequests:  []models.Payload{{Message: []models.OutputBinary{{Type: typ, Data: data}}}},
			GenericResponses: []models.Payload{{Message: []models.OutputBinary{{Type: "string", Data: answer}}}},
			ReqTimestampMock: warmupT0,
		}}
}

const getJWKS = "*2\r\n$3\r\nget\r\n$12\r\nendorsr:jwks\r\n"

func foundNames(w *warmups) []string {
	var out []string
	for _, e := range w.found() {
		out = append(out, e.Name)
	}
	return out
}

// readAll reads mocks into a recording whose one test window holds them all.
func readAll(mocks ...*models.Mock) *warmups {
	return readIn([][2]int{{-3600, 3600}}, mocks...)
}

// readIn reads mocks into a recording whose test windows are windows, each
// [start, end] seconds after warmupT0.
func readIn(windows [][2]int, mocks ...*models.Mock) *warmups {
	w := newWarmups()
	for i, win := range windows {
		w.addTest(&models.TestCase{Name: "test-" + strconv.Itoa(i+1), Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Timestamp: warmupT0.Add(time.Duration(win[0]) * time.Second)},
			HTTPResp: models.HTTPResp{Timestamp: warmupT0.Add(time.Duration(win[1]) * time.Second)}})
	}
	for _, m := range mocks {
		w.addMock(m)
	}
	return w
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A read with one answer made once by each of several processes is a
// warm-up: every recording of it is found, in recorded order, at the set's
// start (no place), marked. A call any rule rules out is not, and one
// recording that rules it out rules out the whole call: a recording of it
// from a process keploy could not tell, or that is a config or session mock,
// may be the one whose answer differs.
func TestWarmupsFindsAReadEachProcessMadeOnce(t *testing.T) {
	unknownPID := jwksFetch("m-c", 0, 3, "rotated")
	config := jwksFetch("m-c", 30, 3, "rotated")
	config.Spec.Metadata = map[string]string{"type": "config"}
	session := jwksFetch("m-c", 30, 3, "rotated")
	session.TestModeInfo.Lifetime = models.LifetimeSession
	connection := jwksFetch("m-c", 30, 3, "keys")
	connection.TestModeInfo.Lifetime = models.LifetimeConnection
	connTyped := jwksFetch("m-c", 30, 3, "keys")
	connTyped.Spec.Metadata = map[string]string{"type": "connection"}
	cases := []struct {
		name  string
		mocks []*models.Mock
		want  []string
	}{
		{"two processes, once each", []*models.Mock{
			jwksFetch("m-b", 20, 2, "keys"), jwksFetch("m-a", 10, 1, "keys"),
		}, []string{"m-a", "m-b"}},
		{"three processes, once each", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), jwksFetch("m-c", 30, 3, "keys"),
		}, []string{"m-a", "m-b", "m-c"}},
		{"one process twice: it does not keep the answer", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), jwksFetch("m-c", 10, 3, "keys"),
		}, nil},
		{"answers differ", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "rotated"),
		}, nil},
		{"one process only", []*models.Mock{jwksFetch("m-a", 10, 1, "keys")}, nil},
		{"a recording from an unknown process rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), unknownPID,
		}, nil},
		{"a config recording rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), config,
		}, nil},
		{"a session recording rules the call out", []*models.Mock{
			session, jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"),
		}, nil},
		{"an unknown process is not a second process", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 0, 2, "keys"),
		}, nil},
		{"a config recording with the same answer still rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), sameAnswer(config),
		}, nil},
		{"a session recording with the same answer still rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), sameAnswer(session),
		}, nil},
		{"a connection recording rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), connection,
		}, nil},
		{"a recording typed connection rules the call out", []*models.Mock{
			jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"), connTyped,
		}, nil},
		{"a write is no warm-up", []*models.Mock{
			fetch("m-a", 10, 1, "POST", "ok"), fetch("m-b", 20, 2, "POST", "ok"),
		}, nil},
		{"an answer too large to read", []*models.Mock{
			jwksFetch("m-a", 10, 1, strings.Repeat("k", maxWarmupAnswer+1)),
			jwksFetch("m-b", 20, 2, strings.Repeat("k", maxWarmupAnswer+1)),
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := foundNames(readAll(c.mocks...)); !sameNames(got, c.want) {
				t.Fatalf("found %v; want %v", got, c.want)
			}
		})
	}

	for _, e := range readAll(jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys")).found() {
		if e.Reason != models.PerProcessWarmupReason || e.Place != "" || e.Kind != string(models.HTTP) || e.ReqTimestampMock == "" {
			t.Fatalf("entry %+v: want the warm-up mark, no place, its kind and recorded time", e)
		}
	}
}

// An SQL call is read as its statements without their comments: a schema read
// each worker makes is one call whatever trace it is tagged with, a different
// query a different call, and only a statement that reads and locks nothing
// is a warm-up.
func TestWarmupsReadsSQL(t *testing.T) {
	if got := foundNames(readAll(
		sqlRead("m-a", 10, "SHOW FULL FIELDS FROM `users` /*trace:1*/"),
		sqlRead("m-b", 20, "SHOW FULL FIELDS FROM `users` /*trace:2*/"),
		sqlRead("m-c", 10, "SELECT * FROM users WHERE id = 1"),
		sqlRead("m-d", 20, "SELECT * FROM users WHERE id = 2"),
	)); !sameNames(got, []string{"m-a", "m-b"}) {
		t.Fatalf("found %v; want the schema reads m-a, m-b", got)
	}
	for _, q := range []string{
		"INSERT INTO audit VALUES (1)",
		"UPDATE users SET seen = 1",
		"SELECT * FROM users WHERE id = 1 FOR UPDATE",
		"SELECT * FROM users LOCK IN SHARE MODE",
		"SELECT 1 INTO @x",
		"SELECT 1 INTO@x",
		"SELECT 1; DELETE FROM users",
		"SET NAMES utf8mb4",
		"EXPLAIN ANALYZE SELECT * FROM users",
		"EXPLAIN DELETE FROM users",
		"SELECT GET_LOCK('job', 10)",
		"SELECT NEXTVAL(order_seq)",
		"SELECT @n := @n + 1",
		"SELECT SLEEP(1)",
		"SELECT * FROM users /*!50000 FOR UPDATE */",
		"SELECT * FROM users /*!80000 , (SELECT 1) AS x */",
	} {
		if got := foundNames(readAll(sqlRead("m-a", 10, q), sqlRead("m-b", 20, q))); got != nil {
			t.Fatalf("%q found %v; want none", q, got)
		}
	}
	notQuery := sqlRead("m-a", 10, "SHOW TABLES")
	notQuery.Spec.MySQLRequests[0].Header.Type = "COM_STMT_EXECUTE"
	if got := foundNames(readAll(notQuery, sqlRead("m-b", 20, "SHOW TABLES"))); got != nil {
		t.Fatalf("a prepared statement's execute found %v; want none", got)
	}
}

// A Redis call is a warm-up only when it is one read command, written as text
// or as base64 bytes.
func TestWarmupsReadsRedis(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(getJWKS))
	if got := foundNames(readAll(
		redisCall("m-a", 10, "string", getJWKS, "keys"),
		redisCall("m-b", 20, "binary", b64, "keys"),
	)); !sameNames(got, []string{"m-a", "m-b"}) {
		t.Fatalf("found %v; want m-a, m-b", got)
	}
	for name, req := range map[string]string{
		"a write":                  "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
		"a counter":                "*2\r\n$4\r\nINCR\r\n$1\r\nk\r\n",
		"a pipeline":               getJWKS + getJWKS,
		"not the protocol's array": "GET endorsr:jwks\r\n",
		"a cut request":            "*2\r\n$3\r\nGET\r\n$12\r\nendorsr\r\n",
	} {
		if got := foundNames(readAll(redisCall("m-a", 10, "string", req, "OK"), redisCall("m-b", 20, "string", req, "OK"))); got != nil {
			t.Fatalf("%s found %v; want none", name, got)
		}
	}
}

// A warm-up is made while each process serves its first request: within the
// window that holds the process's first call, or, for a first call made at
// its boot outside every window, within the first window after it. A read a
// warm process made for a later request is the request's, and keeps the call
// from being a warm-up.
func TestWarmupsAreMadeOnEachProcesssFirstRequest(t *testing.T) {
	// Windows: A [0,5], B [6,9], C [10,15], seconds after warmupT0.
	windows := [][2]int{{0, 5}, {6, 9}, {10, 15}}
	if got := foundNames(readIn(windows,
		jwksFetch("m-a", 10, 1, "keys"), // pid 10's first request, A
		jwksFetch("m-b", 20, 7, "keys"), // pid 20's first request, B
	)); !sameNames(got, []string{"m-a", "m-b"}) {
		t.Fatalf("first requests: found %v; want m-a, m-b", got)
	}

	other := func(name string, pid uint32, sec int) *models.Mock {
		m := jwksFetch(name, pid, sec, "ada")
		m.Spec.HTTPReq.URL = "http://users/users/1"
		return m
	}
	if got := foundNames(readIn(windows,
		jwksFetch("m-a", 10, 1, "keys"), // pid 10 starts in A
		other("m-u1", 10, 12),           // ... and reads user 1 for C
		other("m-u2", 20, 7),            // pid 20 reads user 1 on its first request, B
	)); got != nil {
		t.Fatalf("a warm process's read for a later request was taken for a warm-up: %v", got)
	}

	boot := jwksFetch("m-boot", 10, -30, "cfg")
	boot.Spec.HTTPReq.URL = "http://config/boot"
	if got := foundNames(readIn(windows,
		boot,                            // pid 10's first call, at boot, in no window
		jwksFetch("m-a", 10, 1, "keys"), // ... then its first request, A
		jwksFetch("m-b", 20, 7, "keys"),
	)); !slices.Contains(got, "m-a") || !slices.Contains(got, "m-b") {
		t.Fatalf("a process that called out at boot: found %v; want m-a and m-b", got)
	}

	// Pre-fork: every worker calls out at its boot, before the first test,
	// then reads on its own first request (A and C).
	bootOf := func(name string, pid uint32, sec int) *models.Mock {
		m := jwksFetch(name, pid, sec, "registered")
		m.Spec.HTTPReq.URL = "http://registry/register"
		return m
	}
	if got := foundNames(readIn(windows,
		bootOf("boot-10", 10, -20), bootOf("boot-20", 20, -19),
		jwksFetch("m-a", 10, 1, "keys"),
		jwksFetch("m-b", 20, 11, "keys"),
	)); !slices.Contains(got, "m-a") || !slices.Contains(got, "m-b") {
		t.Fatalf("pre-fork workers that called out at boot: found %v; want m-a and m-b", got)
	}

	// A config mock (a pooled connection's handshake) is no first request:
	// process 20's, in test 1's window (A), leaves its first request in B.
	handshake := jwksFetch("hs", 20, 2, "hello")
	handshake.Spec.HTTPReq.URL = "http://db/handshake"
	handshake.Spec.Metadata = map[string]string{"type": "config"}
	if got := foundNames(readIn(windows, handshake, jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 7, "keys"))); !slices.Contains(got, "m-b") {
		t.Fatalf("a config mock in another's window: found %v; want m-b", got)
	}

	// The earliest calls are kept whatever order the mocks arrive in.
	if got := foundNames(readIn(windows,
		jwksFetch("m-b", 20, 7, "keys"),
		other("m-late", 10, 12),
		jwksFetch("m-a", 10, 1, "keys"),
	)); !slices.Contains(got, "m-a") || !slices.Contains(got, "m-b") {
		t.Fatalf("mocks out of order: found %v; want m-a and m-b", got)
	}

	// Workers that make many calls at their boot (an ORM's, a client's)
	// still have a first request.
	var many []*models.Mock
	for i := 0; i < 40; i++ {
		many = append(many, bootOf("boot-10-"+strconv.Itoa(i), 10, -100+i), bootOf("boot-20-"+strconv.Itoa(i), 20, -100+i))
	}
	many = append(many, jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 7, "keys"))
	if got := foundNames(readIn(windows, many...)); !slices.Contains(got, "m-a") || !slices.Contains(got, "m-b") {
		t.Fatalf("workers with forty boot calls each: found %v; want m-a and m-b", got)
	}

	// A call the agent saw made outside every request (a timer's, a
	// poller's) is no first request, though it falls in another's window.
	poll := jwksFetch("poll", 20, 3, "ok")
	poll.Spec.HTTPReq.URL = "http://metrics/flush"
	poll.OutsideRequests = true
	if got := foundNames(readIn(windows, poll, jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 7, "keys"))); !slices.Contains(got, "m-b") {
		t.Fatalf("a background call in another's window: found %v; want m-b", got)
	}

	if got := foundNames(readIn(nil, jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"))); got != nil {
		t.Fatalf("with no test window there is no first request: found %v", got)
	}
}

// A process's calls are kept in order whatever order they arrive in, at
// most maxEarlyCalls in all.
func TestWarmupsKeepEachProcesssCallsInOrder(t *testing.T) {
	w := newWarmups()
	for i := 20; i > 0; i-- { // latest first
		w.noteCall(10, warmupT0.Add(time.Duration(i)*time.Second))
	}
	got := w.early[10]
	if len(got) != 20 || !got[0].Equal(warmupT0.Add(time.Second)) || !got[19].Equal(warmupT0.Add(20*time.Second)) {
		t.Fatalf("kept %d calls, %v .. %v; want 20, 1s .. 20s", len(got), got[0].Sub(warmupT0), got[len(got)-1].Sub(warmupT0))
	}
	w.earlyCalls = maxEarlyCalls - 1
	w.noteCall(20, warmupT0)
	w.noteCall(20, warmupT0.Add(time.Second))
	if len(w.early[20]) != 1 || w.earlyCalls != maxEarlyCalls {
		t.Fatalf("past the cap: kept %d of process 20's calls (%d in all); want 1 (%d)", len(w.early[20]), w.earlyCalls, maxEarlyCalls)
	}
	// A call past the cap made before one kept: the kept one may not be the
	// process's first request, so it has none; a later one changes nothing.
	tw := newTestWindows([]testSpan{{Start: warmupT0.Add(-time.Hour), End: warmupT0.Add(time.Hour)}})
	if got := w.firstRequest(tw, 10); len(got) != 1 {
		t.Fatalf("process 10 before any drop: first request %v; want one window", got)
	}
	w.noteCall(10, warmupT0.Add(30*time.Second))
	if got := w.firstRequest(tw, 10); len(got) != 1 {
		t.Fatalf("process 10 after a later call was dropped: first request %v; want one window", got)
	}
	w.noteCall(10, warmupT0)
	if got := w.firstRequest(tw, 10); got != nil {
		t.Fatalf("process 10 after an earlier call was dropped: first request %v; want none", got)
	}
}

// The windows that hold a time are found by binary search, overlapping ones
// included.
func TestTestWindowsHoldingATime(t *testing.T) {
	at := func(sec int) time.Time { return warmupT0.Add(time.Duration(sec) * time.Second) }
	byStart := map[time.Time]string{}
	win := func(name string, from, to int) testSpan {
		byStart[at(from)] = name
		return testSpan{Start: at(from), End: at(to)}
	}
	tw := newTestWindows([]testSpan{win("c", 20, 30), win("a", 0, 100), win("b", 10, 15)})
	names := func(ws []testSpan) []string {
		var out []string
		for _, w := range ws {
			out = append(out, byStart[w.Start])
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		sec  int
		want []string
	}{{-1, nil}, {0, []string{"a"}}, {12, []string{"a", "b"}}, {16, []string{"a"}}, {25, []string{"a", "c"}}, {101, nil}} {
		if got := names(tw.holding(at(c.sec))); !sameNames(got, c.want) || tw.holds(at(c.sec)) != (c.want != nil) {
			t.Fatalf("at %ds: holding %v, holds %v; want %v", c.sec, got, tw.holds(at(c.sec)), c.want)
		}
	}
}

// Deciding the warm-ups of a long recording takes well under a second: each
// process's first request is worked out once, its windows found by search.
func TestWarmupsAreFoundQuicklyInALongRecording(t *testing.T) {
	w := newWarmups()
	for i := 0; i < 5000; i++ {
		w.addTest(&models.TestCase{Name: "t" + strconv.Itoa(i), Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Timestamp: warmupT0.Add(time.Duration(10*i) * time.Second)},
			HTTPResp: models.HTTPResp{Timestamp: warmupT0.Add(time.Duration(10*i+5) * time.Second)}})
	}
	for pid := uint32(1); pid <= 8; pid++ {
		for j := 0; j < 200; j++ {
			w.noteCall(pid, warmupT0.Add(-time.Duration(1000-j)*time.Second))
		}
	}
	for i := 0; i < 2000; i++ {
		for _, pid := range []uint32{uint32(1 + i%8), uint32(1 + (i+1)%8)} {
			m := jwksFetch("m-"+strconv.Itoa(i)+"-"+strconv.Itoa(int(pid)), pid, 10*i+1, "keys")
			m.Spec.HTTPReq.URL = "http://idp/" + strconv.Itoa(i)
			w.addMock(m)
		}
	}
	start := time.Now()
	w.found()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("found() took %v", took)
	}
}

// Once a recording tracks maxWarmupCalls calls it tracks no new one.
func TestWarmupsTrackABoundedNumberOfCalls(t *testing.T) {
	w := newWarmups()
	for i := 0; i < maxWarmupCalls; i++ {
		w.calls[warmupDigest{byte(i), byte(i >> 8), byte(i >> 16)}] = &warmupCall{out: true}
	}
	w.addMock(jwksFetch("m-a", 10, 1, "keys"))
	w.addMock(jwksFetch("m-b", 20, 2, "keys"))
	if len(w.calls) != maxWarmupCalls || !w.full || foundNames(w) != nil {
		t.Fatalf("tracked %d calls (full %v), found %v; want %d, full, none", len(w.calls), w.full, foundNames(w), maxWarmupCalls)
	}
}

// write adds the warm-ups to the set's worker scope and leaves every test's
// list as recorded, so a keploy that does not read the scope replays the set
// as before; a set with no mapping file gets none.
func TestWarmupsWriteAddsThemToTheWorkerScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := mapdb.New(zap.NewNop(), dir, "mappings")
	entry := func(m *models.Mock) models.MockEntry {
		return models.MockEntry{Name: m.Name, Kind: string(m.Kind), Timestamp: m.Spec.ReqTimestampMock.Unix(),
			ReqTimestampMock: models.FormatMockTimestamp(m.Spec.ReqTimestampMock)}
	}
	a, b := jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys")
	own := jwksFetch("m-own", 10, 3, "keys")
	own.Spec.HTTPReq.URL = "http://idp/users/1"
	if err := db.Insert(ctx, &models.Mapping{TestSetID: "test-set-0", TestCases: []models.MappedTestCase{
		{ID: "test-1", Mocks: []models.MockEntry{entry(a), entry(own)}},
		{ID: "test-2", Mocks: []models.MockEntry{entry(b)}},
	}}); err != nil {
		t.Fatal(err)
	}
	w := readAll(a, b, own)
	w.write(ctx, zap.NewNop(), db, "test-set-0")

	m, err := db.GetMapping(ctx, "test-set-0")
	if err != nil || m == nil {
		t.Fatalf("read the mapping back: %v", err)
	}
	var worker []string
	for _, e := range m.Worker {
		worker = append(worker, e.Name)
		if e.Reason != models.PerProcessWarmupReason {
			t.Fatalf("worker entry %+v is not marked", e)
		}
	}
	if !sameNames(worker, []string{"m-a", "m-b"}) {
		t.Fatalf("worker scope %v; want m-a, m-b", worker)
	}
	byTest := map[string][]string{}
	for _, tc := range m.TestCases {
		for _, e := range tc.Mocks {
			byTest[tc.ID] = append(byTest[tc.ID], e.Name)
		}
	}
	if !sameNames(byTest["test-1"], []string{"m-a", "m-own"}) || !sameNames(byTest["test-2"], []string{"m-b"}) {
		t.Fatalf("tests hold %v; want them as recorded", byTest)
	}

	core, logs := observer.New(zap.DebugLevel)
	w.write(ctx, zap.New(core), db, "test-set-0") // the scope holds them already
	if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d warnings for warm-ups the scope holds already; want none", n)
	}

	w.write(ctx, zap.New(core), db, "test-set-1")
	if _, err := os.Stat(filepath.Join(dir, "test-set-1")); !os.IsNotExist(err) {
		t.Fatalf("a set with no mapping file got one written (stat: %v)", err)
	}
	if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("no mappings file").Len() != 1 {
		t.Fatalf("a set with no mapping file was not warned of: %v", logs.All())
	}
}

// A store that cannot add to the worker scope, or an add that fails, leaves
// the mappings as recorded, and a set with no warm-ups asks the store nothing.
func TestWarmupsWriteLeavesTheMappingsWhenItCannotAdd(t *testing.T) {
	ctx := context.Background()
	full := readAll(jwksFetch("m-a", 10, 1, "keys"), jwksFetch("m-b", 20, 2, "keys"))

	core, logs := observer.New(zap.DebugLevel)
	full.write(ctx, zap.New(core), fakeMappingDbNoAdd{}, "test-set-0")
	if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("cannot add to the worker scope").Len() != 1 {
		t.Fatalf("a store that cannot add was not warned of: %v", logs.All())
	}
	unread := &fakeAdderReader{err: errors.New("yaml: bad indentation")}
	full.write(ctx, zap.New(core), unread, "test-set-0")
	if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("could not read the test set's mappings").Len() != 1 {
		t.Fatalf("a mappings file that does not read was not warned of as such: %v", logs.All())
	}

	failing := &fakeAdder{err: errors.New("disk full")}
	full.write(ctx, zap.NewNop(), failing, "test-set-0")
	if failing.calls != 1 {
		t.Fatalf("AddToWorker called %d times; want 1", failing.calls)
	}

	empty := &fakeAdder{}
	newWarmups().write(ctx, zap.NewNop(), empty, "test-set-0")
	full.write(ctx, zap.NewNop(), empty, "")
	if empty.calls != 0 {
		t.Fatalf("AddToWorker called %d times with nothing to add or no set; want 0", empty.calls)
	}
}

type fakeMappingDbNoAdd struct{}

func (fakeMappingDbNoAdd) Insert(context.Context, *models.Mapping) error { return nil }
func (fakeMappingDbNoAdd) Upsert(context.Context, string, string, []models.MockEntry) error {
	return nil
}
func (fakeMappingDbNoAdd) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}
func (fakeMappingDbNoAdd) UpsertWorker(context.Context, string, []models.MockEntry) error {
	return nil
}

type fakeAdder struct {
	fakeMappingDbNoAdd
	err   error
	calls int
}

func (f *fakeAdder) AddToWorker(context.Context, string, []models.MockEntry) ([]string, error) {
	f.calls++
	return nil, f.err
}

// recordTwoWorkers runs a recording through r: two workers each fetch the
// JWKS once, on their first request, and the agent maps each fetch to its
// test (as under --sync). It returns the set's mapping as written.
func recordTwoWorkers(t *testing.T, r *Recorder, instr *fakeInstr, dir string) *models.Mapping {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	tc := func(name string, from, to int) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://app/" + name, Timestamp: at(from)},
			HTTPResp: models.HTTPResp{StatusCode: 200, Timestamp: at(to)}}
	}
	fetch := func(name string, pid uint32, ms int) *models.Mock {
		m := jwksFetch(name, pid, 0, "keys")
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at(ms), at(ms+1)
		return m
	}
	send := func(f func()) {
		t.Helper()
		ch := make(chan struct{})
		go func() { f(); close(ch) }()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatal("the recording did not take a frame")
		}
	}
	send(func() { instr.incoming <- tc("test-1", 0, 100) })
	send(func() { instr.incoming <- tc("test-2", 200, 300) })
	send(func() { instr.outgoing <- fetch("jwks-a", 10, 50) })
	send(func() { instr.outgoing <- fetch("jwks-b", 20, 250) })
	send(func() { instr.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"jwks-a"}} })
	send(func() { instr.mappings <- models.TestMockMapping{TestName: "test-2", MockIDs: []string{"jwks-b"}} })
	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Start did not return")
	}
	sets, _ := os.ReadDir(dir)
	if len(sets) != 1 {
		t.Fatalf("recorded sets %v; want one", sets)
	}
	m, err := mapdb.New(zap.NewNop(), dir, "mappings").GetMapping(context.Background(), sets[0].Name())
	if err != nil || m == nil {
		t.Fatalf("read the mapping: %v", err)
	}
	return m
}

// warmupsSeenHook is a record hook that reads the set's warm-ups when the
// recording is complete.
type warmupsSeenHook struct {
	BaseRecordHooks
	dir  string
	seen []string
}

func (h *warmupsSeenHook) AfterRecordingComplete(ctx context.Context, info *RecordingCompleteContext) error {
	m, err := mapdb.New(zap.NewNop(), h.dir, "mappings").GetMapping(ctx, info.TestSetID)
	if err == nil {
		h.seen = m.WarmupMockNames()
	}
	return nil
}

// failingCloseMockDB is a mock store that could not write every mock.
type failingCloseMockDB struct{ *recMockDB }

func (failingCloseMockDB) Close() error { return errors.New("no space left on device") }

// keploy record finds the per-process warm-ups of what it records and adds
// them to the set's worker scope, before the end-of-recording hook runs (so
// a pass it runs over the mappings reads them): two workers each fetch the
// JWKS once, on their first request, and the set's mappings name each fetch
// in its test's list and in the worker scope.
func TestStart_AddsThePerProcessWarmupsToTheWorkerScope(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	dir := t.TempDir()
	instr := newFakeInstr()
	r := exitCodeRecorder(instr)
	r.mappingDb = mapdb.New(zap.NewNop(), dir, "mappings")
	hook := &warmupsSeenHook{dir: dir}
	r.hooks = hook
	m := recordTwoWorkers(t, r, instr, dir)
	if got := m.WarmupMockNames(); !sameNames(got, []string{"jwks-a", "jwks-b"}) {
		t.Fatalf("warm-ups %v; want [jwks-a jwks-b] (worker %+v)", got, m.Worker)
	}
	if !sameNames(hook.seen, []string{"jwks-a", "jwks-b"}) {
		t.Fatalf("the end-of-recording hook read warm-ups %v; want them written before it ran", hook.seen)
	}
}

// A recording whose mock store could not write every mock adds no warm-up:
// a worker entry must name a mock that is on disk.
func TestStart_AddsNoWarmupWhenTheMockStoreDidNotWriteEveryMock(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	dir := t.TempDir()
	instr := newFakeInstr()
	r := exitCodeRecorder(instr)
	r.mappingDb = mapdb.New(zap.NewNop(), dir, "mappings")
	r.mockDB = failingCloseMockDB{&recMockDB{unencodable: map[string]bool{}}}
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	m := recordTwoWorkers(t, r, instr, dir)
	if got := m.WarmupMockNames(); got != nil {
		t.Fatalf("warm-ups %v; want none", got)
	}
	if len(m.TestCases) == 0 {
		t.Fatal("the case needs the mappings written; none were")
	}
}

// fakeAdderReader is a store that adds nothing and cannot read its mappings.
type fakeAdderReader struct {
	fakeAdder
	err error
}

func (f *fakeAdderReader) GetMapping(context.Context, string) (*models.Mapping, error) {
	return nil, f.err
}
