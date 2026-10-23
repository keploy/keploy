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
// Cause models.CauseTimer), each with the stretch of the set's writes it was
// recorded in (models.Stretch), so a replay answers such a call from its stretch
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

// timerScope moves each recording a test's harness reported a repeating timer
// made (models.ScopeWindow.Timers) from its test's mapping (byTest) into the
// set's worker scope, and with it each start-up (pool) recording of a call a
// harness reported a timer made anywhere in the set: a poller's tick between
// tests is no test's either. Which recording a report names is read the way
// the harness counted it: the N-th of the test's calls with its fingerprint,
// in recorded order. Each moved recording carries Cause models.CauseTimer, its
// place (the test it was recorded in, whose own recording it follows when a
// judged commit keeps or drops it; none for a tick between tests, which
// follows the pool), and its stretch: the last write recorded before it and
// the first after it, of the set's HTTP writes that no report names (a timer's
// own write comes as often as its timer fires, so it moves no stretch). mocks
// are the set's data-plane recordings. It returns byTest and startup less what
// moved, and the worker scope, in recorded order; all three unchanged and nil
// when no harness reported a timer.
func timerScope(windows []models.ScopeWindow, mocks []capturedMock, byTest map[string][]models.MockEntry, startup []models.MockEntry) (map[string][]models.MockEntry, []models.MockEntry, []models.MockEntry) {
	reported := map[string]map[string]bool{} // test -> "<fingerprint>#<n>"
	timerFP := map[string]bool{}
	for _, w := range windows {
		for _, c := range w.Timers {
			key := timerKey(c.Method, c.Host, c.URL, c.Body)
			if reported[w.Name] == nil {
				reported[w.Name] = map[string]bool{}
			}
			reported[w.Name][key+"#"+strconv.Itoa(c.N)] = true
			timerFP[key] = true
		}
	}
	if len(timerFP) == 0 {
		return byTest, startup, nil
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
	var pool []models.MockEntry
	for _, e := range startup {
		if mk, ok := byName[e.Name]; ok && timerFP[mk.fp] {
			timer[e.Name] = true
			continue
		}
		pool = append(pool, e)
	}
	if len(timer) == 0 {
		return byTest, startup, nil
	}

	// The set's writes in recorded order: the stretch boundaries.
	ordered := append([]capturedMock(nil), mocks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ts.Before(ordered[j].ts) })
	var writes []capturedMock
	for _, mk := range ordered {
		if mk.fp != "" && mk.write && !timer[mk.name] {
			writes = append(writes, mk)
		}
	}
	var worker []models.MockEntry
	for _, mk := range ordered {
		if !timer[mk.name] {
			continue
		}
		// The first write recorded after it; the one before that is the last
		// recorded before it.
		i := sort.Search(len(writes), func(i int) bool { return writes[i].ts.After(mk.ts) })
		st := &models.Stretch{}
		if i > 0 {
			st.After = writes[i-1].name
		}
		if i < len(writes) {
			st.Until = writes[i].name
		}
		worker = append(worker, models.MockEntry{Name: mk.name, Kind: string(models.HTTP), Place: place[mk.name], Cause: models.CauseTimer, Stretch: st})
	}
	return byTest, pool, worker
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
