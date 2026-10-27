package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A page's poller makes its calls on a timer, so how many fall inside a test,
// and which test, is a matter of timing. The test's harness reports which of a
// test's calls a repeating timer made (models.ScopeWindow.Timers); keploy mock
// record gives their recordings to the set's worker scope (models.Mapping.Worker,
// Cause models.CauseTimer), each with the stretch of the set's writes its answer
// holds in (models.Stretch), so a replay answers such a call from its stretch
// and never charges it to a test.

// WorkerMapper is an optional MappingDB extension: it writes the set's worker
// scope (models.Mapping.Worker).
type WorkerMapper interface {
	UpsertWorker(ctx context.Context, testSetID string, worker []models.MockEntry) error
}

// callFingerprint is an HTTP mock's fingerprint, as a test's harness names a
// timer's call (models.CauseReq): its method, Host, URL path and query, and
// the hex SHA-256 of its body, space-separated; and whether it is a write (its
// parser declares no read, models.IsReadCall). Empty and false for any other
// kind.
func callFingerprint(mk *models.Mock) (string, bool) {
	if mk == nil || mk.Kind != models.HTTP || mk.Spec.HTTPReq == nil {
		return "", false
	}
	r := mk.Spec.HTTPReq
	host, target := "", r.URL
	if u, err := url.Parse(r.URL); err == nil {
		host, target = u.Host, u.EscapedPath()
		if u.RawQuery != "" {
			target += "?" + u.RawQuery
		}
	}
	for k, v := range r.Header {
		if strings.EqualFold(k, "Host") {
			host = v
		}
	}
	sum := sha256.Sum256([]byte(r.Body))
	return timerKey(string(r.Method), host, target, hex.EncodeToString(sum[:])), !models.IsReadCall(mk)
}

func timerKey(method, host, target, body string) string {
	return method + " " + host + " " + target + " " + body
}

// sessionOf is the session an HTTP mock's call was made in: the hex SHA-256 of
// its Cookie and Authorization headers. Two recordings of one call made in
// different sessions (another user's login) answer different callers: a
// replay call matches the one of its own session only. Empty for any other
// kind, and for a call that sends neither.
func sessionOf(mk *models.Mock) string {
	if mk == nil || mk.Kind != models.HTTP || mk.Spec.HTTPReq == nil {
		return ""
	}
	var cookie, auth string
	for k, v := range mk.Spec.HTTPReq.Header {
		switch {
		case strings.EqualFold(k, "Cookie"):
			cookie = v
		case strings.EqualFold(k, "Authorization"):
			auth = v
		}
	}
	if cookie == "" && auth == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookie + "\n" + auth))
	return hex.EncodeToString(sum[:])
}

// answerOf is an HTTP mock's recorded answer, as one value two recordings of a
// call share only when they answered alike: the hex SHA-256 of its status code
// and body. Empty for any other kind and for an answer whose body was not kept
// (models.HTTPResp.BodySkipped), which is then like no other.
func answerOf(mk *models.Mock) string {
	if mk == nil || mk.Kind != models.HTTP || mk.Spec.HTTPResp == nil || mk.Spec.HTTPResp.BodySkipped {
		return ""
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(mk.Spec.HTTPResp.StatusCode) + "\n" + mk.Spec.HTTPResp.Body))
	return hex.EncodeToString(sum[:])
}

