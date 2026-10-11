package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// SPEC-SHARED MOCKS.
//
// A per-test mock is consumed on match, so a request the app makes MORE often
// at replay than it did at record finds its scope empty and misses. That
// happens for reasons the test does not control: a timer that fires more
// times because the test ran longer, a component that re-renders and
// refetches, a worker restart that drops an in-memory cache and makes a
// `beforeAll` load one more page. Measured on the 57-spec atg tape, 16 such
// misses inside `beforeAll` hooks cost 60 tests, because the hook throws and
// every test in its describe is skipped.
//
// Promoting those endpoints to session lifetime fixed the misses but made the
// copies global and the matching pool ~3,800 mocks; the endpoints are not one
// response tape-wide (different users, different apps), so a global copy was
// sometimes the wrong one.
//
// The recording itself says when reuse is safe. Group every HTTP mock a spec
// recorded by its EXACT request (method, host, URL, body). If every copy in
// the group carries the same response, nothing the spec did ever changed the
// answer to that request, so one copy can answer every call from any scope in
// the spec, any number of times, without serving a response from the wrong
// moment. Measured: 88% of exact requests are like this within their spec.
// Where copies differ, the request is part of the test's own sequence (a list
// read before and after a create, a status before and after a merge); those
// stay per-test and are consumed in recorded order, as before.
//
// Writes are held to a stricter bar: a single recorded write (one
// CreateBranch) is never shared, because answering it twice would hand back
// the same created id. A write is shared only when the recording shows it
// REPEATED with an identical response every time (a heartbeat POST, an
// idempotent validation), i.e. the recording proves repetition is harmless.
//
// Scope of reuse is the spec, never wider: no cross-spec copy is ever added.

// specPool is the shared-mock plan for one spec.
type specPool struct {
	// reps holds one representative mock name per shared request, in record
	// order. It is appended to every scope of the spec.
	reps []string
	// members maps every mock in a shared group to its representative, so the
	// redundant copies can be dropped from a scope's list (they are identical).
	members map[string]string
	// The spec's recorded test order and each suite copy's slot in it
	// (setupslot.go): index is a test's 1-based place, starts the ascending
	// recorded starts, setupSlot how many tests had started when a suite copy
	// was recorded.
	index     map[string]int
	starts    []time.Time
	setupSlot map[string]int
}

type specPoolStats struct {
	mocks        int
	groups       int
	sharedGroups int
	sharedWrites int
	dedupDropped int
}

// specOfScope returns the spec file a scope name belongs to. Test scopes are
// "<spec> > <describe> > <title>"; suite scopes are "__suite__:<spec>".
func specOfScope(name string) string {
	s := strings.TrimPrefix(name, "__suite__:")
	if i := strings.Index(s, " > "); i >= 0 {
		s = s[:i]
	}
	return s
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// requestKey identifies an exact request: method, destination host, full URL
// (path and query), body bytes, and whether the request carries an Origin
// header. A body offloaded to an asset (>1MB) is keyed by its reference, so two
// different large bodies never collapse to one key.
//
// Only Origin PRESENCE is keyed, not the full set of header names, because it
// is the only header the matcher checks in BOTH directions (integrations/http
// HeadersContainKeys): every other header is a subset test — a mock's names
// must be on the live request, extra request headers are fine — so a copy
// recorded with FEWER headers also matches a request that carries more. Keying
// all names therefore split one request into groups that can still answer each
// other's calls. Measured on vscode-platform: GET /auth/check before sign-in (no
// Cookie, 401) and after (Cookie, 200) became two single-response groups, both
// shared, and the never-consumed 401 kept answering the signed-in calls.
//
// Origin presence still separates what must stay apart: GET /auth/check from
// the app's server-side helper (no Origin) and from the browser (Origin) answer
// with different CORS headers and the matcher refuses one for the other.
func requestKey(m *models.Mock) string {
	r := m.Spec.HTTPReq
	host, origin := "", "no-origin"
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "host":
			host = v
		case "origin":
			origin = "origin"
		}
	}
	return shortHash(strings.ToUpper(string(r.Method)), host, r.URL, r.Body,
		fmt.Sprintf("%v", r.BodyRef), origin)
}

