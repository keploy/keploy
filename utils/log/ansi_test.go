package log

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// liveSGR matches an SGR colour sequence at the start of a string: the one
// control sequence a Keploy log line may carry to the terminal, when its
// parameters are ones terminals read alike (readAlike).
var liveSGR = regexp.MustCompile(`^\x1b\[([0-9;]*)m`)

// keptSGR returns the SGR sequence at the start of s that a Keploy log line may
// carry to the terminal, or "".
func keptSGR(s string) string {
	if m := liveSGR.FindStringSubmatch(s); m != nil && readAlike(m[1]) {
		return m[0]
	}
	return ""
}

// readAlike reports whether the Linux console and xterm read an SGR sequence
// with these parameters the same way, and the console reads none of them as
// its display mode, SGR 11 or 12, which stops it decoding UTF-8 so that the
// 0x9b ending Л or ě is a CSI to it. The console reads as vt.c's csi_m does: a
// colour after 38 or 48 in a form it knows and in full (vc_t416_color), and
// every other parameter on its own, the colour after 58 among them. xterm
// reads a colour after 38, 48 or 58, and each terminal reads one in another
// form, or cut short, its own way. Terminals agree on no parameter above 255,
// which no SGR has (the console keeps it in 32 bits), and on no sequence with
// more than 16 (the console ignores it; xterm.js acts on 32 of it).
func readAlike(params string) bool {
	var ps []int
	for _, p := range strings.Split(params, ";") {
		v, err := strconv.Atoi(p)
		if p == "" {
			v, err = 0, nil
		}
		if err != nil || v > 255 {
			return false
		}
		ps = append(ps, v)
	}
	if len(ps) > 16 {
		return false
	}
	// Where each reads a parameter on its own.
	var console, xterm []int
	for i := 0; i < len(ps); i++ {
		console = append(console, i)
		if ps[i] == 11 || ps[i] == 12 {
			return false
		}
		if ps[i] == 38 || ps[i] == 48 {
			switch i++; {
			case i < len(ps) && ps[i] == 5 && i+1 < len(ps):
				i++
			case i < len(ps) && ps[i] == 2 && i+3 < len(ps):
				i += 3
			}
		}
	}
	for i := 0; i < len(ps); i++ {
		xterm = append(xterm, i)
		if ps[i] == 38 || ps[i] == 48 || ps[i] == 58 {
			switch {
			case i+2 < len(ps) && ps[i+1] == 5:
				i += 2
			case i+4 < len(ps) && ps[i+1] == 2:
				i += 4
			default:
				return false
			}
		}
	}
	return slices.Equal(console, xterm)
}

// liveControl returns the first thing in s a UTF-8 terminal would act on other
// than an SGR colour terminals read alike, \t, \n or the \r of a \r\n — any
// other C0 control, DEL or C1 control (U+0080 to U+009F) — with a little
// context, or "" when there is none.
func liveControl(s string) string {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b && keptSGR(s[i:]) != "",
			r == '\t', r == '\n', r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return fmt.Sprintf("%q at byte %d in …%q…", s[i:i+size], i, s[max(0, i-24):min(len(s), i+24)])
		}
		i += size
	}
	return ""
}

// leftOn returns the last SGR sequence in s when it is not a reset, so it may
// leave an attribute on for the text after it, or "".
func leftOn(s string) string {
	all := sgrAnywhere.FindAllString(s, -1)
	if len(all) == 0 {
		return ""
	}
	if last := all[len(all)-1]; last != "\x1b[0m" {
		return fmt.Sprintf("%q", last)
	}
	return ""
}

var sgrAnywhere = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// perCharRedactor stands in for enterprise's redactor (pkg/secret: Redact and
// deterministicPerChar). It rewrites the value of every Authorization header
// or JSON key in the encoded line byte for byte, a lowercase letter to a
// lowercase letter, an uppercase one to an uppercase one and a digit to a
// digit, and keeps every other byte, ESC, '[' and ';' among them. As with
// Redact, the value decides what it writes — here each letter and digit moves
// on by the value's length — so the logged data picks the result.
type perCharRedactor struct{}

var authorizationValue = regexp.MustCompile(`(Authorization"?: ?"?)([^"\r\n]+)`)

func (perCharRedactor) RedactEntry(*zapcore.Entry) {}
func (perCharRedactor) RedactField(*zapcore.Field) {}
func (perCharRedactor) RedactEncoded(text string) string {
	return authorizationValue.ReplaceAllStringFunc(text, func(m string) string {
		sub := authorizationValue.FindStringSubmatch(m)
		return sub[1] + perChar(sub[2])
	})
}

func perChar(v string) string {
	shift, b := len(v), []byte(v)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = 'a' + byte((int(c-'a')+shift)%26)
		case 'A' <= c && c <= 'Z':
			b[i] = 'A' + byte((int(c-'A')+shift)%26)
		case '0' <= c && c <= '9':
			b[i] = '0' + byte((int(c-'0')+shift)%10)
		}
	}
	return string(b)
}

// bothRedactors rewrites a line as perCharRedactor does, then as
// insertingRedactor does.
type bothRedactors struct{ perCharRedactor }

func (bothRedactors) RedactEncoded(text string) string {
	return insertingRedactor{}.RedactEncoded(perCharRedactor{}.RedactEncoded(text))
}

// verboseError prints a stacktrace after its message under %+v, as an error
// from github.com/pkg/errors does, so zap logs it twice: as the error, and with
// the trace as its Verbose key.
type verboseError string

func (e verboseError) Error() string { return string(e) }

func (e verboseError) Format(s fmt.State, verb rune) {
	_, _ = io.WriteString(s, string(e))
	if verb == 'v' && s.Flag('+') {
		_, _ = io.WriteString(s, "\ngo.keploy.io/server/v3/pkg/platform/http.(*AgentClient).Hook\n\t/src/pkg/platform/http/agent.go:42")
	}
}

// bodyObject logs a body through zap.Object, as a string, bytes and a
// reflected map.
type bodyObject string

func (o bodyObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("body", string(o))
	enc.AddByteString("bytes", []byte(o))
	return enc.AddReflected("reflected", map[string]string{"body": string(o)})
}

