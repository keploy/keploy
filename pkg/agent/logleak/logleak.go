// Package logleak finds captured content in a rendered log, in any encoding
// a log line could carry it in. Tests plant a body or a query value, run a
// capture path with a debug logger from Logger, and assert Find comes back
// empty: the capture paths log a length, a content type or a name, never the
// bytes.
package logleak

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Window is the run of content Find looks for: any Window bytes of it, in
// any encoding (windowOf). A shorter run matches by chance.
const Window = 8

// windowOf is how long a run of an encoding encoded characters long is when
// it holds Window bytes of content content bytes long, on average: Window
// for an encoding that writes a byte as one character (raw, quoted), more
// for one that writes it as several (hex 16, spaced hex 24, Go's %v of a
// []byte about 32), so a run Find finds always holds about Window bytes of
// the content, never the two or three a log's own hex or decimal holds by
// chance.
func windowOf(encoded, content int) int {
	return max(Window, encoded*Window/content)
}

// Encodings is every form a log line could carry content in: as it is,
// quoted or JSON-escaped, URL-escaped, as hex (also spaced, as hex.Dump and
// "% x" write it), as hex.Dump's text column, as Go prints a []byte, and as
// base64 in each alphabet from each of its three alignments (base64 of a
// slice that starts at a byte offset not a multiple of 3 shares no text with
// the base64 of the whole). hex.Dump's offsets and padding are not content,
// so its two columns are looked for, not its output whole: an offset
// "00000000" or a run of spaces in a log is no leak.
func Encodings(content []byte) map[string]string {
	quoted, _ := json.Marshal(string(content))
	enc := map[string]string{
		"raw":          string(content),
		"quoted":       strconv.Quote(string(content)),
		"json":         string(quoted),
		"query-escape": url.QueryEscape(string(content)),
		"path-escape":  url.PathEscape(string(content)),
		"hex":          hex.EncodeToString(content),
		"HEX":          strings.ToUpper(hex.EncodeToString(content)),
		"%v":           fmt.Sprintf("%v", content),
		"spaced hex":   fmt.Sprintf("% x", content),
		"spaced HEX":   fmt.Sprintf("% X", content),
		"dump text":    dumpText(content),
		"%q of []byte": fmt.Sprintf("%q", content),
	}
	for name, e := range map[string]*base64.Encoding{"std": base64.StdEncoding, "url": base64.URLEncoding, "rawstd": base64.RawStdEncoding, "rawurl": base64.RawURLEncoding} {
		for shift := 0; shift < 3 && shift < len(content); shift++ {
			enc[fmt.Sprintf("base64 %s from byte %d", name, shift)] = e.EncodeToString(content[shift:])
		}
	}
	return enc
}

// dumpText is hex.Dump's text column: each byte printable in ASCII as it is,
// any other as '.'.
func dumpText(content []byte) string {
	out := make([]byte, len(content))
	for i, c := range content {
		if c < 32 || c > 126 {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}

// Find names each encoding of content that has a run of Window bytes of
// content (windowOf) in log,
// with the run found, sorted. It fails t's setup, before it looks, for
// content it cannot look for without matching a log that holds none of it
// (Unplantable): content shorter than Window, which a log holds by chance,
// and content with a byte outside printable ASCII, whose hex.Dump text
// column is '.' there, as any log's dotted text is.
func Find(t testing.TB, log string, content []byte) []string {
	t.Helper()
	if why := Unplantable(content); why != "" {
		t.Fatalf("logleak: a row cannot look for %q: %s; plant printable content of at least %d bytes", content, why, Window)
		return nil
	}
	var found []string
	for name, e := range Encodings(content) {
		w := windowOf(len(e), len(content))
		for i := 0; i+w <= len(e); i++ {
			if s := e[i : i+w]; strings.Contains(log, s) {
				found = append(found, fmt.Sprintf("%s: %q", name, s))
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

// Unplantable says why Find cannot look for content, or "" when it can:
// content shorter than Window (a run that short is in a log by chance) or
// with a byte outside printable ASCII (tab, newline and UTF-8 included),
// which hex.Dump's text column writes '.', so a window of it matches a log's
// "........" whatever the log holds.
func Unplantable(content []byte) string {
	if len(content) < Window {
		return fmt.Sprintf("it is %d bytes, under %d", len(content), Window)
	}
	for i, c := range content {
		if c < 32 || c > 126 {
			return fmt.Sprintf("its byte %d (0x%02x) is not printable ASCII", i, c)
		}
	}
	return ""
}

// Buffer is a rendered log, safe to write from many goroutines.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Sync is a no-op: the log is in memory.
func (b *Buffer) Sync() error { return nil }

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Logger is a Debug logger that renders each entry as production renders
// it, as JSON and as console text, into the Buffer it returns, and passes
// each entry to also (an observer core, say) when it is not nil.
func Logger(also zapcore.Core) (*zap.Logger, *Buffer) {
	out := &Buffer{}
	cfg := zap.NewProductionEncoderConfig()
	cores := []zapcore.Core{
		zapcore.NewCore(zapcore.NewJSONEncoder(cfg), out, zap.DebugLevel),
		zapcore.NewCore(zapcore.NewConsoleEncoder(cfg), out, zap.DebugLevel),
	}
	if also != nil {
		cores = append(cores, also)
	}
	return zap.New(zapcore.NewTee(cores...)), out
}
