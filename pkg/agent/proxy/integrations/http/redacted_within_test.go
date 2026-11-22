package http

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A JSON string that holds a redacted value among other text (a URL whose
// query value the obfuscator redacted) is redacted whole by any pattern
// that names the value, read as MySQL's statements are read
// (util.NoiseChecker.IsNoisyWithin): the anchors of each alternative of an
// alternation are dropped, and so are anchors after a flag group. The live
// call's value there is never shown, at a strict miss or a fuzzy one, in
// the report or a debug line.
func TestAStringHoldingAValueAnAlternationOrAFlaggedPatternNamesIsRedacted(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	recordedBody := `{"cb":"https://hook.example/?k=REDACTED_abc&z=1","a":"alpha1"}`
	liveBody := `{"cb":"https://hook.example/?k=` + liveSecret + `&z=1","a":"alpha1"}`
	for _, noise := range [][]string{
		{`^REDACTED_abc$|^REDACTED_def$`},
		{`(?i)^REDACTED_abc$`},
		{`\AREDACTED_abc\z`},
	} {
		for _, mode := range []string{"off", "on"} {
			t.Run(noise[0]+"/"+mode, func(t *testing.T) {
				m := recorded("m", "/x", jsonHdr, recordedBody)
				m.Noise = noise
				if p := placesOf(m); !p.fields["cb"] {
					t.Errorf("the recording's places hold no cb field: %+v", p.fields)
				}
				core, logs := observer.New(zap.DebugLevel)
				carriesNo(t, poolMiss(t, &HTTP{Logger: zap.New(core)}, mode, "/x", jsonHdr, liveBody, m), liveSecret)
				noValueLogged(t, logs, []string{liveSecret})
			})
		}
	}
}
