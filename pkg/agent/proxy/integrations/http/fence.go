package http

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// PENDING-WRITE FENCE: the forward twin of the write barrier.
//
// A per-test read is consumed in recorded order, so when the app asks for it
// more often than the recording did, the extra call takes the NEXT copy in line
// — and that copy can belong to a later state of the test. Measured on
// branch-review-flow:172: the test records
//
//	requestBranchChanges → reload (GetBranches = CHANGES_REQUESTED ×2)
//	→ requestBranchReview → reload (GetBranches = REVIEW_REQUESTED ×2)
//
// An earlier page load made one extra GetBranches call and used up one
// CHANGES_REQUESTED copy, so the reload after "request changes" got its second
// answer from the copy recorded after the NEXT "request review". The page jumped
// to a state the test had not reached and the assertion failed. request-config:20
// is the same shape: a MyApps copy meant for after GenerateAPITests was used up
// before it, and the page load that needed it got a 502.
//
// THE RULE. While the staged test still has a write of its own pending (one of
// its recorded GraphQL mutations not served yet), no read may be served a copy
// recorded AFTER that write: the replay has not reached that moment. The same
// goes for a suite copy captured after the test's own recording ended
// (FenceLater): it belongs to a later hook, and using it up starves that hook
// (filtering.spec.ts: a later describe's beforeAll got an empty suite list).
//
// REPEAT-LAST. When no copy of the exact request is usable now — every copy is
// held back, or every copy has been used up — the copy the SPEC was last
// served for the same exact request answers again, but ONLY if no write has
// been served in the spec since that copy was. No write since means the state
// it describes cannot have changed, so it is still the true answer
// (request-config:20: an AppById aborted by a page reload used up the copy
// recorded for after the reload). After a write the request falls through to
// matching as before, and a fence never turns a match into a miss.
//
// The memory spans the spec — its tests, hooks and the gaps between them —
// not one test. A request that lands just after a test ends was often recorded
// inside the NEXT test's window, so neither the test nor the gap holds a copy.
// Measured on filtering.spec.ts: test 1106 ends by resetting its filters, the
// page refetches the unfiltered suite list in the gap, all three setup copies
// were rightly used by their own beforeAll hooks, and fuzzy matching answered
// with a filtered EMPTY list; the page showed the empty-app screen and 1167
// timed out. The beforeAll had been served the same exact request seconds
// earlier with no write since — that answer is the true one.
//
// Deliberately narrow:
//   - Only the staged TEST's own GraphQL mutations are fences (the agent tags
//     them, service/agent/fencescope.go). Heartbeat POSTs, validations and
//     suite setup writes are not: a write the replay never repeats would hold
//     every later read back.
//   - Only reads are fenced: GET, and GraphQL queries.
//   - Spec-shared and session mocks are never held back; they answer identically
//     at every moment by construction.
type fence struct {
	mu sync.Mutex
	// spec is the spec the memory below belongs to; it resets on a new spec.
	spec string
	// writes counts the writes served in the spec.
	writes int
	// last is the per-test copy most recently served for each exact live
	// request in the spec, with the write count at that moment.
	last map[string]lastServed
	// doneScope/doneAt: the staged test whose own write was served last, and
	// the latest recorded time among its served own writes.
	doneScope string
	doneAt    time.Time
}