// Every console logger Keploy builds lets SGR colour through to the terminal
// and nothing else, whatever the logged data carries, and ends each line with
// no colour left on. zap writes the message verbatim and the ANSI encoders used
// to turn every escaped ESC in a field back into a raw one, so a server's reply
// could clear the user's screen or retitle the window from a log line: the body
// of a failed agent hook (pkg/platform/http/agent.go), an upload reply that did
// not decode (pkg/platform/storage/storage.go), the replayed response of a
// json_equal failure (pkg/matcher/http/match.go), or an error message built
// from any of them. Under --disable-ansi no ESC reaches the terminal at all.
//
// The loggers are the real ones — New() and the rebuilds behind --debug, agent
// mode, --disable-ansi and --json, in either order with --disable-ansi, and
// the debug file sinks — with their redactingCore and redactingWriter, and a
// redactor registered that rewrites what the encoder wrote the way
// enterprise's does, and brings in a colour of its own, which each output
// shows as its encoder would.
func TestConsoleLoggersPassOnlySGRToTheTerminal(t *testing.T) {
	t.Chdir(t.TempDir()) // New() opens keploy-logs.txt in the working directory.
	// TestOnlyResetSink, not the sink PrimarySink reports: that is an explicit
	// os.Stdout, which a later test that swaps os.Stdout for a pipe would not
	// see through.
	t.Cleanup(TestOnlyResetSink)
	t.Cleanup(func() { SetRedactor(nil) })

	// A response body holding raw control bytes: clear screen, set the window
	// title, a carriage return and backspace to overprint, an 8-bit CSI (U+009B),
	// a lone 0x9b byte and xterm's modifyOtherKeys, a CSI that ends in m.
	body := "HTTP 500 \x1b[2J\x1b]0;pwned\x07 \r\b \u009b2J \x9b3J \x1b[>4;2m"
	shown := []string{`\u001b[2J`, `\u001b]0;pwned`, `\u009b2J`, `\u001b[>4;2m`, `\ufffd3J`}
	// zap escapes the stray byte, while the message, like encoding/json behind
	// zap.Any, writes it as U+FFFD itself.
	asFFFD := append(shown[:len(shown)-1:len(shown)-1], "\ufffd3J")
	// A server's JSON error, which json.Unmarshal decodes into real controls.
	var reply struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(`{"error":"cluster not found \u001b[2J\u001b]0;pwned\u0007 \u009b2J"}`), &reply); err != nil {
		t.Fatal(err)
	}
	decryptErr := fmt.Errorf("kms decrypt: %s", reply.Error)
	// Values the redactor rewrites into something else: an SGR into a cursor
	// position report request (m -> n, 9 -> 6), an SGR into a cursor move (m
	// -> f), and a reset into blink (0 -> 6, m kept).
	cpr := "\x1b[9mBearer" + strings.Repeat("x", 17)
	home := "\x1b[1;1mBearer" + strings.Repeat("x", 7)
	blink := "\x1b[1mBearer" + strings.Repeat("x", 12) + "\x1b[0m"
	// Keploy's HTTP debug dump (pkg/agent/proxy/integrations/http decode.go).
	request := "POST /users HTTP/1.1\r\nHost: localhost:8080\r\nContent-Type: application/json\r\n\r\n{\"name\":\"a\"}"
	// Conceal, and a reset that comes after 40 more parameters.
	hiddenPastReset := "\x1b[8;" + strings.Repeat("1;", 40) + "0m"

	entries := []struct {
		name string
		log  func(*zap.Logger)
		show []string // text the entry still shows
		hide []string // text the entry no longer carries
	}{
		{"a body in a field", func(l *zap.Logger) {
			l.Error("agent hook returned error", zap.Int("status", 500), zap.String("body", body))
		}, shown, nil},
		{"a decoded JSON error", func(l *zap.Logger) { l.Error("failed to decrypt", zap.Error(decryptErr)) }, shown[:3], nil},
		{"an error in the message", func(l *zap.Logger) { l.Error("failed to decrypt: " + decryptErr.Error()) }, shown[:3], nil},
		{"a body in the message", func(l *zap.Logger) { l.Error(fmt.Sprintf("upload failed: %s", body)) }, asFFFD, nil},
		{"a reflected map", func(l *zap.Logger) {
			l.Error("reply", zap.Any("headers", map[string]any{"body": body, "all": []any{body, map[string]string{"k": body}}}))
		}, asFFFD, nil},
		{"a reflected struct", func(l *zap.Logger) {
			l.Error("reply", zap.Any("resp", struct {
				Body  string
				Inner map[string]string
			}{body, map[string]string{"k": body}}))
		}, asFFFD, nil},
		{"strings", func(l *zap.Logger) { l.Error("bodies", zap.Strings("bodies", []string{body, body})) }, shown, nil},
		{"an object", func(l *zap.Logger) { l.Error("reply", zap.Object("resp", bodyObject(body))) }, shown, nil},
		{"a verbose error", func(l *zap.Logger) { l.Error("request failed", zap.Error(verboseError(body))) }, append(shown[:len(shown):len(shown)], "errorVerbose"), nil},
		{"a hostile error key", func(l *zap.Logger) { l.Error("request failed", zap.NamedError(body, verboseError(body))) }, shown, nil},
		{"a namespace", func(l *zap.Logger) { l.Error("reply", zap.Namespace(body), zap.String("body", body)) }, shown, nil},
		{"context", func(l *zap.Logger) { l.With(zap.String("body", body)).Error("reply", zap.String("k", "v")) }, shown, nil},
		{"context that leaves a colour on", func(l *zap.Logger) { l.With(zap.String("body", "x\x1b[8m")).Error("reply") }, nil, nil},
		{"a redacted header", func(l *zap.Logger) {
			l.Error("failed to match headers", zap.Any("input header", map[string]string{"Authorization": cpr}))
		}, nil, []string{"Bearer"}},
		{"a redacted header line", func(l *zap.Logger) { l.Error("upstream request:\nAuthorization: " + home) }, nil, []string{"Bearer"}},
		{"a redacted reset", func(l *zap.Logger) {
			l.Error("failed to match headers", zap.Any("input header", map[string]string{"Authorization": blink}))
		}, nil, []string{"[1mBearer"}},
		{"a colour left on", func(l *zap.Logger) {
			l.Error("replayed response", zap.String("body", `{"err":"x`+"\x1b[8m"+`"}`), zap.String("passed", "false"))
		}, []string{`"passed": "false"`}, nil},
		{"a colour left on in the message", func(l *zap.Logger) { l.Error("upload failed: \x1b[1;38;5;16m") }, nil, nil},
		// A terminal acts on only so many parameters of a sequence (xterm.js on
		// 32) and drops the rest, the reset at the end among them.
		{"a reset past what a terminal keeps", func(l *zap.Logger) {
			l.Error("replayed response", zap.String("body", `{"err":"x`+hiddenPastReset+`"}`), zap.String("passed", "false"))
		}, []string{`"passed": "false"`, `\u001b[8;1;1;`}, nil},
		{"a reset past what a terminal keeps, in the message", func(l *zap.Logger) { l.Error("upload failed: " + hiddenPastReset) }, []string{`\u001b[8;1;1;`}, nil},
		// The Linux console's display mode, SGR 11 and 12, which stops it
		// decoding UTF-8: the 0x9b that ends Л is a CSI to it then, and moves
		// the cursor. It reads the colour after 58 a parameter at a time.
		{"a display mode", func(l *zap.Logger) {
			l.Error("replayed response", zap.String("body", "\x1b[12mЛ5;20H\x1b[0m"), zap.String("passed", "false"))
		}, []string{`"passed": "false"`, `\u001b[12mЛ5;20H`}, nil},
		{"a display mode in the message", func(l *zap.Logger) { l.Error("replayed body: \x1b[11mЛ5;20H") }, []string{`\u001b[11mЛ5;20H`}, nil},
		{"a display mode in an underline colour", func(l *zap.Logger) { l.Error("replayed body: \x1b[58;5;11mЛ5;20H") }, []string{`\u001b[58;5;11mЛ5;20H`}, nil},
		{"an HTTP dump", func(l *zap.Logger) { l.Error(fmt.Sprintf("This is the complete request:\n%v", request)) }, []string{request}, nil},
		{"a colour the redactor brings in", func(l *zap.Logger) { l.Error("reply", zap.String("k", "token")) }, []string{"\x1b[1mtoken"}, nil},
	}
	// Those values do turn into what they are meant to, in the lines the
	// redactor is handed: the rewrites are only worth checking if they happen.
	made := map[string]string{"a redacted header": "\x1b[6n", "a redacted header line": "\x1b[0;0f", "a redacted reset": "\x1b[6m\""}
	for _, e := range entries {
		if want, ok := made[e.name]; ok {
			if got := rewrittenBy(perCharRedactor{}, e.log); !strings.Contains(got, want) {
				t.Fatalf("%s: the redactor no longer makes %q: %q", e.name, want, got)
			}
			delete(made, e.name)
		}
	}
	if len(made) > 0 {
		t.Fatalf("no entries named %v", made)
	}
	SetRedactor(bothRedactors{})

	const colouredError = "\x1b[31mERROR\x1b[0m"
	type output struct {
		name   string
		file   func() string // the file it is written to, known once the logger is built
		colour bool          // whether SGR may reach it
		level  string        // how the ERROR level renders in it
	}
	console := func() string { return PrimarySink().Name() }
	file := func(name string) func() string { return func() string { return name } }
	for _, tc := range []struct {
		name    string
		build   func(t *testing.T) (*zap.Logger, error)
		outputs []output
	}{
		{"New", func(t *testing.T) (*zap.Logger, error) { return newForTest(t), nil },
			[]output{{"console", console, true, colouredError}, {"keploy-logs.txt", file("keploy-logs.txt"), true, colouredError}}},
		{"ChangeLogLevel (--debug)", func(*testing.T) (*zap.Logger, error) { return ChangeLogLevel(zapcore.DebugLevel) },
			[]output{{"console", console, true, colouredError}}},
		{"AddMode (agent)", func(*testing.T) (*zap.Logger, error) { return AddMode("agent") },
			[]output{{"console", console, true, colouredError}}},
		{"ChangeColorEncoding (--disable-ansi)", func(*testing.T) (*zap.Logger, error) { return ChangeColorEncoding() },
			[]output{{"console", console, false, "ERROR"}}},
		{"RedirectToStderr (--json)", func(t *testing.T) (*zap.Logger, error) {
			stderr := os.Stderr
			os.Stderr = createForTest(t, "stderr")
			t.Cleanup(func() { os.Stderr = stderr })
			return RedirectToStderr()
		}, []output{{"stderr", console, true, colouredError}}},
		{"AddDebugFileSink", func(t *testing.T) (*zap.Logger, error) {
			logger, _ := AddDebugFileSink(newForTest(t), createForTest(t, "debug.log"), 0)
			return logger, nil
		}, []output{{"console", console, true, colouredError}, {"debug.log", file("debug.log"), true, colouredError}}},
		// The agent's agent-debug.log: the sink is registered, and the
		// --debug and --disable-ansi rebuilds reattach it.
		{"SetDebugFileSink, then --debug and --disable-ansi", func(t *testing.T) (*zap.Logger, error) {
			_, sink := AddDebugFileSink(newForTest(t), createForTest(t, "agent-debug.log"), 0)
			SetDebugFileSink(sink)
			t.Cleanup(func() { SetDebugFileSink(nil) })
			if _, err := ChangeLogLevel(zapcore.DebugLevel); err != nil {
				return nil, err
			}
			return ChangeColorEncoding()
		}, []output{{"console", console, false, "ERROR"}, {"agent-debug.log", file("agent-debug.log"), false, "ERROR"}}},
		// A rebuild after --disable-ansi keeps it: each takes its encoder from
		// LogCfg, where ChangeColorEncoding left the plain one.
		{"--disable-ansi, then --debug", func(t *testing.T) (*zap.Logger, error) {
			_, sink := AddDebugFileSink(newForTest(t), createForTest(t, "agent-debug.log"), 0)
			SetDebugFileSink(sink)
			t.Cleanup(func() { SetDebugFileSink(nil) })
			if _, err := ChangeColorEncoding(); err != nil {
				return nil, err
			}
			return ChangeLogLevel(zapcore.DebugLevel)
		}, []output{{"console", console, false, "ERROR"}, {"agent-debug.log", file("agent-debug.log"), false, "ERROR"}}},
		{"--disable-ansi, then agent", func(*testing.T) (*zap.Logger, error) {
			if _, err := ChangeColorEncoding(); err != nil {
				return nil, err
			}
			return AddMode("agent")
		}, []output{{"console", console, false, "ERROR"}}},
		{"--disable-ansi, then --json", func(t *testing.T) (*zap.Logger, error) {
			if _, err := ChangeColorEncoding(); err != nil {
				return nil, err
			}
			stderr := os.Stderr
			os.Stderr = createForTest(t, "stderr")
			t.Cleanup(func() { os.Stderr = stderr })
			return RedirectToStderr()
		}, []output{{"stderr", console, false, "ERROR"}}},
		{"--disable-ansi, then AddDebugFileSink", func(t *testing.T) (*zap.Logger, error) {
			logger, err := ChangeColorEncoding()
			if err != nil {
				return nil, err
			}
			logger, _ = AddDebugFileSink(logger, createForTest(t, "debug.log"), 0)
			return logger, nil
		}, []output{{"console", console, false, "ERROR"}, {"debug.log", file("debug.log"), false, "ERROR"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			LogCfg = defaultLogCfg()
			setPrimarySink(createForTest(t, strings.NewReplacer(" ", "-", "(", "", ")", "", ",", "").Replace(tc.name)+".console"))

			logger, err := tc.build(t)
			if err != nil {
				t.Fatal(err)
			}
			read := make([]int, len(tc.outputs)) // how much of each output earlier entries wrote
			for _, e := range entries {
				e.log(logger)
				if err := logger.Sync(); err != nil { // flushes the debug file sinks' buffers
					t.Fatal(err)
				}
				for i, o := range tc.outputs {
					all := readFile(t, o.file())
					out := all[read[i]:]
					read[i] = len(all)
					if !strings.HasSuffix(out, "\n") || !strings.Contains(out, o.level) {
						t.Errorf("%s, %s: not one %q line: %q", o.name, e.name, o.level, out)
					}
					if live := liveControl(out); live != "" {
						t.Errorf("%s, %s: a live control reaches the terminal: %s", o.name, e.name, live)
					}
					if !o.colour && strings.Contains(out, "\x1b") {
						t.Errorf("%s, %s: an ESC reaches the terminal under --disable-ansi: %q", o.name, e.name, out)
					}
					if left := leftOn(out); left != "" {
						t.Errorf("%s, %s: the line leaves %s on: %q", o.name, e.name, left, out)
					}
					// What was neutralised is still there to read, as text, and
					// an SGR that may not reach the output is text as well.
					for _, want := range e.show {
						if !o.colour {
							want = strings.ReplaceAll(want, "\x1b", `\u001b`)
						}
						if !strings.Contains(out, want) {
							t.Errorf("%s, %s: %q is not shown: %q", o.name, e.name, want, out)
						}
					}
					for _, gone := range e.hide {
						if strings.Contains(out, gone) {
							t.Errorf("%s, %s: redactingWriter did not rewrite %q: %q", o.name, e.name, gone, out)
						}
					}
				}
			}
		})
	}
}

