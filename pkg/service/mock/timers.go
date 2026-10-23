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

// timerScope moves each recording a test's harness reported a repeating timer
// made (models.ScopeWindow.Timers) from its test's mapping (byTest) into the
// set's worker scope. Which recording a report names is read the way the
// harness counted it: the N-th of the test's calls with its fingerprint, in
// recorded order. A call made between tests is the pool's and stays there:
// the pool answers it however often a timer makes it.
//
// Each moved recording carries Cause models.CauseTimer, its place (the test
// it was recorded in, whose own recording it follows when a judged commit
// keeps or drops it), and its stretch: where in the set's writes its answer
// holds. A write bounds that stretch only where the call's recorded answer
// changed across it: consecutive recordings of one call (one fingerprint, in
// recorded order) that answered alike (answerOf) share one stretch, from the
// last write before the first of them to the first write after the last of
// them, so a write between them (a session's keep-alive ping) cuts nothing:
// the recording proves the call read the same on both sides of it. The writes
// are the set's HTTP writes no report names (a timer's own write comes as often
// as its timer fires). mocks are the set's data-plane recordings. It returns
// byTest less what moved, and the worker scope in recorded order; byTest
// unchanged and nil when no harness reported a timer.
func timerScope(windows []models.ScopeWindow, mocks []capturedMock, byTest map[string][]models.MockEntry) (map[string][]models.MockEntry, []models.MockEntry) {
	reported := map[string]map[string]bool{} // test -> "<fingerprint>#<n>"
	for _, w := range windows {
		for _, c := range w.Timers {
			if reported[w.Name] == nil {
				reported[w.Name] = map[string]bool{}
			}
			reported[w.Name][timerKey(c.Method, c.Host, c.URL, c.Body)+"#"+strconv.Itoa(c.N)] = true
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
			if want[mk.fp+"#"+strconv.Itoa(n)] {
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
	// Each call's recordings in recorded order, joined into runs that answered
	// alike: a run's stretch is from the write before its first to the write
	// after its last.
	byCall := map[string][]capturedMock{}
	var calls []string
	for _, mk := range ordered {
		if !timer[mk.name] {
			continue
		}
		if _, ok := byCall[mk.fp]; !ok {
			calls = append(calls, mk.fp)
		}
		byCall[mk.fp] = append(byCall[mk.fp], mk)
	}
	stretch := map[string]*models.Stretch{}
	for _, fp := range calls {
		recs := byCall[fp]
		for i := 0; i < len(recs); {
			j := i
			for j+1 < len(recs) && recs[j+1].ans != "" && recs[j+1].ans == recs[i].ans {
				j++
			}
			st := &models.Stretch{}
			if k := at(recs[i]); k > 0 {
				st.After = writes[k-1].name
			}
			if k := at(recs[j]); k < len(writes) {
				st.Until = writes[k].name
			}
			for _, mk := range recs[i : j+1] {
				stretch[mk.name] = st
			}
			i = j + 1
		}
	}
	var worker []models.MockEntry
	for _, mk := range ordered {
		if st, ok := stretch[mk.name]; ok {
			cp := *st
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
