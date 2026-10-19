package logtest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// A line holds the canary however a field wrote the bytes around it.
func TestCarriesFindsTheCanaryInEachFormAFieldWrites(t *testing.T) {
	const canary = "kp-canary-logtest-7d1e0b"
	for _, pre := range []string{"", "a", "ab", "abc"} {
		b := []byte(pre + canary + "tail")
		for name, line := range map[string]string{
			"text":   string(b),
			"base64": base64.StdEncoding.EncodeToString(b),
			"hex":    hex.EncodeToString(b),
			"fmt":    fmt.Sprint(b),
		} {
			if !carries(line, canary) {
				t.Errorf("%s after %q: %q not found in %q", name, pre, canary, line)
			}
		}
	}
	if carries(`{"len":24,"fp":"2cf24dba5fb0"}`, canary) {
		t.Error("a length and fingerprint taken for the canary")
	}
}

// failures is a testing.TB that keeps what CheckDisabledCost fails with.
type failures struct {
	testing.TB
	failed string
}

func (f *failures) Helper()                           {}
func (f *failures) Fatalf(format string, args ...any) { f.failed = fmt.Sprintf(format, args...) }

// The cost check fails a line that builds its logcontent field before its
// level is checked, and passes one that checks first.
func TestCheckDisabledCostFindsAFieldBuiltBeforeTheLevel(t *testing.T) {
	if raceEnabled {
		t.Skip("CheckDisabledCost skips under -race")
	}
	logger := Disabled()
	value := strings.Repeat("v", 16)
	built := &failures{}
	CheckDisabledCost(built, func() { logger.Debug("line", logcontent.String("value", value)) })
	if built.failed == "" {
		t.Fatal("a line that builds its logcontent field before its level is checked passed")
	}
	checked := &failures{}
	CheckDisabledCost(checked, func() {
		if ce := logger.Check(zap.DebugLevel, "line"); ce != nil {
			ce.Write(logcontent.String("value", value))
		}
	})
	if checked.failed != "" {
		t.Fatalf("a line that checks its level first failed: %s", checked.failed)
	}
}
