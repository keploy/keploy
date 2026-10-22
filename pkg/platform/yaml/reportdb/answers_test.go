package reportdb

import (
	"context"
	"reflect"
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
		Differs: []string{"body.documents[0]._id"}, Source: &models.Origin{Comm: "sqs", Exe: "./sqs"}}
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
