package mock

import (
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"gopkg.in/yaml.v3"
)

// What keploy cannot capture at all is written as cannotCapture in a run's
// receipt, in its tests receipt and in a test's evidence there, in YAML and
// JSON, as a test's failure info writes it cannot_capture: notJudged would
// read as the whole not-judged state, which covers more than this one reason.
func TestReceiptsWriteWhatKeployCannotCaptureAsCannotCapture(t *testing.T) {
	lost := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/run/app/db.sock"}}
	for name, v := range map[string]any{
		"receipt":       Receipt{CannotCapture: lost},
		"tests receipt": TestsReceipt{CannotCapture: lost},
		"test evidence": TestReceipt{Name: "o::A", Evidence: &models.ScopeEvidence{CannotCapture: lost}},
	} {
		y, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for format, b := range map[string]string{"yaml": string(y), "json": string(j)} {
			if !strings.Contains(b, "cannotCapture") || strings.Contains(b, "notJudged") {
				t.Fatalf("%s %s: %s, want the losses under cannotCapture", name, format, b)
			}
		}
	}
	var back TestsReceipt
	if err := yaml.Unmarshal([]byte("cannotCapture:\n  - kind: unix_socket\n    count: 1\n    what: /run/app/db.sock\n"), &back); err != nil || len(back.CannotCapture) != 1 || back.CannotCapture[0].What != "/run/app/db.sock" {
		t.Fatalf("read back: %+v, %v", back, err)
	}
}
