package docker

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestGenerateKeployAgentService_AddsDebugFileMountAndEnv(t *testing.T) {
	t.Parallel()

	const hostPath = "/abs/path/to/keploy"
	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{Path: hostPath},
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_TEST,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}

	volumes := mappingValue(serviceNode, "volumes")
	if volumes == nil {
		t.Fatalf("expected volumes block")
	}
	wantMount := hostPath + ":/keploy-host"
	if !sequenceContains(volumes, wantMount) {
		t.Fatalf("expected volume %q in volumes %s", wantMount, formatSequence(volumes))
	}

	env := mappingValue(serviceNode, "environment")
	if env == nil {
		t.Fatalf("expected environment block")
	}
	wantEnv := "KEPLOY_DEBUG_FILE=/keploy-host/agent-debug.log"
	if !sequenceContains(env, wantEnv) {
		t.Fatalf("expected env %q in environment %s", wantEnv, formatSequence(env))
	}
}

func TestGenerateKeployAgentService_SkipsMountWhenPathUnset(t *testing.T) {
	t.Parallel()

	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{}, // Path empty
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_TEST,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}

	env := mappingValue(serviceNode, "environment")
	if env != nil && sequenceContainsPrefix(env, "KEPLOY_DEBUG_FILE=") {
		t.Fatalf("expected no KEPLOY_DEBUG_FILE env when conf.Path is empty; got %s", formatSequence(env))
	}
	volumes := mappingValue(serviceNode, "volumes")
	if volumes != nil && sequenceContainsSuffix(volumes, ":/keploy-host") {
		t.Fatalf("expected no /keploy-host bind-mount when conf.Path is empty; got %s", formatSequence(volumes))
	}
}

func TestGenerateKeployAgentService_SkipsMountWhenPathRelative(t *testing.T) {
	t.Parallel()

	// Docker rejects relative bind-source paths; the generator should
	// silently skip rather than emit a malformed compose.
	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{Path: "./keploy"},
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_TEST,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}

	env := mappingValue(serviceNode, "environment")
	if env != nil && sequenceContainsPrefix(env, "KEPLOY_DEBUG_FILE=") {
		t.Fatalf("expected no KEPLOY_DEBUG_FILE env for relative conf.Path; got %s", formatSequence(env))
	}
}

func sequenceContains(node *yaml.Node, want string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, n := range node.Content {
		if n.Value == want {
			return true
		}
	}
	return false
}

func sequenceContainsPrefix(node *yaml.Node, prefix string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, n := range node.Content {
		if strings.HasPrefix(n.Value, prefix) {
			return true
		}
	}
	return false
}

func sequenceContainsSuffix(node *yaml.Node, suffix string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, n := range node.Content {
		if strings.HasSuffix(n.Value, suffix) {
			return true
		}
	}
	return false
}

func formatSequence(node *yaml.Node) string {
	if node == nil {
		return "<nil>"
	}
	values := make([]string, 0, len(node.Content))
	for _, n := range node.Content {
		values = append(values, n.Value)
	}
	return "[" + strings.Join(values, ", ") + "]"
}
