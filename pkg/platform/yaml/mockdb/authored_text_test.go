package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// An `authored:` block written into a recording's mocks.yaml (what keploy
// mock derive and generate write) is still in the file after keploy
// rewrites it for another reason (here the noise write-back): the rewrite
// decodes each document and encodes it again, and a block the decoder has
// no field for would be lost on the way, with nothing said.
func TestARewriteKeepsAWrittenAuthoredBlock(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	dir := t.TempDir()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	ctx := context.Background()
	set := "test-set-0"
	m := noiseTestMock(`{"card":"4000000000000002","other":"o"}`)
	m.Name = "mock-0"
	if err := ys.InsertMock(ctx, m, set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, set, "mocks.yaml")
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const block = "authored:\n    how: derived\n    reason: declined card\n    source: TestCheckout/mock-7\n    delta:\n        status: 402\n"
	at := strings.Index(string(text), "\nname: mock-0\n")
	if at < 0 {
		t.Fatalf("precondition: no name line in\n%s", text)
	}
	at += len("\nname: mock-0\n")
	written := string(text[:at]) + block + string(text[at:])
	if err := os.WriteFile(file, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
		ReqBodyNoise: map[string][]string{"body.other": {}}}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "body.other") {
		t.Fatalf("precondition: the noise write-back did not rewrite the file:\n%s", after)
	}
	for _, line := range []string{"\nauthored:\n", "reason: declined card", "source: TestCheckout/mock-7", "status: 402"} {
		if !strings.Contains(string(after), line) {
			t.Fatalf("the rewrite dropped %q of the authored block:\n%s", strings.TrimSpace(line), after)
		}
	}
}
