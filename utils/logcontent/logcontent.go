// Package logcontent is the one switch that decides whether a log line may
// carry what an application sent or received: a payload's bytes, a row, a
// header's value, a bind value, a document, a mock, a URL.
//
// The level a line is logged at is not consent. keploy's agent tees its Debug
// log to agent-debug.log on every docker recording and replay, so a Debug line
// is written to disk whether or not anyone asked to debug anything, and
// whatever content a parser put in it lands there in the clear, where secret
// protection, which covers mocks, never looks. So by default, at any level, a
// line that goes through the helpers here does not write a value out: it
// names the value by its length, by its length and a fingerprint, which gives
// away a value that can be guessed, or, for a URL, by a shape that keeps what
// reads as a name (below). Setting EnvVar to 1 or true is the explicit
// opt-in: each helper then logs the value itself, as zap.ByteString,
// zap.Binary, zap.String, zap.Any and zap.Error do.
//
// With the switch off, the default, a line keeps the facts that are not
// content (a message's type, an op's name, a protocol's or keploy's own ids,
// header names, counts) and names a value by what the helper it goes through
// logs instead:
//
//   - Bytes, Binary and String log its exact length and its fingerprint,
//     {"len": 5, "fp": "2cf24dba5fb0"}. The fingerprint (Fingerprint) is the
//     first 48 bits of the value's sha256, with no key and no salt, so it is
//     no redaction: it hides a value only as well as the value is hard to
//     guess. Whoever reads the line hashes guesses and compares, and 48 bits
//     pick the one that matches out of billions, which one GPU hashes in a
//     second. So a value from a small or a structured set is found from its
//     fingerprint: an OTP or a PIN, a status, an enum or a boolean, a small
//     id, a phone number, an SSN or a card number, an email address on a
//     breach list, Basic auth's user:pass with a common password. And a value
//     has the same fingerprint in every line, every run and every
//     installation, so equal values are linked across runs, and across
//     customers whose logs are read side by side. It hides only a value no
//     one can guess: a random token, a body no one can predict.
//   - StringLen and BytesLen log its exact length alone, {"len": 6}, whatever
//     the field's key: for a credential, and for a short value from a small
//     or a structured set, whose fingerprint would give it away. Bytes,
//     Binary and String log a value by its length alone too, and Any by its
//     type and length, when the field's key names a credential or a card's
//     or an account's number (String says which); NamedString does when the
//     name a sibling field gives the value does (a header's, a variable's).
//     The length is still the value's: it says a PIN has four digits.
//   - Any logs a value's Go type, which is not content, and for a string or
//     a byte slice its length and its fingerprint too, as String and Bytes
//     log them, with what they give away.
//   - Error logs an error by what it is (ErrorKind: textproto.ProtocolError,
//     *strconv.NumError, syscall.Errno(connection reset by peer)), its
//     text's length, and its shape: keploy's own words of it, each value
//     they were written around {} ("failed to run test set {}: failed to
//     decode the mock: {}", errorShape). No fingerprint: a parser's error
//     quotes the value it could not read, a length or a header line, and a
//     short one is found from its text's fingerprint as from its own.
//   - Panic logs a recovered panic's value by its type and its text's
//     length, or an error's kind, unless the Go runtime raised it, whose
//     own words (an index out of range, a nil dereference) it logs whole:
//     any code a goroutine runs can panic, and with whatever it was reading.
//   - logurl logs a URL, a path or a call by its shape (models.URLShape):
//     every path segment and query name that does not read as a name, and
//     every query value, written {}, as the lines a default run prints for a
//     call keploy could not answer write them (models.CallShape), and an
//     absolute URL's scheme, host and port kept when they read as such; but
//     for a URL with an '@' after its host, which may end credentials, whose
//     host and whatever precedes that '@' are {} too, as they are for a path
//     with an '@' after a "//" or a ':', as a URL it holds or a user:password
//     has (/proxy/http://user:pw/x@db/x), and for a method that is none of
//     HTTP's, which is {}. A URL's query is content, and so is a path segment
//     that holds an application's value (a token in /reset/<token>, an email
//     address in /users/<email>). A segment that reads as a name is kept, so
//     a value that reads as one too (a user name in lower-case letters, a
//     slug) is still logged: keploy cannot tell it from a route's by looking
//     at it.
//
// The switch decides only what the lines that go through these helpers
// carry: a line that logs a value through zap's own constructors carries it
// whole, whatever the switch says. What keploy records, how it matches and
// the verdicts it reaches never depend on it. A line at Warn or above that a
// helper withheld a value from (Withheld) carries the Hint, which names
// EnvVar, from every logger utils/log builds. A crash report, which leaves
// the machine, never carries a panic's value (PanicText), and goes without
// the run's log while the switch is on, since the log then carries content.
//
// It depends on nothing of keploy's, so the parsers in keploy, integrations
// and enterprise all import the same switch. logurl, which shapes a URL as
// pkg/models does, is a package of its own for that reason.
package logcontent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// EnvVar, set to 1 or true, opts into log lines that carry customer content.
const EnvVar = "KEPLOY_LOG_CONTENT"

