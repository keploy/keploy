package utils

import (
	"encoding/hex"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// A run's agent container takes 8 random bytes after its prefix. With 2, one
// draw in 65536 named a live run's agent, and a compose run force-removes any
// container of its agent's name before its `up`.
func TestGenerateRandomContainerNameDrawsEightBytes(t *testing.T) {
	name := GenerateRandomContainerName(zap.NewNop(), "keploy-v3-")
	id, ok := strings.CutPrefix(name, "keploy-v3-")
	if !ok {
		t.Fatalf("%q lost its prefix", name)
	}
	if b, err := hex.DecodeString(id); err != nil || len(b) != 8 {
		t.Fatalf("%q: the id is %q, want 8 random bytes in hex", name, id)
	}
}
