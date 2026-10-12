package http

import (
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// PROTOTYPE -- write barrier for stale read responses.
//
// THE PROBLEM. Keploy chooses a mock by comparing the incoming request to the
// recorded ones. For a read like GraphQL `MyApps` every request is BYTE
// IDENTICAL, so every recording of it is an equally perfect match. But those
// recordings disagree, because the server's state moved between them:
//
//	MyApps @01:43:57 -> 2 apps
//	CreateApp @01:44:43 -> makes app X
//	MyApps @01:44:43 -> 3 apps (includes X)
//
// Nothing in the request says which side of the create the caller is on, so
// keploy can serve the 2-app recording to a caller that just created X. There
// is no miss and no error: the response is genuine, just from the wrong moment.
// The caller cannot see what it just made.
//
// THE RULE. When a mutation is served, the world changed. Any read recorded
// BEFORE that moment is no longer a truthful answer, so stop serving it.
//
// Deliberately conservative:
//   - Only arms on a mutation that was actually SERVED, never on a guess.
//   - Only filters reads that leave a workable pool behind. If every candidate
//     predates the barrier, or if filtering would starve the pool below
//     minSurvivors, the pool is returned untouched. Both guards are needed: the
//     first alone let the barrier cut MyApps to 20 candidates and drive its miss
//     rate from 2% to 93%. See filterStale.
//   - Keyed per request identity, so a barrier from one operation cannot
//     silently starve an unrelated one.
type writeBarrier struct {
	mu sync.RWMutex
	// at maps a RESOURCE noun ("app", "role", "testsuite") to the recorded time
	// of the latest served mutation of that resource.
	//
	// WHY PER-RESOURCE. A single global timestamp says "something changed, so
	// every earlier read is stale". Measured on the 108-test suite that is far
	// too coarse: 23 distinct mutations armed it (createRole, deleteRoleBinding,
	// createUser, ...) and each then filtered EVERY later read regardless of what
	// it read -- single drops discarded 1784 of 1882 candidates. It fixed 13
	// tests and broke 3, and the 3 it broke were reads of resources no mutation
	// had touched. Keyed by resource, createRole can no longer invalidate a read
	// of apps.
	at map[string]time.Time
}

// mutationOps are the GraphQL operation-name prefixes treated as state changes.
// Prefix-matched and case-insensitive so CreateApp / createATGApp / CreateFoo
// all arm the barrier.
var mutationOps = []string{"create", "update", "delete", "insert", "remove", "add", "set", "link", "unlink"}

func isMutationIdentity(id string) bool {
	i := strings.IndexByte(id, '=')
	if i < 0 || i+1 >= len(id) {
		return false
	}
	op := strings.ToLower(id[i+1:])
	for _, p := range mutationOps {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

// recordedAt is the mock's capture time, the ordering key the barrier compares
// against. Zero when the tape carries no timestamp, which callers treat as
// "unknown, do not filter".
func recordedAt(m *models.Mock) time.Time {
	if m == nil {
		return time.Time{}
	}
	return m.Spec.ReqTimestampMock
}

// resourceOf reduces an identity to the RESOURCE noun it acts on, so a
// mutation and the reads it invalidates land on the same key:
//
//	createATGApp / CreateATGApp -> "atgapp"      MyApps -> "app"? no: see below
//	createRole                  -> "role"
//	deleteRoleBinding           -> "rolebinding"
//
// The verb prefix is stripped, the rest lower-cased, and a trailing plural "s"
// removed so `apps` and `app` agree. Reads use their response data key
// (`apps`, `appById`) reduced the same way. Returns "" when nothing is left,
// which callers treat as "unknown -- do not filter".
func resourceOf(id string) string {
	i := strings.IndexByte(id, '=')
	if i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	low := strings.ToLower(id)
	for _, p := range mutationOps {
		if strings.HasPrefix(low, p) {
			low = low[len(p):]
			break
		}
	}
	// Trim common read suffixes so appById and apps share a key with app.
	for _, suf := range []string{"byid", "bykey", "byname"} {
		low = strings.TrimSuffix(low, suf)
	}
	low = strings.TrimSuffix(low, "s")
	// Strip leading qualifiers so createATGApp, MyApps and apps all agree:
	// "atg" is a product prefix, "my"/"all"/"get" are read-side scoping words.
	for _, p := range []string{"atg", "my", "all", "get"} {
		low = strings.TrimPrefix(low, p)
	}
	low = strings.TrimSuffix(low, "s")
	return low
}

// arm records that a mutation on `res` recorded at ts has been served.
// Monotonic per resource: a later barrier never moves backwards.
func (w *writeBarrier) arm(res string, ts time.Time) {
	if ts.IsZero() || res == "" {
		return
	}
	w.mu.Lock()
	if w.at == nil {
		w.at = make(map[string]time.Time, 8)
	}
	if ts.After(w.at[res]) {
		w.at[res] = ts
	}
	w.mu.Unlock()
}

func (w *writeBarrier) barrierFor(res string) time.Time {
	if res == "" {
		return time.Time{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.at[res]
}

// filterStale drops candidates recorded before the barrier FOR THE RESOURCE
// this request reads. reqID is the live request's identity; only a mutation of
// the same resource can invalidate it.
//
// Returns the ORIGINAL slice when no barrier applies, when no candidate carries
// a timestamp, or when filtering would empty the pool -- so the worst case is
// today's behaviour, never a new miss.
func (w *writeBarrier) filterStale(mocks []*models.Mock, reqID string) ([]*models.Mock, int) {
	res := resourceOf(reqID)
	b := w.barrierFor(res)
	if b.IsZero() || len(mocks) == 0 {
		return mocks, 0
	}
	kept := make([]*models.Mock, 0, len(mocks))
	dropped := 0
	for _, m := range mocks {
		// A spec-shared mock has the same response in every copy its spec
		// recorded, so no mutation within the spec changed it; a barrier armed
		// elsewhere says nothing about it.
		if m.TestModeInfo.SharedInSpec {
			kept = append(kept, m)
			continue
		}
		ts := recordedAt(m)
		if !ts.IsZero() && ts.Before(b) {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	// SURVIVOR FLOOR. The guarantee this type advertises -- "can never
	// manufacture a miss where one did not exist before" -- only held for the
	// all-or-nothing case below (len(kept) == 0). It did NOT hold when a few
	// candidates survived, and that is the case that actually bites.
	//
	// Measured on the 64-spec k8 set: the barrier repeatedly cut MyApps from
	// ~1855 candidates to 20 (sometimes 3). Same request, byte-identical body
	// -- all 420 recorded MyApps bodies are identical -- yet:
	//
	//   pool after barrier > 50 : 132 served,  3 missed   (2% miss)
	//   pool after barrier <= 50:   2 served, 28 missed  (93% miss)
	//
	// The survivors are too few and too clustered to answer the request, so the
	// barrier converts a servable read into a hard miss -- exactly what it
	// promises not to do. When the filter would leave the pool below the floor,
	// decline to filter: serving a slightly stale read is strictly better than
	// serving a 502, which is what the un-filtered path did before this type
	// existed anyway.
	const minSurvivors = 50
	if len(kept) < minSurvivors && len(mocks) >= minSurvivors {
		return mocks, 0
	}
	if len(kept) == 0 || dropped == 0 {
		return mocks, 0
	}
	return kept, dropped
}

// reqIdentityOfMock derives a mock's request identity the same way
// filterByReqIdentity derives the live request's: operationName when the body
// carries one, else the GraphQL root field. Empty when neither applies, which
// callers treat as "not a mutation".
func reqIdentityOfMock(m *models.Mock) string {
	if m == nil || m.Spec.HTTPReq == nil {
		return ""
	}
	body := jsonBodyOf([]byte(m.Spec.HTTPReq.Body))
	id := mismatch.ExtractReqIdentity(body)
	if id == "" {
		id = gqlRootField(body)
	}
	return id
}
