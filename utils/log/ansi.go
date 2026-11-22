package log

import (
	"bytes"
	"crypto/rand"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// A Keploy log line goes to the user's terminal, and much of what Keploy logs
// is not its own: a server's error body, a replayed response, a JSON reply
// whose \u001b json.Unmarshal turned into a real ESC, an error message built
// from any of them. zap does not keep that data inert. It writes the message,
// the prefix and the stacktrace verbatim, and inside a field it escapes C0
// controls but passes DEL and C1 controls (U+0080-U+009F; U+009B is CSI)
// through. So every line this package's console encoders build goes through
// sanitizeLine, and on a UTF-8 terminal it guarantees:
//
//   - the only control characters left in the line, besides the configured
//     line ending, are \t, \n, a \r right before a \n (which cannot overprint
//     anything) and, in the ANSI encoder, an ESC that opens an SGR sequence
//     (ESC [ digits and ';' m) that terminals read alike (readSGR), which sets
//     colour and text attributes and nothing else — no clearing the screen,
//     moving the cursor, setting the window title, writing to the clipboard
//     or making the terminal reply. So an SGR 11 or 12 is text: the Linux
//     console reads it as a display mode in which it stops decoding UTF-8 and
//     acts on each byte, and 0x9B, which ends Л, ě, 丛 and many more
//     characters, is a CSI to it, so the text after could move the cursor or
//     make the console reply;
//   - what the line turns on ends with it: when the line's last SGR leaves an
//     attribute on, a reset, ESC[0m, goes before the line ending;
//   - every other C0 control, DEL and C1 control, and under --disable-ansi
//     every ESC, is written as its JSON escape text, e.g. \u001b. That is how
//     zap already writes an ESC, and most C0 controls, inside a field, so a
//     neutralised sequence reads the same in the message as in a field, and it
//     still says which byte the data carried — U+FFFD would collapse ESC, BEL,
//     CR and CSI into one glyph and lose what a replay mismatch is about.
//
// A stray 0x80-0x9F byte, which is not UTF-8, is written as U+FFFD, the
// replacement character, which shows that a byte that was not UTF-8 was there
// (a terminal shows the raw byte as U+FFFD or, as xterm.js does, not at all).
// A UTF-8 terminal does not act on one, but every overlong form of a C0 or C1
// control ends in one (c0 9b is ESC in two bytes), so even a terminal whose
// decoder takes overlong forms, which a conforming one must not, gets no
// control from them. The other invalid bytes are written as they are.
//
// The guarantee is for what reaches the sink, and redactingWriter lets the
// Redactor rewrite a line after the encoder has made it, so the writer applies
// the rule again to any line the Redactor changed: enterprise's keeps ESC, '['
// and ';' and maps a digit to a digit and a letter to a letter, which turns an
// SGR into any other CSI. A logger built on zap's own encoders gets none of
// this.
//
// Keploy's own colours are SGR, and every line Keploy colours ends in a reset,
// so they reach the terminal byte for byte: the level
// (CapitalColorLevelEncoder), a coloured message (a models.Highlight* string,
// or Sugar().Infoln of one) and, in the ANSI encoder, a Highlight* value in a
// field, whose ESC zap escaped as \u001b and sanitizeLine turns back into a raw
// ESC.
//
// SGR from the data that terminals read alike passes as well in the ANSI
// encoder: until its line ends it can recolour or hide text, and with a \n in
// the message draw what looks like a line of Keploy's own, but it cannot act
// on the terminal.

// consoleEncoder is the console encoder of every Keploy logger: zap's console
// encoder, with each line it writes passed through sanitizeLine.
type consoleEncoder struct {
	*zapcore.EncoderConfig
	zapcore.Encoder
	// colour keeps SGR sequences, and renders the ones zap escaped inside
	// fields.
	colour bool
	// lineEnding is what zap ends each line with.
	lineEnding string
}

// NewANSIConsoleEncoder returns the encoder of the default, "ansiConsole",
// logger: colours render in the message and in fields alike, and no other
// control sequence reaches the terminal.
func NewANSIConsoleEncoder(cfg zapcore.EncoderConfig) zapcore.Encoder {
	return newANSIConsoleEncoder(cfg)
}

func newANSIConsoleEncoder(cfg zapcore.EncoderConfig) consoleEncoder {
	return newConsoleEncoder(cfg, true)
}

// newPlainConsoleEncoder returns the --disable-ansi encoder. No ESC reaches the
// terminal from it: a colour in a field stays the text zap escaped it to, as it
// always has in that mode, and one in the message becomes that text too.
func newPlainConsoleEncoder(cfg zapcore.EncoderConfig) consoleEncoder {
	return newConsoleEncoder(cfg, false)
}

func newConsoleEncoder(cfg zapcore.EncoderConfig, colour bool) consoleEncoder {
	lineEnding := cfg.LineEnding // as zap's encoder settles it
	switch {
	case cfg.SkipLineEnding:
		lineEnding = ""
	case lineEnding == "":
		lineEnding = zapcore.DefaultLineEnding
	}
	return consoleEncoder{EncoderConfig: &cfg, Encoder: zapcore.NewConsoleEncoder(cfg), colour: colour, lineEnding: lineEnding}
}

func (e consoleEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	msg := ent.Message
	ent.Message = messagePlaceholder
	line, err := e.Encoder.EncodeEntry(ent, fields)
	if err != nil {
		return nil, err
	}
	defer line.Free()
	out := linePool.Get()
	sanitizeLine(out, line.Bytes(), msg, e.lineEnding, e.colour)
	return out, nil
}

// sanitizeRewritten writes line, one of this encoder's that something rewrote
// after it, to out under the same rule. Nothing in it is taken for JSON any
// more, so an escape in it stays text.
func (e consoleEncoder) sanitizeRewritten(out *buffer.Buffer, line string) {
	end := ""
	if n := len(line) - len(e.lineEnding); n >= 0 && line[n:] == e.lineEnding {
		line, end = line[:n], e.lineEnding
	}
	w := lineWriter{out: out, sgr: e.colour}
	w.writeString(line)
	w.endLine(end)
}

// withoutEndReset returns line, one of this encoder's, without the reset
// endLine put before its line ending, if it put one there — or the data did,
// which is the same to a reader: sanitizeRewritten puts a reset back wherever
// a line needs one. The reset is Keploy's, not text, so it is not the
// Redactor's to rewrite.
func (e consoleEncoder) withoutEndReset(line string) string {
	if end := sgrReset + e.lineEnding; strings.HasSuffix(line, end) {
		return line[:len(line)-len(end)] + e.lineEnding
	}
	return line
}

func (e consoleEncoder) Clone() zapcore.Encoder {
	return consoleEncoder{
		EncoderConfig: e.EncoderConfig,
		Encoder:       e.Encoder.Clone(),
		colour:        e.colour,
		lineEnding:    e.lineEnding,
	}
}

var linePool = buffer.NewPool()

// messagePlaceholder stands in for the entry's message while zap encodes the
// line, so the message can be told apart from the JSON after it: the message
// is verbatim text, and an escape in it is literal text, not an escape. zap
// writes only the prefix before the message, and the per-process random part
// keeps a logger name in the prefix from matching it.
var (
	messagePlaceholder      = "keploy-message-" + rand.Text()
	messagePlaceholderBytes = []byte(messagePlaceholder)
)

// sanitizeLine writes line to out as sanitised above, with msg in place of
// messagePlaceholder. zap's console line is
//
//	prefix (time, level, name, caller) | message | \t{JSON context} | \nstacktrace | line ending
//
// and only the JSON context is JSON: with colour set, only there does an
// escaped ESC that opens an SGR sequence become a raw ESC. The context holds
// no raw newline — zap and encoding/json escape it — so it ends at the first
// newline, where the stacktrace begins.
func sanitizeLine(out *buffer.Buffer, line []byte, msg, lineEnding string, colour bool) {
	end := ""
	if n := len(line) - len(lineEnding); n >= 0 && string(line[n:]) == lineEnding {
		line, end = line[:n], lineEnding
	}
	w := lineWriter{out: out, sgr: colour}
	at := bytes.Index(line, messagePlaceholderBytes)
	if at < 0 {
		// No MessageKey, so no message and nothing to tell the JSON apart
		// from: every escape stays text.
		w.writeBytes(line)
		w.endLine(end)
		return
	}
	w.writeBytes(line[:at])
	w.writeString(msg)
	context, stack := line[at+len(messagePlaceholder):], []byte(nil)
	if i := bytes.IndexByte(context, '\n'); i >= 0 {
		context, stack = context[:i], context[i:]
	}
	if colour {
		w.writeContext(context)
	} else {
		w.writeBytes(context)
	}
	w.writeBytes(stack)
	w.endLine(end)
}

// lineWriter writes one line to out. It keeps an SGR sequence only when sgr is
// set, and remembers whether the ones it kept leave an attribute on.
type lineWriter struct {
	out     *buffer.Buffer
	sgr, on bool
}

// writeString and writeBytes write verbatim text: every control character but
// \t, \n, the \r of a \r\n and, with sgr, the ESC of an SGR sequence is
// rendered as inert text. They are one loop, written once for the message, a
// string, and once for the rest of zap's line, bytes, so neither pays for a
// conversion or a call through a function value. inertPrefix skips what needs
// no second look, and edit decides the bytes the loop stops at.
func (w *lineWriter) writeString(s string) {
	last := 0
	for i := inertPrefix(s); i < len(s); i += inertPrefix(s[i:]) {
		size, r := edit(s, i, w.sgr)
		if r == keep {
			if s[i] == 0x1b {
				w.on, _ = readSGR(s[i+2:i+size-1], w.on)
			}
		} else {
			w.out.AppendString(s[last:i])
			appendInert(w.out, r)
			last = i + size
		}
		i += size
	}
	w.out.AppendString(s[last:])
}

func (w *lineWriter) writeBytes(s []byte) {
	last := 0
	for i := inertPrefix(s); i < len(s); i += inertPrefix(s[i:]) {
		size, r := edit(s, i, w.sgr)
		if r == keep {
			if s[i] == 0x1b {
				w.on, _ = readSGR(s[i+2:i+size-1], w.on)
			}
		} else {
			w.out.AppendBytes(s[last:i])
			appendInert(w.out, r)
			last = i + size
		}
		i += size
	}
	w.out.AppendBytes(s[last:])
}

// writeContext writes zap's JSON context with each escaped ESC (\u001b) that
// opens an SGR sequence sgrLen keeps as a raw ESC, and the rest as writeBytes
// does. In JSON a backslash is either an escape introducer or half of an
// escaped backslash: in a run of them, an odd length ends in an introducer and
// an even one is literal backslashes, as in the printable text `\\u001b`,
// which stays text. zap and encoding/json write the escape in lowercase.
func (w *lineWriter) writeContext(context []byte) {
	for from := 0; ; {
		i := bytes.Index(context[from:], escapedESC)
		if i < 0 {
			break
		}
		i += from
		run := i // the backslash run ends in the one at i
		for run > 0 && context[run-1] == '\\' {
			run--
		}
		sgr := context[i+len(escapedESC):]
		if n := sgrLen(sgr); (i-run)%2 == 0 && n > 0 {
			w.writeBytes(context[:i])
			w.out.AppendByte(0x1b)
			w.on, _ = readSGR(sgr[1:n-1], w.on)
			context, from = sgr, 0
			continue
		}
		from = i + 1
	}
	w.writeBytes(context)
}

// endLine turns off what the line left on, then writes the line ending.
func (w *lineWriter) endLine(end string) {
	if w.on {
		w.out.AppendString(sgrReset)
	}
	w.out.AppendString(end)
}

// sgrReset turns every attribute off.
const sgrReset = "\x1b[0m"

var escapedESC = []byte(`\u001b`)

// inert is 1 for the bytes that need no second look: printable ASCII, \t, \n
// and the bytes from 0xA0 up, except 0xC2, which starts U+0080-U+009F in UTF-8.
// inertInUTF8 is the same for text known to be UTF-8, which holds no stray
// byte, so a byte from 0x80 to 0x9F is inert in it as well.
var inert, inertInUTF8 = func() (t, u [256]uint8) {
	for c := 0x20; c < 0x7f; c++ {
		t[c] = 1
	}
	for c := 0xa0; c < 0x100; c++ {
		t[c] = 1
	}
	t['\t'], t['\n'], t[0xc2] = 1, 1, 0
	u = t
	for c := 0x80; c < 0xa0; c++ {
		u[c] = 1
	}
	return t, u
}()

// inertPrefix returns how many bytes at the start of s are inert, and
// utf8InertPrefix how many are inert in UTF-8. Each tests eight at a time,
// with one branch for the eight, while it can. They are one loop, written once
// for each table: the scan of the message is about 6% slower when the table is
// passed in.
func inertPrefix[S []byte | string](s S) int {
	t, i := &inert, 0
	for ; len(s)-i >= 8; i += 8 {
		b := s[i : i+8]
		if t[b[0]]&t[b[1]]&t[b[2]]&t[b[3]]&t[b[4]]&t[b[5]]&t[b[6]]&t[b[7]] == 0 {
			break
		}
	}
	for i < len(s) && t[s[i]] != 0 {
		i++
	}
	return i
}

func utf8InertPrefix[S []byte | string](s S) int {
	t, i := &inertInUTF8, 0
	for ; len(s)-i >= 8; i += 8 {
		b := s[i : i+8]
		if t[b[0]]&t[b[1]]&t[b[2]]&t[b[3]]&t[b[4]]&t[b[5]]&t[b[6]]&t[b[7]] == 0 {
			break
		}
	}
	for i < len(s) && t[s[i]] != 0 {
		i++
	}
	return i
}

// keep is what edit returns in place of a character to write when the bytes it
// decided are written as they are.
const keep = -1

// edit decides the bytes at s[i], the first of which is not inert: how many of
// them it covers, and the character to write as inert text in their place, or
// keep.
func edit[S []byte | string](s S, i int, sgr bool) (size int, r rune) {
	switch c := s[i]; {
	case c == 0x1b:
		if n := sgrLen(s[i+1:]); sgr && n > 0 {
			return 1 + n, keep
		}
		return 1, 0x1b
	case c == '\r' && i+1 < len(s) && s[i+1] == '\n':
		return 2, keep
	case c < utf8.RuneSelf: // the other C0 controls, a lone \r, and DEL
		return 1, rune(c)
	case c == 0xc2:
		if i+1 < len(s) && 0x80 <= s[i+1] && s[i+1] < 0xa0 {
			return 2, rune(s[i+1]) // U+0080-U+009F
		}
		return 1, keep
	}
	// A continuation byte from 0x80 to 0x9F. About half the continuation bytes
	// of Chinese or Russian text are, so when this one ends a character the
	// text after it is decided here as well, as far as it is UTF-8 the rule
	// writes as it is, rather than one stop and look back at a time.
	if n := restOfCharacter(s, i); n > 0 {
		return n + utf8Text(s[i+n:]), keep
	}
	return 1, utf8.RuneError
}

// utf8Text returns how many bytes at the start of s are UTF-8 the rule writes
// as it is — printable ASCII, \t, \n and characters from U+00A0 up. It looks
// at them a window at a time, and utf8.Valid checks a window at once, far
// quicker than a character at a time. A window that is not all UTF-8 was
// looked at for nothing past its first bad byte, so the first window is small,
// utf8Window bytes, and each after it twice the one before, up to
// maxUTF8Window: a byte that is not UTF-8 right after a character, as binary
// data has all through it, costs a small window, and a window looked at for
// nothing is never much larger than the text decided before it.
func utf8Text[S []byte | string](s S) int {
	done := 0 // how many bytes are decided
	for window := utf8Window; ; window = min(2*window, maxUTF8Window) {
		end := min(len(s), done+window)
		n := done + utf8InertPrefix(s[done:end])
		more := n == end && end < len(s) // the text may go on past the window
		for k := 1; k < utf8.UTFMax && done < n && n < len(s) && s[n]&0xc0 == 0x80; k++ {
			n-- // back to the start of the character the window cut
		}
		if !valid(s[done:n]) {
			// Up to the first byte that is not UTF-8, so the text before it
			// is not looked at again.
			return done + validPrefix(s[done:n])
		}
		if !more {
			return n
		}
		done = n
	}
}

// utf8Window is the first window utf8Text looks at, and maxUTF8Window the
// largest, which bounds how far validPrefix walks a character at a time. A
// window holds a whole character (utf8.UTFMax bytes), so each one decides some
// text or finds where it ends.
const (
	utf8Window    = 16
	maxUTF8Window = 256
)

func valid[S []byte | string](s S) bool {
	switch s := any(s).(type) {
	case string:
		return utf8.ValidString(s)
	case []byte:
		return utf8.Valid(s)
	}
	return false
}

// validPrefix returns how many bytes at the start of s are UTF-8, a character
// at a time.
func validPrefix[S []byte | string](s S) int {
	i := 0
	switch s := any(s).(type) {
	case string:
		for i < len(s) {
			_, size := utf8.DecodeRuneInString(s[i:])
			if size == 1 && s[i] >= utf8.RuneSelf {
				break
			}
			i += size
		}
	case []byte:
		for i < len(s) {
			_, size := utf8.DecodeRune(s[i:])
			if size == 1 && s[i] >= utf8.RuneSelf {
				break
			}
			i += size
		}
	}
	return i
}

// restOfCharacter returns how many bytes, from s[i] on, are left of the UTF-8
// character that s[i], a continuation byte, belongs to, or 0 when it is stray.
// A decoder starts a character at every byte that is not a continuation byte,
// so s[i] belongs to the one starting at the nearest such byte before it, if
// that decodes to a character long enough to reach s[i].
func restOfCharacter[S []byte | string](s S, i int) int {
	for p := i - 1; p >= 0 && i-p < utf8.UTFMax; p-- {
		if s[p]&0xc0 == 0x80 {
			continue
		}
		var b [utf8.UTFMax]byte
		_, size := utf8.DecodeRune(b[:copy(b[:], s[p:])])
		return max(p+size-i, 0)
	}
	return 0
}

// sgrLen returns the length of the SGR sequence that s, the text right after
// an ESC, completes — "[", parameters of digits and ';', then "m" — when it is
// one the ANSI encoder keeps, one terminals read alike (readSGR), or 0. A
// private marker (<, =, >, ?), an intermediate byte or a ':' before the "m"
// makes it some other sequence.
func sgrLen[S []byte | string](s S) int {
	if len(s) == 0 || s[0] != '[' {
		return 0
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == 'm':
			if _, alike := readSGR(s[1:i], false); alike {
				return i + 1
			}
			return 0
		case c != ';' && (c < '0' || c > '9'):
			return 0
		}
	}
	return 0
}

