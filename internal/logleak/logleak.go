// Package logleak finds a secret in what a test's logger wrote. A test puts a
// made-up secret where a credential or the application's data would be (an
// Authorization header, a password, a body, a row), runs the code with a
// logger that keeps every entry at debug level, and asks whether any entry
// carries the secret: in its message or in any field, in any form code writes
// bytes in, whole or cut short. Only _test.go files import it, so no keploy
// binary links it.
package logleak

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Canary is a made-up secret for a test to put where a credential or the
// application's data would be. It names no real account. Letters and digits
// only, so every protocol carries it as it is, and its URL-encoded and
// JSON-escaped forms are itself.
const Canary = "kpLogCanary7Qx3vZ9mW2"

// Window is the fewest of the secret's bytes in a row a line may not hold. A
// line that prints only a buffer's first bytes (a preview, a hex dump of a
// message's start) holds the part of the secret that fell in them; eight
// bytes of a made-up secret in a row are written only by writing it.
const Window = 8

// Observe is a logger that keeps every entry, at debug level and above, and
// what it kept. The agent runs at debug level in production, so a test of
// what it logs observes at that level.
func Observe() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// Forms are the ways s can appear in a log line written from bytes that hold
// it: as text (a string, a Go-quoted string, a JSON string, URL-encoded as a
// query value or a path segment), as hex (%x, % x, hex.EncodeToString,
// \x-escaped, either case), as base64 at each of the three alignments s
// inside a longer buffer can have (zap.Binary, base64.EncodeToString, a []byte
// in a JSON-encoded struct; standard and URL alphabets), and as a list of its
// bytes in decimal (%v of a []byte, JSON of a []int or a [N]byte).
func Forms(s string) []string {
	b := []byte(s)
	h := hex.EncodeToString(b)
	js, _ := json.Marshal(s)
	forms := []string{
		s,
		strings.Trim(strconv.Quote(s), `"`),
		strings.Trim(string(js), `"`),
		url.QueryEscape(s),
		url.PathEscape(s),
		h,
		strings.ToUpper(h),
		spaced(h, 2, " "),
		strings.ToUpper(spaced(h, 2, " ")),
		`\x` + spaced(h, 2, `\x`),
		decimal(b, " "),
		decimal(b, ","),
		decimal(b, ", "),
	}
	// Base64 encodes three bytes to four characters, so s at offset k in a
	// buffer is written as the encoding of its bytes from the first
	// three-byte boundary on: whole groups of its, whatever is around it.
	for k := 0; k < 3; k++ {
		rest := b[min(k, len(b)):]
		rest = rest[:len(rest)-len(rest)%3]
		if len(rest) < 6 {
			continue
		}
		forms = append(forms,
			base64.RawStdEncoding.EncodeToString(rest),
			base64.RawURLEncoding.EncodeToString(rest))
	}
	return dedup(forms)
}

// WindowForms are the Forms of every run of Window bytes of secret (all of
// it, if it is shorter): a line that holds any part of the secret that long,
// in any form, holds one of them.
func WindowForms(secret string) []string {
	if len(secret) <= Window {
		return Forms(secret)
	}
	var out []string
	for i := 0; i+Window <= len(secret); i++ {
		out = append(out, Forms(secret[i:i+Window])...)
	}
	return dedup(out)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func spaced(s string, n int, sep string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i += n {
		if i > 0 {
			sb.WriteString(sep)
		}
		sb.WriteString(s[i:min(i+n, len(s))])
	}
	return sb.String()
}

// decimal is b's bytes in decimal, sep between them: %v of a []byte (" "),
// JSON of a []int or a [N]byte (","), a Go literal (", ").
func decimal(b []byte, sep string) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.Itoa(int(c))
	}
	return strings.Join(parts, sep)
}