// rewrittenBy logs through log on the default encoder, with r as the
// redactor, and returns what r made of the line redactingWriter handed it.
func rewrittenBy(r Redactor, log func(*zap.Logger)) string {
	rec := &recordingRedactor{Redactor: r}
	SetRedactor(rec)
	defer SetRedactor(nil)
	log(zap.New(newRedactingCore(consoleCore(defaultLogCfg(), &syncBuffer{}, zapcore.DebugLevel))))
	return rec.made
}

// recordingRedactor keeps what its Redactor made of the last line.
type recordingRedactor struct {
	Redactor
	made string
}

func (r *recordingRedactor) RedactEncoded(text string) string {
	r.made = r.Redactor.RedactEncoded(text)
	return r.made
}

// newForTest runs New() and closes the log file it opens when the test ends.
func newForTest(t *testing.T) *zap.Logger {
	t.Helper()
	logger, logFile, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	return logger
}

// createForTest creates a file in the working directory, closed when the test
// ends.
func createForTest(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// verbatim runs s through the rule for zap's verbatim text, as a string (the
// message) and as bytes (the prefix and stacktrace), which must agree, keeping
// SGR as the ANSI encoder does or not as the --disable-ansi one does.
func verbatim(t *testing.T, s string, sgr bool) string {
	t.Helper()
	str, byt := linePool.Get(), linePool.Get()
	defer str.Free()
	defer byt.Free()
	(&lineWriter{out: str, sgr: sgr}).writeString(s)
	(&lineWriter{out: byt, sgr: sgr}).writeBytes([]byte(s))
	if str.String() != byt.String() {
		t.Fatalf("string and bytes disagree on %q: %q, %q", s, str.String(), byt.String())
	}
	return str.String()
}

// Verbatim text keeps \t, \n, the \r of a \r\n and SGR colour; every other
// control character is written as its escape text, so it shows and does
// nothing. Under --disable-ansi an SGR's ESC is escape text too.
func TestVerbatimTextKeepsOnlySGR(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"printable", `a \ backslash, and \u001b[31m as text`, `a \ backslash, and \u001b[31m as text`},
		{"tab and newline", "a\tb\nc", "a\tb\nc"},
		{"SGR", "\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m"},
		{"SGR, many parameters", "\x1b[38;5;208morange\x1b[0;25;0m", "\x1b[38;5;208morange\x1b[0;25;0m"},
		{"SGR, no parameters", "\x1b[mreset", "\x1b[mreset"},
		{"SGR, 16 parameters", "\x1b[1" + strings.Repeat(";1", 15) + "m", "\x1b[1" + strings.Repeat(";1", 15) + "m"},
		{"SGR, the largest parameter an SGR has", "\x1b[255m", "\x1b[255m"},
		{"SGR, 10", "\x1b[10m", "\x1b[10m"},
		{"SGR, 11 and 12 in colours", "\x1b[38;5;11;48;2;11;12;10m", "\x1b[38;5;11;48;2;11;12;10m"},
		// SGRs terminals read apart are text. The Linux console reads 11 and 12
		// as its display mode, and a colour cut short, in a form it does not
		// know, or after 58, a parameter at a time.
		{"SGR 11, then a character that ends in 0x9b", "\x1b[11mЛ5;20H", `\u001b[11mЛ5;20H`},
		{"SGR 12", "\x1b[12mb", `\u001b[12mb`},
		{"SGR 11 after another", "\x1b[1;11m", `\u001b[1;11m`},
		{"SGR 11, padded", "\x1b[011m", `\u001b[011m`},
		{"SGR 11 as the 16th parameter", "\x1b[1" + strings.Repeat(";1", 14) + ";11m", `\u001b[1` + strings.Repeat(";1", 14) + ";11m"},
		{"SGR, 17 parameters", "\x1b[1" + strings.Repeat(";1", 16) + "m", `\u001b[1` + strings.Repeat(";1", 16) + "m"},
		{"SGR, 17 empty parameters", "\x1b[" + strings.Repeat(";", 16) + "m", `\u001b[` + strings.Repeat(";", 16) + "m"},
		{"SGR, a parameter past 255", "\x1b[256;0m", `\u001b[256;0m`},
		{"SGR, a parameter that is 11 in 32 bits", "\x1b[4294967307m", `\u001b[4294967307m`},
		{"SGR, a parameter that is 38 in 32 bits", "\x1b[4294967334;5;0m", `\u001b[4294967334;5;0m`},
		{"SGR, a colour with no form", "\x1b[38m", `\u001b[38m`},
		{"SGR, a colour cut short", "\x1b[48;5m", `\u001b[48;5m`},
		{"SGR, an RGB colour cut short", "\x1b[48;2;11m", `\u001b[48;2;11m`},
		{"SGR, an unknown colour form", "\x1b[38;7;0m", `\u001b[38;7;0m`},
		{"SGR, an underline colour", "\x1b[58;5;0m", `\u001b[58;5;0m`},
		{"SGR, 11 in an underline colour", "\x1b[58;5;11m", `\u001b[58;5;11m`},
		{"SGR, a colour after an underline colour", "\x1b[58;5;48;38;5;11m", `\u001b[58;5;48;38;5;11m`},
		{"clear screen", "\x1b[2J", `\u001b[2J`},
		{"window title", "\x1b]0;pwned\x07", `\u001b]0;pwned\u0007`},
		{"OSC that ends like SGR", "\x1b]0m", `\u001b]0m`},
		{"private mode", "\x1b[?25l", `\u001b[?25l`},
		{"colon parameters", "\x1b[38:5:208m", `\u001b[38:5:208m`},
		{"unterminated SGR", "\x1b[31", `\u001b[31`},
		{"ESC at the end", "end\x1b", `end\u001b`},
		{"terminal reset", "\x1bc", `\u001bc`},
		{"private marker >, ending in m", "\x1b[>4;2m", `\u001b[>4;2m`},
		{"private marker ?, ending in m", "\x1b[?4m", `\u001b[?4m`},
		{"private marker =, ending in m", "\x1b[=5m", `\u001b[=5m`},
		{"private marker <, ending in m", "\x1b[<1m", `\u001b[<1m`},
		{"intermediate byte, ending in m", "\x1b[1 m", `\u001b[1 m`},
		{"other C0 and DEL", "a\rb\bc\x00d\x7fe", `a\u000db\u0008c\u0000d\u007fe`},
		{"CRLF", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"CR, then CRLF", "a\r\r\nb", `a\u000d` + "\r\nb"},
		{"CR at the end", "a\r", `a\u000d`},
		{"LF, then CR", "a\n\rb", "a\n" + `\u000db`},
		{"C1 CSI", "\u009b2J", `\u009b2J`},
		{"C1 NEL", "a\u0085b", `a\u0085b`},
		{"first C1", "\u0080", `\u0080`},
		{"last C1", "\u009f", `\u009f`},
		{"first after C1", "\u00a0", "\u00a0"},
		{"lone C1 byte", "\x9b2J", "\ufffd2J"},
		{"lone first C1 byte", "\x80", "\ufffd"},
		{"lone last C1 byte", "\x9f", "\ufffd"},
		{"lone bytes above C1", "\xa0\xbf\xff", "\xa0\xbf\xff"},
		{"0xc2, then no C1", "\xc2A \xc2\xa9 \xc2", "\xc2A © \xc2"},
		// Every overlong form of a C0 or C1 control ends in a byte from 0x80 to
		// 0x9F: c0 9b is ESC, e0 82 9b is U+009B, CSI.
		{"overlong ESC", "\xc0\x9b[2J", "\xc0\ufffd[2J"},
		{"overlong CSI", "\xe0\x82\x9b2J", "\xe0\ufffd\ufffd2J"},
		{"overlong ESC, in four bytes", "\xf0\x80\x80\x9b", "\xf0\ufffd\ufffd\ufffd"},
		{"a character cut short, then more", "\xe2\x9cx", "\xe2\ufffdx"},
		{"a byte past a character", "é\x9b 🐰\x9f", "é\ufffd 🐰\ufffd"},
		{"five continuation bytes", "\xf0\x9f\x9f\x9f\x9f", "\xf0\x9f\x9f\x9f\ufffd"},
		{"U+FFFD itself", "\ufffd\x9b", "\ufffd\ufffd"},
		{"UTF-8 with 0x80 to 0x9f in it", "✛ 🐰 café П я 中", "✛ 🐰 café П я 中"},
		// The first 0x80-0x9F byte of these comes after a continuation byte from
		// 0xA0 up: 一 is e4 b8 80 and 𠮟 is f0 a0 ae 9f.
		{"0x80 to 0x9f late in a character", "一 𠮟", "一 𠮟"},
		// After a character that ends in 0x80 to 0x9F, as 一 does, the text
		// that follows is decided with it, as far as it is UTF-8 the rule
		// writes as it is.
		{"characters, then ASCII", "一中文x\x1b[2J", "一中文x\\u001b[2J"},
		{"characters, then a C1 control", "一中\u0085\u009b2J", "一中\\u0085\\u009b2J"},
		{"characters, then a stray byte", "一中\x9b\x9f", "一中\ufffd\ufffd"},
		{"characters, then an overlong ESC", "一中\xc0\x9b", "一中\xc0\ufffd"},
		{"characters, then a surrogate", "一中\xed\xa0\x80", "一中\xed\xa0\ufffd"},
		{"characters, then one past U+10FFFF", "一中\xf4\x90\x80\x80", "一中\xf4\ufffd\ufffd\ufffd"},
		{"characters, then a character cut short", "一中\xe4\xb8", "一中\xe4\xb8"},
		{"characters, then a character cut short by a stray byte", "一中\xe4\x9b", "一中\xe4\ufffd"},
		{"characters, then a four-byte one", "一中𠮟\x9f", "一中𠮟\ufffd"},
		{"characters, then U+00A0 and U+FFFD", "一\u00a0\ufffd\x80", "一\u00a0\ufffd\ufffd"},
		{"characters past what is decided at once, then a stray byte", "一" + strings.Repeat("中", 200) + "\x9b", "一" + strings.Repeat("中", 200) + "\ufffd"},
		{"characters past what is decided at once, then a control", "一" + strings.Repeat("中", 200) + "\x1b[2J", "一" + strings.Repeat("中", 200) + "\\u001b[2J"},
		{"a byte that is not UTF-8 among characters", "一" + strings.Repeat("中", 20) + "\xff" + strings.Repeat("中", 20) + "\x9b", "一" + strings.Repeat("中", 20) + "\xff" + strings.Repeat("中", 20) + "\ufffd"},
	} {
		if got := verbatim(t, tc.in, true); got != tc.want {
			t.Errorf("%s: %q became %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if got, want := verbatim(t, tc.in, false), strings.ReplaceAll(tc.want, "\x1b", `\u001b`); got != want {
			t.Errorf("%s, --disable-ansi: %q became %q, want %q", tc.name, tc.in, got, want)
		}
	}
}