// responseKey identifies a response by status code, body and the CORS headers a
// browser enforces. A response the disk store spilled is hydrated so its real
// body is compared. If it cannot be read, the mock gets a key nobody else has,
// which marks its group as differing — never shared on a guess.
//
// Set-Cookie is deliberately NOT part of the key. The recorded cookie values
// are never checked by any mock, so a copy that re-sets the session cookie and
// one that does not are the same answer to the app. Keying it kept identical
// /auth/check and /cluster/proxy-origins copies unshared in five spec x request
// pairs; they were consumed, ran out in a later beforeAll, and the 502 killed
// the describe (filtering:1106 and the two tests after it). The cookie is kept
// where it matters by choosing a cookie-bearing copy as the group's
// representative (see representative).
func responseKey(m *models.Mock) string {
	resp, err := m.HydratedHTTPResp()
	if err != nil || resp == nil {
		return "unreadable:" + m.Name
	}
	var acao, acac string
	for k, v := range resp.Header {
		switch strings.ToLower(k) {
		case "access-control-allow-origin":
			acao = v
		case "access-control-allow-credentials":
			acac = v
		}
	}
	return shortHash(strconv.Itoa(resp.StatusCode), resp.Body, acao, acac)
}

// setsCookie reports whether a mock's recorded response carries Set-Cookie.
func setsCookie(m *models.Mock) bool {
	if m.Spec.HTTPResp == nil {
		return false
	}
	for k := range m.Spec.HTTPResp.Header {
		if strings.EqualFold(k, "set-cookie") {
			return true
		}
	}
	return false
}

// representative picks the copy that answers for a shared group: the earliest
// copy that sets a cookie, else the earliest copy. Preferring the cookie keeps
// what the matcher's own tie-break (ExactBodyMatch) already guarantees — a
// caller that reads the session cookie out of the response (the RBAC seed
// helper reads jwt_token_staging from GET /auth/check) still gets it once the
// other copies are folded into this one. copies must be in record order.
func representative(copies []*models.Mock) *models.Mock {
	for _, c := range copies {
		if setsCookie(c) {
			return c
		}
	}
	return copies[0]
}

// graphQLQuery returns the operation text of a GraphQL-over-HTTP JSON body.
func graphQLQuery(r *models.HTTPReq) (string, bool) {
	var gql struct {
		Query *string `json:"query"`
	}
	if r == nil || r.Body == "" || json.Unmarshal([]byte(r.Body), &gql) != nil || gql.Query == nil {
		return "", false
	}
	return strings.TrimSpace(*gql.Query), true
}

// isGraphQLMutation reports whether a mock records a GraphQL mutation.
func isGraphQLMutation(m *models.Mock) bool {
	if m == nil || m.Kind != models.HTTP {
		return false
	}
	q, ok := graphQLQuery(m.Spec.HTTPReq)
	return ok && strings.HasPrefix(q, "mutation")
}