// AgentFlag is the hidden `keploy agent` flag that carries the opt-in to a
// native agent (AgentArgs): one a user who is not root starts through sudo,
// which drops EnvVar on the way.
const AgentFlag = "log-content"

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

// OptIn turns the switch on for the rest of the process: the agent's, when
// its AgentFlag says the keploy that started it was opted in. It is read at
// start, as EnvVar is, before any line that carries content is logged.
func OptIn() { on.Store(true) }

// AgentArgs is what a launcher appends to the argv of the native agent it
// starts: AgentFlag when the switch is on in this process, nothing otherwise,
// so with it off the argv is what it was.
func AgentArgs() []string {
	if !On() {
		return nil
	}
	return []string{"--" + AgentFlag}
}

// Set turns the switch on or off for a test and returns what puts it back.
// Reading the switch while a test flips it is safe from any goroutine, but
// the switch is process-wide: a test that calls Set must not call t.Parallel,
// or the tests running beside it log under its setting.
func Set(v bool) (restore func()) {
	prev := on.Swap(v)
	return func() { on.Store(prev) }
}

// Bytes is zap.ByteString(key, b) with the switch on. With it off, the field
// is b's exact length and its fingerprint, {"len": 5, "fp": "2cf24dba5fb0"},
// or its length alone when key names a credential (String says which). The
// fingerprint is no redaction (Fingerprint): whoever reads the line finds b
// from it when b is one of a set they can hash, as a short value from a
// small or a structured set is, and an equal b is named alike in every line,
// every run and every installation. Such a value goes to BytesLen instead.
//
// The fingerprint is computed when a core encodes the entry, not here: a call
// site builds its fields before zap checks the level, and a Debug line on a
// hot path whose level is off must not pay for a sha256. So the field holds b,
// as zap.ByteString does, and its key, and reads them later: a disabled line
// allocates as often as with zap.ByteString, 24 bytes more on a 64-bit
// machine, for the key. That is safe because every core keploy builds
// (utils/log's console cores, the agent-debug.log one among them, and
// enterprise's) is a zapcore.NewCore, which encodes the entry in the logging
// goroutine before the log call returns; the debug file's BufferedWriteSyncer
// buffers encoded bytes, never fields. A core that kept fields to encode
// later would read a buffer its caller had since reused, and would need this
// to hash b here instead.
// TestTheAgentDebugLogCarriesContentOnlyWhenOptedIn holds the debug file to it.
func Bytes(key string, b []byte) zap.Field {
	if on.Load() {
		return zap.ByteString(key, b)
	}
	return zap.Object(key, bytesDigest{key, b})
}

// Binary is zap.Binary(key, b), b as base64, with the switch on, and with it
// off what Bytes logs, with what Bytes says of it: the same payload logged by
// Bytes and Binary is named by the same fingerprint, whichever way a line
// would have written it.
func Binary(key string, b []byte) zap.Field {
	if on.Load() {
		return zap.Binary(key, b)
	}
	return zap.Object(key, bytesDigest{key, b})
}

