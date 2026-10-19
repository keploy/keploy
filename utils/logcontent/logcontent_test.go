package logcontent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// canary is content no line may carry with the switch off. Distinctive, so a
// match is the canary and nothing else.
const canary = "kp-canary-4f1c9e-customer-row"

// carries reports whether line holds the canary in any form a zap field
// writes content in: as text, as zap.Binary's base64, or as the observer's
// map prints a []byte.
func carries(line string) bool {
	return strings.Contains(line, canary) ||
		strings.Contains(line, base64.StdEncoding.EncodeToString([]byte(canary))) ||
		strings.Contains(line, strings.Trim(fmt.Sprint([]byte(canary)), "[]"))
}

// row is the shape of a protocol struct or a mock that carries content in its
// fields, as an Any call site dumps one.
type row struct {
	Table string
	Value string
}

// mock is one value, so that the observer's map, which prints its address,
// reads the same for each run that logs it.
var mock = &row{Table: "users", Value: canary}

// rawBody is a named []byte, as json.RawMessage is.
type rawBody []byte

// constructors are the field constructors a call site uses: the helpers, or
// the zap ones they stand in for.
type constructors struct {
	bytes func(string, []byte) zap.Field
	str   func(string, string) zap.Field
	any   func(string, any) zap.Field
}

var (
	helpers = constructors{Bytes, String, Any}
	zaps    = constructors{zap.ByteString, zap.String, zap.Any}
)

// logEach logs one Debug line per constructor, and per kind of value Any
// treats apart, through logger, the way call sites build them.
func logEach(logger *zap.Logger, c constructors) {
	logger.Debug("frame", c.bytes("payload", []byte(canary)))
	logger.Debug("header", c.str("value", canary))
	logger.Debug("mock", c.any("mock", mock))
	logger.Debug("raw", c.any("raw", []byte(canary)))
	logger.Debug("text", c.any("text", canary))
	logger.Debug("named", c.any("body", rawBody(canary)))
}

// observed logs through a Debug-level observer core and returns each entry
// as it reaches a reader: the message and every field, as the observer maps
// them and as a console encoder writes the line.
func observed(t *testing.T, logLines func(*zap.Logger)) []string {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logLines(zap.New(core))
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.TimeKey = "" // so two runs' lines compare equal
	enc := zapcore.NewConsoleEncoder(cfg)
	var out []string
	for _, e := range logs.All() {
		buf, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			t.Fatalf("encode %q: %v", e.Message, err)
		}
		out = append(out, fmt.Sprintf("%s %v %s", e.Message, e.ContextMap(), buf.String()))
		buf.Free()
	}
	return out
}

func TestWanted(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{" 1 ", true},
		{"\ttrue\n", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false},
		{"on", false},
		{"t", false},
		{"2", false},
		{"01", false},
		{"1 1", false},
		{"truee", false},
	} {
		if got := Wanted(tc.v); got != tc.want {
			t.Errorf("Wanted(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// The switch is read once, at start: setting the variable later changes
// nothing.
func TestOnIsReadOnceAtStart(t *testing.T) {
	start := On()
	if start {
		t.Setenv(EnvVar, "0")
	} else {
		t.Setenv(EnvVar, "1")
	}
	if On() != start {
		t.Fatalf("On() followed %s set after start", EnvVar)
	}
}

func TestSetRestores(t *testing.T) {
	start := On()
	restore := Set(!start)
	if On() == start {
		t.Fatalf("Set(%v) left On() = %v", !start, start)
	}
	restore()
	if On() != start {
		t.Fatalf("restore left On() = %v, want %v", !start, start)
	}

	// Nested, each restore puts back what its own Set found.
	outer := Set(true)
	inner := Set(false)
	inner()
	if !On() {
		t.Fatal("the inner restore did not put back the outer Set's true")
	}
	outer()
	if On() != start {
		t.Fatalf("the outer restore left On() = %v, want %v", On(), start)
	}
}

func TestNoHelperLogsContentWithTheSwitchOff(t *testing.T) {
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) { logEach(l, helpers) })
	if len(lines) != 6 {
		t.Fatalf("logged %d lines, want 6", len(lines))
	}
	for _, line := range lines {
		if carries(line) {
			t.Errorf("a line carries the content with the switch off: %s", line)
		}
	}

	// What a line carries instead names the value.
	digest := fmt.Sprintf(`"len": %d, "fp": "%s"`, len(canary), Fingerprint([]byte(canary)))
	for i, want := range []string{
		`"payload": {` + digest + `}`,
		`"value": {` + digest + `}`,
		`"mock": {"type": "*logcontent.row"}`,
		`"raw": {"type": "[]uint8", ` + digest + `}`,
		`"text": {"type": "string", ` + digest + `}`,
		`"body": {"type": "logcontent.rawBody", ` + digest + `}`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d does not carry %s: %s", i, want, lines[i])
		}
	}
}

