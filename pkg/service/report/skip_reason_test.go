package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const quarantineReason = "flaky/replay_only: failed the replay after record with no keploy evidence"

// A case keploy skipped for a reason carries it in the <skipped> message, the
// attribute CI dashboards show for a skipped case, and in the body.
func TestBuildJUnitSuites_QuarantinedCaseSaysWhy(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:   2,
			Ignored: 2,
			Tests: []models.TestResult{
				{TestCaseID: "tc-quarantined", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
				{TestCaseID: "tc-ignored", Status: models.TestStatusIgnored},
			},
		},
	}
	suite := buildJUnitSuites(reports).Suites[0]
	if suite.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", suite.Skipped)
	}
	q := suite.Cases[0].Skipped
	if q == nil {
		t.Fatal("the quarantined case has no <skipped> element")
	}
	if want := "quarantined by keploy (" + quarantineReason + ")"; q.Message != want {
		t.Fatalf("<skipped message> = %q, want %q", q.Message, want)
	}
	if q.Text != quarantineReason {
		t.Fatalf("<skipped> body = %q, want the reason", q.Text)
	}
	// A case skipped without a reason keeps the historical literal and no body.
	if p := suite.Cases[1].Skipped; p == nil || p.Message != "ignored test case" || p.Text != "" {
		t.Fatalf("a case skipped without a reason changed: %+v", p)
	}

	// And it survives as XML a consumer reads back.
	data, err := xml.Marshal(buildJUnitSuites(reports))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed junitTestSuites
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("the JUnit document does not parse back: %v", err)
	}
	if got := parsed.Suites[0].Cases[0].Skipped.Message; !strings.HasPrefix(got, "quarantined by keploy (") {
		t.Fatalf("round trip: message = %q", got)
	}
}

// The text report prints the reason under an IGNORED test's status line.
func TestPrintTests_QuarantinedTestSaysWhy(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printTests(context.Background(), []models.TestResult{
		{TestCaseID: "q", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
		{TestCaseID: "i", Name: "test-set-0", Status: models.TestStatusIgnored},
	})
	if err != nil {
		t.Fatalf("printTests: %v", err)
	}
	out := buf.String()
	want := "Testcase \"q\" (test-set-0) IGNORED ⏭️ ()\n  quarantined by keploy (" + quarantineReason + ")\n"
	if !strings.Contains(out, want) {
		t.Fatalf("the quarantined test does not say why:\n%s", out)
	}
	if strings.Count(out, "quarantined by keploy") != 1 {
		t.Fatalf("a test skipped without a reason grew a quarantine line:\n%s", out)
	}
}
