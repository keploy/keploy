package http

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
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

// raced is the learned noise of the pair name set in a race to values.
func raced(name string, values ...string) map[string][]string {
	var digests []string
	for _, v := range values {
		digests = append(digests, cookieDigest(v))
	}
	return map[string][]string{cookieSetByRace + name: digests}
}

// with is learned and more, merged.
func with(learned map[string][]string, more map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range learned {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// A Cookie header is as recorded pair by pair: a pair learned as the
// page's may be gone, a pair learned as set by a race may hold another of
// the race's values, and every other pair is sent once, in the recorded
// order, with its recorded value, and no pair more.
func TestPairsAsLearned(t *testing.T) {
	page := map[string][]string{cookieSetByPage + "a": {}, cookieSetByPage + "b": {}, cookieSetByPage + "sub": {}}
	sub := map[string][]string{cookieSetByPage + "sub": {}}
	race := raced("session", "s1", "s2")
	for _, c := range []struct {
		name      string
		learned   map[string][]string
		rec, live string
		want      bool
	}{
		{"the page's pair gone", sub, "session=s1; sub=pro", "session=s1", true},
		{"a middle pair gone", page, "a=1; b=2; c=3", "a=1; c=3", true},
		{"every pair gone", page, "a=1; b=2", "", true},
		{"blanks between pairs", page, "a=1;b=2", "a=1", true},
		{"a name sent twice kept whole", page, "a=1; a=2; b=2", "a=1; a=2", true},
		{"another pair gone", sub, "session=s1; sub=pro", "sub=pro", false},
		{"two gone, one learned", sub, "session=s1; sub=pro; a=1", "session=s1", false},
		{"none gone", sub, "session=s1; sub=pro", "session=s1; sub=pro", false},
		{"none gone, another order", page, "a=1; b=2", "b=2; a=1", false},
		{"the rest in another order", page, "a=1; b=2; c=3", "c=3; a=1", false},
		{"another pair's value changed", page, "a=1; c=2", "a=1; c=3", false},
		{"a pair sent in this run only", page, "a=1; b=2", "a=1; x=9", false},
		{"the gone pair named twice", page, "a=1; b=2; b=3", "a=1", false},
		{"a name sent twice lost a value", page, "a=1; a=2; b=2", "a=1", false},
		{"the whole header learned", map[string][]string{cookieLeaf: {}}, "session=s1; sub=pro", "session=s1", false},
		{"the race's other value", race, "session=s1; sub=pro", "session=s2; sub=pro", true},
		{"the race's other value, the page's pair gone", with(race, sub), "session=s1; sub=pro", "session=s2", true},
		{"a value the race did not set", race, "session=s1; sub=pro", "session=s9; sub=pro", false},
		{"the race's value at another pair", race, "session=s1; sub=pro", "session=s1; sub=s2", false},
		{"a race of another pair", raced("other", "s1", "s2"), "session=s1; sub=pro", "session=s2; sub=pro", false},
		{"a raced pair sent twice", race, "session=s1; sub=pro", "session=s2; session=s2; sub=pro", false},
		{"a raced pair recorded twice", race, "session=s1; session=s1; sub=pro", "session=s2; sub=pro", false},
		{"a race that learned no value", map[string][]string{cookieSetByRace + "session": {}}, "session=s1; sub=pro", "session=s2; sub=pro", false},
		{"a raced pair gone", race, "session=s1; sub=pro", "sub=pro", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := pairsAsLearned(c.learned, c.rec, c.live); got != c.want {
				t.Fatalf("pairsAsLearned(%q, %q) = %v, want %v", c.rec, c.live, got, c.want)
			}
		})
	}
}

// setCookieValues reads each line of a recorded Set-Cookie header (lines
// joined with ","), a date's comma in an attribute included, and only a
// line's own first pair.
func TestSetCookieValues(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"one line", "session=s1; Path=/; HttpOnly", []string{"s1"}},
		{"two lines", "a=1; Path=/,session=s2; Path=/", []string{"s2"}},
		{"after a date's comma", "a=1; Expires=Wed, 21 Oct 2026 07:28:00 GMT, session=s2; Secure", []string{"s2"}},
		{"set twice", "session=s1, session=s2", []string{"s1", "s2"}},
		{"a value to the end", "session=s3", []string{"s3"}},
		{"an empty value", "session=; Max-Age=0", []string{""}},
		// RFC 6265 §5.2 step 5: the blanks around a value are not part of it.
		{"blanks around the value", "session= s4 ; Path=/,session=s5 ", []string{"s4", "s5"}},
		{"a name it ends", "xsession=s1; Path=/", nil},
		{"an attribute named like it", "a=1; session=s1", nil},
		{"none", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := setCookieValues(c.text, "session"); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("setCookieValues(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// racedValues finds a race only where answers of the test recorded before
// the call, to requests that sent one earlier value of the pair, set both
// the recorded and the live value: a value no answer set, one a single
// answer set, a rotation (answers to requests that sent other values), an
// answer recorded after the call, one keploy cannot read back, and a
// request that sent the pair twice are none.
func TestRacedValues(t *testing.T) {
	m := learnRecorded("mock-9", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", `{}`, map[string]string{"Cookie": "session=s1"}, `{}`)
	a := pageCall("mock-1", -200*time.Millisecond, "session=s0", "session=s1; Path=/; HttpOnly")
	b := pageCall("mock-2", -150*time.Millisecond, "sub=pro; session=s0", "x=1; Expires=Wed, 21 Oct 2026 07:28:00 GMT, session=s2; Path=/")
	c3 := pageCall("mock-3", -100*time.Millisecond, "session=s0", "session=s3")
	rotated := pageCall("mock-4", -100*time.Millisecond, "session=s1", "session=s2")
	later := pageCall("mock-5", 100*time.Millisecond, "session=s0", "session=s2")
	unread := pageCall("mock-6", -100*time.Millisecond, "session=s0", "session=s2")
	unread.Spec.HTTPResp = nil
	twice := pageCall("mock-7", -100*time.Millisecond, "session=s0; session=s0", "session=s2")
	unsent := pageCall("mock-8", -100*time.Millisecond, "", "session=s2")
	quiet := pageCall("mock-10", -120*time.Millisecond, "session=s0", "")
	setTwice := pageCall("mock-11", -100*time.Millisecond, "session=s0", "session=s8, session=s2")
	noRequest := &models.Mock{Name: "mock-12", Kind: models.Mongo, Spec: models.MockSpec{HTTPResp: &models.HTTPResp{Header: map[string]string{"Set-Cookie": "session=s2"}}}}
	for _, c := range []struct {
		name    string
		earlier []*models.Mock
		live    string
		want    []string
	}{
		{"two answers raced", []*models.Mock{a, quiet, b}, "s2", []string{"s1", "s2"}},
		{"the last value an answer set", []*models.Mock{a, setTwice}, "s2", []string{"s1", "s2"}},
		{"an answer with no request", []*models.Mock{a, noRequest}, "s2", nil},
		{"three answers raced", []*models.Mock{a, b, c3}, "s2", []string{"s1", "s2", "s3"}},
		{"a nil and another protocol's mock among them", []*models.Mock{nil, {Name: "mock-0", Kind: models.Mongo}, a, b}, "s2", []string{"s1", "s2"}},
		{"a value no answer set", []*models.Mock{a, b}, "s9", nil},
		{"one answer set the pair", []*models.Mock{a}, "s2", nil},
		{"a rotation, not a race", []*models.Mock{a, rotated}, "s2", nil},
		{"the other answer recorded after the call", []*models.Mock{a, later}, "s2", nil},
		{"the other answer unread", []*models.Mock{a, unread}, "s2", nil},
		{"the other request sent the pair twice", []*models.Mock{a, twice}, "s2", nil},
		{"the other request sent no pair", []*models.Mock{a, unsent}, "s2", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("racedValues panicked: %v", r)
					}
				}()
				got = racedValues(m, "session", "s1", c.live, c.earlier)
			}()
			slices.Sort(got)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("racedValues = %q, want %q", got, c.want)
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

func urlAt(m *models.Mock, u string) *models.Mock { m.Spec.HTTPReq.URL = u; return m }

func hostAt(m *models.Mock, host string) *models.Mock { m.Spec.HTTPReq.Header["Host"] = host; return m }

func lowerSet(m *models.Mock, set string) *models.Mock {
	m.Spec.HTTPResp.Header["set-cookie"] = set
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

// livePageCall is the POST /v1/apps sent with the Cookie header cookie
// (none where "").
func livePageCall(cookie string) *req {
	if cookie == "" {
		return live("POST", "/v1/apps", `{"name":"a"}`, nil)
	}
	return live("POST", "/v1/apps", `{"name":"a"}`, map[string]string{"Cookie": cookie})
}

// cookiePairsLearned reads past what is no earlier HTTP recording of the call (a
// nil, another protocol's mock), and a recording whose answer keploy cannot
// read back is one that may have set the pair.
func TestCookiePairsLearnedReadsOnlyHTTPRecordings(t *testing.T) {
	m := learnRecorded("mock-9", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", `{}`, map[string]string{"Cookie": "session=s1; sub=pro"}, `{}`)
	without := pageCall("mock-1", -200*time.Millisecond, "session=s1", "")
	other := &models.Mock{Name: "mock-2", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: without.Spec.ReqTimestampMock}}
	unread := pageCall("mock-3", -100*time.Millisecond, "session=s1", "")
	unread.Spec.HTTPResp = nil
	for _, c := range []struct {
		name    string
		earlier []*models.Mock
		page    bool
	}{
		{"a nil among them", []*models.Mock{nil, without}, true},
		{"another protocol's mock", []*models.Mock{other, without}, true},
		{"an answer keploy cannot read back", []*models.Mock{without, unread}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []learn.Learned
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("cookiePairsLearned panicked: %v", r)
					}
				}()
				got, _ = cookiePairsLearned(m, "session=s1; sub=pro", "session=s1", c.earlier)
			}()
			if (len(got) == 1 && got[0].Recorded.Path == cookieSetByPage+"sub") != c.page || len(got) > 1 {
				t.Fatalf("learned %+v, want the page's pair %v", got, c.page)
			}
		})
	}
}

// A call that goes without a Cookie pair the page set while its test ran
// (the test began without it: an earlier call to the same host went
// without it; and no answer before the call set it) learns that pair as
// the page's (cookieSetByPage), not the whole header as noise. A pair the
// test began with, one a service set, or a header that changed otherwise is
// learned as before (nil: the whole header as noise, or the call refused,
// naming the pairs that differ where only values did), never as the
// page's. A call that sends no Cookie header sends no pair: each recorded
// pair is the page's by the same rule.
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
		{"set by the page, the first call's URL names the host", []*models.Mock{urlAt(pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), "http://API.test/v1/plan")}, "session=s1", page},
		{"set by the page, the first call's Host in capitals", []*models.Mock{hostAt(pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), "API.Test")}, "session=s1", page},
		{"set by the page beside a service's pair named like it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "subtotal=9; Path=/")}, "session=s1", page},
		{"a service set it, its header in lower case", []*models.Mock{lowerSet(pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), "sub=pro")}, "session=s1", whole},
		{"the call sent no Cookie header, the page set both pairs", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "", "")}, "", with(page, map[string][]string{cookieSetByPage + "session": {}})},
		{"no earlier call", nil, "session=s1", whole},
		{"the test began with it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, rec, "")}, "session=s1", whole},
		{"only a call to another host went without it", []*models.Mock{other}, "session=s1", whole},
		{"the call without it was recorded later", []*models.Mock{pageCall("mock-1", 200*time.Millisecond, "session=s1", "")}, "session=s1", whole},
		{"a service set it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", "sub=pro; Path=/")}, "session=s1", whole},
		{"a service of another host set it", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), setElsewhere}, "session=s1", whole},
		{"an earlier answer keploy cannot read", []*models.Mock{unread}, "session=s1", whole},
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
			if _, page := got[cookieSetByPage+"sub"]; page || why == "" && !reflect.DeepEqual(got, map[string][]string{cookieLeaf: {}}) {
				t.Fatalf("learned %v (refused: %q), want the whole header as noise or a refusal", got, why)
			}
		})
	}
}

