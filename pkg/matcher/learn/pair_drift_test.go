package learn

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// testJWT is a JWT whose payload holds claims, as a test's fake issuer mints
// one: its header and signature are fixed bytes, which no pairing reads.
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + ".c2ln"
}

// tokenReq is a POST /oauth/token request whose form body (as keploy keeps
// it, decoded) is body.
func tokenReq(body string) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://api/oauth/token", Body: body,
		Header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}
}

// TestARequestHoldingAValueTheTestDriftedPairsWithTheRecordingThatHeldIt:
// where a test sends a call more than once and the requests differ only by
// a value the test's earlier exchanges drifted (an id the application
// minted, a name the test sent before), each word of the run's request that
// holds such a value whole, or as a part of the word between its ':', is
// read as its recording held it, and the request pairs with the recording
// that holds the word so read; a JWT's claims are words too, its claim's
// value read so. With no drift known the same requests pair by order.
func TestARequestHoldingAValueTheTestDriftedPairsWithTheRecordingThatHeldIt(t *testing.T) {
	accounts := func() Drifts {
		d := Drifts{}
		d.Add("acct-L1", "acct-R1")
		d.Add("acct-L2", "acct-R2")
		return d
	}
	tok := func(c map[string]any) string { return testJWT(t, c) }
	for _, c := range []struct {
		name   string
		recs   []string
		live   string
		drifts func() Drifts
		want   int
	}{
		{name: "a drifted id whole tells",
			recs: []string{"account=acct-R1&grant=x", "account=acct-R2&grant=x"},
			live: "account=acct-L2&grant=x", drifts: accounts, want: 1},
		{name: "with no drift known: by order",
			recs: []string{"account=acct-R1&grant=x", "account=acct-R2&grant=x"},
			live: "account=acct-L2&grant=x", want: 0},
		{name: "a drifted part of a word between its ':' tells",
			recs: []string{"audience=keploy:service-account:acct-R1", "audience=keploy:service-account:acct-R2"},
			live: "audience=keploy:service-account:acct-L2", drifts: accounts, want: 1},
		{name: "the word's other parts stay as sent",
			recs: []string{"audience=keploy:service-account:acct-R1", "audience=keploy:service-account:acct-R2"},
			live: "audience=other:service-account:acct-L2", drifts: accounts, want: 0},
		{name: "a value that stood for two recorded ones reads as either",
			recs: []string{"account=acct-R1", "account=acct-R2"},
			live: "account=acct-L9", drifts: func() Drifts {
				d := Drifts{}
				d.Add("acct-L9", "acct-R5")
				d.Add("acct-L9", "acct-R2")
				return d
			}, want: 1},
		{name: "a JWT's claim tells, its token signed anew",
			recs: []string{"subject_token=" + tok(map[string]any{"ref": "refs/heads/main", "iat": 100}), "subject_token=" + tok(map[string]any{"ref": "refs/heads/feature", "iat": 101})},
			live: "subject_token=" + tok(map[string]any{"ref": "refs/heads/feature", "iat": 999}), want: 1},
		{name: "a JWT's claim whose value drifted whole tells",
			recs: []string{"subject_token=" + tok(map[string]any{"repository": "octo/repo-R1"}), "subject_token=" + tok(map[string]any{"repository": "octo/repo-R2"})},
			live: "subject_token=" + tok(map[string]any{"repository": "octo/repo-L2"}), drifts: func() Drifts {
				d := Drifts{}
				d.Add("octo/repo-L1", "octo/repo-R1")
				d.Add("octo/repo-L2", "octo/repo-R2")
				return d
			}, want: 1},
		{name: "and one that drifted as a part between its ':'",
			recs: []string{"subject_token=" + tok(map[string]any{"sub": "repo:octo/repo-R1:ref:main"}), "subject_token=" + tok(map[string]any{"sub": "repo:octo/repo-R2:ref:main"})},
			live: "subject_token=" + tok(map[string]any{"sub": "repo:octo/repo-L2:ref:main"}), drifts: func() Drifts {
				d := Drifts{}
				d.Add("octo/repo-L2", "octo/repo-R2")
				return d
			}, want: 1},
		{name: "a claim that is no string reads as its JSON",
			recs: []string{"subject_token=" + tok(map[string]any{"repository_id": 1296269}), "subject_token=" + tok(map[string]any{"repository_id": 666})},
			live: "subject_token=" + tok(map[string]any{"repository_id": 666}), want: 1},
		{name: "and drifts so",
			recs: []string{"subject_token=" + tok(map[string]any{"repository_id": 1296269}), "subject_token=" + tok(map[string]any{"repository_id": 666})},
			live: "subject_token=" + tok(map[string]any{"repository_id": 777}), drifts: func() Drifts {
				d := Drifts{}
				d.Add("777", "666")
				return d
			}, want: 1},
		{name: "an empty claim is no value that drifted",
			recs: []string{"subject_token=" + tok(map[string]any{"x": "R1"}), "subject_token=" + tok(map[string]any{"x": "R2"})},
			live: "subject_token=" + tok(map[string]any{"x": ""}), drifts: func() Drifts {
				d := Drifts{}
				d.Add("", "R2")
				return d
			}, want: 0},
		{name: "a token's word that is no JWT has no claims",
			recs: []string{"subject_token=e30.eyJyZWYiOiJhIn0.x.y", "subject_token=e30.eyJyZWYiOiJiIn0.x.y"},
			live: "subject_token=e30.eyJyZWYiOiJiIn0.z.w", want: 0},
	} {
		var cases []ClientCase
		for i, r := range c.recs {
			cases = append(cases, ClientCase{Name: strconv.Itoa(i), Req: tokenReq(r)})
		}
		if got, ok := PairCase(cases, nil, tokenReq(c.live), true, c.drifts); got != c.want || !ok {
			t.Errorf("%s: paired with %d (%v); want %d", c.name, got, ok, c.want)
		}
	}

	// The drifts are read where more than one recording is left, and only
	// there; none given reads none.
	calls := 0
	count := func() Drifts { calls++; return accounts() }
	one := []ClientCase{{Name: "0", Req: tokenReq("account=acct-R1")}, {Name: "1", Req: tokenReq("account=acct-R2")}}
	if got, ok := PairCase(one, []bool{true, false}, tokenReq("account=acct-L1"), true, count); got != 1 || !ok || calls != 0 {
		t.Fatalf("one left: paired with %d (%v), the drifts read %d times; want 1, never read", got, ok, calls)
	}
	if got, _ := PairCase(one, nil, tokenReq("account=acct-L2"), true, count); got != 1 || calls != 1 {
		t.Fatalf("two left: paired with %d, the drifts read %d times; want 1, read once", got, calls)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("no drifts given: PairCase panicked: %v", r)
			}
		}()
		if got, _ := PairCase(one, nil, tokenReq("account=acct-L2"), true, nil); got != 0 {
			t.Fatalf("no drifts given: paired with %d; want 0, by order", got)
		}
	}()
}

