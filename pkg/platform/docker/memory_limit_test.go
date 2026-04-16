package docker

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestGetAliasIncludesMemoryLimit(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("linux-specific docker alias test")
	}

	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		ClientNSPID:     4242,
		Mode:            models.MODE_RECORD,
		MemoryLimit:     512,
	}, false)
	if err != nil {
		t.Fatalf("getAlias returned error: %v", err)
	}

	if !containsAll(alias, "--memory-limit 512") {
		t.Fatalf("expected alias to include the agent memory flag, got %s", alias)
	}
	if strings.Contains(alias, " --memory ") {
		t.Fatalf("did not expect alias to include a docker runtime memory flag, got %s", alias)
	}
}

func TestGenerateKeployAgentServiceIncludesMemoryLimit(t *testing.T) {
	t.Parallel()

	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{},
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_RECORD,
		MemoryLimit:     512,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService returned error: %v", err)
	}

	memLimitNode := mappingValue(serviceNode, "mem_limit")
	if memLimitNode != nil {
		t.Fatalf("did not expect mem_limit to be set on the compose service, got %#v", memLimitNode)
	}

	commandNode := mappingValue(serviceNode, "command")
	if commandNode == nil {
		t.Fatalf("expected command node to be present")
	}

	foundFlag := false
	foundValue := false
	for _, node := range commandNode.Content {
		if node.Value == "--memory-limit" {
			foundFlag = true
		}
		if node.Value == "512" {
			foundValue = true
		}
	}
	if !foundFlag || !foundValue {
		t.Fatalf("expected command to include --memory-limit 512, got %#v", commandNode.Content)
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}

func containsAll(value string, expected ...string) bool {
	for _, fragment := range expected {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
