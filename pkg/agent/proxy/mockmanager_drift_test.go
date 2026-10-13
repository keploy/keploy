package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A mock served several times before a drain (a session mock) reports the
// drift of every serve, once per field, not only the latest serve's.
func TestFlagMockAsUsedKeepsTheDriftOfEveryServe(t *testing.T) {
	mgr := NewMockManager(nil, nil, zap.NewNop())
	amount := models.MockFieldDiff{Path: "body.amount", Expected: "100", Actual: "250"}
	currency := models.MockFieldDiff{Path: "body.currency", Expected: "USD", Actual: "EUR"}
	if err := mgr.flagMockAsUsed(models.MockState{Name: "charge", RequestDrift: []models.MockFieldDiff{amount}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.flagMockAsUsed(models.MockState{Name: "charge", RequestDrift: []models.MockFieldDiff{currency, amount}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.flagMockAsUsed(models.MockState{Name: "charge"}); err != nil {
		t.Fatal(err)
	}
	got := mgr.GetConsumedMocks()
	if len(got) != 1 {
		t.Fatalf("one consumed entry per mock; got %d", len(got))
	}
	d := got[0].RequestDrift
	if len(d) != 2 || d[0].Path != "body.amount" || d[1].Path != "body.currency" {
		t.Fatalf("want amount then currency, each once; got %+v", d)
	}
	// The drain starts the next test clean.
	_ = mgr.flagMockAsUsed(models.MockState{Name: "charge"})
	if d := mgr.GetConsumedMocks()[0].RequestDrift; d != nil {
		t.Fatalf("drift must not carry into the next test's drain; got %+v", d)
	}
}
