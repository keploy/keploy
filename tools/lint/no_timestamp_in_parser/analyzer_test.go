package notimestampinparser

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer exercises the three scenarios the rule must distinguish:
//   - goodparser:  a V2 parser that sources timestamps from chunk fields and
//     must produce zero diagnostics.
//   - badparser:   a parser that calls time.Now() at record time and must
//     produce exactly one diagnostic (asserted via // want).
//   - suppressed:  parser-scoped file with per-call `// allow:time.Now`
//     opt-outs that must produce zero diagnostics.
//
// All three fixtures live under testdata/src/<pkg>/recorder/record_v2.go so
// the analyzer's scope check (basename ends with "_v2.go") fires. The
// production analogue is pkg/agent/proxy/integrations/<proto>/recorder/
// record_v2.go and similar V2-suffixed files across the integrations /
// enterprise repos.
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, Analyzer,
		"goodparser/recorder",
		"badparser/recorder",
		"suppressed/recorder",
	)
}
