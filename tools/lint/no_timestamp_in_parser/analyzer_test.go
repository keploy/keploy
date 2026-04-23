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
// All three fixtures live under testdata/src/<pkg>/recorder/ so the
// analyzer's path-based scope check ("contains /recorder/") fires, mirroring
// production layout (pkg/agent/proxy/integrations/<proto>/recorder/...).
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, Analyzer,
		"goodparser/recorder",
		"badparser/recorder",
		"suppressed/recorder",
	)
}
