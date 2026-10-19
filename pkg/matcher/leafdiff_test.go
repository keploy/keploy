package matcher

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestJSONLeafDiffsAgreesWithTheMatcher(t *testing.T) {
	for _, tc := range []struct {
		exp, act string
		noise    map[string][]string
		ordering bool
		want     []string
	}{
		{`{"id":1,"paid":true,"userId":5,"valid":1}`, `{"id":2,"paid":false,"userId":6,"valid":2}`, map[string][]string{"id": {}}, false, []string{"b.paid", "b.userId", "b.valid"}},
		{`{"a":{"id":1},"x":1}`, `{"a":{"id":2},"x":1}`, map[string][]string{"id": {}}, false, nil},
		{`{"at":"1999"}`, `{"at":"2026"}`, map[string][]string{"at": {"^20"}}, false, nil},
		{`{"at":"2025"}`, `{"at":"abc"}`, map[string][]string{"at": {"^20"}}, false, []string{"b.at"}},
		{`{"meta":{"v":1,"w":2}}`, `{"meta":{"v":9,"w":3}}`, map[string][]string{"x.meta": {}, "meta.v": {}}, false, []string{"b.meta.w"}},
		{`{"items":[{"p":1},{"p":2}]}`, `{"items":[{"p":2},{"p":1}]}`, nil, true, nil},
		{`{"items":[{"p":1},{"p":2}]}`, `{"items":[{"p":2},{"p":3}]}`, nil, true, []string{"b.items"}},
		{`{"n":1.0}`, `{"n":1}`, nil, false, nil},
		{`{"gone":1,"keep":1}`, `{"keep":1,"new":2}`, nil, false, []string{"b.gone", "b.new"}},
		{`{"a":[1,2,3,4,5,6,7,8,9,10,11,12]}`, `{"a":[0,0,0,0,0,0,0,0,0,0,0,0]}`, nil, false, []string{"b.a[0]", "b.a[1]", "b.a[2]", "b.a[3]", "b.a[4]", "b.a[5]", "b.a[6]", "b.a[7]", "b.a[8]", "b.a[9]", "b.a[10]", "b.a[11]"}},
	} {
		exp, act := tc.exp, tc.act
		v, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
		if err != nil {
			t.Fatal(err)
		}
		res, err := JSONDiffWithNoiseControl(v, tc.noise, tc.ordering, zap.NewNop())
		matched := err == nil && res.matches
		rows, n := JSONLeafDiffs(tc.exp, tc.act, tc.noise, tc.ordering, "b.", 0, 0)
		var got []string
		for _, r := range rows {
			got = append(got, r.Path)
		}
		if matched != (len(got) == 0) || !reflect.DeepEqual(got, tc.want) || n != len(got) {
			t.Errorf("%s -> %s noise %v: matcher matched=%v, rows %v (want %v)", tc.exp, tc.act, tc.noise, matched, got, tc.want)
		}
	}
}