// isWriteRequest reports whether a request changes server state: a GraphQL
// mutation, or a non-GraphQL POST/PUT/PATCH/DELETE.
func isWriteRequest(m *models.Mock) bool {
	r := m.Spec.HTTPReq
	if q, ok := graphQLQuery(r); ok {
		return strings.HasPrefix(q, "mutation")
	}
	switch strings.ToUpper(string(r.Method)) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// buildSpecPool decides, for every exact request a spec recorded, whether it
// is shared across the spec. Non-HTTP mocks are left alone.
func buildSpecPool(mocks []*models.Mock) (*specPool, specPoolStats) {
	type group struct {
		copies []*models.Mock
		resps  map[string]struct{}
		write  bool
	}
	groups := map[string]*group{}
	var order []string
	st := specPoolStats{}
	for _, m := range mocks {
		if m == nil || m.Kind != models.HTTP || m.Spec.HTTPReq == nil {
			continue
		}
		st.mocks++
		k := requestKey(m)
		g, ok := groups[k]
		if !ok {
			g = &group{resps: map[string]struct{}{}}
			groups[k] = g
			order = append(order, k)
		}
		g.copies = append(g.copies, m)
		g.resps[responseKey(m)] = struct{}{}
		if isWriteRequest(m) {
			g.write = true
		}
	}
	st.groups = len(groups)
	sp := &specPool{members: map[string]string{}}
	for _, k := range order {
		g := groups[k]
		if len(g.resps) != 1 {
			continue // the answer changed within the spec: consume in order
		}
		if g.write && len(g.copies) < 2 {
			continue // one recorded write: answering it twice would repeat its effect
		}
		sort.SliceStable(g.copies, func(i, j int) bool {
			return g.copies[i].Spec.ReqTimestampMock.Before(g.copies[j].Spec.ReqTimestampMock)
		})
		rep := representative(g.copies).Name
		sp.reps = append(sp.reps, rep)
		for _, c := range g.copies {
			sp.members[c.Name] = rep
		}
		st.sharedGroups++
		st.dedupDropped += len(g.copies) - 1
		if g.write {
			st.sharedWrites++
		}
	}
	return sp, st
}

// loadMocksByName returns the stored per-test mocks with the given names, from
// the on-disk store when there is one, plus any resident per-test mocks.
func (a *Agent) loadMocksByName(names []string) []*models.Mock {
	v, ok := a.clientMocks.Load(uint64(0))
	if !ok {
		return nil
	}
	storage := v.(*ClientMockStorage)
	storage.mu.RLock()
	resident := append([]*models.Mock(nil), storage.filtered...)
	disk := storage.diskMocks
	storage.mu.RUnlock()

	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	var out []*models.Mock
	have := map[string]struct{}{}
	if disk != nil {
		loaded, err := disk.LoadByNames(names)
		if err != nil {
			a.logger.Warn("KPSPEC could not load a spec's mocks from the disk store; sharing is skipped for this spec", zap.Error(err))
			return nil
		}
		for _, m := range loaded {
			out = append(out, m)
			have[m.Name] = struct{}{}
		}
	}
	for _, m := range resident {
		if m == nil {
			continue
		}
		if _, w := want[m.Name]; !w {
			continue
		}
		if _, dup := have[m.Name]; dup {
			continue
		}
		out = append(out, m)
		have[m.Name] = struct{}{}
	}
	return out
}

// specPoolFor returns (building once and caching) the shared-mock plan for a
// spec, computed over every scope the spec recorded: all its tests and its
// suite.
func (a *Agent) specPoolFor(spec string) *specPool {
	a.specPoolMu.RLock()
	sp, ok := a.specPools[spec]
	a.specPoolMu.RUnlock()
	if ok {
		return sp
	}

	a.scopeMu.Lock()
	var all []string
	ownerOf := map[string]string{}
	for scopeName, names := range a.scopeTable {
		if specOfScope(scopeName) == spec {
			all = append(all, names...)
			for _, n := range names {
				ownerOf[n] = scopeName
			}
		}
	}
	a.scopeMu.Unlock()

	loaded := a.loadMocksByName(all)
	sp, st := buildSpecPool(loaded)
	sp.placeSetup(ownerOf, loaded)

	a.specPoolMu.Lock()
	if a.specPools == nil {
		a.specPools = map[string]*specPool{}
	}
	if a.sharedInSpec == nil {
		a.sharedInSpec = map[string]struct{}{}
	}
	a.specPools[spec] = sp
	for _, r := range sp.reps {
		a.sharedInSpec[r] = struct{}{}
	}
	a.specPoolMu.Unlock()

	a.logger.Info("KPSPEC pool built",
		zap.String("spec", spec),
		zap.Int("http_mocks", st.mocks),
		zap.Int("exact_requests", st.groups),
		zap.Int("shared_requests", st.sharedGroups),
		zap.Int("shared_writes", st.sharedWrites),
		zap.Int("duplicate_copies_dropped", st.dedupDropped),
		zap.Int("tests_in_order", len(sp.index)),
		zap.Int("setup_copies_slotted", len(sp.setupSlot)))
	return sp
}

// applySpecPool rewrites a scope's mock-name list: redundant copies of shared
// requests are dropped (one representative serves them all) and the spec's
// shared representatives are appended, so every scope in the spec — tests and
// the suite (`beforeAll`) alike — can reach them. Own and suite names keep
// their order and come first.
func (a *Agent) applySpecPool(scopeName string, names []string) []string {
	sp := a.specPoolFor(specOfScope(scopeName))
	if sp == nil || len(sp.reps) == 0 {
		return names
	}
	out := make([]string, 0, len(names)+len(sp.reps))
	seen := make(map[string]struct{}, len(names)+len(sp.reps))
	for _, n := range names {
		if rep, ok := sp.members[n]; ok && rep != n {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	added := 0
	for _, r := range sp.reps {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
		added++
	}
	a.logger.Info("KPSPEC scope pool",
		zap.String("scope", scopeName),
		zap.Int("names_before", len(names)),
		zap.Int("names_after", len(out)),
		zap.Int("shared_added_from_spec", added))
	return out
}

// markSharedInSpec flags the staged mocks that are spec-shared, so the proxy
// does not consume them on match.
func (a *Agent) markSharedInSpec(mocks []*models.Mock) {
	a.specPoolMu.RLock()
	defer a.specPoolMu.RUnlock()
	if len(a.sharedInSpec) == 0 {
		return
	}
	for _, m := range mocks {
		if m == nil {
			continue
		}
		if _, ok := a.sharedInSpec[m.Name]; ok {
			m.TestModeInfo.SharedInSpec = true
		}
	}
}

// resetSpecPools drops cached plans when a new scope table is installed.
func (a *Agent) resetSpecPools() {
	a.specPoolMu.Lock()
	a.specPools = nil
	a.sharedInSpec = nil
	a.specPoolMu.Unlock()
}
