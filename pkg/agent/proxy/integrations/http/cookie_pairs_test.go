package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// The learn step's refusal of a Cookie header names the pairs that differ,
// never a value: a jar holds a session token beside pairs the page sets
// itself, and "header.cookie changed" did not say which one the call sent
// otherwise.
func TestCookiePairsDiffer(t *testing.T) {
	cases := []struct {
		name, rec, live, want string
	}{
		{"a pair the page sets itself, not yet set in this run",
			"refresh_token=r1; jwt_token=j1; subscription=s1", "refresh_token=r1; jwt_token=j1",
			"pair subscription is not sent in this run"},
		{"a pair whose value differs",
			"jwt_token=SECRETRECORDED; theme=dark", "jwt_token=SECRETLIVE; theme=dark",
			"pair jwt_token holds another value"},
		{"a pair sent in this run only",
			"theme=dark", "theme=dark; added=SECRETLIVE",
			"pair added is sent in this run only"},
		{"each kind, the recorded order first",
			"a=1; b=2; c=3", "d=4; b=9; a=1",
			"pair b holds another value; pair c is not sent in this run; pair d is sent in this run only"},
		{"the same pairs in another order",
			"a=1; b=2", "b=2; a=1",
			"its pairs are sent in another order"},
		{"only the bytes between the pairs differ",
			"a=1;b=2 ", "a=1; b=2",
			""},
		{"a name sent twice whose values come in another order",
			"a=1; a=2", "a=2; a=1",
			"pair a holds another value"},
		{"a name sent twice, once in this run",
			"a=1; a=2", "a=1",
			"pair a holds another value"},
		{"no Cookie header in this run",
			"a=1; b=2", "",
			"pair a is not sent in this run; pair b is not sent in this run"},
		{"a part with no '=' is no pair",
			"a=1; flag", "a=1",
			""},
		{"a name keploy quotes",
			`x"y=1`, "",
			`pair "x\"y" is not sent in this run`},
		{"a name with a space",
			"x y=1", "",
			`pair "x y" is not sent in this run`},
		{"a name with a control byte",
			"a\x7fb=1", "",
			`pair "a\x7fb" is not sent in this run`},
		{"a name of letters beyond ASCII",
			"\u00e9t\u00e9=1", "",
			"pair \"\u00e9t\u00e9\" is not sent in this run"},
		{"an empty name",
			"=1; a=2", "a=2",
			`pair "" is not sent in this run`},
		{"more pairs than a refusal names",
			"a=1; b=1; c=1; d=1; e=1; f=1; g=1", "",
			"pair a is not sent in this run; pair b is not sent in this run; pair c is not sent in this run; pair d is not sent in this run; pair e is not sent in this run; 2 more pairs differ"},
		{"exactly as many pairs as a refusal names",
			"a=1; b=1; c=1; d=1; e=1", "",
			"pair a is not sent in this run; pair b is not sent in this run; pair c is not sent in this run; pair d is not sent in this run; pair e is not sent in this run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cookiePairsDiffer(c.rec, c.live)
			if got != c.want {
				t.Fatalf("cookiePairsDiffer(%q, %q) = %q, want %q", c.rec, c.live, got, c.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Fatalf("the text names a value: %q", got)
			}
		})
	}
}

func TestWithCookiePairs(t *testing.T) {
	cookie := func(v string) learn.Leaf { return learn.Leaf{Path: "header.cookie", Value: v} }
	cases := []struct {
		name      string
		why       string
		rec, live learn.Leaf
		want      string
	}{
		{"named after the leaf's path, once",
			"header.cookie changed, and header.cookie is held", cookie("a=1; b=2"), cookie("a=1"),
			"header.cookie (pair b is not sent in this run) changed, and header.cookie is held"},
		{"a refusal that does not start with the path",
			"the test ended before header.cookie was learned", cookie("a=1"), cookie("a=2"),
			"the test ended before header.cookie (pair a holds another value) was learned"},
		{"a refusal that does not name the path",
			"something else", cookie("a=1"), cookie(""),
			"something else (header.cookie: pair a is not sent in this run)"},
		{"no pair differs",
			"header.cookie changed", cookie("a=1;b=2"), cookie("a=1; b=2"),
			"header.cookie changed"},
		{"another header",
			"header.x-cookie changed", learn.Leaf{Path: "header.x-cookie", Value: "a=1"}, learn.Leaf{Path: "header.x-cookie", Value: "a=2"},
			"header.x-cookie changed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withCookiePairs(c.why, c.rec, c.live); got != c.want {
				t.Fatalf("withCookiePairs = %q, want %q", got, c.want)
			}
		})
	}
}

