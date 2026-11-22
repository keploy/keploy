package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestGenerateKeployAgentService_SkipsDebugFileByDefault(t *testing.T) {
	t.Parallel()

	hostPath := filepath.Join(t.TempDir(), "keploy")
	if err := os.Mkdir(hostPath, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{Path: hostPath}, // Debug is false by default
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

	if env := mappingValue(serviceNode, "environment"); env != nil && sequenceContainsPrefix(env, "KEPLOY_DEBUG_FILE=") {
		t.Fatalf("expected no KEPLOY_DEBUG_FILE env when Debug is false; got %s", formatSequence(env))
	}
	if volumes := mappingValue(serviceNode, "volumes"); volumes != nil && sequenceContainsSuffix(volumes, ":/keploy-host") {
		t.Fatalf("expected no /keploy-host bind-mount when Debug is false; got %s", formatSequence(volumes))
	}
}

func TestGenerateKeployAgentService_AddsDebugFileMountAndEnv(t *testing.T) {
	t.Parallel()

	// A keploy folder that is there, in its project: only such a folder is
	// mounted (TestTheAgentMountsOnlyAKeployFolderInsideTheProject), and only
	// when this run asked for its debug file (--debug or --debug-file).
	for _, tc := range []struct {
		name      string
		debug     bool
		extraArgs []string
	}{
		{name: "config debug true", debug: true},
		{name: "extra args --debug-file", extraArgs: []string{"--debug-file"}},
		{name: "extra args --debug-file=true", extraArgs: []string{"--debug-file=true"}},
		{name: "extra args -debug", extraArgs: []string{"-debug"}},
		{name: "extra args --debug-file=false then --debug-file", extraArgs: []string{"--debug-file=false", "--debug-file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hostPath := filepath.Join(t.TempDir(), "keploy")
			if err := os.Mkdir(hostPath, 0o755); err != nil {
				t.Fatal(err)
			}
			serviceNode, err := (&Impl{
				logger: zap.NewNop(),
				conf:   &config.Config{Path: hostPath, Debug: tc.debug},
			}).GenerateKeployAgentService(models.SetupOptions{
				KeployContainer: "keploy-agent",
				AgentPort:       16789,
				ProxyPort:       16790,
				DnsPort:         16791,
				Mode:            models.MODE_TEST,
				ExtraArgs:       tc.extraArgs,
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
		})
	}
}

func TestGenerateKeployAgentService_SkipsDebugFileWhenDisabledInExtraArgs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		extraArgs []string
	}{
		{name: "empty", extraArgs: nil},
		{name: "--debug-file=false", extraArgs: []string{"--debug-file=false"}},
		{name: "--debug=false", extraArgs: []string{"--debug=false"}},
		{name: "--debug-file then --debug-file=false", extraArgs: []string{"--debug-file", "--debug-file=false"}},
		{name: "after -- terminator", extraArgs: []string{"--", "--debug-file"}},
		{name: "invalid bool value", extraArgs: []string{"--debug-file=maybe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hostPath := filepath.Join(t.TempDir(), "keploy")
			if err := os.Mkdir(hostPath, 0o755); err != nil {
				t.Fatal(err)
			}
			serviceNode, err := (&Impl{
				logger: zap.NewNop(),
				conf:   &config.Config{Path: hostPath},
			}).GenerateKeployAgentService(models.SetupOptions{
				KeployContainer: "keploy-agent",
				AgentPort:       16789,
				ProxyPort:       16790,
				DnsPort:         16791,
				Mode:            models.MODE_TEST,
				ExtraArgs:       tc.extraArgs,
			})
			if err != nil {
				t.Fatalf("GenerateKeployAgentService: %v", err)
			}
			if env := mappingValue(serviceNode, "environment"); env != nil && sequenceContainsPrefix(env, "KEPLOY_DEBUG_FILE=") {
				t.Fatalf("%s: expected no KEPLOY_DEBUG_FILE env; got %s", tc.name, formatSequence(env))
			}
			if volumes := mappingValue(serviceNode, "volumes"); volumes != nil && sequenceContainsSuffix(volumes, ":/keploy-host") {
				t.Fatalf("%s: expected no /keploy-host bind-mount; got %s", tc.name, formatSequence(volumes))
			}
		})
	}
}

func TestGenerateKeployAgentService_SkipsMountWhenPathUnset(t *testing.T) {
	t.Parallel()

	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{Debug: true}, // Path empty
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
		conf:   &config.Config{Path: "./keploy", Debug: true},
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