// readSGR reads the parameters of an SGR sequence, each one on its own but
// for the colour a 38 or 48 sets (5;n or 2;r;g;b), and returns whether an
// attribute may be on after it, given whether one was before it — 0 or an
// empty parameter turns every attribute off, 10 turns none on, and any other
// turns one on — and whether terminals read it alike, so that what it leaves
// on is sure and it sets nothing but colour and attributes. They do not read
// alike:
//   - more than maxSGRParameters, as a terminal acts on only so many and
//     drops the rest (xterm.js acts on 32; the Linux console ignores a
//     sequence with more than 16), a reset among them;
//   - a parameter above 255, which no SGR has, as terminals keep one in
//     different widths (the Linux console in 32 bits, so 4294967307 is 11 to
//     it);
//   - a colour in another form, cut short or with no form, or an underline
//     colour (58): the Linux console reads the parameters after it each on
//     its own (vt.c's vc_t416_color, and csi_m, which knows no 58), where
//     another terminal reads a colour;
//   - 11 or 12, which turn the Linux console's display mode on.
func readSGR[S []byte | string](params S, on bool) (leaves, alike bool) {
	form := false                   // the next parameter is a colour's form
	colour, value, count := 0, 0, 0 // how many colour parameters are left; the one being read; how many were read
	for i := 0; i <= len(params); i++ {
		if i < len(params) && params[i] != ';' {
			if value = value*10 + int(params[i]-'0'); value > 255 {
				return on, false
			}
			continue
		}
		if count++; count > maxSGRParameters {
			return on, false
		}
		switch {
		case form:
			switch form = false; value {
			case 5:
				colour = 1
			case 2:
				colour = 3
			default:
				return on, false
			}
		case colour > 0:
			colour--
		case value == 11 || value == 12 || value == 58:
			return on, false
		case value == 0:
			on = false
		case value != 10:
			on = true
			form = value == 38 || value == 48
		}
		value = 0
	}
	return on, !form && colour == 0 // or a colour cut short
}

// maxSGRParameters is how many parameters of an SGR sequence terminals are
// sure to act on: half what xterm.js acts on, as many as the Linux console
// does, and more than any colour Keploy or a library writes needs.
const maxSGRParameters = 16

// appendInert writes r as text a terminal does not act on: a control
// character below U+00A0 as its JSON escape, in zap's lowercase hex, and
// utf8.RuneError, which stands in for a stray byte, as itself.
func appendInert(out *buffer.Buffer, r rune) {
	if r == utf8.RuneError {
		out.AppendString("\ufffd")
		return
	}
	const hex = "0123456789abcdef"
	escaped := [...]byte{'\\', 'u', '0', '0', hex[r>>4], hex[r&0xf]}
	out.AppendBytes(escaped[:])
}
