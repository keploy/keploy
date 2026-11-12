package record

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// warmups finds a recording's per-process warm-ups and adds them to the set's
// worker scope (models.Mapping.Worker, models.PerProcessWarmupReason).
//
// A per-process warm-up is a read an application makes once in each of its
// processes and then keeps the answer of: the signing keys it verifies tokens
// with (a JWKS fetch), a table's column list (an ORM's schema read), a key
// read from a cache. Which request makes it is decided by which process the
// request reached first, not by the request: at record it is the first request
// each worker served, at replay whichever test reaches a cold worker. Owned by
// the test it was recorded in alone, it answers that test only, and every
// other test that reaches a cold worker misses it.
//
// The rule is narrow, so that serving a call to every test never gives a test
// an answer it was not recorded with, nor hides a write it depends on. A call
// is a warm-up only when
//   - it is a read: an HTTP GET, HEAD or OPTIONS; SQL statements each a
//     SELECT that locks and writes nothing, a SHOW, a DESCRIBE or an EXPLAIN
//     that runs nothing; Redis requests each one read command;
//   - every recording of it (its kind and request) has the same answer, came
//     from a process keploy could tell (models.Mock.SourcePID), and is a
//     test's own mock, not a config or session one;
//   - two or more processes made it, each of them exactly once (a process
//     that makes it twice does not keep the answer), and each while serving
//     its first request: within a test window that holds the process's first
//     call made inside any test window (a call it made at its boot, outside
//     every window, is not one). A read two tests made on two warm processes
//     is theirs, not a warm-up. Test windows that overlap cannot say which
//     request a process served, only when: a read a process made for a later
//     request that overlaps its first one passes as made for its first.
//
// The windows are those of every test case the recording kept as it ran; a
// test the end of the recording deletes (capacity-orphaned) still counts, which
// only makes the rule stricter.
//
// A request and an answer are compared without what differs on every call:
// HTTP headers (a trace id, a date) and SQL comments (a query tagged with its
// trace), an executable comment (/*! ... */) excepted.
type warmups struct {
	mu    sync.Mutex
	calls map[warmupDigest]*warmupCall
	// full is set once calls holds maxWarmupCalls calls: a call first seen
	// after that is not tracked.
	full bool
	// early holds, for each process, the request times of its calls that
	// are no config mock and that the agent did not see made outside every
	// request (models.Mock.OutsideRequests), of any kind (at most
	// maxEarlyCalls in all, of at most maxWarmupProcesses processes): the
	// first of them inside a test window is its first request's. A process
	// may make any number of calls at its boot, outside every window.
	early map[uint32][]time.Time
	// earlyCalls counts the times early holds.
	earlyCalls int
	// unsure holds the processes a call past maxEarlyCalls was not kept of,
	// made before one that was: which of its calls was its first request's
	// keploy cannot tell, so it has none.
	unsure map[uint32]struct{}
	// windows is the window of every test case the recording kept.
	windows []testSpan
	// unknownPID counts the recordings of a read whose process keploy could
	// not tell, each of which rules its call out.
	unknownPID int
}

type warmupDigest [sha256.Size]byte

// warmupCall is what the recording holds of one call so far.
type warmupCall struct {
	reply warmupDigest
	pids  map[uint32]struct{}
	// mocks holds its recordings while it may still be a warm-up; nil once
	// it cannot be (out).
	mocks []warmupRec
	out   bool
}

// warmupRec is one recording of a call: its worker-scope entry, the process
// that made it and when.
type warmupRec struct {
	entry models.MockEntry
	pid   uint32
	at    time.Time
}

// maxWarmupProcesses bounds the processes whose earliest calls a recording
// keeps.
const maxWarmupProcesses = 100_000

// maxEarlyCalls is how many call times a recording keeps in all (some 24 MB):
// a call past it is not kept, and a process whose first request is past it
// has none keploy can tell.
const maxEarlyCalls = 1_000_000

// maxWarmupCalls bounds the calls one recording tracks (a few tens of MB): a
// long recording of many distinct reads stops tracking new ones there.
const maxWarmupCalls = 100_000

// maxWarmupAnswer is the largest answer, in bytes of HTTP body, of SQL result
// or of generic payload, read for a warm-up: a call with a larger one is no
// warm-up, and its answer is not hashed on the recording's insert path.
const maxWarmupAnswer = 1 << 20

func newWarmups() *warmups {
	return &warmups{calls: make(map[warmupDigest]*warmupCall), early: make(map[uint32][]time.Time), unsure: make(map[uint32]struct{})}
}