// Text is the entry as keploy writes it: as a JSON log line and as a console
// line (utils/log's encoder is zap's console one), each with its level,
// logger name, message, stack and every field, those of the logger's With
// included (zap.Any and zap.Reflect through encoding/json, an error through
// its Error()).
func Text(e observer.LoggedEntry) string {
	cfg := zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "level",
		NameKey:        "logger",
		StacktraceKey:  "stack",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}
	var sb strings.Builder
	for _, enc := range []zapcore.Encoder{zapcore.NewJSONEncoder(cfg), zapcore.NewConsoleEncoder(cfg)} {
		buf, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			// A field the encoder refuses is still looked at: its own text.
			sb.WriteString(e.Message)
			for _, f := range e.Context {
				sb.WriteString(" " + f.Key + "=" + f.String)
				if f.Interface != nil {
					sb.WriteString(" " + stringOf(f.Interface))
				}
			}
			sb.WriteString("\n")
			continue
		}
		sb.Write(buf.Bytes())
		buf.Free()
	}
	return sb.String()
}

func stringOf(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case error:
		return x.Error()
	case interface{ String() string }:
		return x.String()
	}
	return ""
}

// detector finds Window or more bytes of one secret in a row in a text.
type detector struct {
	// forms are WindowForms of the secret, lower case.
	forms []string
	// windows are the Window-long runs of the secret's letters and digits,
	// lower case.
	windows []string
	// mask are the forms a message's secret is written <secret> by.
	mask []string
}

func newDetector(secret string) detector {
	d := detector{mask: append(Forms(secret), WindowForms(secret)...)}
	for _, f := range WindowForms(secret) {
		d.forms = append(d.forms, strings.ToLower(f))
	}
	norm := normalise(secret)
	if len(norm) <= Window {
		d.windows = []string{norm}
	}
	for i := 0; i+Window <= len(norm); i++ {
		d.windows = append(d.windows, norm[i:i+Window])
	}
	d.forms, d.windows = dedup(d.forms), dedup(d.windows)
	return d
}

// normalise is s in lower case, cut to its ASCII letters and digits.
func normalise(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 'a' - 'A')
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteByte(c)
		}
	}
	return b.String()
}

var (
	// hexRun is a run of eight or more hex byte pairs, each maybe set off
	// by a space, ':', '.', ',', ", ", '-', and maybe written with a \x
	// escape (\\x in JSON), a 0x (%#v of a []byte) or a % (percent-encoding).
	hexRun   = regexp.MustCompile(`(?i)(?:(?:\\{1,2}x|0x|%)?[0-9a-f]{2}(?:,\s?|[\s:.\-])?){8,}`)
	hexEsc   = regexp.MustCompile(`(?i)\\+x|0x|%`)
	notHex   = regexp.MustCompile(`(?i)[^0-9a-f]`)
	b64Run   = regexp.MustCompile(`[A-Za-z0-9+/_\-]{12,}`)
	b64Break = strings.NewReplacer(`\r`, "", `\n`, "", `\t`, "", "\r", "", "\n", "", "\t", "", " ", "")
)

// nuls are a NUL as text holds it: UTF-16 text has one after each ASCII
// letter, and JSON writes it \u0000.
var nuls = strings.NewReplacer(`\u0000`, "", "\x00", "")

// holds says how text holds part of the secret: in one of its forms, case
// aside; in text cut to its letters and digits, NULs (UTF-16) left out; or
// in what a run of hex or of base64 in text (wrapped over lines too)
// decodes to, itself looked at again (base64 of base64). "" for none.
func (d detector) holds(text string) string {
	return d.holdsAt(text, 2)
}

