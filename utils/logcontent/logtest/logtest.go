// Package logtest holds the lines a test drives through keploy's code to what
// logcontent lets them carry. It reads each line as zap's JSON encoder writes
// it, its message and every field, and finds a canary there in each form a
// field writes bytes in.
package logtest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Observe is a logger that keeps every entry at Debug and above, as the
// agent's debug file does, and what it keeps.
func Observe() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// Each runs f once with the switch off and once with it on, as subtests. A
// test that calls it must not call t.Parallel (logcontent.Set).
func Each(t *testing.T, f func(t *testing.T, on bool)) {
	t.Helper()
	for _, on := range []bool{false, true} {
		name := "off"
		if on {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			t.Cleanup(logcontent.Set(on))
			f(t, on)
		})
	}
}

// Check holds the kept lines to the switch: each of want must be the message
// of a kept line, and the canary must be in none of them with it off, and in
// a line of each of want with it on.
func Check(t testing.TB, logs *observer.ObservedLogs, canary string, on bool, want ...string) {
	t.Helper()
	logged := map[string]bool{}
	carrying := map[string]bool{}
	var leaks []string
	for _, e := range logs.AllUntimed() {
		logged[e.Message] = true
		line := encode(t, e)
		if !carries(line, canary) {
			continue
		}
		carrying[e.Message] = true
		leaks = append(leaks, line)
	}
	for _, m := range want {
		if !logged[m] {
			t.Fatalf("precondition: no line %q was logged; logged: %q", m, keys(logged))
		}
	}
	if !on {
		if len(leaks) > 0 {
			t.Fatalf("with the switch off, %d line(s) carry the canary:\n%s", len(leaks), strings.Join(leaks, "\n"))
		}
		return
	}
	for _, m := range want {
		if !carrying[m] {
			t.Fatalf("with the switch on, the line %q does not carry the canary", m)
		}
	}
}

func encode(t testing.TB, e observer.LoggedEntry) string {
	t.Helper()
	buf, err := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()).EncodeEntry(e.Entry, e.Context)
	if err != nil {
		t.Fatalf("encode the line %q: %v", e.Message, err)
	}
	defer buf.Free()
	return buf.String()
}

// carries reports whether line holds canary as text, as the base64 zap.Binary
// writes (at each of the three offsets it can start at in the bytes encoded),
// as hex, or as fmt prints a []byte.
func carries(line, canary string) bool {
	forms := []string{canary, hex.EncodeToString([]byte(canary)), strings.Trim(fmt.Sprint([]byte(canary)), "[]")}
	for pad := 0; pad < 3; pad++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, pad), canary...))
		// The groups of four that hold a pad byte or the padding are not the
		// canary's alone.
		from := 0
		if pad > 0 {
			from = 4
		}
		if len(enc)-4 > from {
			forms = append(forms, enc[from:len(enc)-4])
		}
	}
	for _, f := range forms {
		if strings.Contains(line, f) {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