// addTest reads the window of tc, a test case the recording kept.
func (w *warmups) addTest(tc *models.TestCase) {
	if w == nil || tc == nil {
		return
	}
	win := models.RecordedWindow(tc)
	if win.Start.IsZero() || win.End.Before(win.Start) {
		return
	}
	w.mu.Lock()
	w.windows = append(w.windows, testSpan{Start: win.Start, End: win.End})
	w.mu.Unlock()
}

// addMock reads m, a mock the recording saved.
func (w *warmups) addMock(m *models.Mock) {
	if w == nil || m == nil || m.Name == "" {
		return
	}
	at := m.Spec.ReqTimestampMock
	pid := m.SourcePID
	call, ok := warmupCallOf(m)
	w.mu.Lock()
	defer w.mu.Unlock()
	if pid != 0 && !at.IsZero() && m.Spec.Metadata["type"] != "config" && !m.OutsideRequests {
		w.noteCall(pid, at)
	}
	if !ok {
		return
	}
	c := w.calls[call]
	if c == nil {
		if len(w.calls) >= maxWarmupCalls {
			w.full = true
			return
		}
		c = &warmupCall{}
		w.calls[call] = c
	}
	if c.out {
		return
	}
	if pid == 0 {
		w.unknownPID++
	}
	_, again := c.pids[pid]
	// The cheap rule-outs first: the answer is read (up to maxWarmupReply)
	// only for a call that may still be a warm-up.
	ruledOut := pid == 0 || again ||
		m.Spec.Metadata["type"] == "config" || m.TestModeInfo.Lifetime == models.LifetimeSession ||
		m.TestModeInfo.Lifetime == models.LifetimeConnection || m.Spec.Metadata["type"] == "connection"
	var reply warmupDigest
	answered := false
	if !ruledOut {
		reply, answered = warmupReplyOf(m)
	}
	if ruledOut || !answered || (c.pids != nil && reply != c.reply) {
		c.out, c.mocks, c.pids = true, nil, nil
		return
	}
	if c.pids == nil {
		c.pids = make(map[uint32]struct{}, 2)
		c.reply = reply
	}
	c.pids[pid] = struct{}{}
	c.mocks = append(c.mocks, warmupRec{pid: pid, at: at, entry: models.MockEntry{
		Name:             m.Name,
		Kind:             string(m.Kind),
		Timestamp:        at.Unix(),
		ReqTimestampMock: models.FormatMockTimestamp(at),
		ResTimestampMock: models.FormatMockTimestamp(m.Spec.ResTimestampMock),
		// No place: the set's start. Whichever test reaches a cold
		// process first may make it, the set's first included.
		Reason: models.PerProcessWarmupReason,
	}})
}

// noteCall keeps at among the calls of process pid, in order. Under w.mu.
func (w *warmups) noteCall(pid uint32, at time.Time) {
	calls, seen := w.early[pid]
	if w.earlyCalls >= maxEarlyCalls || !seen && len(w.early) >= maxWarmupProcesses {
		if seen && len(calls) > 0 && at.Before(calls[len(calls)-1]) {
			w.unsure[pid] = struct{}{}
		}
		return
	}
	i, _ := slices.BinarySearchFunc(calls, at, func(a, b time.Time) int { return a.Compare(b) })
	w.early[pid] = slices.Insert(calls, i, at)
	w.earlyCalls++
}

// testSpan is a test window's start and end: all the warm-up rule reads of
// it, kept small since a recording holds one for each of its test cases.
type testSpan struct{ Start, End time.Time }

// testWindows are a recording's test windows sorted by start, with the
// latest end of each prefix, so whether some window holds a time is a binary
// search.
type testWindows struct {
	byStart []testSpan
	maxEnd  []time.Time
}

func newTestWindows(windows []testSpan) testWindows {
	tw := testWindows{byStart: slices.Clone(windows)}
	slices.SortFunc(tw.byStart, func(a, b testSpan) int { return a.Start.Compare(b.Start) })
	tw.maxEnd = make([]time.Time, len(tw.byStart))
	for i, win := range tw.byStart {
		tw.maxEnd[i] = win.End
		if i > 0 && tw.maxEnd[i-1].After(win.End) {
			tw.maxEnd[i] = tw.maxEnd[i-1]
		}
	}
	return tw
}

// starting is how many windows start at or before t.
func (tw testWindows) starting(t time.Time) int {
	n, _ := slices.BinarySearchFunc(tw.byStart, t, func(win testSpan, t time.Time) int {
		if win.Start.After(t) {
			return 1
		}
		return -1
	})
	return n
}