func (d detector) holdsAt(text string, depth int) string {
	lower := strings.ToLower(text)
	for i, f := range d.forms {
		if strings.Contains(lower, f) {
			return "form " + strconv.Itoa(i) + " of logleak.WindowForms"
		}
	}
	if d.inPlain(lower) || d.inPlain(nuls.Replace(lower)) {
		return "its letters and digits"
	}
	if depth == 0 {
		return ""
	}
	for _, run := range hexRun.FindAllString(text, -1) {
		digits := notHex.ReplaceAllString(hexEsc.ReplaceAllString(run, ""), "")
		for off := 0; off < 2 && off < len(digits); off++ {
			h := digits[off:]
			if b, err := hex.DecodeString(h[:len(h)/2*2]); err == nil && (d.holdsAt(string(b), depth-1) != "" || d.inGzip(b, depth)) {
				return "a hex run"
			}
		}
	}
	for _, run := range b64Run.FindAllString(b64Break.Replace(text), -1) {
		std := strings.NewReplacer("-", "+", "_", "/").Replace(run)
		for off := 0; off < 4 && off < len(std); off++ {
			r := std[off:]
			if b, err := base64.RawStdEncoding.DecodeString(r[:len(r)/4*4]); err == nil && (d.holdsAt(string(b), depth-1) != "" || d.inGzip(b, depth)) {
				return "a base64 run"
			}
		}
	}
	return ""
}

// inGzip reports whether b is a gzip stream (a compressed body logged
// encoded) whose first MiB holds the secret.
func (d detector) inGzip(b []byte, depth int) bool {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return false
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return false
	}
	out, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	return d.holdsAt(string(out), depth-1) != ""
}

// inPlain reports whether lower, a text in lower case, holds a window of the
// secret, as it is or cut to its letters and digits.
func (d detector) inPlain(lower string) bool {
	norm := normalise(lower)
	for _, w := range d.windows {
		if strings.Contains(lower, w) || strings.Contains(norm, w) {
			return true
		}
	}
	return false
}

// Find is each entry that holds Window or more bytes of secret in a row, in
// any form (detector.holds), as a line naming the entry's level, the start
// of its message, its fields' keys and how the secret was found. The line
// never holds the secret: a failing test says where it was written, not
// what.
func Find(entries []observer.LoggedEntry, secret string) []string {
	d := newDetector(secret)
	var found []string
	for _, e := range entries {
		if how := d.holds(Text(e)); how != "" {
			msg := messageStart(e.Message, d.mask)
			if d.holds(msg) != "" {
				msg = "(message withheld: it holds the secret)"
			}
			found = append(found, e.Level.String()+" "+strconv.Quote(msg)+
				" (fields: "+strings.Join(fieldKeys(e), ",")+"): part of the secret, in "+how)
		}
	}
	return found
}

// Holds reports whether text holds Window or more bytes of secret in a row,
// in any form Find looks for: for a returned error or a report, which the
// caller logs.
func Holds(text, secret string) bool {
	return newDetector(secret).holds(text) != ""
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func fieldKeys(e observer.LoggedEntry) []string {
	keys := make([]string, 0, len(e.Context))
	for _, f := range e.Context {
		keys = append(keys, f.Key)
	}
	return keys
}

// messageStart is msg's first line, at most 80 bytes of it, each form of the
// secret in it written <secret>, with the letters and digits around one (a
// run shorter than Window) taken in.
func messageStart(msg string, forms []string) string {
	for _, f := range forms {
		msg = strings.ReplaceAll(msg, f, "<secret>")
	}
	for strings.Contains(msg, "<secret>") {
		i := strings.Index(msg, "<secret>")
		j := i + len("<secret>")
		for j < len(msg) && isWordByte(msg[j]) {
			j++
		}
		for i > 0 && isWordByte(msg[i-1]) {
			i--
		}
		msg = msg[:i] + "\x00SECRET\x00" + msg[j:]
	}
	msg = strings.ReplaceAll(msg, "\x00SECRET\x00", "<secret>")
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] + " ..."
	}
	if len(msg) > 80 {
		msg = msg[:80] + " ..."
	}
	return msg
}

// Check fails t for each entry logs kept that holds part of secret, saying
// what wrote it.
func Check(t testing.TB, logs *observer.ObservedLogs, secret, what string) {
	t.Helper()
	for _, line := range Find(logs.All(), secret) {
		t.Errorf("%s logged the secret it was given: %s", what, line)
	}
}
