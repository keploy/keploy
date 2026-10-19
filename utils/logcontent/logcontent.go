// Package logcontent is the one switch that decides whether a log line may
// carry what an application sent or received: a payload's bytes, a row, a
// header's value, a bind value, a document, a mock.
//
// The level a line is logged at is not consent. keploy's agent tees its Debug
// log to agent-debug.log on every docker recording and replay, so a Debug line
// is written to disk whether or not anyone asked to debug anything, and
// whatever content a parser put in it lands there in the clear, where secret
// protection, which covers mocks, never looks. So by default no line carries
// content at any level. The field helpers here log a value as its length and
// fingerprint (Fingerprint), next to the facts a line keeps that are not
// content: a message's type, an op's name, ids, header names, counts. Setting
// EnvVar to 1 or true is the explicit opt-in: each helper then logs the value
// itself, as zap.ByteString, zap.String and zap.Any do.
//
// It depends on nothing of keploy's, so the parsers in keploy, integrations
// and enterprise all import the same switch.
package logcontent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// EnvVar, set to 1 or true, opts into log lines that carry customer content.
const EnvVar = "KEPLOY_LOG_CONTENT"

// on is EnvVar's answer, read once at start. It is atomic so that a test's
// Set does not race the tests logging beside it.
var on atomic.Bool

func init() { on.Store(Wanted(os.Getenv(EnvVar))) }

// Wanted reports whether v, EnvVar's value, opts in.
func Wanted(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true")
}

// On reports whether log lines carry customer content (EnvVar, read once at
// start).
func On() bool { return on.Load() }

// Set turns the switch on or off for a test and returns what puts it back.
// Reading the switch while a test flips it is safe from any goroutine, but
// the switch is process-wide: a test that calls Set must not call t.Parallel,
// or the tests running beside it log under its setting.
func Set(v bool) (restore func()) {
	prev := on.Swap(v)
	return func() { on.Store(prev) }
}

// Bytes is zap.ByteString(key, b) with the switch on. With it off, the field
// is b's length and fingerprint: {"len": 5, "fp": "2cf24dba5fb0"}.
//
// The fingerprint is computed when a core encodes the entry, not here: a call
// site builds its fields before zap checks the level, and a Debug line on a
// hot path whose level is off must not pay for a sha256. So the field holds b,
// as zap.ByteString does, and reads it later. That is safe because every core
// keploy builds (utils/log's console cores, the agent-debug.log one among
// them, and enterprise's) is a zapcore.NewCore, which encodes the entry in the
// logging goroutine before the log call returns; the debug file's
// BufferedWriteSyncer buffers encoded bytes, never fields. A core that kept
// fields to encode later would read a buffer its caller had since reused, and
// would need this to hash b here instead.
// TestTheAgentDebugLogCarriesContentOnlyWhenOptedIn holds the debug file to it.
func Bytes(key string, b []byte) zap.Field {
	if on.Load() {
		return zap.ByteString(key, b)
	}
	return zap.Object(key, bytesDigest(b))
}

// String is zap.String(key, s) with the switch on, and s's length and
// fingerprint with it off, computed as Bytes computes them. Its off field
// costs a disabled line one small allocation, for the string it holds, that
// zap.String does not; content that is still a []byte should go to Bytes,
// which costs what zap.ByteString does, rather than be converted for this.
func String(key, s string) zap.Field {
	if on.Load() {
		return zap.String(key, s)
	}
	return zap.Object(key, stringDigest(s))
}

// Any is zap.Any(key, v) with the switch on, for the dumps of mocks, requests
// and protocol structs that carry content in their fields. With it off, the
// field is v's Go type and nothing of its value: {"type": "*models.Mock"}.
// A v whose kind is string or []byte also gets its length and fingerprint,
// as String and Bytes log them, so a line can still say which value it was.
// Like Bytes, it reads v when the entry is encoded, and like String's, its off
// field costs a disabled line one small allocation that zap.Any's does not.
func Any(key string, v any) zap.Field {
	if on.Load() {
		return zap.Any(key, v)
	}
	return zap.Object(key, typeOnly{v})
}

// Fingerprint names b without carrying it: the first 12 hex digits of its
// sha256, so two lines, or two runs, name the same payload alike. It hides
// what the payload is only as well as the payload is hard to guess: a short
// value from a small set, a flag or a PIN, is found by hashing the guesses.
func Fingerprint(b []byte) string {
	sum := sum256(b)
	return hex.EncodeToString(sum[:6])
}

// sum256 is the hash every fingerprint goes through, a variable so a test can
// count the hashes a line costs.
var sum256 = sha256.Sum256

// stringBytes is s's bytes without the copy []byte(s) makes, for hashing a
// string the size of a response body. The hash only reads them.
func stringBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func addDigest(enc zapcore.ObjectEncoder, b []byte) {
	enc.AddInt("len", len(b))
	enc.AddString("fp", Fingerprint(b))
}

type bytesDigest []byte

func (b bytesDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	addDigest(enc, b)
	return nil
}

type stringDigest string

func (s stringDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	addDigest(enc, stringBytes(string(s)))
	return nil
}

type typeOnly struct{ v any }

func (t typeOnly) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if t.v == nil {
		enc.AddString("type", "<nil>")
		return nil
	}
	rv := reflect.ValueOf(t.v)
	enc.AddString("type", rv.Type().String())
	switch {
	case rv.Kind() == reflect.String:
		addDigest(enc, stringBytes(rv.String()))
	case rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8:
		addDigest(enc, rv.Bytes())
	}
	return nil
}