// TestADriftedWordsFormsAreItsRecordedValuesWholeThenByItsParts: the forms
// of a word as its recording held it are each recorded value it stood for
// whole, then the word with each of its ':' parts that drifted read as a
// recorded value, every other part kept, each form once and none the word
// itself; at most maxDriftForms of them with the word itself, the first.
func TestADriftedWordsFormsAreItsRecordedValuesWholeThenByItsParts(t *testing.T) {
	d := Drifts{}
	d.Add("a:b", "a:c")
	d.Add("b", "c")
	d.Add("x", "X")
	d.Add("z", "Z1")
	d.Add("z", "Z2")
	d.Add("v", "w")
	for _, c := range []struct {
		w    string
		d    Drifts
		want []string
	}{
		{w: "v", d: d, want: []string{"w"}},
		{w: "a:b", d: d, want: []string{"a:c"}},
		{w: "x:y:z", d: d, want: []string{"x:y:Z1", "x:y:Z2", "X:y:z", "X:y:Z1", "X:y:Z2"}},
		{w: "q:y", d: d, want: nil},
		{w: "v", d: nil, want: nil},
	} {
		if got := driftForms(c.w, c.d); !reflect.DeepEqual(got, c.want) {
			t.Errorf("forms of %q: %q; want %q", c.w, got, c.want)
		}
	}
	many := Drifts{}
	parts := make([]string, 5)
	for i := range parts {
		parts[i] = "p" + strconv.Itoa(i)
		for k := 0; k < 3; k++ {
			many.Add(parts[i], parts[i]+"-r"+strconv.Itoa(k))
		}
	}
	if got := driftForms(strings.Join(parts, ":"), many); len(got) != maxDriftForms-1 {
		t.Fatalf("a word of 5 parts each of 3 recorded values made %d forms; want %d", len(got), maxDriftForms-1)
	}

	// A value is noted once for each recorded value it stood for, and one
	// that did not drift is none.
	n := Drifts{}
	n.Add("same", "same")
	n.Add("l", "r")
	n.Add("l", "r")
	if want := (Drifts{"l": {"r"}}); !reflect.DeepEqual(n, want) {
		t.Fatalf("drifts %v; want %v", n, want)
	}
}