func TestJSONLeafDiffsSaysWhichSideHasAField(t *testing.T) {
	got, _ := JSONLeafDiffs(`{"id":"123","gone":true}`, `{"id":123,"new":"a\nb"}`, nil, false, "body.", 0, 0)
	want := []models.MockFieldDiff{
		{Path: "body.gone", Kind: models.DiffKindMissingInLive, Expected: "true"},
		{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: `"123"`, Actual: "123"},
		{Path: "body.new", Kind: models.DiffKindMissingInMock, Actual: `"a\nb"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if d, _ := JSONLeafDiffs("{", "{}", nil, false, "", 0, 0); d != nil {
		t.Fatal("a body that is not JSON has no leaves")
	}
}

func TestJSONLeafDiffsCapsRowsAndNamesTopLevelArrays(t *testing.T) {
	exp := "[" + strings.Repeat(`{"id":1},`, 99) + `{"id":1}]`
	act := "[" + strings.Repeat(`{"id":2},`, 99) + `{"id":2}]`
	got, n := JSONLeafDiffs(exp, act, nil, false, "body.", 0, 50)
	if len(got) != 50 || n != 100 || got[0].Path != "body[0].id" || got[10].Path != "body[10].id" {
		t.Fatalf("got %d rows of %d, first %q", len(got), n, got[0].Path)
	}
	if got, _ := JSONLeafDiffs(`1`, `2`, nil, false, "body.", 0, 0); got[0].Path != "body" {
		t.Fatalf("a scalar body is %q", got[0].Path)
	}
}

func TestJSONLeafDiffsStopsAtADepthItCanAfford(t *testing.T) {
	deep := func(leaf string) string {
		return strings.Repeat(`{"a":`, 10000) + leaf + strings.Repeat("}", 10000)
	}
	rows, n := JSONLeafDiffs(deep("1"), deep("2"), nil, false, "body.", 0, 50)
	if n != 1 || len(rows) != 1 || strings.Count(rows[0].Path, ".a") != maxLeafDepth {
		t.Fatalf("got %d rows of %d: %+v", len(rows), n, rows)
	}
}

func TestJSONLeafDiffsAgreesWithTheMatcherBelowTheCap(t *testing.T) {
	for _, tc := range []struct {
		exp, act string
		noise    map[string][]string
		ordering bool
	}{
		{`{"id":1,"x":1}`, `{"id":2,"x":1}`, map[string][]string{"id": {}}, false},
		{`{"at":"2025","x":1}`, `{"at":"2026","x":1}`, map[string][]string{"at": {"^20"}}, false},
		{`{"at":"2025"}`, `{"at":"abc"}`, map[string][]string{"at": {"^20"}}, false},
		{`{"l":[1,2,3]}`, `{"l":[3,2,1]}`, nil, true},
		{`{"l":[1,2,3]}`, `{"l":[3,2,4]}`, nil, true},
		{`{"l":[{"p":1},{"p":2}]}`, `{"l":[{"p":2},{"p":1}]}`, nil, true},
		{`{"v":null}`, `{"v":"2026"}`, map[string][]string{"v": {"^20"}}, false},
		{`{"v":null}`, `{"v":"abc"}`, map[string][]string{"v": {"^20"}}, false},
		{`{"v":null}`, `{"v":"2026"}`, map[string][]string{"@.v": {"^20"}}, false},
		{`{"m":{"ts":1,"k":1}}`, `{"m":{"ts":2,"k":1}}`, map[string][]string{"@.m.ts": {}}, false},
		{`{"m":{"ts":1,"k":1}}`, `{"m":{"ts":2,"k":2}}`, map[string][]string{"@.m.ts": {}}, false},
		{`{"n":1}`, `{"n":1}`, nil, false},
	} {
		for _, depth := range []int{0, 3, maxLeafDepth - 1, maxLeafDepth, maxLeafDepth + 6} {
			nest := func(s string) string { return strings.Repeat(`{"a":`, depth) + s + strings.Repeat("}", depth) }
			noise := map[string][]string{}
			for k, v := range tc.noise {
				if rest, ok := strings.CutPrefix(k, "@."); ok {
					k = strings.Repeat("a.", depth) + rest
				}
				noise[k] = v
			}
			exp, act := nest(tc.exp), nest(tc.act)
			e, a := exp, act
			v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
			if err != nil {
				t.Fatal(err)
			}
			res, err := JSONDiffWithNoiseControl(v, noise, tc.ordering, zap.NewNop())
			matched := err == nil && res.matches
			rows, _ := JSONLeafDiffs(exp, act, noise, tc.ordering, "b.", 0, 0)
			if matched != (len(rows) == 0) {
				t.Errorf("depth %d %s -> %s noise %v ordering %v: matcher matched=%v, rows %+v", depth, tc.exp, tc.act, tc.noise, tc.ordering, matched, rows)
			}
		}
	}
}

func FuzzJSONLeafDiffsAgreesWithTheMatcher(f *testing.F) {
	f.Add(`{"a":{"id":1,"l":[1,2]}}`, `{"a":{"id":2,"l":[2,1]}}`, "id", "", true, 70)
	f.Add(`{"v":null}`, `{"v":"2026"}`, "v", "^20", false, 66)
	f.Fuzz(func(t *testing.T, exp, act, key, pattern string, ordering bool, depth int) {
		if depth < 0 || depth > 200 {
			return
		}
		exp = strings.Repeat(`{"a":`, depth) + exp + strings.Repeat("}", depth)
		act = strings.Repeat(`{"a":`, depth) + act + strings.Repeat("}", depth)
		if !json.Valid([]byte(exp)) || !json.Valid([]byte(act)) {
			return
		}
		noise := map[string][]string{}
		if key != "" && !strings.Contains(key, ".") {
			noise[key] = []string{}
			if pattern != "" {
				if _, err := regexp.Compile(pattern); err != nil {
					return
				}
				noise[key] = []string{pattern}
			}
		}
		e, a := exp, act
		v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
		if err != nil || !v.IsIdentical() {
			return
		}
		res, err := JSONDiffWithNoiseControl(v, noise, ordering, zap.NewNop())
		matched := err == nil && res.matches
		rows, _ := JSONLeafDiffs(exp, act, noise, ordering, "b.", 0, 0)
		if matched != (len(rows) == 0) {
			t.Fatalf("matcher matched=%v, rows %+v", matched, rows)
		}
	})
}

func TestJSONLeafDiffsShowsAMissingFieldThatCrossesTheCap(t *testing.T) {
	wrap := func(s string) string { return strings.Repeat(`{"a":`, 62) + s + strings.Repeat("}", 62) }
	deep := `{"x":{"y":{"z":{"v":1}}}}`
	rows, _ := JSONLeafDiffs(wrap(`{"k":1,"gone":`+deep+`}`), wrap(`{"k":1,"new":`+deep+`}`), nil, false, "", 0, 0)
	if len(rows) != 2 || rows[0].Expected != deep || rows[1].Actual != deep {
		t.Fatalf("rows %+v", rows)
	}
}