// With the switch on, each helper logs exactly what the zap constructor it
// stands in for does.
func TestEveryHelperLogsContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(Set(true))
	lines := observed(t, func(l *zap.Logger) { logEach(l, helpers) })
	want := observed(t, func(l *zap.Logger) { logEach(l, zaps) })
	if len(lines) != 6 || len(want) != 6 {
		t.Fatalf("logged %d and %d lines, want 6", len(lines), len(want))
	}
	for i, line := range lines {
		if !carries(line) {
			t.Errorf("a line does not carry the content with the switch on: %s", line)
		}
		if line != want[i] {
			t.Errorf("line %d is not what zap logs:\n got %s\nwant %s", i, line, want[i])
		}
	}
}

func TestAnyOfNilNamesNoType(t *testing.T) {
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) { l.Debug("nil", Any("v", nil)) })
	if len(lines) != 1 || !strings.Contains(lines[0], `"v": {"type": "<nil>"}`) {
		t.Fatalf("Any(nil) logged %q", lines)
	}
}

func TestFingerprint(t *testing.T) {
	b := []byte(canary)
	sum := sha256.Sum256(b)
	if got, want := Fingerprint(b), fmt.Sprintf("%x", sum[:6]); got != want {
		t.Fatalf("Fingerprint = %q, want %q", got, want)
	}
	if got := Fingerprint([]byte("hello")); got != "2cf24dba5fb0" {
		t.Fatalf(`Fingerprint("hello") = %q, want the first 12 hex digits of its sha256`, got)
	}
}

// countHashes makes every fingerprint count itself until the test ends.
func countHashes(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := sum256
	sum256 = func(b []byte) [sha256.Size]byte {
		n.Add(1)
		return orig(b)
	}
	t.Cleanup(func() { sum256 = orig })
	return &n
}

// With the switch off, a field costs no hash until a core encodes its entry:
// a Debug line whose level is off, on the console as in the debug file once
// its cap has disabled it, hashes nothing.
func TestADisabledLineHashesNothing(t *testing.T) {
	t.Cleanup(Set(false))
	hashes := countHashes(t)
	payload := bytes.Repeat([]byte(canary), 64)
	var out bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&out), zapcore.InfoLevel)
	logger := zap.New(core)

	for range 100 {
		logger.Debug("frame", Bytes("payload", payload), String("value", string(payload)), Any("raw", payload), Any("mock", &row{Value: canary}))
	}
	if n := hashes.Load(); n != 0 {
		t.Fatalf("100 disabled lines hashed %d times, want 0", n)
	}
	if out.Len() != 0 {
		t.Fatalf("a disabled line was written: %s", out.String())
	}

	// The same fields on a line that is written are hashed then, once each.
	logger.Info("frame", Bytes("payload", payload), String("value", string(payload)), Any("raw", payload), Any("mock", &row{Value: canary}))
	if n := hashes.Load(); n != 3 {
		t.Fatalf("an encoded line hashed %d times, want 3 (Bytes, String and Any of a []byte)", n)
	}
	if !strings.Contains(out.String(), Fingerprint(payload)) {
		t.Fatalf("the written line does not name the payload: %s", out.String())
	}
}

// Bytes with the switch off costs a disabled line nothing zap.ByteString does
// not, so a hot path that switches to it pays nothing for the switch.
func TestBytesCostsADisabledLineWhatByteStringDoes(t *testing.T) {
	t.Cleanup(Set(false))
	payload := bytes.Repeat([]byte(canary), 64)
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	ours := testing.AllocsPerRun(1000, func() { logger.Debug("frame", Bytes("payload", payload)) })
	zaps := testing.AllocsPerRun(1000, func() { logger.Debug("frame", zap.ByteString("payload", payload)) })
	if ours != zaps {
		t.Fatalf("a disabled line with Bytes allocates %v times, with zap.ByteString %v", ours, zaps)
	}
}

// Bytes at a disabled level, against zap.ByteString and against hashing the
// payload when the field is made, which is what the lazy digest saves.
func BenchmarkBytesAtADisabledLevel(b *testing.B) {
	payload := bytes.Repeat([]byte(canary), 2048) // ~60 KiB, a large frame
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	b.Run("off", func(b *testing.B) {
		b.Cleanup(Set(false))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", Bytes("payload", payload))
		}
	})
	b.Run("on", func(b *testing.B) {
		b.Cleanup(Set(true))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", Bytes("payload", payload))
		}
	})
	b.Run("zap.ByteString", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", zap.ByteString("payload", payload))
		}
	})
	b.Run("eager-hash", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", zap.String("payload", Fingerprint(payload)))
		}
	})
}
