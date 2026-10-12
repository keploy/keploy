package matcher

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestJSONLeafDiffsNamesEveryChangedLeaf(t *testing.T) {
	got := JSONLeafDiffs(
		`{"items":[{"p":1,"q":1},{"p":5,"q":1}],"id":"123","gone":true,"ts":1}`,
		`{"items":[{"p":9,"q":2},{"p":5,"q":2}],"id":123,"new":"a\nb","ts":2}`,
		map[string][]string{"ts": {}}, "body.", 0)
	want := []models.MockFieldDiff{
		{Path: "body.gone", Kind: models.DiffKindMissingInLive, Expected: "true"},
		{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: `"123"`, Actual: "123"},
		{Path: "body.items[0].p", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "9"},
		{Path: "body.items[0].q", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "body.items[1].q", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "body.new", Kind: models.DiffKindMissingInMock, Actual: `"a\nb"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if JSONLeafDiffs("{", "{}", nil, "", 0) != nil {
		t.Fatal("a body that is not JSON has no leaves")
	}
}