// learnPlan's refusal of a Cookie header names the pair that differs; a
// refusal of a leaf after it names that leaf alone.
func TestLearnPlanNamesCookiePairs(t *testing.T) {
	rec := learn.Shape{Fixed: map[string]string{"body": ""}}
	m := &models.Mock{Name: "mock-18"}
	cookieRec := learn.Leaf{Path: "header.cookie", Noise: "header.cookie", Type: learn.String, Value: "jwt_token=SECRETJWT; subscription=s1"}
	cookieLive := learn.Leaf{Path: "header.cookie", Noise: "header.cookie", Type: learn.String, Value: "jwt_token=SECRETJWT"}
	t.Run("the Cookie header refused", func(t *testing.T) {
		learned := []learn.Learned{{Kind: learn.KindUnaccounted, Recorded: cookieRec, Live: cookieLive, Why: "keploy cannot tell"}}
		_, why := learnPlan(nil, 1, m, rec, true, learned, nil, nil)
		want := "header.cookie (pair subscription is not sent in this run) changed, and keploy cannot tell"
		if why != want {
			t.Fatalf("learnPlan refusal = %q, want %q", why, want)
		}
		if strings.Contains(why, "SECRET") || strings.Contains(why, "s1") {
			t.Fatalf("the refusal names a value: %q", why)
		}
	})
	t.Run("a leaf after a learned Cookie header", func(t *testing.T) {
		other := learn.Leaf{Path: "header.x-trace", Noise: "header.x-trace", Type: learn.String, Value: "t1"}
		otherLive := other
		otherLive.Value = "t2"
		learned := []learn.Learned{
			{Kind: learn.KindNoise, Recorded: cookieRec, Live: cookieLive},
			{Kind: learn.KindUnaccounted, Recorded: other, Live: otherLive, Why: "keploy cannot tell"},
		}
		_, why := learnPlan(nil, 1, m, rec, true, learned, nil, nil)
		if want := "header.x-trace changed, and keploy cannot tell"; why != want {
			t.Fatalf("learnPlan refusal = %q, want %q", why, want)
		}
	})
	t.Run("a Cookie header learned as noise is no refusal", func(t *testing.T) {
		learned := []learn.Learned{{Kind: learn.KindNoise, Recorded: cookieRec, Live: cookieLive}}
		p, why := learnPlan(nil, 1, m, rec, true, learned, nil, nil)
		if why != "" {
			t.Fatalf("learnPlan refused: %q", why)
		}
		if _, ok := p.noise["header.cookie"]; !ok {
			t.Fatalf("header.cookie is not learned as noise: %v", p.noise)
		}
	})
}

// pairsGone is the pairs a Cookie header lost and nothing else: every other
// pair sent once, in the recorded order, with its recorded value, and no
// pair more.
func TestPairsGone(t *testing.T) {
	for _, c := range []struct {
		name, rec, live string
		gone            []string
	}{
		{"one pair gone", "session=s1; sub=pro", "session=s1", []string{"sub"}},
		{"a middle pair gone", "a=1; b=2; c=3", "a=1; c=3", []string{"b"}},
		{"every pair gone", "a=1; b=2", "", []string{"a", "b"}},
		{"blanks between pairs", "a=1;b=2", "a=1", []string{"b"}},
		{"a name sent twice kept whole", "a=1; a=2; b=2", "a=1; a=2", []string{"b"}},
		{"none gone", "session=s1; sub=pro", "session=s1; sub=pro", nil},
		{"none gone, another order", "a=1; b=2", "b=2; a=1", nil},
		{"the rest in another order", "a=1; b=2; c=3", "c=3; a=1", nil},
		{"another pair's value changed", "a=1; b=2", "a=2", nil},
		{"a pair sent in this run only", "a=1; b=2", "a=1; x=9", nil},
		{"the gone pair named twice", "a=1; b=2; b=3", "a=1", nil},
		{"a name sent twice lost a value", "a=1; a=2; b=2", "a=1", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			gone, ok := pairsGone(c.rec, c.live)
			if ok != (c.gone != nil) || !reflect.DeepEqual(gone, c.gone) {
				t.Fatalf("pairsGone(%q, %q) = %v, %v; want %v", c.rec, c.live, gone, ok, c.gone)
			}
		})
	}
}

