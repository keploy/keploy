package http

import (
	"net/url"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

const (
	gqlGetBranches = `{"operationName":"GetBranches","query":"query GetBranches($appId: String!) { branches(appId: $appId) { id status } }","variables":{"appId":"a1"}}`
	gqlMyApps      = `{"operationName":"MyApps","query":"query MyApps { apps { id } }"}`
	gqlReview      = `{"query":"mutation($id: String!) { requestBranchReview(branchId: $id) }","variables":{"id":"b1"}}`
)

func fenceMock(name, body string, at int, scope string, write bool) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "/query", Body: body},
			HTTPResp:         &models.HTTPResp{StatusCode: 200},
			ReqTimestampMock: time.Unix(int64(1000+at), 0),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, FenceScope: scope, FenceWrite: write},
	}
}

func liveReq(method, body string) *req {
	u, _ := url.Parse("http://localhost:8083/query")
	return &req{method: method, url: u, body: []byte(body)}
}

// The branch-review-flow:172 shape: one read recorded before the test's pending
// write and one after it. The later copy is the NEXT state and must not answer
// while the write is still pending.
func TestFenceHoldsBackCopiesAfterAPendingWrite(t *testing.T) {
	const scope = "spec > review"
	w := fenceMock("w", gqlReview, 10, scope, true)
	before := fenceMock("r-before", gqlGetBranches, 5, scope, false)
	after := fenceMock("r-after", gqlGetBranches, 15, scope, false)
	pool := []*models.Mock{w, before, after}
	in := liveReq("POST", gqlGetBranches)
	var f fence

	got := f.apply(pool, []*models.Mock{before, after}, in)
	if got.held != 1 || len(got.candidates) != 1 || got.candidates[0] != before {
		t.Fatalf("held=%d candidates=%v: the copy recorded after the pending write must be held back", got.held, names(got.candidates))
	}

	// before is served and consumed; only the after copy is left.
	f.served(got.scope, in, before)
	got = f.apply([]*models.Mock{w, after}, []*models.Mock{after}, in)
	if got.repeat != before {
		t.Fatalf("repeat=%v: with only a future copy left, the last copy served must answer again", got.repeat)
	}

	// The write is served (consumed), so the fence lifts.
	got = f.apply([]*models.Mock{after}, []*models.Mock{after}, in)
	if got.repeat != nil || got.held != 0 || len(got.candidates) != 1 {
		t.Fatalf("after the write is served the next state must be reachable: %+v", got)
	}
}

// A fence never invents a miss: with nothing served yet for this request in the
// test, matching proceeds exactly as before.
func TestFenceWithNothingServedYetChangesNothing(t *testing.T) {
	const scope = "spec > t"
	w := fenceMock("w", gqlReview, 10, scope, true)
	after := fenceMock("r-after", gqlGetBranches, 15, scope, false)
	var f fence
	got := f.apply([]*models.Mock{w, after}, []*models.Mock{after}, liveReq("POST", gqlGetBranches))
	if got.repeat != nil || got.held != 0 || len(got.candidates) != 1 {
		t.Fatalf("got %+v, want the candidates untouched", got)
	}
}

// Only copies of THIS request are judged: /query carries every GraphQL
// operation, and another operation's future copies are not this read's concern.
func TestFenceJudgesOnlyTheSameOperation(t *testing.T) {
	const scope = "spec > t"
	w := fenceMock("w", gqlReview, 10, scope, true)
	before := fenceMock("b-before", gqlGetBranches, 5, scope, false)
	after := fenceMock("b-after", gqlGetBranches, 15, scope, false)
	apps := fenceMock("apps-after", gqlMyApps, 20, scope, false)
	var f fence
	got := f.apply([]*models.Mock{w, before, after, apps}, []*models.Mock{before, after, apps}, liveReq("POST", gqlGetBranches))
	if got.held != 1 || len(got.candidates) != 2 || got.candidates[1] != apps {
		t.Fatalf("candidates=%v held=%d: only the GetBranches copy after the write may be held", names(got.candidates), got.held)
	}
}

