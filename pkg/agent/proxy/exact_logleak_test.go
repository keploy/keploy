package proxy

import (
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A second value given under a name the open test has bound already is
// refused by an error that says so without either value: the error is
// logged (warnCaseBind) and answered to the test's caller, and a variable
// holds what the app sent or was sent (a session's token). It quoted the
// value bound.
func TestARefusedGivenNamesNeitherValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := varMockOf("own-1", "/apps", "app-1761609600123456789", true, models.LifetimePerTest)
	v := own.Spec.Variables["key"]
	own.Spec.Variables = models.MockVariables{"given:app": v}
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own}, nil)

	given := models.VariableBinding{Name: "given:app", Value: logleak.Canary, Recorded: "app-1761609600123456789", Mock: "test test-1", Place: "apps_test.go:20", Given: true}
	if err := mm.GiveExactTest("test-1", given); err != nil {
		t.Fatalf("the open test's given: %v", err)
	}
	given.Value = "kq9Zp2Lx7Rt8Yb1JmW4cN"
	err := mm.GiveExactTest("test-1", given)
	if err == nil {
		t.Fatal("a second given of one name was not refused")
	}
	for _, s := range []string{logleak.Canary, given.Value} {
		if logleak.Holds(err.Error(), s) {
			t.Errorf("the refusal quotes a value given: %d bytes of error text", len(err.Error()))
		}
	}
}
