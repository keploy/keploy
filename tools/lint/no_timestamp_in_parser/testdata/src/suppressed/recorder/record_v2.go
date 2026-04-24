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

// LogUptimeBlockComment exercises the BLOCK-comment suppression form
// `/* allow:time.Now */`. README.md documents both line and block
// comments as supported, but the analysistest fixtures previously
// only exercised the line form. Without this fixture the
// block-comment branch in collectSuppressLines (analyzer.go ~L201)
// would be untested and a future refactor could silently regress it.
func LogUptimeBlockComment(boot time.Time) {
	/* allow:time.Now */
	fmt.Println("uptime_ns=", time.Since(boot).Nanoseconds())
}

// LogBootTwoLineBlock exercises a MULTI-line block comment whose
// last line sits immediately above the time.Now() call. The
// analyzer keys off the comment's End line, so a two-line block
// must also suppress the call directly below it.
func LogBootTwoLineBlock() {
	/* allow:time.Now — telemetry only,
	   not a mock-matcher timestamp. */
	fmt.Println("boot_at=", time.Now().UnixNano())
}
