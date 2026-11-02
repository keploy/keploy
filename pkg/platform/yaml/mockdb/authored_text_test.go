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
// rewrites it for another reason (here the noise write-back onto the
// file's recorded mock): the rewrite decodes each document and encodes it
// again, and a block the decoder has no field for would be lost on the
// way, with nothing said. The same write-back writes nothing onto the
// authored mock itself (learn.MergeLearned), though it carries the same
// learning.
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
	rec := noiseTestMock(`{"card":"4242424242424242","other":"p"}`)
	rec.Name = "mock-1"
	if err := ys.InsertMock(ctx, rec, set); err != nil {
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
	learned := map[string][]string{"body.other": {}}
	if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{
		"mock-0": {Name: "mock-0", ReqBodyNoise: learned},
		"mock-1": {Name: "mock-1", ReqBodyNoise: learned},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for _, doc := range strings.Split(string(after), "\n---\n") {
		for _, name := range []string{"mock-0", "mock-1"} {
			if strings.Contains(doc, "\nname: "+name+"\n") || strings.HasPrefix(doc, "name: "+name+"\n") {
				docs[name] = doc
			}
		}
	}
	if !strings.Contains(docs["mock-1"], "body.other") {
		t.Fatalf("precondition: the noise write-back did not rewrite the file:\n%s", after)
	}
	if strings.Contains(docs["mock-0"], "body.other") {
		t.Fatalf("the noise write-back wrote learned noise onto the authored mock:\n%s", docs["mock-0"])
	}
	for _, line := range []string{"\nauthored:\n", "reason: declined card", "source: TestCheckout/mock-7", "status: 402"} {
		if !strings.Contains(string(after), line) {
			t.Fatalf("the rewrite dropped %q of the authored block:\n%s", strings.TrimSpace(line), after)
		}
	}
}
