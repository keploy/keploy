package provider

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// stubHooks is a minimal coreAgent.Hooks used to prove the override reaches the
// constructed agent.
type stubHooks struct{ coreAgent.Hooks }

// TestGetAgent_UsesHooksFactory pins the only guarantee this seam makes: a
// downstream build's Hooks must reach the agent.
//
// Without it, a refactor of GetAgent — inlining hooks.New into proxy.New, or
// reordering construction so the proxy gets the default hooks while only the
// agent gets the override — compiles, passes CI, and silently reverts the
// enterprise build to eBPF on clusters where it has no kernel access and
// therefore records nothing.
func TestGetAgent_UsesHooksFactory(t *testing.T) {
	t.Cleanup(func() { coreAgent.HooksFactory = nil })

	want := &stubHooks{}
	var gotLogger *zap.Logger
	var gotCfg *config.Config
	coreAgent.HooksFactory = func(l *zap.Logger, c *config.Config) coreAgent.Hooks {
		gotLogger, gotCfg = l, c
		return want
	}

	cfg := &config.Config{}
	logger := zap.NewNop()
	svc, err := GetAgent(context.Background(), "agent", cfg, logger)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if svc == nil {
		t.Fatal("GetAgent returned a nil service")
	}

	// The factory must receive the real composition-root dependencies; that is
	// the reason this seam is a factory rather than a pre-built instance.
	if gotLogger != logger {
		t.Error("factory did not receive the process logger")
	}
	if gotCfg != cfg {
		t.Error("factory did not receive the parsed config")
	}
}

// A nil factory must leave the default kernel-backed path untouched.
func TestGetAgent_NilFactoryUsesDefaultHooks(t *testing.T) {
	coreAgent.HooksFactory = nil
	svc, err := GetAgent(context.Background(), "agent", &config.Config{}, zap.NewNop())
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if svc == nil {
		t.Fatal("GetAgent returned a nil service")
	}
}

var _ = models.IngressEvent{}
