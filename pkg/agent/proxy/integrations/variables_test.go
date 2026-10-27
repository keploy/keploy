package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A follower registered for a kind is that kind's; a nil one unregisters it,
// and a kind with none has none.
func TestRegisterVariableFollower(t *testing.T) {
	const kind models.Kind = "RegisteredKind"
	if _, ok := VariableFollowerOf(kind); ok {
		t.Fatal("a kind nothing registered has a follower")
	}
	RegisterVariableFollower(kind, func(*models.Mock) (bool, string) { return true, "" })
	f, ok := VariableFollowerOf(kind)
	if !ok {
		t.Fatal("the registered follower is not found")
	}
	if follows, _ := f(&models.Mock{}); !follows {
		t.Fatal("another follower was found")
	}
	RegisterVariableFollower(kind, nil)
	if _, ok := VariableFollowerOf(kind); ok {
		t.Fatal("an unregistered follower is still found")
	}
}
