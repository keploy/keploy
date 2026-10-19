package mocknoise

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch a correlation is never found by a value's shape:
// only the learn step learns one.
func TestMaterializeCorrelationsFindsNoneUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const id = "550e8400-e29b-41d4-a716-446655440000"
	echo := mockWithBodies(`{"idempotencyKey":"`+id+`"}`, `{"idempotencyKey":"`+id+`"}`)
	MaterializeCorrelations(echo)
	if len(echo.Spec.Correlations) != 0 {
		t.Fatalf("a UUID's shape found a correlation under the switch: %+v", echo.Spec.Correlations)
	}
}

// The learn step's noise in a request's other parts is not body noise.
func TestStripBodyPrefixKeepsBodyNoiseOnlyUnderTheSwitch(t *testing.T) {
	stored := map[string][]string{"body.a": {}, "header.x-trace": {}, "url.query.at": {}, "url.path.2": {}}
	t.Setenv(provenswitch.Env, "0")
	if got := StripBodyPrefix(stored); len(got) != 4 {
		t.Fatalf("without the switch nothing changes: %v", got)
	}
	t.Setenv(provenswitch.Env, "1")
	if got := StripBodyPrefix(stored); !reflect.DeepEqual(got, map[string][]string{"a": {}}) {
		t.Fatalf("under the switch: %v", got)
	}
}
