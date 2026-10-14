package log

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// fixedClock stamps every entry with the same instant, in UTC, so a pinned
// line does not depend on when or where the test runs.
type fixedClock struct{}

func (fixedClock) Now() time.Time                         { return time.Date(2026, 9, 24, 10, 0, 0, 123456789, time.UTC) }
func (fixedClock) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

// logKeployColours logs every shape in which Keploy puts colour into a log
// line, the way its call sites do.
func logKeployColours(logger *zap.Logger) {
	// pkg/http2.go (zap.String) and pkg/util.go (zap.Any): the test case being started.
	logger.Info("starting test for", zap.String("test case", models.HighlightString("get-user-1")), zap.String("test set", models.HighlightString("test-set-0")))
	logger.Info("starting test for", zap.Any("test case", models.HighlightString("get-user-1")), zap.Any("test set", models.HighlightString("test-set-0")))
	// pkg/service/replay/replay.go: the test set being run.
	logger.Info("running", zap.String("test-set", models.HighlightString("test-set-0")), zap.Int("attempt", 1))
	// replay.go: a test case's result, failed and passed.
	logger.Info("result", zap.String("testcase id", models.HighlightFailingString("get-user-1")), zap.String("testset id", models.HighlightFailingString("test-set-0")), zap.String("passed", models.HighlightFailingString(false)))
	logger.Info("result", zap.String("testcase id", models.HighlightPassingString("get-user-1")), zap.String("testset id", models.HighlightPassingString("test-set-0")), zap.String("passed", models.HighlightPassingString(true)))
	// replay.go: the coverage line, a coloured message through the sugared logger.
	logger.Sugar().Infoln(models.HighlightPassingString("Total Coverage Percentage: ", "85.5%"))
	// cli/provider/cmd.go (Info) and replay.go (utils.LogError): a message with a coloured command in it.
	logger.Info(fmt.Sprintf("No test-sets found. Please record testcases using %s command", models.HighlightGrayString("keploy record")))
	logger.Error(fmt.Sprintf("No test sets found in the keploy folder. Please record testcases using %s command", models.HighlightGrayString("keploy record")), zap.Error(nil))
	// Every level's own colour (CapitalColorLevelEncoder).
	logger.Debug("debug level")
	logger.Warn("warn level")
	// A coloured value the logger carries as context rather than the entry.
	logger.With(zap.String("test-set", models.HighlightString("test-set-0"))).Info("with context", zap.String("k", "v"))
	// No colour at all: JSON, a quote and backslashes in a field stay as zap wrote them.
	logger.Info("request", zap.String("body", `{"name":"a\"b","path":"C:\\temp\\new"}`), zap.Strings("ids", []string{"a", "b"}))
}

// Keploy's own colours reach the terminal byte for byte as they did before the
// encoders were made safe. Each shape Keploy logs colour in goes through the
// real logger constructors, and the output is pinned to what main
// (4800a30ec) wrote for it: the pins were generated from, and pass against,
// that code. The one change is under --disable-ansi with the Highlight*
// colours still on, where main wrote a colour in the message raw.
func TestKeployColoursAreByteIdentical(t *testing.T) {
	t.Chdir(t.TempDir()) // New() opens keploy-logs.txt in the working directory.
	origNoColor, origAnsiDisabled := color.NoColor, models.IsAnsiDisabled
	t.Cleanup(TestOnlyResetSink) // the lazy os.Stdout, which PrimarySink would report as an explicit one
	t.Cleanup(func() { color.NoColor, models.IsAnsiDisabled = origNoColor, origAnsiDisabled })
	// fatih/color only colours on a terminal; the helpers must colour here.
	color.NoColor = false

	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (*zap.Logger, error)
		// ansiDisabled is models.IsAnsiDisabled, which cli/provider sets for
		// --disable-ansi and which turns the Highlight* colours off.
		ansiDisabled bool
		want         []string
	}{
		{"New", func(t *testing.T) (*zap.Logger, error) { return newForTest(t), nil }, false, goldenNew},
		{"ChangeLogLevel (--debug)", func(*testing.T) (*zap.Logger, error) { return ChangeLogLevel(zapcore.DebugLevel) }, false, goldenDebug},
		{"AddMode (agent)", func(*testing.T) (*zap.Logger, error) { return AddMode("agent") }, false, goldenAgent},
		{"ChangeColorEncoding (--disable-ansi)", func(*testing.T) (*zap.Logger, error) { return ChangeColorEncoding() }, true, goldenDisableANSI},
		// enterprise runs ChangeColorEncoding without setting
		// models.IsAnsiDisabled, so the Highlight* colours are still on. No
		// ESC reaches the terminal under --disable-ansi now, so a colour in the
		// message reads as the text a colour in a field always has there.
		{"ChangeColorEncoding, Highlight colours on", func(*testing.T) (*zap.Logger, error) { return ChangeColorEncoding() }, false, escapeESC(goldenDisableANSIColoursOn)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			LogCfg = defaultLogCfg()
			setPrimarySink(createForTest(t, strings.NewReplacer(" ", "-", "(", "", ")", "", ",", "").Replace(tc.name)+".console"))

			logger, err := tc.build(t)
			if err != nil {
				t.Fatal(err)
			}
			models.IsAnsiDisabled = tc.ansiDisabled
			logKeployColours(logger.WithOptions(zap.WithClock(fixedClock{})))
			models.IsAnsiDisabled = origAnsiDisabled

			got := strings.SplitAfter(readFile(t, PrimarySink().Name()), "\n")
			got = got[:len(got)-1] // after the last line ending
			if len(got) != len(tc.want) {
				t.Errorf("%d lines, want %d", len(got), len(tc.want))
			}
			for i := range min(len(got), len(tc.want)) {
				if got[i] != tc.want[i] {
					t.Errorf("line %d changed:\n got %q\nwant %q", i+1, got[i], tc.want[i])
				}
			}
		})
	}
}

