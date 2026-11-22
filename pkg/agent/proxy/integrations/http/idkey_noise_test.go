package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

const (
	idkU1 = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	idkU2 = "3f2504e0-4f89-11d3-9a0c-0305e82c3302"
	idkU3 = "3f2504e0-4f89-11d3-9a0c-0305e82c3303"
)

// keploy.yml's request-body noise naming id keys (matcher.NoiseIDKey) covers
// a field both requests carry under any id, and never a key only the live
// request has: a new id, or the field under an id that lacked it, is a
// difference, as it is for an entry naming one id.
func TestStrictIDKeyBodyNoiseExcusesNoAddedKey(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/hold", ct, `{"options":{"`+idkU1+`":{"hold":"a"},"`+idkU2+`":{"price":1}},"seen":{"`+idkU1+`":"a"}}`)
	noise := map[string][]string{"options.{id}.hold": {}, "seen.{id}": {}}
	if got, diag := strictMatch(t, sent("/hold", ct, `{"options":{"`+idkU1+`":{"hold":"b"},"`+idkU2+`":{"price":1}},"seen":{"`+idkU1+`":"b"}}`), nil, noise, nil, rec); got == nil {
		t.Fatalf("fields the noise names under recorded ids moved and were compared: %v", differs(diag))
	}
	// A miss names a field by its shape, an id key written {}.
	for _, c := range []struct{ name, body, field string }{
		{"the field under an id that lacked it", `{"options":{"` + idkU1 + `":{"hold":"b"},"` + idkU2 + `":{"price":1,"hold":"b"}},"seen":{"` + idkU1 + `":"b"}}`, "body.options.{}.hold"},
		{"a new id", `{"options":{"` + idkU1 + `":{"hold":"b"},"` + idkU2 + `":{"price":1}},"seen":{"` + idkU1 + `":"b","` + idkU3 + `":"b"}}`, "body.seen.{}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, diag := strictMatch(t, sent("/hold", ct, c.body), nil, noise, nil, rec)
			if got != nil || !hasField(differs(diag), c.field) {
				t.Fatalf("served %v, differs %v: want %s refused", got != nil, differs(diag), c.field)
			}
		})
	}
}

// A form key only the live request has is not covered by an entry naming id
// keys either.
func TestStrictIDKeyFormNoiseExcusesNoAddedKey(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/f", ct, "a=1&slot."+idkU1+"=x")
	noise := map[string][]string{"slot.{id}": {}}
	if got, diag := strictMatch(t, sent("/f", ct, "a=1&slot."+idkU1+"=y"), nil, noise, nil, rec); got == nil {
		t.Fatalf("a recorded key the noise names was compared: %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent("/f", ct, "a=1&slot."+idkU1+"=y&slot."+idkU2+"=z"), nil, noise, nil, rec); got != nil || !hasField(differs(diag), "body.{}") {
		t.Fatalf("an added key: served %v, differs %v", got != nil, differs(diag))
	}
}

// The learn step reads keploy.yml's body noise the same way: a leaf only the
// live request has is excused by an entry without the segment, not by one
// naming id keys.
func TestLearnIDKeyBodyNoiseExcusesNoAddedLeaf(t *testing.T) {
	k := httpKnown{n: learnNoise{body: matcher.NewNoiseLookup(map[string][]string{"options.{id}.hold": {}, "plain.ts": {}})}}
	leaf := func(p string) learn.Leaf {
		return learn.Leaf{Path: "body." + p, Noise: "body." + p, Type: learn.String, Value: "v"}
	}
	held := leaf("options." + idkU1 + ".hold")
	for _, c := range []struct {
		name          string
		l             learn.Leaf
		inRec, inLive bool
		excused       bool
	}{
		{"both requests", held, true, true, true},
		{"only the recorded one", held, true, false, true},
		{"only the live one", held, false, true, false},
		{"only the live one, an entry without the segment", leaf("plain.ts"), false, true, true},
	} {
		if excused, _ := k.ExcusedPair(c.l, c.l, c.inRec, c.inLive); excused != c.excused {
			t.Errorf("%s: excused=%v, want %v", c.name, excused, c.excused)
		}
	}
}
