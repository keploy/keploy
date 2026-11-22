package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch an HTTP response array is compared in order even when
// the caller passes ignoreOrdering=true (test.ignoreOrdering defaults to true):
// a reorder is a difference, never forgiven as a multiset.
func TestMatch_ProvenSwitchComparesArraysInOrder(t *testing.T) {
	tc := &models.TestCase{Name: "reordered", HTTPResp: models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"items":[{"id":1},{"id":2}],"tags":["a","b"]}`,
	}}
	actual := func() *models.HTTPResp {
		return &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"items":[{"id":2},{"id":1}],"tags":["b","a"]}`,
		}
	}

	t.Setenv(provenswitch.Env, "")
	if ok, _ := Match(tc, actual(), nil, true, false, zap.NewNop(), false); !ok {
		t.Fatal("control: with the switch off and ignoreOrdering=true a reordered array must still match, as today")
	}
	t.Setenv(provenswitch.Env, "1")
	if ok, _ := Match(tc, actual(), nil, true, false, zap.NewNop(), false); ok {
		t.Fatal("KEPLOY_PROVEN=1: a reordered response array matched; arrays are compared in order")
	}
}
