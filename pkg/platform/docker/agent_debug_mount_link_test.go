//go:build !windows

package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

func agentServiceFor(t *testing.T, keployPath string) (*yaml.Node, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	node, err := (&Impl{
		logger: zap.New(core),
		conf:   &config.Config{Path: keployPath},
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_RECORD,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}
	return node, logs
}

// Docker follows a link at a bind source on the host, and the agent writes
// its debug log there as root. The keploy folder comes with the repository,
// so it may be a link: it is mounted at /keploy-host only when it is inside
// the project, and a missing one is made by keploy, inside the project.
func TestTheAgentMountsOnlyAKeployFolderInsideTheProject(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{
		{"relative link out of the project", filepath.Join("..", "outside")},
		{"absolute link", outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			keployDir := filepath.Join(project, "keploy")
			if err := os.Symlink(tc.target, keployDir); err != nil {
				t.Fatal(err)
			}
			node, logs := agentServiceFor(t, keployDir)
			if v := mappingValue(node, "volumes"); v != nil && sequenceContainsSuffix(v, ":/keploy-host") {
				t.Fatalf("a keploy folder linking to %q is mounted: %s", tc.target, formatSequence(v))
			}
			if e := mappingValue(node, "environment"); e != nil && sequenceContainsPrefix(e, "KEPLOY_DEBUG_FILE=") {
				t.Fatalf("KEPLOY_DEBUG_FILE is set for an unmounted folder: %s", formatSequence(e))
			}
			said := logs.FilterMessageSnippet("not mounted into the agent container").All()
			if len(said) != 1 || !strings.Contains(said[0].ContextMap()["keployFolder"].(string), "keploy") {
				t.Fatalf("want one warning naming the folder; got %v", logs.All())
			}
			if fi, err := os.Lstat(keployDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the link was not kept: %v, %v", fi, err)
			}
			if ents, _ := os.ReadDir(outside); len(ents) != 0 {
				t.Errorf("something was made outside the project: %v", ents)
			}
		})
	}

	t.Run("a keploy folder inside the project, a link inside it, and a missing one are mounted", func(t *testing.T) {
		project := t.TempDir()
		if err := os.MkdirAll(filepath.Join(project, "data", "keploy"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("data", "keploy"), filepath.Join(project, "keploy")); err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(t.TempDir(), "keploy")
		for _, dir := range []string{filepath.Join(project, "keploy"), filepath.Join(project, "data", "keploy"), missing} {
			node, logs := agentServiceFor(t, dir)
			if !sequenceContains(mappingValue(node, "volumes"), dir+":/keploy-host") {
				t.Errorf("%q is not mounted: %s", dir, formatSequence(mappingValue(node, "volumes")))
			}
			if !sequenceContains(mappingValue(node, "environment"), "KEPLOY_DEBUG_FILE=/keploy-host/agent-debug.log") {
				t.Errorf("%q: KEPLOY_DEBUG_FILE is not set", dir)
			}
			if logs.Len() != 0 {
				t.Errorf("%q: unexpected warnings %v", dir, logs.All())
			}
		}
		if fi, err := os.Lstat(missing); err != nil || !fi.IsDir() {
			t.Errorf("the missing keploy folder was not made as a folder: %v, %v", fi, err)
		}
	})
}
