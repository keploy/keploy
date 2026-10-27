package generic

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// againStore is answeredStore whose open test's marked background
// recordings that answered are marked (integrations.AnsweredBackgroundReader).
type againStore struct {
	answeredStore
	marked []*models.Mock
}

func (s againStore) AnsweredBackground() []*models.Mock { return s.marked }

// A timer's exchange that judging marked as the application's background
// work answered once in its test's window, and the timer sends it again
// there. The second exchange is answered again with the marked recording,
// unchecked, and reported as such naming it, instead of failing the test as
// one exchange more than it recorded. An unmarked recording's second
// exchange, and any exchange with the switch off, still miss.
func TestABackgroundExchangeMadeAgainInItsWindowIsAnsweredAgain(t *testing.T) {
	tick := exchange("mock-bg", "PING")
	tick.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PONG"}}}}
	t.Setenv(provenswitch.Env, "1")

	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
	app, proxySide := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), proxySide, nil, againStore{answeredStore{answered: []*models.Mock{tick}}, []*models.Mock{tick}}, models.OutgoingOptions{})
	}()
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
		t.Fatalf("a background exchange made again got %q (%v); want mock-bg's PONG", got, err)
	}
	_ = app.Close()
	select {
	case err := <-done:
		// How the parser ends once the app closed is no part of the row.
		_ = err
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
		t.Fatalf("the exchange was reported as %v, with no report", reported)
	}
	if r := withReport.MismatchReport(); r.Answer != models.AnswerBackgroundAgain || r.ClosestMock != "mock-bg" || r.CountsAgainstTest() {
		t.Fatalf("reported %+v; want answered again naming mock-bg", r)
	}

	// Unmarked: the test's own exchange made once more than it recorded.
	if r := missReport(t, againStore{answeredStore: answeredStore{answered: []*models.Mock{tick}}}, "PING"); r.MissKind != models.MissExtraCall || r.Answer != "" {
		t.Fatalf("an unmarked recording's exchange made again was reported %+v; want the extra-call miss", r)
	}
	// Marked, but another exchange.
	if r := missReport(t, againStore{answeredStore{answered: []*models.Mock{tick}}, []*models.Mock{tick}}, "SET k v"); r.Answer != "" || r.MissKind == models.MissExtraCall {
		t.Fatalf("an exchange that repeats no marked recording was reported %+v; want a miss", r)
	}
	t.Setenv(provenswitch.Env, "0")
	if r := missReport(t, againStore{answeredStore{answered: []*models.Mock{tick}}, []*models.Mock{tick}}, "PING"); r.Answer != "" {
		t.Fatalf("with the switch off a background exchange made again was reported %+v; want the miss main makes", r)
	}
}