// learnCalls runs the learn step on r, a POST /v1/apps, against the test's
// recordings mocks: the recording it served and what it learned onto it,
// or why it refused the call.
func learnCalls(t *testing.T, mocks []*models.Mock, r *req) (string, map[string][]string, string) {
	t.Helper()
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: mocks}
	served, miss, err := h.learnMatch(context.Background(), r, db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		return "", nil, miss.res.Why
	}
	if served == nil || len(db.consumed) != 1 {
		t.Fatalf("served %v, consumed %d", served, len(db.consumed))
	}
	return db.consumed[0].Name, db.consumed[0].Spec.ReqBodyNoise, ""
}

// A call that sends no Cookie header, where its recording sent pairs the
// page set while its test ran (C1's rule, pair by pair), is read as a call
// that sends the header with no pair: it pairs with that recording, which
// learns every pair as the page's, and any other value that drifted as it
// would. Its miss stands otherwise: a pair the test began with, one a
// service set, no earlier call; a recording it pairs with as sent; a call
// that sends a Cookie header (read as sent); a header the recording learned
// whole (typed noise: one the call lacks differs in more than values); a
// reading that pairs with no one recording.
func TestLearnMatchReadsACallWithNoCookieHeader(t *testing.T) {
	at := func(ms int) time.Duration { return -time.Hour + time.Duration(ms)*time.Millisecond }
	apps := func(name string, ms int, header map[string]string, body string) *models.Mock {
		return learnRecorded(name, at(ms), "POST", "/v1/apps", body, header, `{"id":7}`)
	}
	call := func(header map[string]string, body string) *req { return live("POST", "/v1/apps", body, header) }
	without := pageCall("mock-1", -200*time.Millisecond, "", "")
	learnedWhole := apps("mock-9", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"zq7w"}`)
	learnedWhole.Spec.ReqBodyNoise = map[string][]string{cookieLeaf: {}}
	const missing = "in more than values: header"
	for _, c := range []struct {
		name  string
		mocks []*models.Mock
		r     *req
		// served and keys: the recording served and the keys it learned;
		// else why names the miss.
		served string
		keys   []string
		why    string
	}{
		{"the page set every pair", []*models.Mock{without, apps("mock-9", 300, map[string]string{"Cookie": "session=s1; sub=pro"}, `{"name":"a"}`)}, call(nil, `{"name":"a"}`),
			"mock-9", []string{cookieSetByPage + "session", cookieSetByPage + "sub"}, ""},
		{"the page set its only pair, another value drifted", []*models.Mock{without, apps("mock-9", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"zq7w"}`)}, call(nil, `{"name":"yv3x"}`),
			"mock-9", []string{"body.name", cookieSetByPage + "sub"}, ""},
		{"the test began with a pair", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "session=s1", ""), apps("mock-9", 300, map[string]string{"Cookie": "session=s1; sub=pro"}, `{"name":"a"}`)}, call(nil, `{"name":"a"}`),
			"", nil, missing},
		{"a service set the pair", []*models.Mock{pageCall("mock-1", -200*time.Millisecond, "", "sub=pro; Path=/"), apps("mock-9", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"a"}`)}, call(nil, `{"name":"a"}`),
			"", nil, missing},
		{"no earlier call", []*models.Mock{apps("mock-9", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"a"}`)}, call(nil, `{"name":"a"}`),
			"", nil, missing},
		{"a recording it pairs with as sent", []*models.Mock{without, apps("mock-5", 250, nil, `{"name":"zq7w"}`), apps("mock-9", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"yv3x"}`)}, call(nil, `{"name":"yv3x"}`),
			"mock-5", []string{"body.name"}, ""},
		{"a call that sends a Cookie header", []*models.Mock{without, apps("mock-8", 300, map[string]string{"Cookie": "sub=pro"}, `{"name":"a"}`), apps("mock-9", 310, map[string]string{"Cookie": "x=1"}, `{"name":"b"}`)}, call(map[string]string{"Cookie": "x=1"}, `{"name":"a"}`),
			"", nil, "differ from it as much"},
		{"the header learned whole", []*models.Mock{without, learnedWhole}, call(nil, `{"name":"yv3x"}`),
			"", nil, missing},
		{"no one recording pairs", []*models.Mock{without, apps("mock-8", 300, map[string]string{"Cookie": "sub=pro", "X-Trace": "t2"}, `{"name":"b"}`), apps("mock-9", 310, map[string]string{"Cookie": "sub=pro", "X-Trace": "t1"}, `{"name":"a"}`)}, call(map[string]string{"X-Trace": "t2"}, `{"name":"a"}`),
			"", nil, missing},
	} {
		t.Run(c.name, func(t *testing.T) {
			served, got, why := learnCalls(t, c.mocks, c.r)
			if c.why != "" {
				if !strings.Contains(why, c.why) {
					t.Fatalf("served %q learned %v (refused: %q), want a miss naming %q", served, got, why, c.why)
				}
				return
			}
			keys := slices.Sorted(maps.Keys(got))
			if why != "" || served != c.served || !slices.Equal(keys, c.keys) {
				t.Fatalf("served %q learned %v (refused: %q), want %s learning %v", served, keys, why, c.served, c.keys)
			}
		})
	}
}

// A Cookie header is learned pair by pair: a pair the page set that the
// call goes without, and a pair a race of the test's answers set to its
// other value (both answers to requests that sent the same earlier value),
// learn their keys; a race's values are kept as digests, never as values.
// A pair that differs otherwise is learned or refused by the header's own
// rule, read with those pairs excused where it learns them as noise (a
// session with no source: the header as noise) or refuses them; anything
// else learned of them (a value an answer sent) reads the header whole, as
// before. A refusal names the pairs that differ.
// A value no racing answer set, a pair one answer set, and a rotation are
// no race.
func TestLearnMatchLearnsCookiePairsOneByOne(t *testing.T) {
	const rec = "session=s1; sub=pro"
	without := pageCall("mock-1", -300*time.Millisecond, "session=s0", "")
	a := pageCall("mock-2", -200*time.Millisecond, "session=s0", "session=s1; Path=/")
	b := pageCall("mock-3", -150*time.Millisecond, "session=s0", "session=s2; Path=/")
	rotated := pageCall("mock-3", -150*time.Millisecond, "session=s1", "session=s2; Path=/")
	race := raced("session", "s1", "s2")
	slices.Sort(race[cookieSetByRace+"session"])
	page := map[string][]string{cookieSetByPage + "sub": {}}
	for _, c := range []struct {
		name    string
		earlier []*models.Mock
		live    string
		want    map[string][]string
		// refused names the pairs a refusal names, where want is nil.
		refused string
	}{
		{"the race's other value", []*models.Mock{a, b}, "session=s2; sub=pro", race, ""},
		{"the race's other value, the page's pair gone", []*models.Mock{without, a, b}, "session=s2", with(race, page), ""},
		{"the page's pair gone, the session changed", []*models.Mock{without}, "session=s2", with(page, map[string][]string{cookieLeaf: {}}), ""},
		{"the race's other value, another pair with no source", []*models.Mock{a, b}, "session=s2; sub=pro; t=yv3x", with(race, map[string][]string{cookieLeaf: {}}), ""},
		{"the race's other value, a pair sent in this run only", []*models.Mock{a, b}, "session=s2; sub=pro; x=1", nil, "(pair session holds another value; pair x is sent in this run only)"},
		{"a value no racing answer set", []*models.Mock{a, b}, "session=s9; sub=pro", nil, "(pair session holds another value)"},
		{"one answer set the pair", []*models.Mock{b}, "session=s2; sub=pro", nil, "(pair session holds another value)"},
		{"a rotation, not a race", []*models.Mock{a, rotated}, "session=s2; sub=pro", nil, "(pair session holds another value)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorded := rec
			if strings.Contains(c.live, "t=") {
				// A pair whose value no recording holds.
				recorded += "; t=zq7w"
			}
			got, why := learnPageCookie(t, c.earlier, recorded, c.live)
			if c.want != nil {
				if why != "" || !reflect.DeepEqual(got, c.want) {
					t.Fatalf("learned %v (refused: %q), want %v", got, why, c.want)
				}
				return
			}
			if why == "" {
				t.Fatalf("learned %v, want a refusal naming %s", got, c.refused)
			}
			if !strings.Contains(why, c.refused) {
				t.Fatalf("refusal %q does not name %s", why, c.refused)
			}
			for _, v := range []string{"s0", "s1", "s2", "s9", "pro"} {
				if strings.Contains(why, "="+v) || strings.Contains(why, " "+v+" ") {
					t.Fatalf("refusal %q holds the value %q", why, v)
				}
			}
		})
	}
}

// A replay serves a call that goes without a pair its mock learned as the
// page's, and compares every other pair, the page's own value where it is
// sent, and a header keploy.yml's patterns guard as before. A call that
// sends no Cookie header is served only where every recorded pair is the
// page's; another header it does not send is still missing.
func TestStrictServesACallWithoutAPairThePageSets(t *testing.T) {
	both := map[string][]string{cookieSetByPage + "session": {}, cookieSetByPage + "sub": {}}
	for _, c := range []struct {
		name    string
		learned map[string][]string
		noise   map[string][]string
		live    string
		served  bool
		// field is how the miss names the header: a Cookie header by the
		// pairs that differ (cookieShape).
		field string
	}{
		{"the page's pair gone", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1", true, ""},
		{"as recorded", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1; sub=pro", true, ""},
		{"the page's pair with another value", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s1; sub=free", false, "header.cookie (pair sub holds another value)"},
		{"the session gone", map[string][]string{cookieSetByPage + "sub": {}}, nil, "sub=pro", false, "header.cookie (pair session is not sent in this run)"},
		{"the session changed", map[string][]string{cookieSetByPage + "sub": {}}, nil, "session=s2", false, "header.cookie (pair session holds another value; pair sub is not sent in this run)"},
		{"nothing learned", nil, nil, "session=s1", false, "header.cookie (pair sub is not sent in this run)"},
		{"keploy.yml's patterns guard the header", map[string][]string{cookieSetByPage + "sub": {}}, map[string][]string{"cookie": {"^z$"}}, "session=s1", false, "header.cookie (pair sub is not sent in this run)"},
		{"a pair sent in this run only", nil, nil, "session=s1; sub=pro; ab=b", false, "header.cookie (pair ab is sent in this run only)"},
		{"the pairs in another order", nil, nil, "sub=pro; session=s1", false, "header.cookie (its pairs are sent in another order)"},
		{"only the bytes between pairs differ", nil, nil, "session=s1;sub=pro", false, "header.cookie"},
		{"another header holding pairs", map[string][]string{cookieSetByPage + "sub": {}}, nil, "x-jar:session=s1", false, "header.x-jar"},
		{"the race's other value", raced("session", "s1", "s2"), nil, "session=s2; sub=pro", true, ""},
		{"the race's other value, the page's pair gone", with(raced("session", "s1", "s2"), map[string][]string{cookieSetByPage + "sub": {}}), nil, "session=s2", true, ""},
		{"a value the race did not set", raced("session", "s1", "s2"), nil, "session=s3; sub=pro", false, "header.cookie (pair session holds another value)"},
		{"the race's value, another pair changed", raced("session", "s1", "s2"), nil, "session=s2; sub=free", false, "header.cookie (pair session holds another value; pair sub holds another value)"},
		{"the race's value, keploy.yml's patterns guard the header", raced("session", "s1", "s2"), map[string][]string{"cookie": {"^z$"}}, "session=s2; sub=pro", false, "header.cookie (pair session holds another value)"},
		// "-": the call sends no such header, no pair at all.
		{"no Cookie header, every pair the page's", both, nil, "-", true, ""},
		{"every pair the page's, one sent with another value", both, nil, "session=s2; sub=pro", false, "header.cookie (pair session holds another value)"},
		{"no Cookie header, one pair the page's", map[string][]string{cookieSetByPage + "sub": {}}, nil, "-", false, "header.cookie"},
		{"no Cookie header, nothing learned", nil, nil, "-", false, "header.cookie"},
		{"no Cookie header, keploy.yml's patterns guard the header", both, map[string][]string{"cookie": {"^z$"}}, "-", false, "header.cookie"},
		{"no Cookie header, a race's pair beside the page's", with(raced("session", "s1", "s2"), map[string][]string{cookieSetByPage + "sub": {}}), nil, "-", false, "header.cookie"},
		{"no other header holding pairs", both, nil, "x-jar:-", false, "header.x-jar"},
	} {
		t.Run(c.name, func(t *testing.T) {
			name, live := "Cookie", c.live
			if v, ok := strings.CutPrefix(live, "x-jar:"); ok {
				name, live = "X-Jar", v
			}
			rec := recorded("m", "/v1/apps", map[string]string{name: "session=s1; sub=pro"}, "")
			rec.Spec.ReqBodyNoise = c.learned
			header := map[string]string{name: live}
			if live == "-" {
				header = nil
			}
			got, diag := strictMatch(t, sent("/v1/apps", header, ""), c.noise, nil, nil, rec)
			if (got != nil) != c.served {
				t.Fatalf("served %v, want %v (differs %v)", got != nil, c.served, differs(diag))
			}
			if c.served {
				return
			}
			if !hasField(differs(diag), c.field) {
				t.Fatalf("differs %v does not name %s", differs(diag), c.field)
			}
			for _, d := range differs(diag) {
				for _, v := range []string{"s1", "s2", "s3", "pro", "free"} {
					if strings.Contains(d, "="+v) || strings.Contains(d, " "+v) {
						t.Fatalf("differs %v holds the value %q", differs(diag), v)
					}
				}
			}
			// The report's path stays the header's: the redaction and
			// keploy.yml's hints read it.
			for _, f := range diag.differs {
				if !strings.HasPrefix(c.field, f.diff.Path) {
					t.Fatalf("the report's path %q is not the header's (%s)", f.diff.Path, c.field)
				}
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
		t.Fatal("a header the call did not send is excused where a pair is not the page's")
	}
	// A call that sends no Cookie header (the zero leaf) where every
	// recorded pair is the page's: excused, and untyped so the leaf may be
	// missing; the same pairs on a header keploy.yml's patterns guard, or
	// on another header, are not.
	allPage := httpKnown{n: n, learned: map[string][]string{cookieSetByPage + "session": {}, cookieSetByPage + "sub": {}}}
	if ok, typed := allPage.ExcusedPair(rec, learn.Leaf{}, true, false); !ok || typed {
		t.Fatalf("a call with no Cookie header, every pair the page's: excused %v typed %v, want excused untyped", ok, typed)
	}
	if ok, typed := allPage.ExcusedPair(rec, gone, true, true); !ok || !typed {
		t.Fatalf("the page's pair gone: excused %v typed %v, want excused typed", ok, typed)
	}
	if ok, _ := allPage.ExcusedPair(learn.Leaf{}, rec, false, true); ok {
		t.Fatal("a Cookie header the recording lacks is excused")
	}
	racedLive, notRaced := rec, rec
	racedLive.Value, notRaced.Value = "session=s2; sub=pro", "session=s3; sub=pro"
	kr := httpKnown{n: n, learned: raced("session", "s1", "s2")}
	if ok, _ := kr.ExcusedPair(rec, racedLive, true, true); !ok {
		t.Fatal("the race's other value is not excused")
	}
	if ok, _ := kr.ExcusedPair(rec, notRaced, true, true); ok {
		t.Fatal("a value the race did not set is excused")
	}
	jar, jarGone := rec, gone
	jar.Path, jar.Noise, jarGone.Path, jarGone.Noise = "header.x-jar", "header.x-jar", "header.x-jar", "header.x-jar"
	if ok, _ := k.ExcusedPair(jar, jarGone, true, true); ok {
		t.Fatal("another header holding pairs is excused")
	}
	if ok, _ := allPage.ExcusedPair(jar, learn.Leaf{}, true, false); ok {
		t.Fatal("another header holding pairs, not sent, is excused")
	}
	opts.NoiseConfig = map[string]map[string][]string{"header": {"cookie": {"^z$"}}}
	if ok, _ := (httpKnown{n: h.newLearnNoise(opts, nil, nil), learned: learned}).ExcusedPair(rec, gone, true, true); ok {
		t.Fatal("a header keploy.yml's patterns guard is excused")
	}
	if ok, _ := (httpKnown{n: h.newLearnNoise(opts, nil, nil), learned: allPage.learned}).ExcusedPair(rec, learn.Leaf{}, true, false); ok {
		t.Fatal("a header keploy.yml's patterns guard, not sent, is excused")
	}
}

// A Cookie header read with pairs excused keeps what was learned of it
// only where its other pairs are noise or refused: a variable, an echo or
// a read of the clock of the header holds of its whole text.
func TestPairwiseKept(t *testing.T) {
	cookie := learn.Leaf{Path: cookieLeaf}
	other := learn.Leaf{Path: "header.x-trace"}
	jar := func(tok string) learn.Leaf { return learn.Leaf{Path: cookieLeaf, Value: "theme=dark; session=" + tok} }
	whole := func(tok string) learn.Leaf { return learn.Leaf{Path: cookieLeaf, Value: tok} }
	for _, c := range []struct {
		name    string
		learned []learn.Learned
		want    bool
	}{
		{"nothing learned", nil, true},
		{"the header as noise", []learn.Learned{{Kind: learn.KindNoise, Recorded: cookie}}, true},
		{"the header refused", []learn.Learned{{Kind: learn.KindUnaccounted, Recorded: cookie}}, true},
		{"the header a variable", []learn.Learned{{Kind: learn.KindNoise, Recorded: other}, {Kind: learn.KindVariable, Recorded: cookie}}, false},
		{"the header an echo", []learn.Learned{{Kind: learn.KindEcho, Recorded: cookie}}, false},
		{"the header a read of the clock", []learn.Learned{{Kind: learn.KindClockRead, Recorded: cookie}}, false},
		{"another header a variable", []learn.Learned{{Kind: learn.KindVariable, Recorded: other}}, true},
		{"the header a given's use at a pair", []learn.Learned{{Kind: learn.KindVariable, Recorded: jar(givenTokRec), Live: jar(givenTokFull)}}, true},
		{"the header a given's use at a pair, learned as a clock", []learn.Learned{{Kind: learn.KindClockRead, Recorded: jar(givenTokRec), Live: jar(givenTokFull)}}, true},
		{"the header holding a given whole", []learn.Learned{{Kind: learn.KindVariable, Recorded: whole(givenTokRec), Live: whole(givenTokFull)}}, false},
		{"the header holding another value than the given", []learn.Learned{{Kind: learn.KindVariable, Recorded: jar(givenTokRec), Live: jar(givenTokOther)}}, false},
		{"another pair changed beside the given", []learn.Learned{{Kind: learn.KindVariable, Recorded: jar(givenTokRec), Live: learn.Leaf{Path: cookieLeaf, Value: "theme=light; session=" + givenTokFull}}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := pairwiseKept(c.learned, []variables.Binding{tokGiven}); got != c.want {
				t.Fatalf("pairwiseKept = %v, want %v", got, c.want)
			}
		})
	}
}

// cookiePairsLearned learns a key for each pair its rule excuses and gives
// the header's text with those pairs as the call sends them: a pair the
// page set dropped, a raced pair at its live value, every other part kept
// as recorded (a part with no '=' is no pair, though named like one), and
// no blank left at its start. A pair named twice is no pair a rule reads.
func TestCookiePairsLearned(t *testing.T) {
	m := learnRecorded("mock-9", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", `{}`, nil, `{}`)
	without := pageCall("mock-1", -300*time.Millisecond, "session=s0", "")
	a := pageCall("mock-2", -200*time.Millisecond, "session=s0", "session=s1; Path=/")
	b := pageCall("mock-3", -150*time.Millisecond, "session=s0", "session=s2; Path=/")
	earlier := []*models.Mock{without, a, b}
	for _, c := range []struct {
		name, rec, live string
		keys            []string
		rest            string
	}{
		{"the page's pair first", "sub=pro; session=s1", "session=s1", []string{cookieSetByPage + "sub"}, "session=s1"},
		{"a part named like the page's pair", "session=s1; sub; sub=pro", "session=s1; sub", []string{cookieSetByPage + "sub"}, "session=s1; sub"},
		{"the race's other value", "session=s1; x=1", "session=s2; x=2", []string{cookieSetByRace + "session"}, "session=s2; x=1"},
		{"a part named like the raced pair", "session=s1; session; x=1", "session=s2; session; x=2", []string{cookieSetByRace + "session"}, "session=s2; session; x=1"},
		{"a sent pair the page could have set", "session=s1; sub=pro; x=1", "session=s1; x=1; sub=pro", nil, "session=s1; sub=pro; x=1"},
		{"the page's pair named twice", "session=s1; sub=a; sub=b", "session=s1", nil, "session=s1; sub=a; sub=b"},
		{"a raced pair sent twice", "session=s1; x=1", "session=s2; session=s2; x=2", nil, "session=s1; x=1"},
		{"a raced pair recorded twice", "session=s1; session=s1; x=1", "session=s2; x=2", nil, "session=s1; session=s1; x=1"},
		{"nothing differs", "session=s1", "session=s1", nil, "session=s1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, rest := cookiePairsLearned(m, c.rec, c.live, earlier)
			var keys []string
			for _, l := range got {
				keys = append(keys, l.Recorded.Path)
			}
			if !reflect.DeepEqual(keys, c.keys) || rest != c.rest {
				t.Fatalf("cookiePairsLearned = %v, %q; want %v, %q", keys, rest, c.keys, c.rest)
			}
		})
	}
}

// A token the test gave keploy (given:session): as recorded, in record's
// full round, and another value.
const (
	givenTokRec   = "tok-1761609600123"
	givenTokFull  = "tok-1761700000456"
	givenTokOther = "tok-1761700000999"
)

var tokGiven = variables.Binding{Name: "given:session", Value: givenTokFull, Recorded: givenTokRec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}

// learnGivenPageCookie runs the learn step on a POST /v1/apps recorded with
// the Cookie header rec after the test's earlier calls, while the open test
// gave given:session (tokGiven), sent
// in this run with live: the recording it served, or why it refused it.
func learnGivenPageCookie(t *testing.T, earlier []*models.Mock, rec, live string) (*models.Mock, string) {
	t.Helper()
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-9", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", `{"name":"a"}`, map[string]string{"Cookie": rec}, `{"id":7}`)
	db := &givenDb{learnDb: &learnDb{perTest: append(append([]*models.Mock(nil), earlier...), m)}}
	db.table.Start(nil)
	if _, ok := db.table.Give(tokGiven); !ok {
		t.Fatal("the test's given did not bind")
	}
	tr := learn.NewTracker()
	tr.Begin()
	tr.GiveValue(learn.TestGiven(tokGiven.Mock, tokGiven.Name, tokGiven.Recorded, tokGiven.Value))
	served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), livePageCall(live), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		return nil, miss.res.Why
	}
	if served == nil || len(db.consumed) != 1 {
		t.Fatalf("served %v, consumed %d", served, len(db.consumed))
	}
	return &db.consumed[0], ""
}

// A pair holding a value the test gave, beside a pair the page set (gone in
// this run) or a pair two racing answers set (at the race's other value), is
// read pair by pair like every other: the given's use at its pair, and the
// page's or the race's key at the other. A pair whose value is not the one
// the test gave, or another pair that changed with it, is read as it is with
// no page or race pair beside it: here, a session the test's earlier call
// sent as recorded, which keploy follows from there.
func TestLearnMatchFollowsAGivenPairBesideAPageOrRacedPair(t *testing.T) {
	without := pageCall("mock-1", -300*time.Millisecond, "session="+givenTokRec, "")
	a := pageCall("mock-2", -200*time.Millisecond, "session="+givenTokRec+"; sid=s0", "sid=s1; Path=/")
	b := pageCall("mock-3", -150*time.Millisecond, "session="+givenTokRec+"; sid=s0", "sid=s2; Path=/")
	race := raced("sid", "s1", "s2")
	slices.Sort(race[cookieSetByRace+"sid"])
	page := map[string][]string{cookieSetByPage + "sub": {}}
	use := []string{"header.cookie|cookie.session"}
	for _, c := range []struct {
		name      string
		earlier   []*models.Mock
		rec, live string
		// noise is what the mock learns; use, the given's use (nil: none).
		noise map[string][]string
		use   []string
		// refused names what a refusal says, where noise is nil.
		refused string
	}{
		{"beside a pair the page set", []*models.Mock{without}, "session=" + givenTokRec + "; sub=pro", "session=" + givenTokFull, page, use, ""},
		{"beside a raced pair", []*models.Mock{a, b}, "session=" + givenTokRec + "; sid=s1", "session=" + givenTokFull + "; sid=s2", race, use, ""},
		{"beside both", []*models.Mock{without, a, b}, "session=" + givenTokRec + "; sid=s1; sub=pro", "session=" + givenTokFull + "; sid=s2", with(race, page), use, ""},
		{"no pair excused", []*models.Mock{without}, "session=" + givenTokRec + "; sub=pro", "session=" + givenTokFull + "; sub=pro", map[string][]string{}, use, ""},
		{"not the value the test gave", []*models.Mock{without}, "session=" + givenTokRec + "; sub=pro", "session=" + givenTokOther, nil, nil, "(pair session holds another value; pair sub is not sent in this run) carries a value the test's recording holds earlier (in mock-1)"},
		{"another pair changed too", []*models.Mock{without}, "session=" + givenTokRec + "; theme=dark; sub=pro", "session=" + givenTokFull + "; theme=light", nil, nil, "given:session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, why := learnGivenPageCookie(t, c.earlier, c.rec, c.live)
			if c.noise == nil {
				if m != nil || !strings.Contains(why, c.refused) {
					t.Fatalf("served %v (refused: %q); want a refusal naming %s", m != nil, why, c.refused)
				}
				return
			}
			if m == nil {
				t.Fatalf("refused: %s", why)
			}
			noise := m.Spec.ReqBodyNoise
			if noise == nil {
				noise = map[string][]string{}
			}
			if !reflect.DeepEqual(noise, c.noise) {
				t.Fatalf("learned %v, want %v", noise, c.noise)
			}
			v, used := m.Spec.Learned()["given:session"]
			if (c.use != nil) != used || used && (!reflect.DeepEqual(v.Use, c.use) || v.Recorded != givenTokRec) {
				t.Fatalf("given:session %+v (used %v), want used at %v", v, used, c.use)
			}
		})
	}
}

// A replay holds a given used at a pair of the Cookie header beside a pair
// the mock learned as the page's: the call that carries the value the test
// gave and goes without the page's pair, or sends it as recorded, is
// answered; one that carries another value is refused naming the given, and
// one that sends the page's pair with another value is refused on the
// header.
func TestVariables_AGivenPairBesideAPagePairIsPassedOnOrTheCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	match := func(cookie string) (bool, *matchDiag) {
		h := newHTTP()
		m := at(varMock("mock-1", "POST", "http://store/apps", `{"name":"a"}`, 201, `{"id":7}`,
			models.MockVariables{"given:session": useV(givenTokRec, []string{"header.cookie|cookie.session"})}), 1)
		m.Spec.HTTPReq.Header["Cookie"] = "session=" + givenTokRec + "; sub=pro"
		m.Spec.ReqBodyNoise = map[string][]string{cookieSetByPage + "sub": {}}
		db := &varDb{perTest: []*models.Mock{m}}
		db.table.Start(nil)
		if _, ok := db.table.Give(tokGiven); !ok {
			t.Fatal("the test's given did not bind")
		}
		r := liveReq("POST", "/apps", `{"name":"a"}`)
		r.header.Set("Cookie", cookie)
		ok, _, diag := matchVar(t, h, db, r)
		return ok, diag
	}
	for _, served := range []string{"session=" + givenTokFull, "session=" + givenTokFull + "; sub=pro"} {
		if ok, diag := match(served); !ok {
			t.Fatalf("%q, the given's value without or with the page's pair as recorded, was refused: %+v", served, diag)
		}
	}
	ok, diag := match("session=" + givenTokOther)
	wantProblem(t, ok, diag, `the test gave given:session = "`+givenTokFull+`" (apps_test.go:20)`)
	ok, diag = match("session=" + givenTokFull + "; sub=free")
	if ok || diag == nil || diag.variable != nil {
		t.Fatalf("the page's pair with another value: answered %v, miss %+v; want a miss of the header, not of the given", ok, diag)
	}
}