// String is zap.String(key, s) with the switch on. With it off, the field is
// s's exact length and its fingerprint, computed as Bytes computes them and
// no redaction for the same reasons (Fingerprint): whoever reads the line
// finds s from it when s is one of a set they can hash, and an equal s is
// named alike in every line, every run and every installation. A short value
// from a small or a structured set, a status, a code or a phone number, goes
// to StringLen instead.
//
// A field whose key names a credential is logged by its length alone, as
// StringLen logs it: a password, a PIN or a token from a small set is found
// from its fingerprint, and enterprise's secret protection, which redacts a
// zap.String field by its key, never sees this field, which is an object. So
// is a field whose key names a card's or an account's number, or a code that
// verifies one. A key names one when, read as words (split at every ASCII
// character that is not a letter or a digit, and between camelCase words),
// one of them, with or without its trailing digits, is a credential's word
// (credentialWords: pass, pwd, auth, pin, otp, cvv, csc, ssn, sid, csrf,
// iban, ...), its words run together hold a credential's stem
// (credentialStems: password, secret, token, cookie, session, authoriz,
// authoris, apikey, authkey, sslkey, cardnum, accountnum, socialsecurity,
// verificationcode, ...), or they spell one of the names secret protection
// redacts by default that neither covers, or pan (credentialKeys: key,
// connectionstring, authresponse, ...); credentialName says how a word may
// also end in a credential's word (dbpass), be its plural (OTPs), or hold one
// in a key written in capitals alone (DBPASS), and that a key whose last word
// is key names one (x-functions-key, cache key, key2). So "Authorization",
// "x-api-key", "db_password", "authResponse", "pin_code", "cvv2", "mysqlpwd",
// "DBPASS", "JSESSIONID", "connect.sid", "cardNumber", "acct_no",
// "authorisation" and "social_security_number" name one, and "author",
// "mapping", "keyspace", "span_id", "status_code" and "userIDs" do not. A
// name a user configures secret protection to redact is not among them: with
// the switch off, a String field under it is fingerprinted where a zap.String
// field was redacted, so a value logged under such a name goes to StringLen,
// as does any other value from a small set under a key that names none (a
// status, an email address). A value whose name is in a sibling field
// ("name", then "value") goes to NamedString.
//
// Its off field costs a disabled line one small allocation, for the string
// and the key it holds, that zap.String does not; content that is still a
// []byte should go to Bytes, which allocates as often as zap.ByteString
// does, rather than be converted for this.
func String(key, s string) zap.Field {
	if on.Load() {
		return zap.String(key, s)
	}
	return zap.Object(key, stringDigest{key: key, s: s})
}

// NamedString is String for the value of what a sibling field names, a
// header, a variable or a parameter called name, logged under a key of its
// own: zap.String("name", h), NamedString("value", h, v). It is
// zap.String(key, s) with the switch on, and with it off what String logs,
// but by its length alone when name names a credential, as String logs a
// field whose key does (String says which): a header called Authorization,
// a variable called resetToken. It costs a disabled line what String does.
// With the switch on, the field is what zap.String(key, s) logs, as a line
// that logged the value under "value" did before: secret protection, which
// redacts a zap.String field by its key, reads key, not name.
func NamedString(key, name, s string) zap.Field {
	if on.Load() {
		return zap.String(key, s)
	}
	return zap.Object(key, stringDigest{key: key, name: name, s: s})
}

// StringLen is zap.String(key, s) with the switch on, and s's exact length
// alone with it off, {"len": 6}, whatever its key: for a credential, and for
// a short value from a small or a structured set (an OTP or a PIN, a status,
// an enum, a boolean as text, a small id, a phone number, an SSN or a card
// number, an email address), whose fingerprint would give it away. The
// length is still the value's: it says a PIN has four digits. Its off field
// costs a disabled line nothing zap.String does not for a value shorter than
// 256 bytes, and one small allocation, for the length, for a longer one.
func StringLen(key, s string) zap.Field {
	if on.Load() {
		return zap.String(key, s)
	}
	return zap.Object(key, lenOnly(len(s)))
}

// BytesLen is zap.ByteString(key, b) with the switch on, and b's exact length
// alone with it off, as StringLen logs a string's. Its off field costs a
// disabled line no allocation zap.ByteString's does not: none for a value
// shorter than 256 bytes, and one of 8 bytes, for the length, for a longer
// one, where zap.ByteString's is one of 24, for the slice.
func BytesLen(key string, b []byte) zap.Field {
	if on.Load() {
		return zap.ByteString(key, b)
	}
	return zap.Object(key, lenOnly(len(b)))
}