// escapeESC writes each raw ESC in lines as zap writes one in a field.
func escapeESC(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.ReplaceAll(l, "\x1b", `\u001b`)
	}
	return out
}

// The lines main (4800a30ec) wrote for logKeployColours, one set per logger.
// The Highlight* colours are fatih/color's: 38;5;208 (orange) resets with
// 0;25;0, the rest with 0. Under --disable-ansi with them still on, a colour in
// a field has always been text, as zap escaped it, while main wrote one in the
// message raw.

var goldenNew = []string{
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\trunning\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"attempt\": 1}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[31mget-user-1\x1b[0m\", \"testset id\": \"\x1b[31mtest-set-0\x1b[0m\", \"passed\": \"\x1b[31mfalse\x1b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[32mget-user-1\x1b[0m\", \"testset id\": \"\x1b[32mtest-set-0\x1b[0m\", \"passed\": \"\x1b[32mtrue\x1b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tNo test-sets found. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[31mERROR\x1b[0m\tNo test sets found in the keploy folder. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[33mWARN\x1b[0m\twarn level\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\twith context\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"k\": \"v\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\trequest\t{\"body\": \"{\\\"name\\\":\\\"a\\\\\\\"b\\\",\\\"path\\\":\\\"C:\\\\\\\\temp\\\\\\\\new\\\"}\", \"ids\": [\"a\", \"b\"]}\n",
}

var goldenDebug = []string{
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\trunning\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"attempt\": 1}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[31mget-user-1\x1b[0m\", \"testset id\": \"\x1b[31mtest-set-0\x1b[0m\", \"passed\": \"\x1b[31mfalse\x1b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[32mget-user-1\x1b[0m\", \"testset id\": \"\x1b[32mtest-set-0\x1b[0m\", \"passed\": \"\x1b[32mtrue\x1b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tNo test-sets found. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[31mERROR\x1b[0m\tNo test sets found in the keploy folder. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[35mDEBUG\x1b[0m\tdebug level\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[33mWARN\x1b[0m\twarn level\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\twith context\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"k\": \"v\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\trequest\t{\"body\": \"{\\\"name\\\":\\\"a\\\\\\\"b\\\",\\\"path\\\":\\\"C:\\\\\\\\temp\\\\\\\\new\\\"}\", \"ids\": [\"a\", \"b\"]}\n",
}