// TestAMapRangedSetOfTokenExchangesPairsEachWithItsOwnRecording: a CI test's
// OIDC token exchanges, as the api-server's e2e test sends them: one that
// the account's policy grants, then six refusals ranged over a Go map (in
// any order, in its recording and in the run), then two more that the
// policy grants. Each request names its service account in its RFC 8693
// audience (keploy:service-account:<id>, an id the application minted, so
// one this run's earlier exchanges drifted) and asks by its token's claims
// (a repository named from the test's clock, sent to the application
// before); every token is signed anew. In every order each request pairs
// with its own recording. With no drift known, some order pairs one with
// another's.
func TestAMapRangedSetOfTokenExchangesPairsEachWithItsOwnRecording(t *testing.T) {
	type exchange struct {
		name     string
		account  string // "a" or "idle"
		repo     string // "" or "-other"
		repoID   any
		ref, aud string
		event    string
	}
	granted := exchange{name: "granted", account: "a", repoID: "1296269", ref: "refs/heads/main", aud: "http://api", event: "push"}
	refusals := []exchange{
		{name: "another repository", account: "a", repo: "-other", repoID: "1296269", ref: "refs/heads/main", aud: "http://api", event: "push"},
		{name: "another repository ID", account: "a", repoID: "666", ref: "refs/heads/main", aud: "http://api", event: "push"},
		{name: "another branch", account: "a", repoID: "1296269", ref: "refs/heads/feature", aud: "http://api", event: "push"},
		{name: "another audience", account: "a", repoID: "1296269", ref: "refs/heads/main", aud: "sts.amazonaws.com", event: "push"},
		{name: "an account that trusts no one", account: "idle", repoID: "1296269", ref: "refs/heads/main", aud: "http://api", event: "push"},
		{name: "an issue comment", account: "a", repoID: "1296269", ref: "refs/heads/main", aud: "http://api", event: "issue_comment"},
	}
	ids := map[string][2]string{"a": {"6af6423ff58abf56d81d3120", "7b07534006b9cb67e92e4231"}, "idle": {"6af6423ff58abf56d81d3131", "7b07534006b9cb67e92e4242"}}
	repos := [2]string{"repo-1794523711000000001", "repo-1794600000000000002"}
	body := func(e exchange, run int, iat int64) string {
		repo := "octo-org/" + repos[run] + e.repo
		claims := map[string]any{"aud": e.aud, "sub": "repo:" + repo + ":ref:refs/heads/main", "iat": iat, "nbf": iat, "exp": iat + 300,
			"repository": repo, "repository_id": e.repoID, "repository_owner": "octo-org", "repository_owner_id": "9919",
			"ref": e.ref, "ref_type": "branch", "event_name": e.event, "run_id": "1", "iss": "http://127.0.0.1:4" + strconv.Itoa(1000+run)}
		return "audience=keploy:service-account:" + ids[e.account][run] + "&grant_type=urn:ietf:params:oauth:grant-type:token-exchange&subject_token=" +
			testJWT(t, claims) + "&subject_token_type=urn:ietf:params:oauth:token-type:id_token"
	}
	drifts := func() Drifts {
		d := Drifts{}
		for _, id := range ids {
			d.Add(id[1], id[0])
		}
		d.Add("octo-org/"+repos[1], "octo-org/"+repos[0])
		d.Add("http://127.0.0.1:41001", "http://127.0.0.1:41000")
		return d
	}
	// sequence is the exchanges in the order the test sends them, the
	// refusals in order, by their index: 0 the first granted, 1-6 the
	// refusals, 7 and 8 the granted after them.
	sequence := func(order []int) []int {
		out := []int{0}
		for _, i := range order {
			out = append(out, 1+i)
		}
		return append(out, 7, 8)
	}
	all := append(append([]exchange{granted}, refusals...), granted, granted)
	var recReqs, liveReqs []*models.HTTPReq
	for k, e := range all {
		recReqs = append(recReqs, tokenReq(body(e, 0, int64(1794523711+k))))
		liveReqs = append(liveReqs, tokenReq(body(e, 1, int64(1794600000+k))))
	}
	var perms [][]int
	var permute func(prefix, rest []int)
	permute = func(prefix, rest []int) {
		if len(rest) == 0 {
			perms = append(perms, prefix)
			return
		}
		for i := range rest {
			next := append(append([]int(nil), rest[:i]...), rest[i+1:]...)
			permute(append(append([]int(nil), prefix...), rest[i]), next)
		}
	}
	permute(nil, []int{0, 1, 2, 3, 4, 5})
	mispaired := false
	for _, recOrder := range [][]int{perms[0], perms[len(perms)-1], perms[400]} {
		var cases []ClientCase
		for _, x := range sequence(recOrder) {
			cases = append(cases, ClientCase{Name: strconv.Itoa(x), Req: recReqs[x]})
		}
		for _, liveOrder := range perms {
			for _, withDrifts := range []bool{true, false} {
				if !withDrifts && mispaired {
					continue
				}
				used := make([]bool, len(cases))
				var of func() Drifts
				if withDrifts {
					of = drifts
				}
				for _, x := range sequence(liveOrder) {
					i, ok := PairCase(cases, used, liveReqs[x], true, of)
					if !ok {
						t.Fatalf("recorded %v, sent %v: %s paired with none", recOrder, liveOrder, all[x].name)
					}
					used[i] = true
					// The granted three are one request but for their
					// tokens' times: each pairs with the first left.
					own := cases[i].Name == strconv.Itoa(x) || all[x].name == "granted" && all[mustAtoi(t, cases[i].Name)].name == "granted"
					if withDrifts && !own {
						t.Fatalf("recorded %v, sent %v: %s (%d) paired with %s", recOrder, liveOrder, all[x].name, x, all[mustAtoi(t, cases[i].Name)].name)
					}
					mispaired = mispaired || !own
				}
			}
		}
	}
	if !mispaired {
		t.Fatal("with no drift known every order paired each with its own: the row tells nothing")
	}
}

