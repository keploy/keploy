package models

import (
	"encoding/json"
	"reflect"
	"testing"
)

// keploy test's exact serving reaches the agent as JSON (UpdateMockParamsReq
// on /updatemockparams): the staging's worker scope with its places and the
// run's order, and a pool call's end of a pass (Passed), come out on the
// agent's side as they were sent. A field the wire drops would leave the
// agent reaching a place only when its test case opens, which no in-process
// row would see.
func TestExactServingCrossesTheWireWhole(t *testing.T) {
	for _, ex := range []*ExactServing{
		{Stage: true, Pool: []string{"mock-pool"},
			Worker: []MockEntry{{Name: "mock-boot"}, {Name: "mock-0", Kind: "Http", Place: "get-a-1"}},
			Order:  []string{"get-health-1", "get-a-1", "get-b-1"}},
		{Ahead: []string{"mock-3"}, Passed: 3},
	} {
		sent := UpdateMockParamsReq{FilterParams: MockFilterParams{Exact: ex}}
		b, err := json.Marshal(sent)
		if err != nil {
			t.Fatal(err)
		}
		var got UpdateMockParamsReq
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.FilterParams.Exact == nil || !reflect.DeepEqual(*got.FilterParams.Exact, *ex) {
			t.Fatalf("the agent read %+v; sent %+v (wire %s)", got.FilterParams.Exact, ex, b)
		}
	}
}
