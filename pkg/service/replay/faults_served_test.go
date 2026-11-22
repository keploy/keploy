package replay

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// keploy test's set summary names each fault mock the set's replay served,
// with the set, in the order it first served them: a mock read with its
// payload in memory and one whose payload was left on disk (as keploy test
// reads a set's mocks) alike. A fault mock no call consumed, and a consumed
// mock with a whole answer, are not named.
func TestLogFaultsServedNamesEachFaultTheSetServed(t *testing.T) {
	onDisk := &models.Mock{Name: "mock-4", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://api:80/orders"},
		HTTPResp: &models.HTTPResp{StatusCode: 200},
		Fault:    &models.Fault{Kind: models.FaultTruncate, BodyBytes: 7},
	}}
	onDisk.LeavePayloadOnDisk(noPayloads{}, models.PayloadAt{Name: "mock-4"})
	lookup := map[string]mockDisplayInfo{}
	addMockDisplayInfo(lookup, []*models.Mock{
		{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: models.FaultReset}}},
		{Name: "mock-2", Kind: models.HTTP},
		{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: models.FaultEOF}}},
		onDisk,
	})
	consumed := map[string]models.MockState{
		"mock-1": {Name: "mock-1", Timestamp: 20},
		"mock-2": {Name: "mock-2", Timestamp: 5},
		"mock-4": {Name: "mock-4", Timestamp: 10},
	}
	core, logs := observer.New(zap.InfoLevel)
	logFaultsServed(zap.New(core), "test-set-0", consumed, lookup)

	var got []string
	for _, e := range logs.All() {
		f := e.ContextMap()
		if e.Message != "served a mock's fault" || f["testSet"] != "test-set-0" {
			t.Fatalf("line %q %v, want the set's fault line", e.Message, f)
		}
		got = append(got, f["fault"].(string))
	}
	want := []string{
		"mock-4: upstream sent the head and 7 body bytes, then closed, as recorded",
		"mock-1: upstream reset after 0 bytes, as recorded",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the summary named %q, want %q", got, want)
	}
}
