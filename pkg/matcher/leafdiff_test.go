package matcher

import (
	"reflect"
	"strings"
	"testing"
	"time"

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
	start := time.Now()
	rows, n := JSONLeafDiffs(deep("1"), deep("2"), nil, false, "body.", 0, 50)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("depth 10000 took %s", took)
	}
	if n != 1 || len(rows) != 1 || strings.Count(rows[0].Path, ".a") != maxLeafDepth {
		t.Fatalf("got %d rows of %d: %+v", len(rows), n, rows)
	}
}