// holds says whether some window holds t.
func (tw testWindows) holds(t time.Time) bool {
	n := tw.starting(t)
	return n > 0 && !tw.maxEnd[n-1].Before(t)
}

// holding is every window that holds t.
func (tw testWindows) holding(t time.Time) []testSpan {
	var out []testSpan
	for i := tw.starting(t) - 1; i >= 0 && !tw.maxEnd[i].Before(t); i-- {
		if win := tw.byStart[i]; !win.End.Before(t) {
			out = append(out, win)
		}
	}
	return out
}

// firstRequest is the windows that hold process pid's first call made inside
// any test window: while it served its first request. None for a process
// none of whose calls is inside a window. Under w.mu.
func (w *warmups) firstRequest(tw testWindows, pid uint32) []testSpan {
	if _, ok := w.unsure[pid]; ok {
		return nil
	}
	for _, t := range w.early[pid] {
		if tw.holds(t) {
			return tw.holding(t)
		}
	}
	return nil
}

// found returns the warm-ups among what addMock read, as worker-scope entries
// in recorded order.
func (w *warmups) found() []models.MockEntry {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	tw := newTestWindows(w.windows)
	// Each process's first request, worked out once.
	firsts := map[uint32][]testSpan{}
	firstOf := func(pid uint32) []testSpan {
		f, ok := firsts[pid]
		if !ok {
			f = w.firstRequest(tw, pid)
			firsts[pid] = f
		}
		return f
	}
	var out []models.MockEntry
	for _, c := range w.calls {
		if c.out || len(c.pids) < 2 {
			continue
		}
		first := true
		for _, r := range c.mocks {
			if !slices.ContainsFunc(firstOf(r.pid), func(win testSpan) bool {
				return !r.at.Before(win.Start) && !r.at.After(win.End)
			}) {
				first = false
				break
			}
		}
		if !first {
			continue
		}
		for _, r := range c.mocks {
			out = append(out, r.entry)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].RecordedAt(), out[j].RecordedAt(); !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// write adds the warm-ups found to the set's worker scope, when db can
// (AddToWorker), and says what it added. A store that cannot, a set with no
// mapping file, or an add that fails leaves the mappings as recorded.
func (w *warmups) write(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string) {
	if w == nil || logger == nil {
		return
	}
	adds := w.found()
	w.mu.Lock()
	tracked, full, unknown, procs := len(w.calls), w.full, w.unknownPID, len(w.early)
	w.mu.Unlock()
	logger.Debug("per-process warm-up scan", zap.String("testSetID", testSetID),
		zap.Int("callsTracked", tracked), zap.Bool("trackingLimitReached", full),
		zap.Int("processes", procs), zap.Int("readsOfAnUnknownProcess", unknown),
		zap.Int("warmups", len(adds)))
	if len(adds) == 0 || db == nil || testSetID == "" {
		return
	}
	adder, ok := db.(interface {
		AddToWorker(ctx context.Context, testSetID string, entries []models.MockEntry) ([]string, error)
	})
	if !ok {
		logger.Warn("the mapping store cannot add to the worker scope; per-process warm-ups stay with the tests that recorded them only, and a test that reaches a cold process at replay may miss them",
			zap.String("testSetID", testSetID), zap.Int("warmups", len(adds)),
			zap.String("next_step", "record with a keploy whose mapping store adds to the worker scope"))
		return
	}
	added, err := adder.AddToWorker(ctx, testSetID, adds)
	if err != nil {
		logger.Warn("failed to add per-process warm-ups to the worker scope; they stay with the tests that recorded them only",
			zap.String("testSetID", testSetID), zap.Int("warmups", len(adds)), zap.Error(err),
			zap.String("next_step", "replay this set as it is (a test that reaches a cold process may miss the call), or fix the error above and re-record it"))
		return
	}
	if len(added) > 0 {
		logger.Info("added per-process warm-ups to the worker scope: a replay serves them to whichever test reaches a cold process",
			zap.String("testSetID", testSetID), zap.Strings("mocks", added))
		return
	}
	names := make([]string, 0, len(adds))
	for _, e := range adds {
		names = append(names, e.Name)
	}
	if reader, ok := db.(interface {
		GetMapping(ctx context.Context, testSetID string) (*models.Mapping, error)
	}); ok {
		m, err := reader.GetMapping(ctx, testSetID)
		if err != nil {
			logger.Warn("found per-process warm-ups but could not read the test set's mappings to add them; a test that reaches a cold process at replay may miss them",
				zap.String("testSetID", testSetID), zap.Strings("mocks", names), zap.Error(err),
				zap.String("next_step", "fix the mappings file's error above and re-record this test set"))
			return
		}
		if m != nil {
			logger.Debug("the worker scope holds the per-process warm-ups already",
				zap.String("testSetID", testSetID), zap.Strings("mocks", names))
			return
		}
	}
	// AddToWorker adds none to a set with no mapping file: a recording made
	// without --sync, whose first replay maps it.
	logger.Warn("found per-process warm-ups but did not add them to the worker scope: the test set has no mappings file; a test that reaches a cold process at replay may miss them",
		zap.String("testSetID", testSetID), zap.Strings("mocks", names),
		zap.String("next_step", "record with --sync, so the set gets a mappings file the warm-ups are added to"))
}

// warmupCallOf is the digest of m's call (its kind and request), and false
// for a call that is no read, or of a kind warmups does not read.
func warmupCallOf(m *models.Mock) (warmupDigest, bool) {
	h := sha256.New()
	writeParts(h, string(m.Kind))
	switch m.Kind {
	case models.HTTP:
		req := m.Spec.HTTPReq
		if req == nil || m.Spec.HTTPResp == nil {
			return warmupDigest{}, false
		}
		method := strings.ToUpper(string(req.Method))
		switch method {
		case "GET", "HEAD", "OPTIONS":
		default:
			return warmupDigest{}, false
		}
		writeParts(h, method, req.URL, req.Body)
	case models.MySQL:
		if len(m.Spec.MySQLRequests) == 0 {
			return warmupDigest{}, false
		}
		for _, r := range m.Spec.MySQLRequests {
			q, ok := mysqlQuery(r)
			if !ok {
				return warmupDigest{}, false
			}
			q = strings.TrimSpace(withoutComments(q))
			if !readOnlySQL(q) {
				return warmupDigest{}, false
			}
			writeParts(h, q)
		}
	case models.REDIS:
		if len(m.Spec.GenericRequests) == 0 {
			return warmupDigest{}, false
		}
		for _, p := range m.Spec.GenericRequests {
			for _, b := range p.Message {
				data, ok := payloadBytes(b)
				if !ok {
					return warmupDigest{}, false
				}
				cmd, ok := respCommand(data)
				if !ok || !redisReads[cmd] {
					return warmupDigest{}, false
				}
				writeParts(h, string(data))
			}
		}
	default:
		return warmupDigest{}, false
	}
	var d warmupDigest
	h.Sum(d[:0])
	return d, true
}

// warmupReplyOf is the digest of m's answer, and false for one too large to
// read (maxWarmupAnswer) or that cannot be read.
func warmupReplyOf(m *models.Mock) (warmupDigest, bool) {
	h := sha256.New()
	switch m.Kind {
	case models.HTTP:
		res := m.Spec.HTTPResp
		if len(res.Body) > maxWarmupAnswer {
			return warmupDigest{}, false
		}
		writeParts(h, strconv.Itoa(res.StatusCode), res.Body)
	case models.MySQL:
		cw := &cappedWriter{h: h, left: maxWarmupAnswer}
		if err := json.NewEncoder(cw).Encode(m.Spec.MySQLResponses); err != nil {
			return warmupDigest{}, false
		}
	default: // models.REDIS
		n := 0
		for _, p := range m.Spec.GenericResponses {
			for _, b := range p.Message {
				if n += len(b.Data); n > maxWarmupAnswer {
					return warmupDigest{}, false
				}
				writeParts(h, b.Type, b.Data)
			}
		}
	}
	var d warmupDigest
	h.Sum(d[:0])
	return d, true
}

// writeParts hashes each part with its length, so no two lists of parts hash
// alike.
func writeParts(h hash.Hash, parts ...string) {
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
}

// errAnswerTooLarge is cappedWriter's, past its bytes.
var errAnswerTooLarge = errors.New("answer too large to read for a warm-up")

// cappedWriter hashes what is written to it, up to left bytes.
type cappedWriter struct {
	h    hash.Hash
	left int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > c.left {
		return 0, errAnswerTooLarge
	}
	c.left -= len(p)
	return c.h.Write(p)
}

// sqlComment matches a SQL block comment, which tags a query with its trace.
var sqlComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// withoutComments is q less its block comments, but for an executable one
// (/*! ... */), which MySQL runs as part of the statement.
func withoutComments(q string) string {
	return sqlComment.ReplaceAllStringFunc(q, func(c string) string {
		if strings.HasPrefix(c, "/*!") {
			return c
		}
		return ""
	})
}

// sqlInto matches the INTO of a SELECT that writes its result to a variable
// or a file, whatever follows it.
var sqlInto = regexp.MustCompile(`\bINTO\b`)

// sqlSideEffects are what makes a statement that reads change something
// besides: an advisory lock, a sequence, a session variable assigned.
var sqlSideEffects = []string{"GET_LOCK(", "RELEASE_LOCK(", "RELEASE_ALL_LOCKS(", "IS_USED_LOCK(", "NEXTVAL(", "SETVAL(", "LASTVAL(", ":=", "SLEEP("}

// mysqlQuery is the text of r, a COM_QUERY, and false for any other packet.
func mysqlQuery(r mysql.Request) (string, bool) {
	if r.Header == nil || r.Header.Type != "COM_QUERY" {
		return "", false
	}
	switch q := r.Message.(type) {
	case *mysql.QueryPacket:
		if q == nil {
			return "", false
		}
		return q.Query, true
	case mysql.QueryPacket:
		return q.Query, true
	}
	return "", false
}

// readOnlySQL says whether q, one statement without its comments, only reads:
// a SELECT that takes no lock and writes nothing, a SHOW, a DESCRIBE or an
// EXPLAIN.
func readOnlySQL(q string) bool {
	u := strings.ToUpper(strings.Join(strings.Fields(q), " "))
	if strings.Contains(strings.TrimSuffix(u, ";"), ";") {
		return false // several statements
	}
	if strings.Contains(u, "/*!") {
		return false // an executable comment may say anything
	}
	verb, rest, _ := strings.Cut(u, " ")
	switch verb {
	case "SHOW", "DESCRIBE", "DESC":
		return true
	case "EXPLAIN":
		// EXPLAIN ANALYZE runs the statement it explains.
		return !strings.HasPrefix(rest, "ANALYZE") && readOnlySQL(rest)
	case "SELECT":
		for _, w := range []string{" FOR UPDATE", " FOR SHARE", " LOCK IN SHARE MODE"} {
			if strings.Contains(u, w) {
				return false
			}
		}
		if sqlInto.MatchString(u) {
			return false
		}
		compact := strings.ReplaceAll(u, " (", "(")
		for _, f := range sqlSideEffects {
			if strings.Contains(compact, f) {
				return false
			}
		}
		return true
	}
	return false
}

// payloadBytes is the bytes of b, a recorded generic message.
func payloadBytes(b models.OutputBinary) ([]byte, bool) {
	if b.Type == "binary" {
		d, err := base64.StdEncoding.DecodeString(b.Data)
		return d, err == nil
	}
	return []byte(b.Data), true
}

// respCommand is the command, upper-cased, of data: one Redis request in the
// protocol's array form (*N, then N bulk strings). It is false for data that
// is anything else, a pipeline of several requests included.
func respCommand(data []byte) (string, bool) {
	s := string(data)
	head, rest, ok := strings.Cut(s, "\r\n")
	if !ok || !strings.HasPrefix(head, "*") {
		return "", false
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil || n < 1 {
		return "", false
	}
	var cmd string
	for i := 0; i < n; i++ {
		lenLine, after, ok := strings.Cut(rest, "\r\n")
		if !ok || !strings.HasPrefix(lenLine, "$") {
			return "", false
		}
		l, err := strconv.Atoi(lenLine[1:])
		if err != nil || l < 0 || len(after) < l+2 || after[l:l+2] != "\r\n" {
			return "", false
		}
		if i == 0 {
			cmd = strings.ToUpper(after[:l])
		}
		rest = after[l+2:]
	}
	return cmd, rest == ""
}

// redisReads are the Redis commands that only read.
var redisReads = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true, "EXISTS": true, "TYPE": true,
	"TTL": true, "PTTL": true, "HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true,
	"HVALS": true, "HLEN": true, "HEXISTS": true, "HSTRLEN": true, "LRANGE": true, "LLEN": true,
	"LINDEX": true, "SMEMBERS": true, "SISMEMBER": true, "SMISMEMBER": true, "SCARD": true,
	"ZRANGE": true, "ZRANGEBYSCORE": true, "ZREVRANGE": true, "ZREVRANGEBYSCORE": true,
	"ZSCORE": true, "ZMSCORE": true, "ZCARD": true, "ZCOUNT": true, "ZRANK": true, "ZREVRANK": true,
	"JSON.GET": true,
}