// A Cookie header without pairs learned as the page's is as recorded, and
// only where every pair gone was learned so.
func TestGoneAsLearned(t *testing.T) {
	sub := map[string][]string{cookieSetByPage + "sub": {}}
	for _, c := range []struct {
		name      string
		learned   map[string][]string
		rec, live string
		want      bool
	}{
		{"the page's pair gone", sub, "session=s1; sub=pro", "session=s1", true},
		{"another pair gone", sub, "session=s1; sub=pro", "sub=pro", false},
		{"two gone, one learned", sub, "session=s1; sub=pro; a=1", "session=s1", false},
		{"none gone", sub, "session=s1; sub=pro", "session=s1; sub=pro", false},
		{"the whole header learned", map[string][]string{cookieLeaf: {}}, "session=s1; sub=pro", "session=s1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := goneAsLearned(c.learned, c.rec, c.live); got != c.want {
				t.Fatalf("goneAsLearned = %v, want %v", got, c.want)
			}
		})
	}
}

// pageCall is a GET /v1/plan the test recorded at an offset from the
// learned call, sent with the Cookie header cookie (none where "") and
// answered with the Set-Cookie header set (none where "").
func pageCall(name string, at time.Duration, cookie, set string) *models.Mock {
	h := map[string]string{}
	if cookie != "" {
		h["Cookie"] = cookie
	}
	m := learnRecorded(name, -time.Hour+300*time.Millisecond+at, "GET", "/v1/plan", "", h, `{"plan":"pro"}`)
	if set != "" {
		m.Spec.HTTPResp.Header["Set-Cookie"] = set
	}
	return m
}

// learnPageCookie runs the learn step on a POST /v1/apps recorded with the
// Cookie header rec after the test's earlier calls, sent in this run with
// live: what it learned onto the mock, or why it refused the call.
func learnPageCookie(t *testing.T, earlier []*models.Mock, rec, live string) (map[string][]string, string) {
	t.Helper()
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-9", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", `{"name":"a"}`, map[string]string{"Cookie": rec}, `{"id":7}`)
	db := &learnDb{perTest: append(append([]*models.Mock(nil), earlier...), m)}
	served, miss, err := h.learnMatch(context.Background(), livePageCall(live), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		return nil, miss.res.Why
	}
	if served == nil || len(db.consumed) != 1 {
		t.Fatalf("served %v, consumed %d", served, len(db.consumed))
	}
	return db.consumed[0].Spec.ReqBodyNoise, ""
}

func livePageCall(cookie string) *req {
	return live("POST", "/v1/apps", `{"name":"a"}`, map[string]string{"Cookie": cookie})
}

// A call that goes without a Cookie pair the page set while its test ran
// (the test began without it: an earlier call to the same host went
// without it; and no answer before the call set it) learns that pair as
// the page's (cookieSetByPage), not the whole header as noise. A pair the
// test began with, one a service set, or a header that changed otherwise is
// learned as before (nil: the whole header as noise, or the call refused
// naming the pairs that differ), never as the page's.
func TestLearnMatchLearnsAPairThePageSets(t *testing.T) {
	const rec = "session=s1; sub=pro"
	page := map[string][]string{cookieSetByPage + "sub": {}}
	var whole map[string][]string
	other := pageCall("mock-1", -200*time.Millisecond, "session=s1", "")
	other.Spec.HTTPReq.URL = "http://other.test/v1/plan"
	other.Spec.HTTPReq.Header["Host"] = "other.test"
	unread := pageCall("mock-1", -200*time.Millisecond, "session=s1", "")
	unread.Spec.HTTPResp = nil
	setElsewhere := pageCall("mock-2", -100*time.Millisecond, "", "sub=pro; Path=/")
	setElsewhere.Spec.HTTPReq.URL = "http://other.test/v1/plan"
	setElsewhere.Spec.HTTPReq.Header["Host"] = "other.test"
	for _, c := range []struct {
		name    string
		earlier []*models.Mock
		live    string
		want    map[string][]string
	}{
		{"set by the page", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "")}, "session=s1", page},
		{"set by the page, the first call sent no Cookie", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "", "")}, "session=s1", page},
		{"no earlier call", nil, "session=s1", whole},
		{"the test began with it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, rec, "")}, "session=s1", whole},
		{"only a call to another host went without it", []*models.Mock{other}, "session=s1", whole},
		{"the call without it was recorded later", []*models.Mock{pageCall("mock-1", 200*time.Millisecond, "session=s1", "")}, "session=s1", whole},
		{"a service set it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "sub=pro; Path=/")}, "session=s1", whole},
		{"a service of another host set it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), setElsewhere}, "session=s1", whole},
		{"an earlier answer keploy cannot read", []*models.Mock{unread}, "session=s1", whole},
		{"the session changed too", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "")}, "session=s2", whole},
		{"a pair sent in this run only", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "")}, "session=s1; sub=pro; x=1", whole},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, why := learnPageCookie(t, c.earlier, rec, c.live)
			if c.want != nil {
				if why != "" || !reflect.DeepEqual(got, c.want) {
					t.Fatalf("learned %v (refused: %q), want %v", got, why, c.want)
				}
				return
			}
			if _, page := got[cookieSetByPage+"sub"]; page || why != "" && !strings.Contains(why, "header.cookie (pair") || why == "" && !reflect.DeepEqual(got, map[string][]string{cookieLeaf: {}}) {
				t.Fatalf("learned %v (refused: %q), want the whole header as noise or a refusal naming its pairs", got, why)
			}
		})
	}
}

