package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The set's log line for a repeat of a recording made outside every test
// says it answered a call outside every test, never an earlier test's; one
// of an earlier test's recording names that test, or an earlier test.
func TestARepeatLineNamesARecordingMadeOutsideEveryTest(t *testing.T) {
	call := func(n *models.RepeatNote) *models.UnmatchedCall {
		return &models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /echo/rates", Repeat: n}
	}
	for _, c := range []struct {
		note      *models.RepeatNote
		want, not string
	}{
		{&models.RepeatNote{Mock: "mock-0", OutsideTests: true}, "repeats mock-0, answered for a call outside every test", "an earlier test"},
		{&models.RepeatNote{Mock: "mock-0", OutsideTests: true, Test: "rates::TestQuote"}, "answered for a call outside every test", "rates::TestQuote"},
		{&models.RepeatNote{Mock: "mock-0"}, "repeats mock-0, answered for an earlier test", "outside every test"},
		{&models.RepeatNote{Mock: "mock-0", Test: "rates::TestQuote"}, "answered for rates::TestQuote", "outside every test"},
	} {
		got := repeatLine("rates::TestReload", call(c.note))
		if !strings.Contains(got, c.want) || strings.Contains(got, c.not) {
			t.Errorf("note %+v: line %q, want it to say %q and not %q", c.note, got, c.want, c.not)
		}
	}
}