// byTheRule writes s by the rule for verbatim text the plain way, a character
// at a time as a UTF-8 terminal decodes it, where lineWriter skips what needs
// no second look.
func byTheRule(s string, sgr bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b && sgr && keptSGR(s[i:]) != "":
			size = len(keptSGR(s[i:]))
			b.WriteString(s[i : i+size])
		case r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
			size = 2
			b.WriteString("\r\n")
		case r == '\t', r == '\n':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, 0x80 <= r && r < 0xa0:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == utf8.RuneError && size == 1 && s[i] < 0xa0: // a stray byte from 0x80 to 0x9F
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// verbatimPieces are what the rule for verbatim text turns on: controls, SGR
// and other sequences, and the parameters that decide whether terminals read
// an SGR alike, the bytes that start, continue or break a UTF-8 character,
// characters on either side of U+00A0 and with 0x80 to 0x9F in them, and runs
// of text longer than utf8Text's first window and its largest.
var verbatimPieces = []string{
	"a", "abcdefgh", "\t", "\n", "\r", "\r\n", "\x1b", "\x1b[31m", "\x1b[0m", "\x1b[2J", "[", "m", "0", ";", "\x00", "\x07", "\x7f",
	"\x1b[11m", "1;", "11", "12", "38;", "58;", "5;", "2;", "255", "256", "1;1;1;1;1;1;1;1;",
	"\xc2", "\x80", "\x9b", "\x9f", "\xa0", "\xbf", "\xc0", "\xe0", "\xe4", "\xed", "\xf0", "\xf4", "\xff",
	"é", "П", "中", "一", "𠮟", "🐰", "\u0085", "\u009b", "\u00a0", "\ufffd",
	strings.Repeat("a", utf8Window-1), strings.Repeat("中", utf8Window/3+1), strings.Repeat("я", utf8Window/2+1), strings.Repeat("𠮟", utf8Window/4),
	strings.Repeat("中", maxUTF8Window/3+1), strings.Repeat("я", maxUTF8Window/2+1),
}

// lineWriter agrees with byTheRule on text made of verbatimPieces in any
// order, so at any offset from the eight bytes it tests at a time.
func TestVerbatimTextFollowsTheRule(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range rng.IntN(24) {
			b.WriteString(verbatimPieces[rng.IntN(len(verbatimPieces))])
		}
		s := b.String()
		for _, sgr := range []bool{true, false} {
			if got, want := verbatim(t, s, sgr), byTheRule(s, sgr); got != want {
				t.Fatalf("sgr %t: %q became %q, want %q", sgr, s, got, want)
			}
		}
	}
}

