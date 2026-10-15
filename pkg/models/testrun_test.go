package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// What keploy cannot capture at all is written as cannot_capture in a test's
// failure info, in YAML and JSON: not_judged would read as the test's whole
// not-judged state (NotJudged), which covers more than this one reason.
func TestCannotCaptureIsWrittenAsCannotCapture(t *testing.T) {
	fi := FailureInfo{CannotCapture: CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1, What: "/run/app/db.sock"}}}
	y, err := yaml.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}
	for format, b := range map[string]string{"yaml": string(y), "json": string(j)} {
		if !strings.Contains(b, "cannot_capture") || strings.Contains(b, "not_judged") {
			t.Fatalf("%s: %s, want the losses under cannot_capture", format, b)
		}
	}
	var back FailureInfo
	if err := yaml.Unmarshal(y, &back); err != nil || len(back.CannotCapture) != 1 || back.CannotCapture[0].What != "/run/app/db.sock" {
		t.Fatalf("read back: %+v, %v", back, err)
	}
}
