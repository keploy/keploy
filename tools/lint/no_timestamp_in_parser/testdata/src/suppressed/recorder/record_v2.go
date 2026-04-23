// Package recorder is a fixture for analysistest: a parser file that uses
// time.Now() but tags each call with the // allow:time.Now magic comment.
// Expect zero diagnostics.
package recorder

import (
	"fmt"
	"time"
)

// LogBootBanner emits a telemetry-style log line. The timestamp here is for
// observability, not for mock replay — so it is opted out explicitly.
func LogBootBanner() {
	// allow:time.Now
	fmt.Println("boot at", time.Now())
}

// UptimeSinceBoot is another telemetry site; time.Since calls time.Now
// internally and is therefore also covered by the rule, but the opt-out
// comment exempts it.
func UptimeSinceBoot(boot time.Time) time.Duration {
	// allow:time.Now
	return time.Since(boot)
}