func FuzzVerbatimTextFollowsTheRule(f *testing.F) {
	for _, p := range verbatimPieces {
		f.Add(p + p + "一" + p)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, sgr := range []bool{true, false} {
			if got, want := verbatim(t, s, sgr), byTheRule(s, sgr); got != want {
				t.Fatalf("sgr %t: %q became %q, want %q", sgr, s, got, want)
			}
		}
	})
}

// In zap's JSON context an escaped ESC becomes a raw one only when it is a real
// escape, not text after an escaped backslash, and opens an SGR sequence.
func TestContextUnescapesOnlySGR(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"SGR", `{"k": "\u001b[31mred\u001b[0m"}`, "{\"k\": \"\x1b[31mred\x1b[0m\"}"},
		{"SGR back to back", `\u001b[1m\u001b[31m`, "\x1b[1m\x1b[31m"},
		{"not SGR", `{"k": "\u001b[2J\u001b]0;t\u0007"}`, `{"k": "\u001b[2J\u001b]0;t\u0007"}`},
		{"SGR terminals read apart", `\u001b[11mЛ5;20H\u001b[58;5;1m\u001b[38;5;11m`, `\u001b[11mЛ5;20H\u001b[58;5;1m` + "\x1b[38;5;11m"},
		{"unterminated", `"\u001b[31"`, `"\u001b[31"`},
		{"escape at the end", `"a\u001b"`, `"a\u001b"`},
		{"uppercase, which zap never writes", `\u001B[31m`, `\u001B[31m`},
		{"an escaped backslash, then text", `\\u001b[31m`, `\\u001b[31m`},
		{"an escaped backslash, then an escape", `\\\u001b[31m`, `\\` + "\x1b[31m"},
		{"two escaped backslashes, then text", `\\\\u001b[31m`, `\\\\u001b[31m`},
		{"text, then an escape", `a\\u001b \u001b[0m`, `a\\u001b ` + "\x1b[0m"},
		{"private marker >", `\u001b[>4;2m`, `\u001b[>4;2m`},
		{"private marker ?", `\u001b[?4m`, `\u001b[?4m`},
		{"private marker =", `\u001b[=5m`, `\u001b[=5m`},
		{"private marker <", `\u001b[<1m`, `\u001b[<1m`},
		{"intermediate byte", `\u001b[1 m`, `\u001b[1 m`},
		{"a raw C1 control", "\"\u009b2J\"", `"\u009b2J"`},
		{"other escapes", `\n\t\"\u0007`, `\n\t\"\u0007`},
		{"a trailing backslash", `tail \`, `tail \`},
	} {
		out := linePool.Get()
		(&lineWriter{out: out, sgr: true}).writeContext([]byte(tc.in))
		if got := out.String(); got != tc.want {
			t.Errorf("%s: %q became %q, want %q", tc.name, tc.in, got, tc.want)
		}
		out.Free()
	}
}

func encodeLine(t *testing.T, enc zapcore.Encoder, ent zapcore.Entry, fields ...zapcore.Field) string {
	t.Helper()
	buf, err := enc.EncodeEntry(ent, fields)
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Free()
	return buf.String()
}

// Only zap's JSON context is JSON. The message, the logger name and the
// stacktrace are verbatim: a `\u001b[31m` in them is text, as it would be in
// a Windows path, and stays text, while a raw control in them is neutralised
// like anywhere else in the line.
func TestOnlyTheJSONContextIsUnescaped(t *testing.T) {
	got := encodeLine(t, NewANSIConsoleEncoder(defaultLogCfg().EncoderConfig), zapcore.Entry{
		Level:      zapcore.InfoLevel,
		Time:       fixedClock{}.Now(),
		LoggerName: `name \u001b[31m`,
		Message:    `message \u001b[31m`,
		Stack:      "goroutine 1 [running]:\nmain.main()\n\tC:\\src\\u001b[31m\\main.go:10 \x1b[2J\nmain.run()\n\tC:\\src\\run.go:4",
	}, zap.String("k", "\x1b[32mgreen\x1b[0m"))
	want := "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\tname \\u001b[31m\tmessage \\u001b[31m\t{\"k\": \"\x1b[32mgreen\x1b[0m\"}\n" +
		"goroutine 1 [running]:\nmain.main()\n\tC:\\src\\u001b[31m\\main.go:10 \\u001b[2J\nmain.run()\n\tC:\\src\\run.go:4\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The message goes back where zap wrote it even when the logger name, which
// zap writes before it, holds what a fixed placeholder could be: the one an
// earlier draft of this encoder used, or this one's own fixed part. The
// placeholder's per-process random part is what keeps a name from matching it.
func TestALoggerNameCannotStandInForTheMessage(t *testing.T) {
	for _, tc := range []struct{ name, shown string }{
		{"\x00keploy-log-message\x00", `\u0000keploy-log-message\u0000`},
		{"keploy-message-", "keploy-message-"},
	} {
		got := encodeLine(t, NewANSIConsoleEncoder(defaultLogCfg().EncoderConfig), zapcore.Entry{
			Level:      zapcore.InfoLevel,
			Time:       fixedClock{}.Now(),
			LoggerName: tc.name,
			Message:    "hello",
		}, zap.String("k", "v"))
		want := "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t" + tc.shown + "\thello\t{\"k\": \"v\"}\n"
		if got != want {
			t.Errorf("logger %q:\n got %q\nwant %q", tc.name, got, want)
		}
	}
}

// Without a MessageKey zap writes no message, and nothing marks where the JSON
// starts, so no escape is taken for one: a colour in a field stays text, and
// controls are still neutralised.
func TestWithoutAMessageKeyEveryEscapeStaysText(t *testing.T) {
	cfg := defaultLogCfg().EncoderConfig
	cfg.MessageKey = ""
	got := encodeLine(t, NewANSIConsoleEncoder(cfg), zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), Message: "dropped"},
		zap.String("k", "\x1b[32mgreen\x1b[0m"), zap.String("b", "\u009b2J"))
	want := "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t{\"k\": \"\\u001b[32mgreen\\u001b[0m\", \"b\": \"\\u009b2J\"}\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The line ending is zap's, not data: a configured "\r" one survives while a
// carriage return in the message that no newline follows is neutralised, and a
// data colour left on is turned off before the line ending — the one zap
// writes when none is configured, and none when it is told to skip it, so a
// newline the stacktrace ends with stays in the line.
func TestTheLineEndingIsKept(t *testing.T) {
	const prefix = "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t"
	for _, tc := range []struct {
		name       string
		lineEnding string
		skip       bool
		message    string
		stack      string
		want       string
	}{
		{"\\r", "\r", false, "a\rb\r\nc\x1b[1m", "", "a\\u000db\r\nc\x1b[1m\x1b[0m\r"},
		{"none configured", "", false, "a\x1b[1m", "", "a\x1b[1m\x1b[0m\n"},
		{"skipped", "\n", true, "a\x1b[1m", "main.main()\n", "a\x1b[1m\nmain.main()\n\x1b[0m"},
	} {
		cfg := defaultLogCfg().EncoderConfig
		cfg.LineEnding, cfg.SkipLineEnding = tc.lineEnding, tc.skip
		got := encodeLine(t, NewANSIConsoleEncoder(cfg), zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), Message: tc.message, Stack: tc.stack})
		if want := prefix + tc.want; got != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, want)
		}
	}
}

// A colour the data turns on ends with its line: when the last SGR in the line
// leaves an attribute on, a reset goes before the line ending, so a replayed
// body cannot conceal or recolour the next line, Keploy's summary or the
// user's shell prompt. A line whose last SGR turns everything off, as every
// line Keploy colours itself does, is written as it is.
func TestDataColourEndsWithItsLine(t *testing.T) {
	const prefix = "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t"
	enc := NewANSIConsoleEncoder(defaultLogCfg().EncoderConfig)
	for _, tc := range []struct {
		name, message string
		fields        []zapcore.Field
		stack         string
		want          string
	}{
		{"conceal in a field", "reply", []zapcore.Field{zap.String("body", "x\x1b[8m"), zap.String("passed", "false")}, "",
			"reply\t{\"body\": \"x\x1b[8m\", \"passed\": \"false\"}\x1b[0m\n"},
		{"bold in the message", "a\x1b[1mb", nil, "", "a\x1b[1mb\x1b[0m\n"},
		{"bold, then a stacktrace", "a\x1b[1mb", nil, "main.main()", "a\x1b[1mb\nmain.main()\x1b[0m\n"},
		{"bold in the stacktrace", "a", nil, "main.main() \x1b[1m", "a\nmain.main() \x1b[1m\x1b[0m\n"},
		{"turned off again", "a\x1b[1mb\x1b[0m", nil, "", "a\x1b[1mb\x1b[0m\n"},
		{"turned off by a later field", "a\x1b[1mb", []zapcore.Field{zap.String("k", "\x1b[32mv\x1b[0m")}, "", "a\x1b[1mb\t{\"k\": \"\x1b[32mv\x1b[0m\"}\n"},
		{"no parameters", "a\x1b[1mb\x1b[m", nil, "", "a\x1b[1mb\x1b[m\n"},
		{"an empty last parameter", "a\x1b[1;m", nil, "", "a\x1b[1;m\n"},
		{"0 last", "a\x1b[1;0m", nil, "", "a\x1b[1;0m\n"},
		{"0 padded", "a\x1b[1;000m", nil, "", "a\x1b[1;000m\n"},
		{"fatih/color's orange reset", "a\x1b[38;5;208mb\x1b[0;25;0m", nil, "", "a\x1b[38;5;208mb\x1b[0;25;0m\n"},
		{"0 first", "a\x1b[0;1m", nil, "", "a\x1b[0;1m\x1b[0m\n"},
		{"black, from the 256-colour palette", "a\x1b[38;5;0m", nil, "", "a\x1b[38;5;0m\x1b[0m\n"},
		{"black on black, in RGB", "a\x1b[38;2;0;0;0;48;2;0;0;0m", nil, "", "a\x1b[38;2;0;0;0;48;2;0;0;0m\x1b[0m\n"},
		{"a reset after a colour", "a\x1b[38;5;16;0m", nil, "", "a\x1b[38;5;16;0m\n"},
		{"the largest parameter an SGR has", "a\x1b[255;0m", nil, "", "a\x1b[255;0m\n"},
		// Every terminal acts on all of 16 parameters, a reset among them.
		{"a reset as the 16th parameter", "a\x1b[8;" + strings.Repeat("1;", 14) + "0m", nil, "", "a\x1b[8;" + strings.Repeat("1;", 14) + "0m\n"},
		// An SGR terminals read apart is text, and leaves nothing on: one with
		// more parameters than a terminal is sure to act on, one no SGR has, a
		// colour in a form not every terminal knows, and the Linux console's
		// display mode, which it reads from 11 and 12 on their own.
		{"a reset as the 17th parameter", "a\x1b[8;" + strings.Repeat("1;", 15) + "0m", nil, "", "a\\u001b[8;" + strings.Repeat("1;", 15) + "0m\n"},
		{"a reset past what any terminal keeps", "a", []zapcore.Field{zap.String("body", "x\x1b[8;"+strings.Repeat("1;", 40)+"0m"), zap.String("passed", "false")}, "",
			"a\t{\"body\": \"x\\u001b[8;" + strings.Repeat("1;", 40) + "0m\", \"passed\": \"false\"}\n"},
		{"one past the largest parameter", "a\x1b[256;0m", nil, "", "a\\u001b[256;0m\n"},
		{"a colour cut short", "a\x1b[48;5m", nil, "", "a\\u001b[48;5m\n"},
		{"an underline colour", "a\x1b[58;5;0m", nil, "", "a\\u001b[58;5;0m\n"},
		{"display mode 11", "a\x1b[11mЛ5;20H", nil, "", "a\\u001b[11mЛ5;20H\n"},
		{"display mode 12, then a reset", "a\x1b[12mb\x1b[0m", nil, "", "a\\u001b[12mb\x1b[0m\n"},
		{"display mode in a field", "reply", []zapcore.Field{zap.String("body", "x\x1b[12mЛ5;20H\x1b[1m")}, "", "reply\t{\"body\": \"x\\u001b[12mЛ5;20H\x1b[1m\"}\x1b[0m\n"},
		{"display mode in the stacktrace", "a", nil, "main.main() \x1b[11m", "a\nmain.main() \\u001b[11m\n"},
		{"display mode in a colour cut short", "a\x1b[48;2;11m", nil, "", "a\\u001b[48;2;11m\n"},
		{"display mode in an underline colour", "a\x1b[58;5;11m", nil, "", "a\\u001b[58;5;11m\n"},
		// 10 turns the display mode off, and no attribute on.
		{"10 after a reset", "a\x1b[1mb\x1b[0m\x1b[10m", nil, "", "a\x1b[1mb\x1b[0m\x1b[10m\n"},
		{"10 leaves a colour on", "a\x1b[1mb\x1b[10m", nil, "", "a\x1b[1mb\x1b[10m\x1b[0m\n"},
		// The same state carried where the case above (the message, writeString)
		// cannot reach: a field's JSON context and the stacktrace.
		{"10 leaves a colour on, in a field", "a", []zapcore.Field{zap.String("k", "\x1b[1mb\x1b[10m")}, "", "a\t{\"k\": \"\x1b[1mb\x1b[10m\"}\x1b[0m\n"},
		{"10 leaves a colour on, in the stacktrace", "a", nil, "main.main() \x1b[1mb\x1b[10m", "a\nmain.main() \x1b[1mb\x1b[10m\x1b[0m\n"},
		{"11 as a colour", "a\x1b[38;5;11mb\x1b[0m", nil, "", "a\x1b[38;5;11mb\x1b[0m\n"},
		{"11 and 12 in an RGB colour", "a\x1b[48;2;11;12;10mb\x1b[0m", nil, "", "a\x1b[48;2;11;12;10mb\x1b[0m\n"},
		{"11 and 12 in an RGB colour, left on", "a\x1b[48;2;11;12;10mb", nil, "", "a\x1b[48;2;11;12;10mb\x1b[0m\n"},
	} {
		got := encodeLine(t, enc, zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), Message: tc.message, Stack: tc.stack}, tc.fields...)
		if want := prefix + tc.want; got != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, want)
		}
	}
	// The logger name, which zap writes before the message, is data as well.
	got := encodeLine(t, enc, zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), LoggerName: "x\x1b[8m", Message: "m"})
	if want := prefix + "x\x1b[8m\tm\x1b[0m\n"; got != want {
		t.Errorf("conceal in the logger name:\n got %q\nwant %q", got, want)
	}
}

// Under --disable-ansi the user asked for no ANSI, so no ESC reaches the
// terminal: a colour in the message, the logger name or the stacktrace is
// written as the escape text zap writes for one in a field.
func TestDisableANSIWritesNoESC(t *testing.T) {
	cfg := defaultLogCfg().EncoderConfig
	cfg.EncodeLevel = zapcore.CapitalLevelEncoder
	got := encodeLine(t, newPlainConsoleEncoder(cfg), zapcore.Entry{
		Level:      zapcore.InfoLevel,
		Time:       fixedClock{}.Now(),
		LoggerName: "\x1b[1mname",
		Message:    "\x1b[32mTotal Coverage Percentage: 85.5%\x1b[0m",
		Stack:      "main.main()\x1b[31m",
	}, zap.String("k", "\x1b[32mv\x1b[0m"))
	want := "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\t\\u001b[1mname\t\\u001b[32mTotal Coverage Percentage: 85.5%\\u001b[0m\t{\"k\": \"\\u001b[32mv\\u001b[0m\"}\n" +
		"main.main()\\u001b[31m\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// insertingRedactor rewrites every "token" in the line into a bold one, the
// way no real redactor does, so a test can tell whether redactingWriter keeps
// the SGR a rewrite brings in.
type insertingRedactor struct{}

func (insertingRedactor) RedactEntry(*zapcore.Entry) {}
func (insertingRedactor) RedactField(*zapcore.Field) {}
func (insertingRedactor) RedactEncoded(text string) string {
	return strings.ReplaceAll(text, "token", "\x1b[1mtoken")
}

// redactorFunc is a Redactor that rewrites each encoded line with the func.
type redactorFunc func(string) string

func (redactorFunc) RedactEntry(*zapcore.Entry)         {}
func (redactorFunc) RedactField(*zapcore.Field)         {}
func (f redactorFunc) RedactEncoded(text string) string { return f(text) }

// redactingWriter applies the rule of the encoder it carries to a line the
// redactor rewrote, whatever the rewrite's length: the rewrite's CSI, and an
// SGR terminals read apart, is neutralised; the reset the encoder ended the
// line with is not the redactor's to rewrite, and goes back only if the
// rewritten line leaves something on; the line ending the encoder was
// configured with is kept, and one the redactor dropped is not put back; and
// under --disable-ansi no ESC gets through. A line the redactor did not change
// is written as it came. Write reports the bytes it took from its caller, not
// the ones it wrote.
func TestRedactingWriterAppliesItsEncodersRule(t *testing.T) {
	t.Cleanup(func() { SetRedactor(nil) })
	cfg := defaultLogCfg().EncoderConfig
	cfg.LineEnding = "\r" // a lone \r, which the rule alone would neutralise
	plain := cfg          // what ChangeColorEncoding sets
	plain.EncodeLevel = zapcore.CapitalLevelEncoder
	const prefix = "🐰 Keploy: 2026-09-24T10:00:00.123456789Z \t\x1b[34mINFO\x1b[0m\t"
	header := zap.Any("input header", map[string]string{"Authorization": "\x1b[9mBearer" + strings.Repeat("x", 17)})
	token := zap.String("k", "token")
	// shortening rewrites the header's value into a request for a cursor
	// position report, shorter than the value.
	shortening := redactorFunc(func(line string) string { return authorizationValue.ReplaceAllString(line, "${1}\x1b[6n") })
	// displayMode rewrites "token" into the Linux console's display mode, and
	// a character that ends in 0x9b.
	displayMode := redactorFunc(func(line string) string { return strings.ReplaceAll(line, "token", "\x1b[11mЛ5;20H") })
	// unended leaves a colour on, and drops the line ending.
	unended := redactorFunc(func(line string) string {
		return strings.TrimSuffix(insertingRedactor{}.RedactEncoded(line), "\r")
	})
	const msg = "failed to match headers"
	for _, tc := range []struct {
		name     string
		redactor Redactor
		enc      consoleEncoder
		msg      string
		fields   []zapcore.Field
		want     string
	}{
		{"a CSI the redactor made", perCharRedactor{}, newANSIConsoleEncoder(cfg), msg, []zapcore.Field{header},
			prefix + "failed to match headers\t{\"input header\": {\"Authorization\":\"\\u001b[6nCfbsfsyyyyyyyyyyyyyyyyy\"}}\r"},
		{"a CSI in a line the redactor shortened", shortening, newANSIConsoleEncoder(cfg), msg, []zapcore.Field{header},
			prefix + "failed to match headers\t{\"input header\": {\"Authorization\":\"\\u001b[6n\"}}\r"},
		{"a colour the redactor left on", insertingRedactor{}, newANSIConsoleEncoder(cfg), msg, []zapcore.Field{header, token},
			prefix + "failed to match headers\t{\"input header\": {\"Authorization\":\"\x1b[9mBearerxxxxxxxxxxxxxxxxx\"}, \"k\": \"\x1b[1mtoken\"}\x1b[0m\r"},
		{"a line ending the redactor dropped", unended, newANSIConsoleEncoder(cfg), msg, []zapcore.Field{token},
			prefix + "failed to match headers\t{\"k\": \"\x1b[1mtoken\"}\x1b[0m"},
		// The reset the encoder puts at the end of a line is not the redactor's
		// to rewrite, even when the value it rewrites runs to the end of the
		// line: it would turn into a CSI, shown as junk text.
		{"the encoder's reset, after a value the redactor rewrites", perCharRedactor{}, newANSIConsoleEncoder(cfg),
			"upstream request:\nAuthorization: \x1b[1;1mBearer" + strings.Repeat("x", 7), nil,
			prefix + "upstream request:\nAuthorization: \\u001b[0;0fUxtkxkqqqqqqq\r"},
		{"a display mode the redactor brings in", displayMode, newANSIConsoleEncoder(cfg), msg, []zapcore.Field{token},
			prefix + "failed to match headers\t{\"k\": \"\\u001b[11mЛ5;20H\"}\r"},
		// A line the redactor leaves as it is goes to the sink as the encoder
		// made it, with the reset it ends in.
		{"a line the redactor leaves as it is", redactorFunc(func(line string) string { return line }), newANSIConsoleEncoder(cfg), "done\x1b[0m", nil,
			prefix + "done\x1b[0m\r"},
		{"--disable-ansi", insertingRedactor{}, newPlainConsoleEncoder(plain), msg, []zapcore.Field{header, token},
			"🐰 Keploy: 2026-09-24T10:00:00.123456789Z \tINFO\tfailed to match headers\t{\"input header\": {\"Authorization\":\"\\u001b[9mBearerxxxxxxxxxxxxxxxxx\"}, \"k\": \"\\u001b[1mtoken\"}\r"},
	} {
		SetRedactor(tc.redactor)
		line := []byte(encodeLine(t, tc.enc, zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), Message: tc.msg}, tc.fields...))
		var sink syncBuffer
		if n, err := wrapWriter(&sink, tc.enc).Write(line); n != len(line) || err != nil {
			t.Errorf("%s: Write(%d bytes) = %d, %v", tc.name, len(line), n, err)
		}
		if got := sink.String(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// A line the encoders wrote is already under the rule redactingWriter would
// apply to it again, so the writer passes on a line the redactor did not
// change as it is.
func FuzzEncodedLinesAreUnderTheRule(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "\x1b[31mred\x1b[0m", "\x1b[8m", "\x1b[2J\x1b]0;pwned\x07", "a\r\nb\rc", "\u009b2J \x9b3J",
		"\x1b[>4;2m", "\x1b[38;5;0m", "é\x9b 🐰\x9f \xc2", "\\u001b[31m", `\u001b[31m`, "\xf0\x9f\x9f\x9f\x9f",
		"\x1b[11mЛ5;20H", "\x1b[58;5;11m", "\x1b[48;2;11m", `\u001b[12m`,
	} {
		f.Add(seed, seed, seed)
	}
	cfg := defaultLogCfg().EncoderConfig
	cfg.EncodeCaller = zapcore.ShortCallerEncoder
	encoders := []consoleEncoder{newANSIConsoleEncoder(cfg), newPlainConsoleEncoder(cfg)}
	f.Fuzz(func(t *testing.T, msg, value, stack string) {
		for _, enc := range encoders {
			buf, err := enc.EncodeEntry(zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), LoggerName: value, Message: msg, Stack: stack},
				[]zapcore.Field{zap.String("k", value), zap.Any("m", map[string]string{value: value}), zap.Error(verboseError(value))})
			if err != nil {
				t.Fatal(err)
			}
			line := buf.String()
			buf.Free()
			if live := liveControl(line); live != "" {
				t.Fatalf("colour %t: a live control reaches the terminal: %s", enc.colour, live)
			}
			if !enc.colour && strings.Contains(line, "\x1b") {
				t.Fatalf("an ESC reaches the terminal under --disable-ansi: %q", line)
			}
			again := linePool.Get()
			enc.sanitizeRewritten(again, line)
			if again.String() != line {
				t.Fatalf("colour %t: the rule changes the line it made:\n%q\n%q", enc.colour, line, again.String())
			}
			again.Free()
		}
	})
}

// inertPrefix and utf8InertPrefix stop at the first byte that is not inert,
// wherever it falls in the eight they test at a time. In UTF-8 a byte from 0x80
// to 0x9F is inert; 0xC2, which starts a C1 control, is not, in either.
func TestInertPrefixStopsAtTheFirstByteNotInert(t *testing.T) {
	for _, table := range []struct {
		name          string
		bytes, string func(string) int
		inert         func(c byte) bool
	}{
		{"inertPrefix", func(s string) int { return inertPrefix([]byte(s)) }, inertPrefix[string],
			func(c byte) bool { return c == '\t' || c == '\n' || 0x20 <= c && c < 0x7f || c >= 0xa0 && c != 0xc2 }},
		{"utf8InertPrefix", func(s string) int { return utf8InertPrefix([]byte(s)) }, utf8InertPrefix[string],
			func(c byte) bool { return c == '\t' || c == '\n' || 0x20 <= c && c < 0x7f || c >= 0x80 && c != 0xc2 }},
	} {
		for c := range 256 {
			for at := range 17 {
				s := []byte(strings.Repeat("a", 17))
				s[at] = byte(c)
				want := len(s)
				if !table.inert(byte(c)) {
					want = at
				}
				if got := table.bytes(string(s)); got != want {
					t.Errorf("%s: %#x at %d: %d, want %d", table.name, c, at, got, want)
				}
				if got := table.string(string(s)); got != want {
					t.Errorf("%s: %#x at %d, as a string: %d, want %d", table.name, c, at, got, want)
				}
			}
		}
	}
}

// BenchmarkANSIConsoleEncoder measures the default encoder on a plain line, a
// coloured test result line and a 1 MB JSON body in a field, and on what
// Keploy logs verbatim in the message: a 1 MB JSON dump, in English, Chinese
// and Russian and in Chinese that is not all UTF-8, 1 MB of characters each
// followed by a byte that is not UTF-8, 1 MB of binary and the HTTP debug dump
// of a gzip response (decode.go's "Mock Response sending back to client:\n%v").
func BenchmarkANSIConsoleEncoder(b *testing.B) {
	enc := NewANSIConsoleEncoder(defaultLogCfg().EncoderConfig)
	mb := func(record string) string { return strings.Repeat(record, 1<<20/len(record)+1)[:1<<20] }
	record := `{"id":42,"name":"keploy","tags":["a","b"],"note":"x"}` + "\t\n"
	body := mb(record)
	// About half the continuation bytes of Chinese and Russian text are from
	// 0x80 to 0x9F, the range a C1 control or a stray byte is in.
	chinese := mb(`{"id":42,"name":"用户名称一二三","note":"测试数据：订单已发货，请注意查收。"},`)
	russian := mb(`{"id":42,"name":"Пользователь","note":"Заказ отправлен, пожалуйста, проверьте."},`)
	// Chinese with a Latin-1 é in each record, which is not UTF-8.
	mixed := mb(`{"id":42,"name":"用户名称一二三","note":"caf` + "\xe9" + ` 测试数据：订单已发货，请注意查收。"},`)
	// 一 is e4 b8 80, so the text after each one is looked at for UTF-8, and
	// the byte after it is not: 0xff never is, and 0x9b is a stray one.
	interleaved := mb("一\xff")
	interleavedStray := mb("一\x9b")
	binary := make([]byte, 1<<20)
	for i, x := 0, uint32(1); i < len(binary); i++ { // xorshift: the same bytes every run
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		binary[i] = byte(x)
	}
	var users strings.Builder // a JSON array that compresses the way a real response does
	for i := 0; users.Len() < 1<<20; i++ {
		fmt.Fprintf(&users, `{"id":%d,"name":"user-%d","email":"u%d@example.com","tags":["a","b"]},`, i, i*7, i*13)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte(users.String())); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	dump := fmt.Sprintf("Mock Response sending back to client:\nHTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s", gz.Len(), gz.Bytes())
	for _, bc := range []struct {
		name   string
		msg    string
		fields []zapcore.Field
	}{
		{"plain line", "Test Sets to be Replayed", []zapcore.Field{zap.String("testSets", "test-set-0"), zap.Int("count", 1)}},
		{"coloured line", "result", []zapcore.Field{zap.String("testcase id", "\x1b[32mget-user-1\x1b[0m"), zap.String("testset id", "\x1b[32mtest-set-0\x1b[0m"), zap.String("passed", "\x1b[32mtrue\x1b[0m")}},
		{"1 MB field", "agent hook returned error", []zapcore.Field{zap.Int("status", 500), zap.String("body", body)}},
		{"1 MB message", body, nil},
		{"1 MB Chinese message", chinese, nil},
		{"1 MB Russian message", russian, nil},
		{"1 MB Chinese message, not all UTF-8", mixed, nil},
		{"1 MB message, a character then a byte not UTF-8", interleaved, nil},
		{"1 MB message, a character then a stray byte", interleavedStray, nil},
		{"1 MB binary message", string(binary), nil},
		{"gzip HTTP dump", dump, nil},
	} {
		ent := zapcore.Entry{Level: zapcore.InfoLevel, Time: fixedClock{}.Now(), Message: bc.msg}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				buf, err := enc.EncodeEntry(ent, bc.fields)
				if err != nil {
					b.Fatal(err)
				}
				buf.Free()
			}
		})
	}
}