func TestFenceLeavesWritesSharedAndUnscopedAlone(t *testing.T) {
	const scope = "spec > t"
	w := fenceMock("w", gqlReview, 10, scope, true)
	after := fenceMock("r-after", gqlGetBranches, 15, scope, false)
	before := fenceMock("r-before", gqlGetBranches, 5, scope, false)

	t.Run("a write is matched normally", func(t *testing.T) {
		var f fence
		w2 := fenceMock("w2", gqlReview, 20, scope, false)
		got := f.apply([]*models.Mock{w, w2}, []*models.Mock{w, w2}, liveReq("POST", gqlReview))
		if got.held != 0 || len(got.candidates) != 2 {
			t.Fatalf("writes are never fenced: %+v", got)
		}
	})
	t.Run("a spec-shared copy is never held", func(t *testing.T) {
		var f fence
		shared := fenceMock("r-shared", gqlGetBranches, 15, scope, false)
		shared.TestModeInfo.SharedInSpec = true
		got := f.apply([]*models.Mock{w, before, shared}, []*models.Mock{before, shared}, liveReq("POST", gqlGetBranches))
		if got.held != 0 {
			t.Fatalf("a spec-shared copy answers identically at every moment: %+v", got)
		}
	})
	t.Run("no staged test, no fence", func(t *testing.T) {
		var f fence
		suiteW := fenceMock("sw", gqlReview, 10, "", false)
		sb := fenceMock("s-before", gqlGetBranches, 5, "", false)
		sa := fenceMock("s-after", gqlGetBranches, 15, "", false)
		got := f.apply([]*models.Mock{suiteW, sb, sa}, []*models.Mock{sb, sa}, liveReq("POST", gqlGetBranches))
		if got.held != 0 || got.scope != "" {
			t.Fatalf("a suite/setup pool has no test sequence to fence: %+v", got)
		}
	})
	t.Run("repeat-last spans the spec, never another spec", func(t *testing.T) {
		var f fence
		in := liveReq("POST", gqlGetBranches)
		f.apply([]*models.Mock{w, before, after}, []*models.Mock{before, after}, in)
		f.served(scope, in, before)
		otherSpec := fenceMock("w-x", gqlReview, 10, "other-spec > t", true)
		afterX := fenceMock("r-after-x", gqlGetBranches, 15, "other-spec > t", false)
		if got := f.apply([]*models.Mock{otherSpec, afterX}, []*models.Mock{afterX}, in); got.repeat != nil {
			t.Fatal("a copy served in one spec must never answer in another")
		}
	})
}

func TestIsFencedRead(t *testing.T) {
	for _, c := range []struct {
		method, body string
		want         bool
	}{
		{"GET", "", true},
		{"POST", gqlGetBranches, true},
		{"POST", gqlReview, false},
		{"POST", `{"url":"http://x"}`, false},
		{"POST", "", false},
	} {
		if got := isFencedRead(liveReq(c.method, c.body)); got != c.want {
			t.Errorf("%s %.30q: got %v want %v", c.method, c.body, got, c.want)
		}
	}
}