// Any is zap.Any(key, v) with the switch on, for the dumps of mocks, requests
// and protocol structs that carry content in their fields. With it off, the
// field is v's Go type and nothing of its value: {"type": "*models.Mock"}.
// A v whose kind is string, or a slice of bytes, also gets its exact length
// and its fingerprint, as String and Bytes log them and with what String
// says of them: no redaction, since a value from a small set is found from
// it and an equal value is named alike in every line, run and installation,
// and a key that names a credential gets the length alone. So a line can
// still say which value it was.
//
// Like Bytes, it reads v when the entry is encoded. Its off field costs a
// disabled line one small allocation, for v and the key it holds, which
// zap.Any's costs only for some kinds of v (a []byte, which it boxes again).
func Any(key string, v any) zap.Field {
	if on.Load() {
		return zap.Any(key, v)
	}
	return zap.Object(key, typeOnly{key, v})
}

// Error is zap.Error(err) with the switch on, for an error whose text can
// quote content, as a parser's can: net/http quotes a header line it cannot
// read whole, strconv the value it could not parse, a decoder the packet, a
// database the statement it refused. With it off, the field is what err is
// (ErrorKind) and its text's exact length, {"kind":
// "textproto.ProtocolError", "len": 57}, and no fingerprint: the text holds
// the value it quotes among words anyone can read in the parser's source, so
// a short value (a PIN in a header line, the number strconv could not
// parse) is found by hashing the texts it would make. When keploy's own
// words make err's text, the field has its shape too (errorShape): those
// words, each value they were written around {}, "failed to run test set {}:
// failed to decode the mock: {}". A nil err is zap.Error's, which logs
// nothing.
//
// ErrorKind, the text and the shape are computed when a core encodes the
// entry, not here, so a disabled line costs one small allocation, for err,
// that zap.Error's does not.
func Error(err error) zap.Field {
	return NamedError("error", err)
}

// NamedError is Error under key, as zap.NamedError(key, err) is zap.Error's.
func NamedError(key string, err error) zap.Field {
	if on.Load() || err == nil {
		return zap.NamedError(key, err)
	}
	return zap.Object(key, errorDigest{err})
}

// HintKey and Hint are the field a logger adds to a line, at Warn and above,
// one of whose fields the switch withheld (Withheld): what a user reading a
// failed run's console needs to see the value, and what that costs.
const (
	HintKey = "withheld"
	Hint    = "a value above is named by its kind or length only; set " + EnvVar + "=1 to log it whole (it may hold your app's data)"
)

// Withheld reports whether f is a field one of the helpers here built with
// the switch off, which names a value by its length, kind or fingerprint
// instead of writing it: a logger adds the Hint to a line at Warn and above
// that holds one.
func Withheld(f zapcore.Field) bool {
	if f.Type != zapcore.ObjectMarshalerType {
		return false
	}
	switch f.Interface.(type) {
	case bytesDigest, stringDigest, lenOnly, errorDigest, typeOnly, panicDigest:
		return true
	}
	return false
}

// Panic is a recovered panic's value v under key: zap.Any(key, v) with the
// switch on. With it off, a panic the Go runtime raised (a runtime.Error of
// package runtime's own: an index out of range, a nil dereference, a failed
// type assertion, a divide by zero), whose text names only types, indices and
// lengths, is logged whole, as zap.String(key, its text); an error by what
// Error logs of it, its kind and its text's length; and any other value by its
// Go type, and a string's or a []byte's exact length. Nothing else of it, and
// no fingerprint: a panic can come from any code its goroutine runs, a
// third-party decoder's among it, and its value is what that code made it,
// panic(err) of an error that quotes the payload, or panic(fmt.Sprintf("bad
// frame %x", b)), which is found from a fingerprint as an error's text is
// (Error).
func Panic(key string, v any) zap.Field {
	if on.Load() {
		return zap.Any(key, v)
	}
	if s, ok := runtimeText(v); ok {
		return zap.String(key, s)
	}
	return zap.Object(key, panicDigest{v})
}

// PanicText is v, a recovered panic's value, as a text that carries no
// content whatever the switch says, for what is no log line and leaves the
// machine, a crash report: the Go runtime's own error whole, as Panic logs
// it; a text or an error keploy's own words make, by its shape, as Error
// writes an error's (the words of the constant texts and formats keploy's
// code panics with and makes errors with, and {} for each value they
// format), "a panic of string: failed to read {} of {}: {}", so a crash
// report of keploy's own panic is grouped and read by what it says; and any
// other value by its type and its text's length, "a panic of string (31
// bytes)", or for an error by its kind, "a panic of *strconv.NumError (40
// bytes)".
func PanicText(v any) string {
	if s, ok := runtimeText(v); ok {
		return s
	}
	switch x := v.(type) {
	case string:
		if shape := textShape(x); shape != "" {
			return "a panic of string: " + shape
		}
	case error:
		if shape := panicErrorShape(x); shape != "" {
			return "a panic of " + ErrorKind(x) + ": " + shape
		}
	}
	kind, n := panicKind(v)
	if n < 0 {
		return "a panic of " + kind
	}
	return "a panic of " + kind + " (" + strconv.Itoa(n) + " bytes)"
}