// timerScope moves each recording a test's harness reported a timer made
// (models.ScopeWindow.Timers) from its test's mapping (byTest) into the set's
// worker scope: every one a repeating timer made, and a read (not a write) a
// timer that fires once made; a write such a timer made (a delayed save) stays
// the test's. Which recording a report names is read the way the harness
// counted it: the N-th of the test's calls with its fingerprint, in recorded
// order. A call made between tests is the pool's and stays there: the pool
// answers it however often a timer makes it.
//
// Each moved recording carries Cause models.CauseTimer, its place (the test
// it was recorded in, whose own recording it follows when a judged commit
// keeps or drops it), and its stretch: where in the set's writes its answer
// holds. A write bounds that stretch only where the call's recorded answer
// changed across it: consecutive recordings of one call made in one session
// (one fingerprint and one sessionOf, in recorded order) that answered alike
// (answerOf) share one stretch, from the
// last write before the first of them to the first write after the last of
// them, so a write between them (a session's keep-alive ping) cuts nothing:
// the recording proves the call read the same on both sides of it. Its reads
// no timer made (its test's own, the pool's) are in the run too and stay
// where they are, named in the stretch (models.Stretch.With): the call read
// the same there as well. The timer's recordings share its Run: each answers
// once, then the last again. The writes
// are the set's HTTP writes no report names (a timer's own write comes as often
// as its timer fires). mocks are the set's data-plane recordings. It returns
// byTest less what moved, and the worker scope in recorded order; byTest
// unchanged and nil when no harness reported a timer.
func timerScope(windows []models.ScopeWindow, mocks []capturedMock, byTest map[string][]models.MockEntry) (map[string][]models.MockEntry, []models.MockEntry) {
	reported := map[string]map[string]bool{} // test -> "<fingerprint>#<n>" -> the timer fires once
	for _, w := range windows {
		for _, c := range w.Timers {
			if reported[w.Name] == nil {
				reported[w.Name] = map[string]bool{}
			}
			reported[w.Name][timerKey(c.Method, c.Host, c.URL, c.Body)+"#"+strconv.Itoa(c.N)] = c.Once
		}
	}
	if len(reported) == 0 {
		return byTest, nil
	}
	byName := make(map[string]capturedMock, len(mocks))
	for _, mk := range mocks {
		byName[mk.name] = mk
	}
	timer := map[string]bool{}
	place := map[string]string{}
	for test, entries := range byTest {
		want := reported[test]
		if len(want) == 0 {
			continue
		}
		ordered := append([]models.MockEntry(nil), entries...)
		sort.SliceStable(ordered, func(i, j int) bool { return byName[ordered[i].Name].ts.Before(byName[ordered[j].Name].ts) })
		seen := map[string]int{}
		var kept []models.MockEntry
		for _, e := range ordered {
			mk, ok := byName[e.Name]
			if !ok || mk.fp == "" {
				kept = append(kept, e)
				continue
			}
			n := seen[mk.fp]
			seen[mk.fp] = n + 1
			if once, ok := want[mk.fp+"#"+strconv.Itoa(n)]; ok && !(once && mk.write) {
				timer[e.Name] = true
				place[e.Name] = test
				continue
			}
			kept = append(kept, e)
		}
		byTest[test] = kept
	}
	if len(timer) == 0 {
		return byTest, nil
	}

	// The set's writes in recorded order: where a stretch can end.
	ordered := append([]capturedMock(nil), mocks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ts.Before(ordered[j].ts) })
	var writes []capturedMock
	for _, mk := range ordered {
		if mk.fp != "" && mk.write && !timer[mk.name] {
			writes = append(writes, mk)
		}
	}
	// at is the number of writes recorded before mk: the stretch it was
	// recorded in runs from writes[at-1] to writes[at].
	at := func(mk capturedMock) int {
		return sort.Search(len(writes), func(i int) bool { return writes[i].ts.After(mk.ts) })
	}
	// Each timer's call's recordings in each session, in recorded order,
	// joined into runs that answered alike: the timer's, and the reads of the
	// call no timer made (its test made it, or it came between tests), which
	// prove its answer held at their time too and stay where they were
	// recorded (Stretch.With). A run's stretch is from the write before its
	// first to the write after its last. A recording of another session is
	// never in the run: a replay call cannot match it, so it must not count
	// as the run's to answer.
	timerCall := map[string]bool{}
	for _, mk := range ordered {
		if timer[mk.name] {
			timerCall[mk.fp+"\x00"+mk.sess] = true
		}
	}
	byCall := map[string][]capturedMock{}
	var calls []string
	for _, mk := range ordered {
		call := mk.fp + "\x00" + mk.sess
		if mk.fp == "" || !timerCall[call] || (!timer[mk.name] && mk.write) {
			continue
		}
		if _, ok := byCall[call]; !ok {
			calls = append(calls, call)
		}
		byCall[call] = append(byCall[call], mk)
	}
	stretch := map[string]*models.Stretch{}
	// Where each of a timer's runs of a call ended: the write after its last
	// recording, when that was, and what the run answered.
	type runEnd struct {
		until string
		at    time.Time
		ans   string
	}
	ends := map[string][]runEnd{}
	for _, fp := range calls {
		recs := byCall[fp]
		for i := 0; i < len(recs); {
			j := i
			for j+1 < len(recs) && recs[j+1].ans != "" && recs[j+1].ans == recs[i].ans {
				j++
			}
			sum := sha256.Sum256([]byte(fp + "\n" + recs[i].ts.UTC().Format(time.RFC3339Nano)))
			st := &models.Stretch{Run: hex.EncodeToString(sum[:8])}
			if k := at(recs[i]); k > 0 {
				st.After = writes[k-1].name
			}
			k := at(recs[j])
			if k < len(writes) {
				st.Until = writes[k].name
			}
			timed := false
			for _, mk := range recs[i : j+1] {
				if timer[mk.name] {
					stretch[mk.name] = st
					timed = true
				} else {
					st.With = append(st.With, mk.name)
				}
			}
			if timed && st.Until != "" {
				ends[fp] = append(ends[fp], runEnd{until: st.Until, at: writes[k].ts, ans: recs[i].ans})
			}
			i = j + 1
		}
	}
	// A test's own read of the call, recorded after the write that ended a
	// run of the timer's and answered otherwise than the run, read what that
	// write left. Exact serving answers a test's own recordings first, so a
	// replay that makes the timer's call before that write (the timer fired
	// sooner) would take it from the test there and read a state the
	// recording never saw before the write. It answers only once the run has
	// passed the write (Stretch.After; the latest such write before it).
	type ownedAt struct {
		test string
		i    int
	}
	pos := map[string]ownedAt{}
	for test, entries := range byTest {
		for i := range entries {
			pos[entries[i].Name] = ownedAt{test, i}
		}
	}
	for _, fp := range calls {
		for _, mk := range byCall[fp] {
			p, owned := pos[mk.name]
			if timer[mk.name] || !owned || mk.ans == "" {
				continue
			}
			bound := ""
			for _, e := range ends[fp] {
				if e.at.Before(mk.ts) && e.ans != mk.ans {
					bound = e.until
				}
			}
			if bound != "" {
				byTest[p.test][p.i].Stretch = &models.Stretch{After: bound}
			}
		}
	}
	var worker []models.MockEntry
	for _, mk := range ordered {
		if st, ok := stretch[mk.name]; ok {
			cp := *st
			cp.With = append([]string(nil), st.With...)
			worker = append(worker, models.MockEntry{Name: mk.name, Kind: string(models.HTTP), Place: place[mk.name], Cause: models.CauseTimer, Stretch: &cp})
		}
	}
	return byTest, worker
}

// upsertWorker writes the set's worker scope (timerScope), whole: an empty one
// clears what an earlier recording of the set left. Its recordings are in no
// test's mapping any more, so a failed write fails the recording, as a failed
// mappings write does.
func (m *mockService) upsertWorker(ctx context.Context, name string, worker []models.MockEntry) error {
	wm, ok := m.mappingDB.(WorkerMapper)
	if !ok {
		return errors.New("this mapping store keeps no worker scope")
	}
	if err := wm.UpsertWorker(ctx, name, worker); err != nil {
		return err
	}
	if len(worker) > 0 {
		m.logger.Info("gave the calls a repeating timer made to the set's worker scope, each with its stretch of writes", zap.String("mock-set", name), zap.Int("calls", len(worker)))
	}
	return nil
}