func names(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// request-config:20: the copy recorded for after a page reload was used up by
// an extra call the reload aborted. With no write since the last copy, the last
// copy is still the true answer and repeats; once a write is served it misses
// as before.
func TestFenceRepeatsWhenUsedUpOnlyIfNoWriteSince(t *testing.T) {
	const scope = "spec > rc"
	read := fenceMock("r-1", gqlGetBranches, 5, scope, false)
	in := liveReq("POST", gqlGetBranches)
	var f fence

	got := f.apply([]*models.Mock{read}, []*models.Mock{read}, in)
	if got.repeat != nil || len(got.candidates) != 1 {
		t.Fatalf("a usable copy must be served normally: %+v", got)
	}
	f.served(got.scope, in, read)

	// read is consumed; nothing of this request is left, but the test is still
	// staged (every mock staged with it carries its scope).
	other := fenceMock("suite-other", gqlMyApps, 1, scope, false)
	got = f.apply([]*models.Mock{other}, nil, in)
	if got.repeat != read || got.why != "used-up" {
		t.Fatalf("repeat=%v why=%q: used up with no write since must repeat the last copy", got.repeat, got.why)
	}

	// A write is served: the state may have changed, so no repeat.
	w := fenceMock("w", gqlReview, 9, scope, true)
	f.served(scope, liveReq("POST", gqlReview), w)
	got = f.apply([]*models.Mock{other}, nil, in)
	if got.repeat != nil {
		t.Fatal("after a write the old answer may be stale; it must not repeat")
	}
}

// filtering.spec.ts: a suite copy captured after the test's own recording
// belongs to a later beforeAll. The test keeps it untouched while it has its
// own copy, and repeats its own last copy instead of using the later one up.
func TestFenceHoldsBackLaterSuiteCopies(t *testing.T) {
	const scope = "spec > t"
	own := fenceMock("own", gqlGetBranches, 5, scope, false)
	later := fenceMock("suite-later", gqlGetBranches, 50, "", false)
	later.TestModeInfo.FenceLater = true
	in := liveReq("POST", gqlGetBranches)
	var f fence

	got := f.apply([]*models.Mock{own, later}, []*models.Mock{own, later}, in)
	if got.held != 1 || got.later != 1 || len(got.candidates) != 1 || got.candidates[0] != own {
		t.Fatalf("held=%d later=%d candidates=%v: the later suite copy must be held back", got.held, got.later, names(got.candidates))
	}
	f.served(scope, in, own)

	keep := fenceMock("own-other", gqlMyApps, 1, scope, false)
	got = f.apply([]*models.Mock{keep, later}, []*models.Mock{later}, in)
	if got.repeat != own || got.why != "held" {
		t.Fatalf("repeat=%v why=%q: with only the later copy left, the test's own last copy must answer", got.repeat, got.why)
	}

	// Never served this request in the test: nothing to repeat, so the later
	// copy answers, but is not used up; its own hook still gets it.
	var g fence
	got = g.apply([]*models.Mock{keep, later}, []*models.Mock{later}, in)
	if got.repeat != later || !got.peek {
		t.Fatalf("with nothing to repeat, the later copy answers without being used up: %+v", got)
	}
}

const gqlCreateApp = `{"query":"mutation($n: String!) { createATGApp(name: $n) { id } }","variables":{"n":"a"}}`

func laterSetup(name, body string, at int, scope string) *models.Mock {
	m := fenceMock(name, body, at, scope, false)
	m.TestModeInfo.FenceLater = true
	return m
}

// filtering.spec.ts: in the gap after "status code" the page refetches the
// suite list, and the only copy left belongs to the next describe's beforeAll.
// It answers, but is not used up, so that beforeAll still gets it.
func TestFenceAnswersFromALaterSetupCopyWithoutUsingItUp(t *testing.T) {
	const scope = "setup:f.spec.ts#1"
	used := fenceMock("hook1-list", gqlMyApps, 5, scope, false) // same request, served earlier and consumed
	_ = used
	hook2 := laterSetup("hook2-list", gqlMyApps, 25, scope)
	other := fenceMock("other", gqlGetBranches, 6, scope, false)
	in := liveReq("POST", gqlMyApps)
	var f fence

	got := f.apply([]*models.Mock{other, hook2}, []*models.Mock{hook2}, in)
	if got.repeat != hook2 || !got.peek || got.why != "later-setup" {
		t.Fatalf("repeat=%v peek=%v why=%q: the later hook's copy must answer without being used up", got.repeat, got.peek, got.why)
	}
}

// A hook's extra call must not use up a later hook's copy while its own
// usable copy is still there.
func TestFenceKeepsALaterHooksCopyOutWhileAUsableOneExists(t *testing.T) {
	const scope = "setup:f.spec.ts#0"
	mine := fenceMock("hook1-list", gqlMyApps, 5, scope, false)
	hook2 := laterSetup("hook2-list", gqlMyApps, 25, scope)
	var f fence
	got := f.apply([]*models.Mock{mine, hook2}, []*models.Mock{mine, hook2}, liveReq("POST", gqlMyApps))
	if got.repeat != nil || got.held != 1 || len(got.candidates) != 1 || got.candidates[0] != mine {
		t.Fatalf("got %+v: only this hook's copy may be matched", got)
	}
}

// Writes too: a re-run beforeAll's create must not use up the next
// describe's create.
func TestFenceHoldsALaterHooksWrite(t *testing.T) {
	const scope = "setup:d.spec.ts#3"
	next := laterSetup("hook-next-create", gqlCreateApp, 40, scope)
	in := liveReq("POST", gqlCreateApp)
	var f fence
	got := f.apply([]*models.Mock{next}, []*models.Mock{next}, in)
	if got.repeat != next || !got.peek {
		t.Fatalf("got %+v: the later hook's create answers but is not used up", got)
	}
	mine := fenceMock("hook-mine-create", gqlCreateApp, 10, scope, false)
	got = f.apply([]*models.Mock{mine, next}, []*models.Mock{mine, next}, in)
	if got.repeat != nil || len(got.candidates) != 1 || got.candidates[0] != mine {
		t.Fatalf("got %+v: with its own create still there, the hook must use that", got)
	}
}

// Repeat-last, now also inside hooks and gaps, is preferred over answering
// from a later hook's copy: the last answer is this period's own.
func TestFenceRepeatsInASetupPeriodBeforeAnsweringFromALaterCopy(t *testing.T) {
	const scope = "setup:f.spec.ts#2"
	mine := fenceMock("hook-list", gqlMyApps, 5, scope, false)
	later := laterSetup("next-hook-list", gqlMyApps, 25, scope)
	in := liveReq("POST", gqlMyApps)
	var f fence
	got := f.apply([]*models.Mock{mine, later}, []*models.Mock{mine, later}, in)
	f.served(got.scope, in, mine)
	got = f.apply([]*models.Mock{later}, []*models.Mock{later}, in)
	if got.repeat != mine || got.peek || got.why != "held" {
		t.Fatalf("repeat=%v peek=%v why=%q: the hook's own last answer must repeat", got.repeat, got.peek, got.why)
	}
}

// A test's own copy held behind its pending write is never answered early:
// the request matches as before (it may use that copy), so no new miss and no
// new behaviour for the existing fence.
func TestFenceDoesNotPeekATestsOwnFutureCopy(t *testing.T) {
	const scope = "spec > t"
	w := fenceMock("w", gqlReview, 10, scope, true)
	after := fenceMock("r-after", gqlGetBranches, 15, scope, false)
	var f fence
	got := f.apply([]*models.Mock{w, after}, []*models.Mock{after}, liveReq("POST", gqlGetBranches))
	if got.repeat != nil || got.peek || len(got.candidates) != 1 {
		t.Fatalf("got %+v: an own future copy falls through as before", got)
	}
}

// filtering.spec.ts: test 1106 resets its filters as it ends; the refetch lands
// in the gap, where every copy of the request has rightly been used by its own
// hook. The answer the spec's beforeAll got for the same exact request, with no
// write since, is the true one.
func TestFenceRepeatsTheSpecsLastAnswerInAGap(t *testing.T) {
	in := liveReq("POST", gqlMyApps)
	hook := fenceMock("hook3-list", gqlMyApps, 35, "setup:f.spec.ts#3", false)
	var f fence
	got := f.apply([]*models.Mock{hook}, []*models.Mock{hook}, in)
	f.served(got.scope, in, hook)

	// Test 1106 runs (no write), then the gap after it: nothing of this request left.
	t1106 := fenceMock("t1106-own", gqlGetBranches, 40, "f.spec.ts > d3 > t1106", false)
	f.apply([]*models.Mock{t1106}, nil, liveReq("POST", gqlGetBranches))
	gapOther := fenceMock("gap-other", gqlGetBranches, 1, "setup:f.spec.ts#4", false)
	got = f.apply([]*models.Mock{gapOther}, nil, in)
	if got.repeat != hook || got.why != "used-up" {
		t.Fatalf("repeat=%v why=%q: the beforeAll's answer must repeat in the gap", got.repeat, got.why)
	}

	// A login (a REST write) served anywhere in the spec stops it.
	login := &models.Mock{Name: "login", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "POST", URL: "/auth/login"}}}
	f.served("f.spec.ts > d3 > t1167", liveReq("POST", `{"code":"x"}`), login)
	if got = f.apply([]*models.Mock{gapOther}, nil, in); got.repeat != nil {
		t.Fatal("after a write the old answer may be stale; it must not repeat")
	}
}

