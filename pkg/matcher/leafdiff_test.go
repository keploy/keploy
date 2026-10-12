package matcher

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestJSONLeafDiffsNamesEveryChangedLeaf(t *testing.T) {
	got, n := JSONLeafDiffs(
		`{"items":[{"p":1,"q":1},{"p":5,"q":1}],"id":"123","gone":true,"TS":1,"at":"2025-01","b":"x"}`,
		`{"items":[{"p":9,"q":2},{"p":5,"q":2}],"id":123,"new":"a\nb","TS":2,"at":"2026-02","b":"y"}`,
		map[string][]string{"ts": {}, "at": {"^20"}, "b": {"^z"}}, "body.", 0, 0)
	want := []models.MockFieldDiff{
		{Path: "body.b", Kind: models.DiffKindValueChanged, Expected: `"x"`, Actual: `"y"`},
		{Path: "body.gone", Kind: models.DiffKindMissingInLive, Expected: "true"},
		{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: `"123"`, Actual: "123"},
		{Path: "body.items[0].p", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "9"},
		{Path: "body.items[0].q", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "body.items[1].q", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "body.new", Kind: models.DiffKindMissingInMock, Actual: `"a\nb"`},
	}
	if !reflect.DeepEqual(got, want) || n != len(want) {
		t.Fatalf("got  %d %+v\nwant %+v", n, got, want)
	}
	if d, _ := JSONLeafDiffs("{", "{}", nil, "", 0, 0); d != nil {
		t.Fatal("a body that is not JSON has no leaves")
	}
}

func TestJSONLeafDiffsCapsRowsAndNamesTopLevelArrays(t *testing.T) {
	exp := "[" + strings.Repeat(`{"id":1},`, 99) + `{"id":1}]`
	act := "[" + strings.Repeat(`{"id":2},`, 99) + `{"id":2}]`
	got, n := JSONLeafDiffs(exp, act, nil, "body.", 0, 50)
	if len(got) != 50 || n != 100 || got[0].Path != "body[0].id" {
		t.Fatalf("got %d rows of %d, first %q", len(got), n, got[0].Path)
	}
	if got, _ := JSONLeafDiffs(`1`, `2`, nil, "body.", 0, 0); got[0].Path != "body" {
		t.Fatalf("a scalar body is %q", got[0].Path)
	}
}
