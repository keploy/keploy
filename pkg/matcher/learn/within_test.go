package learn

import (
	"strings"
	"testing"
	"time"
)

// A value stands in a longer one as a bindable part only once, whole,
// between characters that are no letter or digit or at an end; the reason
// names where it stands, never its text.
func TestPartWithin(t *testing.T) {
	id := "cf5d2c03-50e1-48ee-96e7-6e4eec946178"
	for _, c := range []struct {
		name, longer string
		want         Within
		why          string // a word the reason holds, "" for none
	}{
		{name: "a constant before it", longer: "APP:" + id, want: Within{Before: "APP:"}},
		{name: "a constant after it", longer: id + "/settings", want: Within{After: "/settings"}},
		{name: "constants on both sides", longer: "orgs/" + id + "/apps", want: Within{Before: "orgs/", After: "/apps"}},
		{name: "whole", longer: id},
		{name: "not in it", longer: "APP:other"},
		{name: "two places", longer: "APP:" + id + "|" + id, why: "at 2 places"},
		{name: "a part of a word before", longer: "APPx" + id, why: "a part of a word"},
		{name: "a part of a word after", longer: id + "z/apps", why: "a part of a word"},
	} {
		got, why := PartWithin(c.longer, id)
		if got != c.want || (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: PartWithin = %+v, %q; want %+v and a reason holding %q", c.name, got, why, c.want, c.why)
		}
		if strings.Contains(why, id) {
			t.Errorf("%s: the reason %q names the value by its text", c.name, why)
		}
	}
	// Overlapping places count: "aa" stands at two places of "x:aaa".
	if _, why := PartWithin("x:aaa", "aa"); !strings.Contains(why, "at 2 places") {
		t.Errorf("an overlapping second place: %q", why)
	}
}

// A replay reads the value at a part only from a leaf the part makes: the
// constant text as recorded, and a value standing there as PartWithin reads
// it.
func TestWithinValueIn(t *testing.T) {
	w := Within{Before: "APP:"}
	if got := w.Render("b-7"); got != "APP:b-7" {
		t.Fatalf("Render = %q", got)
	}
	for leaf, want := range map[string]string{"APP:b-7": "b-7", "APP:x:y": "x:y"} {
		if got, ok := w.ValueIn(leaf); !ok || got != want {
			t.Errorf("ValueIn(%q) = %q, %v; want %q", leaf, got, ok, want)
		}
	}
	for _, leaf := range []string{"ORG:b-7", "APP:", "APP", "b-7"} {
		if got, ok := w.ValueIn(leaf); ok {
			t.Errorf("ValueIn(%q) = %q: a leaf the part does not make", leaf, got)
		}
	}
	if _, ok := (Within{After: "/apps"}).ValueIn("x/apps"); !ok {
		t.Error("a constant after the value")
	}
	if _, ok := (Within{}).ValueIn("b-7"); ok {
		t.Error("the zero Within names no part")
	}
}

// A request value the test sent earlier whole, at a variable place, that a
// later call holds as one part of a longer value ("APP:" before the app id
// it minted, as api-server's role-binding find on resourceKey): a variable
// bound at the earlier place and used here inside the longer value, the
// constant text beside it still compared. Two places, a part of a word, or
// text beside it that changed too are no such part, and the place says why.
func TestClassifyAVariableInsideALongerValue(t *testing.T) {
	at := time.Date(2026, 10, 30, 10, 0, 42, 0, time.UTC)
	rec, live := "cf5d2c03-50e1-48ee-96e7-6e4eec946178", "0b7e3a51-9c2d-4f60-8a1b-2d3c4e5f6a7b"
	minted := []Earlier{{Mock: "mock-693", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.docs.msgs.0.appId", Noise: "body.docs.msgs.0.appId", Type: String, Value: rec}, Live: live, Place: "body.docs.msgs.0.appId"},
	}}}
	for _, c := range []struct {
		name, rec, live string
		want            Within
		why             string // a word NotWithin holds, "" for none
	}{
		{name: "<const>:<var>", rec: "APP:" + rec, live: "APP:" + live, want: Within{Before: "APP:"}},
		{name: "<var>/<const>", rec: rec + "/settings", live: live + "/settings", want: Within{After: "/settings"}},
		{name: "two places", rec: "APP:" + rec + "|" + rec, live: "APP:" + live + "|" + live, why: "at 2 places"},
		{name: "a part of a word", rec: "APPc" + rec, live: "APPc" + live, why: "a part of a word"},
		{name: "the text beside it changed", rec: "APP:" + rec, live: "ORG:" + live, why: "the text beside the value changed"},
	} {
		got := classifyOne(c.rec, c.live, at, minted, nil, nil)
		f := got.From
		if got.Kind != KindVariable || f.Mock != "mock-693" || f.Var != "body.docs.msgs.0.appId" || f.Whole || f.Live != live {
			t.Errorf("%s: %s from %+v (%s), want a variable from mock-693's appId, not whole", c.name, got.Kind, f, got.Why)
			continue
		}
		if f.Within != c.want || (c.why == "") != (f.NotWithin == "") || !strings.Contains(f.NotWithin, c.why) {
			t.Errorf("%s: within %+v, not within %q; want %+v and a reason holding %q", c.name, f.Within, f.NotWithin, c.want, c.why)
		}
	}
	// The other way round, the earlier leaf holding the drifted value
	// inside a longer one, is no part to bind: no Within, and no reason of
	// its own (the caller's "inside a longer value" reason stands).
	longer := []Earlier{{Mock: "mock-5", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.ref", Noise: "body.ref", Type: String, Value: "APP:" + rec}, Live: "APP:" + live, Place: "body.ref"},
	}}}
	if got := classifyOne(rec, live, at, longer, nil, nil); got.Kind != KindVariable || got.From.Whole || got.From.Within.Set() || got.From.NotWithin != "" {
		t.Errorf("a value found inside a longer earlier one: %s from %+v", got.Kind, got.From)
	}
	// A whole place stays whole, with no part.
	if got := classifyOne(rec, live, at, minted, nil, nil); !got.From.Whole || got.From.Within.Set() || got.From.NotWithin != "" {
		t.Errorf("a whole value: from %+v", got.From)
	}
}
