package log

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Structured zap.Time fields must format as pure RFC3339Nano, never carrying
// the console line prefix ("🐰 Keploy: " or "🐰 Keploy(agent): "), while the
// console line itself keeps its prefix ahead of the entry timestamp.
func TestZapTimeFieldHasNoLinePrefix(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Cleanup(TestOnlyResetSink)

	windowStart := time.Date(2026, 9, 24, 10, 0, 0, 123456789, time.UTC)
	wantTime := windowStart.Format(time.RFC3339Nano)

	for _, tc := range []struct {
		name       string
		build      func(t *testing.T) (*zap.Logger, error)
		wantPrefix string
	}{
		{"New (CLI)", func(t *testing.T) (*zap.Logger, error) { return newForTest(t), nil }, "🐰 Keploy: "},
		{"AddMode (agent)", func(*testing.T) (*zap.Logger, error) { return AddMode("agent") }, "🐰 Keploy(agent): "},
		{"ChangeColorEncoding (--disable-ansi)", func(*testing.T) (*zap.Logger, error) { return ChangeColorEncoding() }, "🐰 Keploy: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			LogCfg = defaultLogCfg()
			sinkFile := createForTest(t, strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(tc.name)+".log")
			setPrimarySink(sinkFile)

			logger, err := tc.build(t)
			if err != nil {
				t.Fatal(err)
			}
			logger.WithOptions(zap.WithClock(fixedClock{})).
				With(zap.Time("since", windowStart)).
				Info("window checked", zap.Time("windowStart", windowStart))

			got := readFile(t, PrimarySink().Name())
			if !strings.HasPrefix(got, tc.wantPrefix+wantTime) {
				t.Fatalf("line prefix mismatch:\n got %q\nwant prefix %q", got, tc.wantPrefix+wantTime)
			}
			jsonStart := strings.IndexByte(got, '{')
			if jsonStart < 0 {
				t.Fatalf("no JSON fields in line: %q", got)
			}
			var fields map[string]string
			if err := json.Unmarshal([]byte(strings.TrimSpace(got[jsonStart:])), &fields); err != nil {
				t.Fatalf("unmarshal JSON fields %q: %v", got[jsonStart:], err)
			}
			for _, key := range []string{"windowStart", "since"} {
				if fields[key] != wantTime {
					t.Errorf("field %s = %q, want pure RFC3339Nano %q (full line: %q)", key, fields[key], wantTime, got)
				}
			}
		})
	}
}
