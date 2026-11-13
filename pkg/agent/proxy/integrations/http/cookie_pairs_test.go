package http

import (
	"strings"
	"testing"

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
