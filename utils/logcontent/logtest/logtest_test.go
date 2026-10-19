package logtest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
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
