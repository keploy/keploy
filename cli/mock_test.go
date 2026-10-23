package cli

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMockHelpSaysRedisIsNotMocked: the open-source keploy relays Redis to the
// real server in keploy mock record and in keploy mock replay (the Redis skip,
// pkg/agent/proxy/redis_skip.go), so `keploy mock`'s help must not tell the
// user that the dependencies can be offline at replay. It said so, and a
// suite that calls Redis then failed on every Redis call. It says that Redis
// must be running, and where Keploy Enterprise, which mocks Redis, is.
func TestMockHelpSaysRedisIsNotMocked(t *testing.T) {
	cmd := Mock(context.Background(), zap.NewNop(), nil, nil, noFlags{})
	long := strings.Join(strings.Fields(cmd.Long), " ")
	for _, want := range []string{
		"this open-source keploy neither records nor mocks Redis",
		"must be running and reachable during keploy mock replay",
		"Keploy Enterprise records and mocks Redis: " + models.RedisEnterpriseURL,
		"(every dependency but Redis can be offline)",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("keploy mock's help does not say %q:\n%s", want, cmd.Long)
		}
	}
	if strings.Contains(long, "(dependencies can be offline)") {
		t.Errorf("keploy mock's help says the dependencies can be offline, Redis among them:\n%s", cmd.Long)
	}
}