// runtimeText is v's text when v is a panic the Go runtime raised: a
// runtime.Error whose type is package runtime's, as every error the runtime
// panics with is. A type of any other package may have a RuntimeError method
// too, and its text is whatever that package made it.
func runtimeText(v any) (string, bool) {
	re, ok := v.(runtime.Error)
	if !ok {
		return "", false
	}
	t := reflect.TypeOf(re)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() != "runtime" {
		return "", false
	}
	return re.Error(), true
}

// panicKind is what a panic's value v is, for a line or a report that does not
// carry it: an error's kind (ErrorKind) or any other value's Go type, and the
// length of its text (an error's, a string's, a []byte's), or -1 for a value
// that has none.
func panicKind(v any) (string, int) {
	switch x := v.(type) {
	case nil:
		return "<nil>", -1
	case error:
		return ErrorKind(x), len(x.Error())
	}
	rv := reflect.ValueOf(v)
	switch {
	case rv.Kind() == reflect.String:
		return rv.Type().String(), rv.Len()
	case rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8:
		return rv.Type().String(), rv.Len()
	}
	return rv.Type().String(), -1
}

// panicDigest is a panic's value by its kind and its text's length (Panic).
type panicDigest struct{ v any }

func (p panicDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	kind, n := panicKind(p.v)
	if _, isErr := p.v.(error); isErr {
		enc.AddString("kind", kind)
	} else {
		enc.AddString("type", kind)
	}
	if n >= 0 {
		enc.AddInt("len", n)
	}
	return nil
}

// ErrorKind is what err is, for a line that does not carry its text: the Go
// type of the last error in its chain, as errors.Unwrap follows it, that is
// more than text. errors.New's and fmt.Errorf's carry nothing but their text,
// so an error is named by its cause however it was wrapped:
// textproto.ProtocolError for net/http's "malformed MIME header",
// *strconv.NumError for a length that is not a number, and an errno by what
// it says, syscall.Errno(connection reset by peer), which is the kernel's and
// not the application's. A chain of text alone is named by the standard
// library's sentinel it ends in (io.ErrUnexpectedEOF), and failing that by
// its own type, as errors.Join's is.
func ErrorKind(err error) string {
	if err == nil {
		return "<nil>"
	}
	var cause error
	sentinel := ""
	for e := err; e != nil; e = errors.Unwrap(e) {
		if !textOnly(reflect.TypeOf(e)) {
			cause = e
		}
		for _, s := range sentinels {
			if e == s.err {
				sentinel = s.name
			}
		}
	}
	if errno, ok := cause.(syscall.Errno); ok {
		return "syscall.Errno(" + errno.Error() + ")"
	}
	switch {
	case cause != nil:
		return reflect.TypeOf(cause).String()
	case sentinel != "":
		return sentinel
	}
	return reflect.TypeOf(err).String()
}

// sentinels are the standard library's errors that are text alone, which a
// parser's error most often ends in, by the names Go gives them.
var sentinels = []struct {
	err  error
	name string
}{
	{io.EOF, "io.EOF"},
	{io.ErrUnexpectedEOF, "io.ErrUnexpectedEOF"},
	{io.ErrClosedPipe, "io.ErrClosedPipe"},
	{io.ErrShortBuffer, "io.ErrShortBuffer"},
	{io.ErrShortWrite, "io.ErrShortWrite"},
	{context.Canceled, "context.Canceled"},
	{io.ErrNoProgress, "io.ErrNoProgress"},
	{os.ErrNotExist, "os.ErrNotExist"},
	{os.ErrExist, "os.ErrExist"},
	{os.ErrPermission, "os.ErrPermission"},
	{os.ErrInvalid, "os.ErrInvalid"},
	{os.ErrClosed, "os.ErrClosed"},
	{os.ErrProcessDone, "os.ErrProcessDone"},
	{bufio.ErrBufferFull, "bufio.ErrBufferFull"},
	{bufio.ErrTooLong, "bufio.ErrTooLong"},
	{bufio.ErrNegativeCount, "bufio.ErrNegativeCount"},
}