// specOfFenceScope returns the spec a staged scope belongs to: a test scope is
// "<spec> > <describe> > <title>", a setup period "setup:<spec>#<position>"
// (service/agent/setupslot.go).
func specOfFenceScope(scope string) string {
	if rest, ok := strings.CutPrefix(scope, "setup:"); ok {
		if i := strings.LastIndex(rest, "#"); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	if i := strings.Index(scope, " > "); i >= 0 {
		return scope[:i]
	}
	return scope
}

type lastServed struct {
	mock   *models.Mock
	writes int
}

// fenceKey identifies an exact live request for repeat-last.
func fenceKey(input *req) string {
	return input.method + " " + input.url.String() + "\x00" + string(input.body)
}

// gqlQueryOf returns the operation text of a GraphQL-over-HTTP JSON body.
func gqlQueryOf(body []byte) (string, bool) {
	var gql struct {
		Query *string `json:"query"`
	}
	if len(body) == 0 || json.Unmarshal(body, &gql) != nil || gql.Query == nil {
		return "", false
	}
	return strings.TrimSpace(*gql.Query), true
}

// isFencedRead reports whether a live request is a read the fence applies to.
func isFencedRead(input *req) bool {
	if strings.EqualFold(input.method, "GET") {
		return true
	}
	q, ok := gqlQueryOf(input.body)
	return ok && !strings.HasPrefix(q, "mutation") && !strings.HasPrefix(q, "subscription")
}

// isServedWrite reports whether a served mock changes state for repeat-last:
// a GraphQL mutation, or a non-GraphQL POST/PUT/PATCH/DELETE (a login), that
// is not spec-shared (a shared one answered the same every time it was
// recorded, so repeating around it is safe: the session heartbeat).
func isServedWrite(m *models.Mock) bool {
	if m == nil || m.TestModeInfo.SharedInSpec || m.Spec.HTTPReq == nil {
		return false
	}
	if m.TestModeInfo.FenceWrite {
		return true
	}
	if q, ok := gqlQueryOf([]byte(m.Spec.HTTPReq.Body)); ok {
		return strings.HasPrefix(q, "mutation")
	}
	switch strings.ToUpper(string(m.Spec.HTTPReq.Method)) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// pendingWrite returns the staged test's scope (every mock staged with a test
// carries it) and the recorded time of its earliest own write still in the
// per-test pool. Served writes are consumed
// (removed from the pool), so what remains is what the replay has not done yet.
func pendingWrite(perTest []*models.Mock) (string, time.Time) {
	var scope string
	var at time.Time
	for _, m := range perTest {
		if m == nil || m.TestModeInfo.FenceScope == "" {
			continue
		}
		scope = m.TestModeInfo.FenceScope
		if !m.TestModeInfo.FenceWrite {
			continue
		}
		if ts := m.Spec.ReqTimestampMock; !ts.IsZero() && (at.IsZero() || ts.Before(at)) {
			at = ts
		}
	}
	return scope, at
}

// fenceable reports whether a candidate is a consumable per-test copy the fence
// may hold back.
func fenceable(m *models.Mock) bool {
	return m != nil &&
		m.TestModeInfo.Lifetime == models.LifetimePerTest &&
		!m.TestModeInfo.SharedInSpec &&
		!m.Spec.ReqTimestampMock.IsZero()
}

// fenceResult is what apply decided for one live request.
type fenceResult struct {
	candidates []*models.Mock // the candidates the request may be served from now
	held       int            // copies of this request held back
	later      int            // of those, suite copies recorded after the test
	repeat     *models.Mock   // non-nil: answer with this copy, without consuming anything
	why        string         // why repeat: "held" / "used-up" (repeat-last) or "later-setup" (peek)
	peek       bool           // repeat is a later setup copy answering early, not a repeat
	stale      int            // copies of this read skipped as recorded before a served own write
	scope      string
	at         time.Time
	done       time.Time
}

// apply decides which copies of this request the staged test may use now.
// candidates is the schema-matched set (it may mix other operations on the
// same URL, e.g. every GraphQL query on /query); only copies of THIS request
// are judged.
func (f *fence) apply(perTest, candidates []*models.Mock, input *req) fenceResult {
	scope, at := pendingWrite(perTest)
	f.mu.Lock()
	if spec := specOfFenceScope(scope); scope != "" && spec != f.spec {
		f.spec, f.last, f.writes = spec, nil, 0
	}
	last, haveLast := f.last[fenceKey(input)]
	repeatable := haveLast && last.writes == f.writes
	var done time.Time
	if scope != "" && f.doneScope == scope {
		done = f.doneAt
	}
	f.mu.Unlock()

	res := fenceResult{candidates: candidates, scope: scope, at: at, done: done}
	if scope == "" {
		return res
	}
	// Reads are fenced against the test's pending writes AND later setup
	// copies. Writes only against later setup copies: a later hook's create
	// must not be used up by an earlier hook's extra call (setupslot.go).
	read := isFencedRead(input)
	future := func(m *models.Mock) bool {
		if !fenceable(m) {
			return false
		}
		if m.TestModeInfo.FenceLater {
			return true
		}
		return read && !at.IsZero() && m.Spec.ReqTimestampMock.After(at)
	}
	body := string(input.body)
	held := make(map[*models.Mock]struct{})
	usableExact := 0
	var peek *models.Mock // earliest held exact copy, when every held exact copy is a later setup copy
	peekable := true
	for _, m := range filterByReqIdentity(candidates, input.body) {
		exact := m.Spec.HTTPReq != nil && m.Spec.HTTPReq.Body == body
		if future(m) {
			held[m] = struct{}{}
			if m.TestModeInfo.FenceLater {
				res.later++
			}
			if exact {
				if !m.TestModeInfo.FenceLater {
					peekable = false
				} else if peek == nil || m.Spec.ReqTimestampMock.Before(peek.Spec.ReqTimestampMock) {
					peek = m
				}
			}
			continue
		}
		if exact {
			usableExact++
		}
	}
	if usableExact == 0 {
		// Nothing for this exact request is usable now: all held back, or all
		// used up. Repeat the last answer if the state cannot have changed.
		if read && repeatable {
			res.repeat, res.why = last.mock, "used-up"
			if len(held) > 0 {
				res.why = "held"
			}
			return res
		}
		// Only a later hook's copy matches exactly: answer with it, but leave
		// it in place for its own hook. A test's own copy held behind a
		// pending write is not peeked: the request falls through and matches
		// as before, so a fence never turns a match into a miss.
		if peek != nil && peekable {
			res.repeat, res.why, res.peek = peek, "later-setup", true
		}
		return res
	}
	// THE BACKWARD TWIN. Once the test's own write recorded at `done` has been
	// served, a copy of this read recorded before it describes the state before
	// that write. Measured on rbac-groups:87: the record fetched GetAllGroups
	// twice before CreateGroup, the replay once, and the leftover pre-create copy
	// (an empty list) answered the refetch after the create. Such a copy is
	// skipped, but only when a copy recorded after the write is usable for the
	// SAME caller (same login cookies, callerOf): in a two-user test the newer copy may
	// be another user's answer (load-reports: holder, then non-holder).
	if read && !done.IsZero() {
		after := map[string]bool{}
		for _, m := range candidates {
			if _, h := held[m]; h || !fenceable(m) || m.Spec.HTTPReq == nil || m.Spec.HTTPReq.Body != body {
				continue
			}
			if m.Spec.ReqTimestampMock.After(done) {
				after[callerOf(m)] = true
			}
		}
		for _, m := range candidates {
			if _, h := held[m]; h || !fenceable(m) || m.TestModeInfo.FenceLater || m.Spec.HTTPReq == nil || m.Spec.HTTPReq.Body != body {
				continue
			}
			if m.Spec.ReqTimestampMock.Before(done) && after[callerOf(m)] {
				held[m] = struct{}{}
				res.stale++
			}
		}
	}
	if len(held) == 0 {
		return res
	}
	kept := make([]*models.Mock, 0, len(candidates)-len(held))
	for _, m := range candidates {
		if _, h := held[m]; !h {
			kept = append(kept, m)
		}
	}
	res.candidates, res.held = kept, len(held)-res.stale
	return res
}

// callerOf returns who a mock was recorded for: its login cookies (name holds
// token, session, jwt, auth or sid) and Authorization header. The rest of the
// Cookie header is no identity: analytics cookies (_ga, _fbp, _clck) appear and
// change between page loads of the same user (rbac-groups:87: the copies before
// and after CreateGroup carry the same jwt_token_staging but different _ga).
func callerOf(m *models.Mock) string {
	var ids []string
	for k, v := range m.Spec.HTTPReq.Header {
		switch {
		case strings.EqualFold(k, "Authorization"):
			ids = append(ids, "authorization="+v)
		case strings.EqualFold(k, "Cookie"):
			for _, part := range strings.Split(v, ";") {
				name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
				if isLoginCookie(name) {
					ids = append(ids, strings.TrimSpace(part))
				}
			}
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "; ")
}

func isLoginCookie(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"token", "session", "jwt", "auth", "sid"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// served records a mock the matcher just served while scope was staged: a
// write moves the spec's state on, a read becomes the answer repeat-last uses.
func (f *fence) served(scope string, input *req, m *models.Mock) {
	if scope == "" || m == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if specOfFenceScope(scope) != f.spec {
		return
	}
	if isServedWrite(m) {
		f.writes++
		// The test's own write (tagged by the agent) moves the test past its
		// recorded time; reads recorded before it are no longer its answers.
		if m.TestModeInfo.FenceWrite && fenceable(m) && m.TestModeInfo.FenceScope == scope {
			if f.doneScope != scope {
				f.doneScope, f.doneAt = scope, time.Time{}
			}
			if ts := m.Spec.ReqTimestampMock; ts.After(f.doneAt) {
				f.doneAt = ts
			}
		}
		return
	}
	if !isFencedRead(input) || !fenceable(m) {
		return
	}
	if f.last == nil {
		f.last = map[string]lastServed{}
	}
	f.last[fenceKey(input)] = lastServed{mock: m, writes: f.writes}
}
