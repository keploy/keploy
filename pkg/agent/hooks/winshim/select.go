//go:build windows && amd64

package winshim

import (
	"os"
	"strings"
)

// Enabled reports whether Keploy should instrument the application it launches.
//
// It is true by default: this is the only Windows interception backend, so
// turning it off means no interception at all. The switch exists as an escape
// hatch — if instrumenting a particular application ever goes wrong, a user can
// get their run back to an uninstrumented launch without downgrading Keploy,
// and tell us what broke.
func Enabled() bool {
	v, ok := os.LookupEnv(EnvDisable)
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return false
	default:
		return true
	}
}