// textOnly reports whether t is an error type of the errors or fmt package,
// which carry only their text and what they wrap.
func textOnly(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath() == "errors" || t.PkgPath() == "fmt"
}

// Fingerprint names b by a digest instead of its bytes: the first 12 hex
// digits, 48 bits, of its sha256, with no key and no salt. Two lines, two
// runs or two installations name the same payload alike, so it links equal
// values across all of them. It hides what the payload is only as well as
// the payload is hard to guess: whoever has a fingerprint hashes the guesses
// and compares, and 48 bits pick the one that matches out of billions, so a
// value from a small or a structured set (a flag, a PIN, an OTP, a card or a
// phone number, a known email address) is found from it.
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

// addDigest names b under key: its length, and its fingerprint unless key
// names a credential.
func addDigest(enc zapcore.ObjectEncoder, key string, b []byte) {
	enc.AddInt("len", len(b))
	if !credentialName(key) {
		enc.AddString("fp", Fingerprint(b))
	}
}

type bytesDigest struct {
	key string
	b   []byte
}

func (d bytesDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	addDigest(enc, d.key, d.b)
	return nil
}

// stringDigest is a string under key, and the name a sibling field gives it
// (NamedString), if any.
type stringDigest struct{ key, name, s string }

func (d stringDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if d.name != "" && credentialName(d.name) {
		enc.AddInt("len", len(d.s))
		return nil
	}
	addDigest(enc, d.key, stringBytes(d.s))
	return nil
}

type lenOnly int

func (n lenOnly) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt("len", int(n))
	return nil
}

// errorDigest is an error by its kind and its text's length (Error).
type errorDigest struct{ err error }

func (e errorDigest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	text := e.err.Error()
	enc.AddString("kind", ErrorKind(e.err))
	enc.AddInt("len", len(text))
	if shape := errorShape(e.err, text); shape != "" {
		enc.AddString("shape", shape)
	}
	return nil
}

type typeOnly struct {
	key string
	v   any
}

func (t typeOnly) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if t.v == nil {
		enc.AddString("type", "<nil>")
		return nil
	}
	rv := reflect.ValueOf(t.v)
	enc.AddString("type", rv.Type().String())
	switch {
	case rv.Kind() == reflect.String:
		addDigest(enc, t.key, stringBytes(rv.String()))
	case rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8:
		addDigest(enc, t.key, rv.Bytes())
	}
	return nil
}

// credentialWords, credentialStems and credentialKeys are what makes a key
// name a credential (String), or a card's or an account's number, which is
// as easily found from its fingerprint: a word of it, a stem its words run
// together hold, or what they spell. Over-matching costs a line only a
// fingerprint; the value is withheld either way.
var (
	credentialWords = map[string]bool{
		"pass": true, "pw": true, "pwd": true, "auth": true, "oauth": true,
		"cred": true, "creds": true, "pin": true, "otp": true, "totp": true,
		"hotp": true, "mfa": true, "2fa": true, "cvv": true, "cvc": true,
		"ccv": true, "csc": true, "cvn": true, "ssn": true, "sig": true,
		"dsn": true, "sid": true, "csrf": true, "xsrf": true, "iban": true,
	}
	credentialStems = []string{
		"password", "passwd", "passphrase", "passcode", "secret", "token",
		"credential", "cookie", "session", "authoriz", "authoris", "authent",
		"apikey", "authkey", "passkey", "privatekey", "privkey", "accesskey",
		"signingkey", "encryptionkey", "masterkey", "sharedkey", "sslkey",
		"tlskey", "aeskey", "rsakey", "hmac", "signature", "bearer", "jwt",
		"pincode", "otpcode", "mfacode", "verificationcode", "securitycode",
		"cardverification", "socialsecurity",
		"cardnum", "cardno", "cardpan", "creditcard", "ccnum", "pannumber",
		"accountnum", "accountno", "acctnum", "acctno", "routingnum",
		"routingno",
	}
	credentialKeys = map[string]bool{
		"key": true, "connectionstring": true, "pan": true,
		"authresponse": true, "authplugindata": true,
		"authmechanismproperties": true, "authmessage": true,
		"serverkey": true, "storedkey": true, "clientkey": true,
	}
)