var goldenAgent = []string{
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\tstarting test for\t{\"test case\": \"\x1b[38;5;208mget-user-1\x1b[0;25;0m\", \"test set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\"}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\trunning\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"attempt\": 1}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[31mget-user-1\x1b[0m\", \"testset id\": \"\x1b[31mtest-set-0\x1b[0m\", \"passed\": \"\x1b[31mfalse\x1b[0m\"}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\tresult\t{\"testcase id\": \"\x1b[32mget-user-1\x1b[0m\", \"testset id\": \"\x1b[32mtest-set-0\x1b[0m\", \"passed\": \"\x1b[32mtrue\x1b[0m\"}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\t\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\tNo test-sets found. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[31mERROR\x1b[0m\tNo test sets found in the keploy folder. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[33mWARN\x1b[0m\twarn level\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\twith context\t{\"test-set\": \"\x1b[38;5;208mtest-set-0\x1b[0;25;0m\", \"k\": \"v\"}\n",
	"🐰 Keploy(agent): 2026-09-24T10:00:00Z\t\x1b[34mINFO\x1b[0m\trequest\t{\"body\": \"{\\\"name\\\":\\\"a\\\\\\\"b\\\",\\\"path\\\":\\\"C:\\\\\\\\temp\\\\\\\\new\\\"}\", \"ids\": [\"a\", \"b\"]}\n",
}

var goldenDisableANSI = []string{
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tstarting test for\t{\"test case\": \"get-user-1\", \"test set\": \"test-set-0\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tstarting test for\t{\"test case\": \"get-user-1\", \"test set\": \"test-set-0\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\trunning\t{\"test-set\": \"test-set-0\", \"attempt\": 1}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tresult\t{\"testcase id\": \"get-user-1\", \"testset id\": \"test-set-0\", \"passed\": \"false\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tresult\t{\"testcase id\": \"get-user-1\", \"testset id\": \"test-set-0\", \"passed\": \"true\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tTotal Coverage Percentage: 85.5%\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tNo test-sets found. Please record testcases using keploy record command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tERROR\tNo test sets found in the keploy folder. Please record testcases using keploy record command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tWARN\twarn level\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\twith context\t{\"test-set\": \"test-set-0\", \"k\": \"v\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\trequest\t{\"body\": \"{\\\"name\\\":\\\"a\\\\\\\"b\\\",\\\"path\\\":\\\"C:\\\\\\\\temp\\\\\\\\new\\\"}\", \"ids\": [\"a\", \"b\"]}\n",
}

var goldenDisableANSIColoursOn = []string{
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tstarting test for\t{\"test case\": \"\\u001b[38;5;208mget-user-1\\u001b[0;25;0m\", \"test set\": \"\\u001b[38;5;208mtest-set-0\\u001b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tstarting test for\t{\"test case\": \"\\u001b[38;5;208mget-user-1\\u001b[0;25;0m\", \"test set\": \"\\u001b[38;5;208mtest-set-0\\u001b[0;25;0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\trunning\t{\"test-set\": \"\\u001b[38;5;208mtest-set-0\\u001b[0;25;0m\", \"attempt\": 1}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tresult\t{\"testcase id\": \"\\u001b[31mget-user-1\\u001b[0m\", \"testset id\": \"\\u001b[31mtest-set-0\\u001b[0m\", \"passed\": \"\\u001b[31mfalse\\u001b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tresult\t{\"testcase id\": \"\\u001b[32mget-user-1\\u001b[0m\", \"testset id\": \"\\u001b[32mtest-set-0\\u001b[0m\", \"passed\": \"\\u001b[32mtrue\\u001b[0m\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\t\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tNo test-sets found. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tERROR\tNo test sets found in the keploy folder. Please record testcases using \x1b[90mkeploy record\x1b[0m command\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tWARN\twarn level\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\twith context\t{\"test-set\": \"\\u001b[38;5;208mtest-set-0\\u001b[0;25;0m\", \"k\": \"v\"}\n",
	"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\trequest\t{\"body\": \"{\\\"name\\\":\\\"a\\\\\\\"b\\\",\\\"path\\\":\\\"C:\\\\\\\\temp\\\\\\\\new\\\"}\", \"ids\": [\"a\", \"b\"]}\n",
}

// legacyANSIEncode is the ansiConsole encoder as main shipped it: zap's console
// line with every \u001b and \u001B in it turned into a raw ESC.
func legacyANSIEncode(cfg zapcore.EncoderConfig, with []zapcore.Field, ent zapcore.Entry, fields []zapcore.Field) (string, error) {
	enc := zapcore.NewConsoleEncoder(cfg)
	for _, f := range with {
		f.AddTo(enc)
	}
	buf, err := enc.EncodeEntry(ent, fields)
	if err != nil {
		return "", err
	}
	defer buf.Free()
	b := bytes.ReplaceAll(buf.Bytes(), []byte(`\u001b`), []byte("\x1b"))
	return string(bytes.ReplaceAll(b, []byte(`\u001B`), []byte("\x1b"))), nil
}

// legacyPlainEncode is the --disable-ansi encoder as main shipped it, zap's
// console encoder, with each raw ESC it wrote as zap writes one in a field: no
// ESC reaches the terminal under --disable-ansi now.
func legacyPlainEncode(cfg zapcore.EncoderConfig, with []zapcore.Field, ent zapcore.Entry, fields []zapcore.Field) (string, error) {
	enc := zapcore.NewConsoleEncoder(cfg)
	for _, f := range with {
		f.AddTo(enc)
	}
	buf, err := enc.EncodeEntry(ent, fields)
	if err != nil {
		return "", err
	}
	defer buf.Free()
	return strings.ReplaceAll(buf.String(), "\x1b", `\u001b`), nil
}

// Whatever carries Keploy's colours — any level, the message, a string, an
// array, a reflected map, an error, a namespace, the logger's context — and
// whatever surrounds them — a logger name, a caller, a stacktrace — each
// console encoder writes exactly what the encoder main shipped for it wrote,
// under the encoder configurations Keploy runs with; under --disable-ansi,
// with no raw ESC.
func TestEncodersMatchTheOldOnesOnKeployColours(t *testing.T) {
	orange := "\x1b[38;5;208mtest-set-0\x1b[0;25;0m" // models.HighlightString
	red := "\x1b[31mget-user-1\x1b[0m"               // models.HighlightFailingString
	debug := defaultLogCfg().EncoderConfig           // what ChangeLogLevel(zap.DebugLevel) adds
	debug.EncodeCaller = zapcore.ShortCallerEncoder
	plain := defaultLogCfg().EncoderConfig // what ChangeColorEncoding sets
	plain.EncodeLevel = zapcore.CapitalLevelEncoder

	encoders := []struct {
		name   string
		cfg    zapcore.EncoderConfig
		encode func(cfg zapcore.EncoderConfig) zapcore.Encoder
		legacy func(cfg zapcore.EncoderConfig, with []zapcore.Field, ent zapcore.Entry, fields []zapcore.Field) (string, error)
	}{
		{"ansiConsole (New)", defaultLogCfg().EncoderConfig, NewANSIConsoleEncoder, legacyANSIEncode},
		{"ansiConsole (--debug)", debug, NewANSIConsoleEncoder, legacyANSIEncode},
		{"console (--disable-ansi)", plain, func(cfg zapcore.EncoderConfig) zapcore.Encoder { return newPlainConsoleEncoder(cfg) }, legacyPlainEncode},
	}
	fieldSets := map[string][]zapcore.Field{
		"none":      nil,
		"string":    {zap.String("test case", red), zap.String("test set", orange), zap.Int("attempt", 1)},
		"array":     {zap.Strings("test sets", []string{orange, red})},
		"reflected": {zap.Any("result", map[string]any{"testcase id": red, "passed": false})},
		"error":     {zap.Error(errors.New("failed: " + red))},
		"namespace": {zap.Namespace("run"), zap.String("test set", orange)},
		"plain":     {zap.String("body", `{"a":"b\"c","p":"C:\\x"}`)},
	}
	withs := map[string][]zapcore.Field{"": nil, "with": {zap.String("test-set", orange)}}
	stack := "goroutine 1 [running]:\ngo.keploy.io/server/v3/pkg/service/replay.(*Replayer).Start()\n\t/src/pkg/service/replay/replay.go:891 +0x1d"
	caller := zapcore.NewEntryCaller(0, "/src/pkg/service/replay/replay.go", 891, true)

	for _, e := range encoders {
		for fieldsName, fields := range fieldSets {
			for withName, with := range withs {
				for _, level := range []zapcore.Level{zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel, zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel} {
					for _, msg := range []string{"result", "\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m", ""} {
						for _, ent := range []zapcore.Entry{
							{Level: level, Time: fixedClock{}.Now(), Message: msg},
							{Level: level, Time: fixedClock{}.Now(), Message: msg, LoggerName: "jsse_attribution", Caller: caller, Stack: stack},
						} {
							enc := e.encode(e.cfg)
							for _, f := range with {
								f.AddTo(enc)
							}
							buf, err := enc.EncodeEntry(ent, fields)
							if err != nil {
								t.Fatal(err)
							}
							got := buf.String()
							buf.Free()
							want, err := e.legacy(e.cfg, with, ent, fields)
							if err != nil {
								t.Fatal(err)
							}
							if got != want {
								t.Errorf("%s, %s fields, %q context, %v, message %q, stack %t:\n got %q\nwant %q", e.name, fieldsName, withName, level, msg, ent.Stack != "", got, want)
							}
						}
					}
				}
			}
		}
	}
}