// TestTheLearnStepPairsAClientRequestByWhatTheTestsEarlierExchangesDrifted:
// in record's full round a client test's request pairs with its recording
// by the values the test's earlier exchanges drifted, each kind of earlier
// exchange as the others: the run's (a start-up's call), the test's own
// calls, its requests paired before, and the values it gave keploy.
func TestTheLearnStepPairsAClientRequestByWhatTheTestsEarlierExchangesDrifted(t *testing.T) {
	at := time.Date(2026, 11, 13, 6, 1, 42, 0, time.UTC)
	bind := func(principal string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://api/bind", Body: `{"principal":"` + principal + `"}`, Header: map[string]string{"Content-Type": "application/json"}, Timestamp: at}
	}
	cases := []ClientCase{{Name: "post-bind-1", Req: bind("acct-R2")}, {Name: "post-token-1", Req: tokenReq("audience=keploy:service-account:acct-R1")}, {Name: "post-token-2", Req: tokenReq("audience=keploy:service-account:acct-R2")}}
	drifted := []Seen{{Side: Request, Leaf: Leaf{Path: "body.documents.0._id.$oid", Value: "acct-R2"}, Live: "acct-L2", Place: "body.documents.0._id.$oid"}}
	live := tokenReq("audience=keploy:service-account:acct-L2")
	placedAt := func(tr *Tracker) string {
		t.Helper()
		p := tr.Placed().Earlier
		var names []string
		for _, e := range p {
			if e.incoming {
				names = append(names, e.Mock)
			}
		}
		return strings.Join(names, ",")
	}
	for _, c := range []struct {
		name    string
		earlier func(tr *Tracker)
		want    string
	}{
		{name: "nothing drifted: by order", earlier: func(tr *Tracker) {}, want: "test case post-token-1"},
		{name: "the test's own call", earlier: func(tr *Tracker) {
			tr.Served(tr.Placed().Test, Earlier{Mock: "mock-1", At: at, Own: true, Seen: drifted})
		}, want: "test case post-token-2"},
		{name: "a value the test gave", earlier: func(tr *Tracker) {
			tr.GiveValue(Earlier{Mock: "given", Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "account", Value: "acct-R2"}, Live: "acct-L2"}}})
		}, want: "test case post-token-2"},
		{name: "a request of the test paired before", earlier: func(tr *Tracker) {
			if !tr.Incoming(bind("acct-L2"), "", at) {
				t.Fatal("the earlier request paired with none")
			}
		}, want: "test case post-bind-1,test case post-token-2"},
	} {
		tr := NewTracker()
		tr.Begin()
		tr.SetCases(cases)
		c.earlier(tr)
		if !tr.Incoming(live, "", at.Add(time.Second)) {
			t.Fatalf("%s: the request paired with none", c.name)
		}
		if got := placedAt(tr); got != c.want {
			t.Errorf("%s: placed at %s; want %s", c.name, got, c.want)
		}
	}

	// A start-up's call that sent a value the run made, before the set's
	// first test began, is the run's: its drift pairs a later test's request.
	tr := NewTracker()
	tr.Served(tr.Placed().Test, Earlier{Mock: "mock-0", At: at, Seen: drifted})
	tr.Begin()
	tr.SetCases(cases)
	if !tr.Incoming(live, "", at.Add(time.Second)) {
		t.Fatal("the request paired with none")
	}
	if got := placedAt(tr); got != "test case post-token-2" {
		t.Fatalf("after a start-up call that drifted: placed at %s; want test case post-token-2", got)
	}
	if !slices.ContainsFunc(tr.Placed().Earlier, func(e Earlier) bool { return e.Run }) {
		t.Fatal("the start-up's call is no place of the run's: the row tells nothing")
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
