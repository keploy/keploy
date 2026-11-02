package report

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test keploy.yml's ignore list skipped is the user's choice, not keploy's
// quarantine: its JUnit <skipped> and its text line say "ignored by
// keploy.yml", and the summary does not count or list it as quarantined.
func TestAKeployYmlIgnoreIsNotAQuarantine(t *testing.T) {
	byYml := models.TestResult{TestCaseID: "y", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: models.IgnoredByConfigReason}
	quarantined := models.TestResult{TestCaseID: "q", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: quarantineReason}

	if s := ignoredSkip(byYml); s == nil || s.Message != models.IgnoredByConfigReason || s.Text != "" {
		t.Fatalf("keploy.yml's <skipped> = %+v, want message %q and no body", s, models.IgnoredByConfigReason)
	}

	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.printTests(context.Background(), []models.TestResult{byYml, quarantined}); err != nil {
		t.Fatalf("printTests: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Testcase \"y\" (test-set-0) IGNORED ⏭️ ()\n  ignored by keploy.yml\n") ||
		strings.Count(out, "quarantined by keploy") != 1 {
		t.Fatalf("keploy.yml's test does not say keploy.yml, or reads as quarantined:\n%s", out)
	}

	got := quarantinedTests(map[string]*models.TestReport{"test-set-0": {Tests: []models.TestResult{byYml, quarantined}}})
	if len(got) != 1 || got[0].test != "q" {
		t.Fatalf("quarantined %+v, want test q alone", got)
	}

	for reason, want := range map[string]bool{"": false, models.IgnoredByConfigReason: false, quarantineReason: true, "manual: why": true} {
		if models.KeploySetAside(reason) != want {
			t.Errorf("KeploySetAside(%q) = %v, want %v", reason, !want, want)
		}
	}
}