func TestSpecOfFenceScope(t *testing.T) {
	for in, want := range map[string]string{
		"setup:a/b.spec.ts#3":        "a/b.spec.ts",
		"a/b.spec.ts > d > t":        "a/b.spec.ts",
		"a/b.spec.ts > d > t#with #": "a/b.spec.ts",
	} {
		if got := specOfFenceScope(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

const (
	gqlGetAllGroups = `{"operationName":"GetAllGroups","query":"query GetAllGroups { getAllGroups { id name } }"}`
	gqlCreateGroup  = `{"operationName":"CreateGroup","query":"mutation CreateGroup($n: String!) { createGroup(name: $n) { id } }","variables":{"n":"g"}}`
)

func withCookie(m *models.Mock, c string) *models.Mock {
	m.Spec.HTTPReq.Header = map[string]string{"Cookie": c}
	return m
}

// rbac-groups:87: the record read the list twice before CreateGroup, the replay
// once. After the create is served, the leftover pre-create copy must not
// answer while a post-create copy is there.
func TestFenceSkipsCopiesRecordedBeforeAServedOwnWrite(t *testing.T) {
	const scope = "spec > groups"
	pre1 := fenceMock("pre1", gqlGetAllGroups, 1, scope, false)
	pre2 := fenceMock("pre2", gqlGetAllGroups, 2, scope, false)
	w := fenceMock("create", gqlCreateGroup, 5, scope, true)
	post := fenceMock("post", gqlGetAllGroups, 6, scope, false)
	in := liveReq("POST", gqlGetAllGroups)
	var f fence

	got := f.apply([]*models.Mock{pre1, pre2, w, post}, []*models.Mock{pre1, pre2, post}, in)
	f.served(got.scope, in, pre1)
	f.served(got.scope, liveReq("POST", gqlCreateGroup), w)

	got = f.apply([]*models.Mock{pre2, post}, []*models.Mock{pre2, post}, in)
	if got.stale != 1 || len(got.candidates) != 1 || got.candidates[0] != post {
		t.Fatalf("stale=%d candidates=%v: the pre-create copy must be skipped once the create is served", got.stale, names(got.candidates))
	}
}

// Nothing newer to answer with: the older copy still answers, never a miss.
func TestFenceKeepsAnOlderCopyWhenNoNewerOneExists(t *testing.T) {
	const scope = "spec > t"
	pre := fenceMock("pre", gqlGetAllGroups, 1, scope, false)
	w := fenceMock("create", gqlCreateGroup, 5, scope, true)
	var f fence
	f.apply([]*models.Mock{pre, w}, []*models.Mock{pre}, liveReq("POST", gqlGetAllGroups))
	f.served(scope, liveReq("POST", gqlCreateGroup), w)
	got := f.apply([]*models.Mock{pre}, []*models.Mock{pre}, liveReq("POST", gqlGetAllGroups))
	if got.stale != 0 || len(got.candidates) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// load-reports: the holder's copies, a seed write, then the non-holder's copies.
// The newer copy is another user's answer and must not replace the holder's.
func TestFenceDoesNotSkipToAnotherUsersCopy(t *testing.T) {
	const scope = "spec > two users"
	holder := withCookie(fenceMock("holder", gqlGetAllGroups, 1, scope, false), "jwt_token=holder")
	w := fenceMock("seed", gqlCreateGroup, 5, scope, true)
	other := withCookie(fenceMock("other", gqlGetAllGroups, 6, scope, false), "jwt_token=other")
	var f fence
	f.apply([]*models.Mock{holder, w, other}, []*models.Mock{holder, other}, liveReq("POST", gqlGetAllGroups))
	f.served(scope, liveReq("POST", gqlCreateGroup), w)
	got := f.apply([]*models.Mock{holder, other}, []*models.Mock{holder, other}, liveReq("POST", gqlGetAllGroups))
	if got.stale != 0 || len(got.candidates) != 2 {
		t.Fatalf("stale=%d candidates=%v: a different user's newer copy must not displace this one", got.stale, names(got.candidates))
	}
}

// A write served for ANOTHER test (or a suite hook) does not move this test on.
func TestFenceIgnoresAnotherScopesWrite(t *testing.T) {
	pre := fenceMock("pre", gqlGetAllGroups, 1, "spec > b", false)
	post := fenceMock("post", gqlGetAllGroups, 6, "spec > b", false)
	w := fenceMock("create", gqlCreateGroup, 5, "spec > a", true)
	var f fence
	f.apply([]*models.Mock{w}, []*models.Mock{w}, liveReq("POST", gqlCreateGroup))
	f.served("spec > a", liveReq("POST", gqlCreateGroup), w)
	got := f.apply([]*models.Mock{pre, post}, []*models.Mock{pre, post}, liveReq("POST", gqlGetAllGroups))
	if got.stale != 0 || len(got.candidates) != 2 {
		t.Fatalf("got stale=%d %v", got.stale, names(got.candidates))
	}
}

// rbac-groups:87 as recorded: the same admin before and after the create, but
// the later copies also carry analytics cookies. Still the same caller.
func TestFenceTreatsAnalyticsCookiesAsTheSameCaller(t *testing.T) {
	const scope = "spec > groups"
	pre := withCookie(fenceMock("pre", gqlGetAllGroups, 1, scope, false), "jwt_token_staging=a; proxy_csp=x%253A")
	w := fenceMock("create", gqlCreateGroup, 5, scope, true)
	post := withCookie(fenceMock("post", gqlGetAllGroups, 6, scope, false), "jwt_token_staging=a; proxy_csp=x%3A; _ga=GA1; _fbp=fb.0")
	var f fence
	f.apply([]*models.Mock{pre, w, post}, []*models.Mock{pre, post}, liveReq("POST", gqlGetAllGroups))
	f.served(scope, liveReq("POST", gqlCreateGroup), w)
	got := f.apply([]*models.Mock{pre, post}, []*models.Mock{pre, post}, liveReq("POST", gqlGetAllGroups))
	if got.stale != 1 || len(got.candidates) != 1 || got.candidates[0] != post {
		t.Fatalf("stale=%d candidates=%v", got.stale, names(got.candidates))
	}
}
