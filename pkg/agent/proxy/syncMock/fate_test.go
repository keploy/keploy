package manager

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// What a line says becomes of the test cases over a span follows the record
// streams open on the manager: kept while every one charges what its test
// cases lost, both while some do, and left out otherwise: with none open,
// with only streams that do not charge it, and always with the proven switch
// off. A stream's closer counts it out once, however often it is called.
//
// Not parallel: it sets the switch.
func TestTheOpenRecordStreamsSayWhatBecomesOfTheTestCasesOverASpan(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(nil)
	if f := m.LossFate(); f != models.LossFateLeftOut {
		t.Fatalf("no stream open: %v, want left out", f)
	}
	closePlain := m.OpenRecordStream(false)
	if f := m.LossFate(); f != models.LossFateLeftOut {
		t.Fatalf("a stream that does not charge open: %v, want left out", f)
	}
	closeCharging := m.OpenRecordStream(true)
	if f := m.LossFate(); f != models.LossFateMixed {
		t.Fatalf("one stream of each kind open: %v, want mixed", f)
	}
	closePlain()
	closePlain()
	if f := m.LossFate(); f != models.LossFateKept {
		t.Fatalf("only a stream that charges open (the other's closer called twice): %v, want kept", f)
	}
	second := m.OpenRecordStream(true)
	closeCharging()
	if f := m.LossFate(); f != models.LossFateKept {
		t.Fatalf("a second stream that charges still open: %v, want kept", f)
	}
	t.Setenv(provenswitch.Env, "")
	if f := m.LossFate(); f != models.LossFateLeftOut {
		t.Fatalf("with the switch off: %v, want left out", f)
	}
	t.Setenv(provenswitch.Env, "1")
	second()
	if f := m.LossFate(); f != models.LossFateLeftOut {
		t.Fatalf("every stream closed: %v, want left out", f)
	}
	var none *SyncMockManager
	none.OpenRecordStream(true)()
	if f := none.LossFate(); f != models.LossFateLeftOut {
		t.Fatalf("a nil manager: %v, want left out", f)
	}
}