// credentialWordList is credentialWords in a slice, for the scan
// credentialWord makes of a word for one it ends in or holds.
var credentialWordList = func() []string {
	words := make([]string, 0, len(credentialWords))
	for w := range credentialWords {
		words = append(words, w)
	}
	return words
}()

// credentialName reports whether key names a credential (String). It runs
// when an entry is encoded, never for a line whose level is off, and
// allocates nothing for a key of up to 64 bytes.
//
// key is read as lower-case words: split at every byte that is not an ASCII
// letter or digit (a byte of a multi-byte character is kept in its word),
// before a capital that follows a lower-case letter (authResponse) or a
// digit in a word that is not digits alone (OAuth2Token, but 2FA), and
// before the last capital of a run that lower-case letters follow, unless
// they are a plural's lone s (OTPCode is otp, code; OTPs is otps). A word
// names a credential when it is a credential's word (credentialWords) as it
// is, without its trailing digits (cvv2), or without a plural's s (pins), or
// when it ends in one (dbpass, mysqlpwd); a word written in capitals alone
// (DBPASSHASH), whose own words' boundaries are lost, does when it holds one
// of three letters or more anywhere. The last word names one when it is key
// or keys, with or without its trailing digits (x-functions-key, key2).
func credentialName(key string) bool {
	var stack [64]byte
	joined := stack[:0]
	if len(key) > len(stack) {
		joined = make([]byte, 0, len(key))
	}
	start, last := -1, -1
	caps, digits := false, false
	for i := 0; i < len(key); i++ {
		c := key[i]
		upper := isUpperByte(c)
		wordByte := upper || isLowerOrDigit(c) || c >= 0x80
		split := !wordByte || (upper && start >= 0 && splitsBefore(key, i, digits))
		if split && start >= 0 {
			if credentialWord(joined[start:], caps) {
				return true
			}
			last, start = start, -1
		}
		if !wordByte {
			continue
		}
		if start < 0 {
			start, caps, digits = len(joined), true, true
		}
		if c < '0' || c > '9' {
			digits = false
			if !upper {
				caps = false
			}
		}
		if upper {
			c += 'a' - 'A'
		}
		joined = append(joined, c)
	}
	if start >= 0 {
		if credentialWord(joined[start:], caps) {
			return true
		}
		last = start
	}
	if last >= 0 {
		if w := bytesString(bytes.TrimRight(joined[last:], "0123456789")); w == "key" || w == "keys" {
			return true
		}
	}
	if credentialKeys[string(joined)] {
		return true
	}
	all := bytesString(joined)
	for _, stem := range credentialStems {
		if strings.Contains(all, stem) {
			return true
		}
	}
	return false
}

// splitsBefore reports whether the capital at key[i], inside a word, starts a
// new one (credentialName); digits says the word so far is digits alone, as
// 2FA's 2 is.
func splitsBefore(key string, i int, digits bool) bool {
	prev := key[i-1]
	switch {
	case prev >= 'a' && prev <= 'z':
		return true
	case prev >= '0' && prev <= '9':
		return !digits
	case isUpperByte(prev) && i+1 < len(key) && key[i+1] >= 'a' && key[i+1] <= 'z':
		// The last capital of a run starts the word its lower-case letters
		// end, unless they are a plural's lone s.
		plural := key[i+1] == 's' && (i+2 == len(key) || key[i+2] < 'a' || key[i+2] > 'z')
		return !plural
	}
	return false
}

// credentialWord reports whether w, a lower-cased word of a key, names a
// credential (credentialName); caps says the key wrote it in capitals alone.
func credentialWord(w []byte, caps bool) bool {
	if credentialWords[string(w)] {
		return true
	}
	if t := bytes.TrimRight(w, "0123456789"); len(t) < len(w) && credentialWords[string(t)] {
		return true
	}
	if n := len(w); n > 1 && w[n-1] == 's' && credentialWords[string(w[:n-1])] {
		return true
	}
	word := bytesString(w)
	for _, cw := range credentialWordList {
		if strings.HasSuffix(word, cw) || (caps && len(cw) >= 3 && strings.Contains(word, cw)) {
			return true
		}
	}
	return false
}

// bytesString is b as a string without the copy string(b) makes, for a
// search that only reads it while b is unchanged.
func bytesString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func isUpperByte(c byte) bool { return c >= 'A' && c <= 'Z' }

func isLowerOrDigit(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
