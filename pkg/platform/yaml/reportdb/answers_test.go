package reportdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A report's answered calls (a test's Answers, the set's SetAnswers: proven
// switch) are written and read back whole, whatever the switch reading it: a
// report is read for what it holds.
func TestAReportsAnswersRoundTrip(t *testing.T) {
	answer := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "insert keploy.events", ClosestMock: "mock-6",
		RecordedIn: "post-url-1", Kind: models.MissOutsideTest, BeforeSet: true, Answer: models.AnswerProvisional, Drift: true,
		Differs: []string{"body.documents[0]._id"}, Source: &models.Origin{Comm: "sqs", Exe: "./sqs"},
		FieldReasons: []models.FieldReason{{Field: "body.documents[0].at", Why: "body.documents[0].at was learned as a read of the application's clock: it changed"}}}
	byKind := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Kind: models.MissInTest,
		Answer: models.AnswerByKind, Reused: true, WriteKind: "Mongo insert keploy.events", Source: &models.Origin{Comm: "sqs", Exe: "./sqs"}}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		for _, on := range []string{"1", ""} {
			t.Run(format.FileExtension()+"/switch="+on, func(t *testing.T) {
				t.Setenv(provenswitch.Env, on)
				ctx := context.Background()
				dir := t.TempDir()
				want := goldenReport()
				want.Tests[0].Answers = []models.UnmatchedCall{byKind}
				want.SetAnswers = []models.UnmatchedCall{answer}
				if err := NewWithFormat(zap.NewNop(), dir, format).InsertReport(ctx, "test-run-0", "test-set-0", &want); err != nil {
					t.Fatal(err)
				}
				got, err := NewWithFormat(zap.NewNop(), dir, format).GetReport(ctx, "test-run-0", "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Tests[0].Answers, want.Tests[0].Answers) {
					t.Errorf("the test's answers read back as %+v; want %+v", got.Tests[0].Answers, want.Tests[0].Answers)
				}
				if len(got.Tests[1].Answers) != 0 {
					t.Errorf("a test with no answers read back with %+v", got.Tests[1].Answers)
				}
				if !reflect.DeepEqual(got.SetAnswers, want.SetAnswers) {
					t.Errorf("the set's answers read back as %+v; want %+v", got.SetAnswers, want.SetAnswers)
				}
			})
		}
	}
}

// Why keploy named no process a call came from (it runs in a PID namespace of
// its own), and the kernel pid, travel with the call and are never written to
// a report, in either format: a named process's comm and argv[0] are; the
// report the replay holds keeps them all.
func TestAReportHoldsNoKernelPid(t *testing.T) {
	unnamed := &models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
	named := &models.Origin{Comm: "sqs", Exe: "./sqs"}
	call := func(src *models.Origin, answer string) models.UnmatchedCall {
		return models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Kind: models.MissInTest, Answer: answer, Source: src}
	}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			report := goldenReport()
			report.Tests[0].Answers = []models.UnmatchedCall{call(unnamed, models.AnswerByKind), call(named, models.AnswerByKind)}
			report.Tests[0].FailureInfo.UnmatchedCalls = []models.UnmatchedCall{call(unnamed, "")}
			report.SetAnswers = []models.UnmatchedCall{call(unnamed, models.AnswerProvisional)}
			report.UnclaimedCalls = []models.UnmatchedCall{call(unnamed, "")}
			if err := NewWithFormat(zap.NewNop(), dir, format).InsertReport(ctx, "test-run-0", "test-set-0", &report); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "test-run-0", "test-set-0-report."+format.FileExtension()))
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{"2912345", models.UnnamedPIDNamespace, "source: {}", `"source":{}`} {
				if strings.Contains(string(raw), leak) {
					t.Fatalf("the %s report holds %q:\n%s", format.FileExtension(), leak, raw)
				}
			}
			got, err := NewWithFormat(zap.NewNop(), dir, format).GetReport(ctx, "test-run-0", "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			if a := got.Tests[0].Answers; len(a) != 2 || a[0].Source != nil || a[1].Source == nil || *a[1].Source != *named {
				t.Fatalf("the answers read back as %+v", a)
			}
			if report.Tests[0].Answers[0].Source != unnamed || report.SetAnswers[0].Source != unnamed || report.UnclaimedCalls[0].Source != unnamed || report.Tests[0].FailureInfo.UnmatchedCalls[0].Source != unnamed {
				t.Fatal("writing the report edited the replay's report")
			}
		})
	}
}