// A replay serves a call that goes without a pair its mock learned as the
// page's, and compares every other pair, the page's own value where it is
// sent, and a header keploy.yml's patterns guard as before.
func TestStrictServesACallWithoutAPairThePageSets(t *testing.T) {
	for _, c := range []struct {
		name    string
		learned map[string][]string
		noise   map[string][]string
		live    string
		served  bool
	}{
		{"the page's pair gone", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1", true},
		{"as recorded", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1; sub=pro", true},
		{"the page's pair with another value", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1; sub=free", false},
		{"the session gone", map[string][]string{cookieSetByPage + "sub": {}}, nil, "sub=pro", false},
		{"the session changed", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s2", false},
		{"nothing learned", nil, nil, "session=s1", false},
		{"keploy.yml's patterns guard the header", map[string][]string{cookieSetByPage + "sub": {}}, map[string][]string{"cookie": {"^z$"}}, "session=s1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := recorded("m", "/v1/apps", map[string]string{"Cookie": "session=s1; sub=pro"}, "")
			rec.Spec.ReqBodyNoise = c.learned
			got, diag := strictMatch(t, sent("/v1/apps", map[string]string{"Cookie": c.live}, ""), c.noise, nil, nil, rec)
			if (got != nil) != c.served {
				t.Fatalf("served %v, want %v (differs %v)", got != nil, c.served, differs(diag))
			}
			if !c.served && !hasField(differs(diag), "header.cookie") {
				t.Fatalf("differs %v does not name header.cookie", differs(diag))
			}
		})
	}
}

// The learn step's pairing excuses a Cookie header without pairs the mock
// learned as the page's, where both sides send the header and keploy.yml's
// patterns do not guard it.
func TestExcusedPairOfAPairThePageSets(t *testing.T) {
	h, n, opts := learnOn(t)
	learned := map[string][]string{cookieSetByPage + "sub": {}}
	rec := learn.Leaf{Path: cookieLeaf, Noise: cookieLeaf, Type: learn.String, Value: "session=s1; sub=pro"}
	gone, changed := rec, rec
	gone.Value, changed.Value = "session=s1", "session=s2"
	k := httpKnown{n: n, learned: learned}
	if ok, _ := k.ExcusedPair(rec, gone, true, true); !ok {
		t.Fatal("the page's pair gone is not excused")
	}
	if ok, _ := k.ExcusedPair(rec, changed, true, true); ok {
		t.Fatal("a changed session is excused")
	}
	if ok, _ := k.ExcusedPair(rec, gone, true, false); ok {
		t.Fatal("a header the call did not send is excused")
	}
	opts.NoiseConfig = map[string]map[string][]string{"header": {"cookie": {"^z$"}}}
	if ok, _ := (httpKnown{n: h.newLearnNoise(opts, nil, nil), learned: learned}).ExcusedPair(rec, gone, true, true); ok {
		t.Fatal("a header keploy.yml's patterns guard is excused")
	}
}
