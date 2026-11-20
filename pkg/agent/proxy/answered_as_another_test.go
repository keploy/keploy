package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call answered with another call's recording (models.AnsweredAsAnother),
// with the proven switch off, reaches the test's read with the misses, its
// answer kept, and like an answer by kind it changes no verdict of keploy
// test: it counts against no test (missCounts), and one dropped once its
// window holds as many answers as it keeps leaves the read complete, where a
// pairing's (models.AnswerProvisional) loss does not.
func TestACallAnsweredWithAnotherCallsRecordingIsReadAndNoMiss(t *testing.T) {
	answered := func(answer string) error {
		return models.NewMockMismatchError(models.ErrAnswered, &models.MockMismatchReport{
			Protocol: "HTTP", ActualSummary: "GET /users/3", Answer: answer, ClosestMock: "mock-user-1",
			Differs: []string{"url.path.1"}, Diff: "GET /users/3 was answered with the recording of GET /users/1",
		})
	}
	for _, answer := range []string{models.AnswerOtherID, models.AnswerClosestBody} {
		t.Run(answer, func(t *testing.T) {
			if missCounts(asParserError(answered(answer))) {
				t.Errorf("missCounts says a %q answer counts against its test", answer)
			}
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			p.setMockManager(mm)
			p.BeginTestErrorCapture()
			p.sendMockNotFoundError(answered(answer))
			got, err := p.GetMockErrors(context.Background())
			if err != nil || len(got) != 1 {
				t.Fatalf("want the one answered call, got %+v (%v)", got, err)
			}
			c := got[0]
			if c.Answer != answer || c.ClosestMock != "mock-user-1" || len(c.Differs) != 1 || c.Diff == "" || c.CountsAgainstTest() {
				t.Fatalf("the read call is %+v, want the answer kept, naming its recording and where it differs, counting against no test", c)
			}

			// One dropped for room is noted as an answer by kind is
			// (noteAnswerDropped), never as a lost miss.
			if !isByKindAnswer(asParserError(answered(answer))) {
				t.Errorf("a dropped %q answer is noted as a lost miss, which leaves its read incomplete", answer)
			}
		})
	}
	if !missCounts(asParserError(answered(models.AnswerProvisional))) {
		t.Error("missCounts says a pairing's answer, which record's judging reads, is lost as no miss")
	}
}

// asParserError is err as the sink files it: the ParserError missCounts
// reads, carrying err's report.
func asParserError(err error) error {
	pe := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: err}
	if r, ok := err.(interface {
		MismatchReport() *models.MockMismatchReport
	}); ok {
		pe.MismatchReport = r.MismatchReport()
	}
	return pe
}
