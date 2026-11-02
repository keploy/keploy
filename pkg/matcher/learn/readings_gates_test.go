package learn

import (
	"encoding/base64"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The rows below hold two cuts of what reading a payload's encoded runs
// costs (b64Read) to the reads they replace, kept here as they were: each
// gives what the old read gives, and does not do the work it cuts.

// joinWrappedCopying is joinWrapped as it was: it copied s whole, joined
// or not.
func joinWrappedCopying(s string) (string, []int) {
	var b strings.Builder
	var joins []int
	line, glued := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		if b64Byte(c) {
			b.WriteByte(c)
			line++
			glued++
			i++
			if c == '=' && i < len(s) && s[i] != '=' && b64Byte(s[i]) {
				line = 0
			}
			if (c == '-' || c == '_') && i < len(s) && s[i] != '-' && s[i] != '_' {
				glued = 0
			}
			continue
		}
		j := i + 1
		switch c {
		case '\r', '\n', ' ', '\t':
			// A line break, an escaped one read as spaces, a blank line,
			// trailing or leading spaces: a join a line before the base64
			// takes in is undone by trying the run from its first join.
			for j < len(s) && (s[j] == '\r' || s[j] == '\n' || s[j] == ' ' || s[j] == '\t') {
				j++
			}
		default:
			b.WriteByte(c)
			line, glued = 0, 0
			i++
			continue
		}
		if line >= 16 && (line%4 == 0 || glued >= 16 && glued%4 == 0) && j < len(s) && b64Byte(s[j]) {
			line, glued = 0, 0
			i = j
			joins = append(joins, b.Len())
			continue
		}
		b.WriteString(s[i:j])
		line, glued = 0, 0
		i = j
	}
	return b.String(), joins
}

// b64DecodeWhole is b64Decode as it was: it decoded every run whole.
func b64DecodeWhole(s string) (string, bool) {
	b, ok := b64DecodeAny(s)
	if !ok {
		if t := strings.TrimRight(s, "="); t != s {
			b, ok = b64DecodeAny(t)
		}
	}
	if !ok || len(b) == 0 || !isText(b) {
		return "", false
	}
	return string(b), true
}

// wrappedTexts are texts joinWrapped reads: base64 wrapped at 60, 64 and 76
// columns by "\n", "\r\n", escaped line breaks read as spaces and indents,
// with a key glued by '=' or a prefix by '-' or '_', and lines of each
// length about 16; and random texts of lines of the alphabet, '=', '-',
// '_' and other bytes, between runs of spaces, tabs and line breaks.
func wrappedTexts(r *rand.Rand) []string {
	enc := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a wrapped secret, ", 20)))
	var out []string
	for _, width := range []int{12, 15, 16, 17, 20, 60, 64, 76} {
		for _, brk := range []string{"\n", "\r\n", " ", "  \t", "\n\n", "\\n"} {
			var lines []string
			for i := 0; i < len(enc); i += width {
				lines = append(lines, enc[i:min(i+width, len(enc))])
			}
			body := strings.Join(lines, brk)
			out = append(out, body, "data="+body, "v1_"+body+brk+"tail", `{"k":"`+body+`"}`)
		}
	}
	const alphabet = "AZaz09+/=-_"
	const others = ".:\"{}x"
	const breaks = " \t\r\n"
	for n := 0; n < 4000; n++ {
		var b strings.Builder
		for line := r.IntN(6); line >= 0; line-- {
			for k := 12 + r.IntN(14); k > 0; k-- {
				if r.IntN(20) == 0 {
					b.WriteByte(others[r.IntN(len(others))])
				} else {
					b.WriteByte(alphabet[r.IntN(len(alphabet))])
				}
			}
			for k := 1 + r.IntN(3); k > 0; k-- {
				b.WriteByte(breaks[r.IntN(len(breaks))])
			}
		}
		out = append(out, b.String())
	}
	return out
}

// joinWrapped gives what it gave copying s whole (joinWrappedCopying), of
// every wrapped text, the texts it joins and those it does not; and of a
// text it joins nowhere, the bound row's payload of words, it gives s
// itself and allocates nothing.
func TestJoinWrappedCopiesOnlyFromItsFirstJoin(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	joined := 0
	for _, s := range wrappedTexts(r) {
		got, gotJoins := joinWrapped(s)
		want, wantJoins := joinWrappedCopying(s)
		if got != want || !reflect.DeepEqual(gotJoins, wantJoins) {
			t.Fatalf("joinWrapped(%q) = %q, %v; copying it whole gave %q, %v", s, got, gotJoins, want, wantJoins)
		}
		if len(wantJoins) > 0 {
			joined++
		}
	}
	if joined < 100 {
		t.Fatalf("only %d of the texts were joined anywhere; the row reads both kinds", joined)
	}
	payload := boundRowPayload(64 << 10)
	if j, joins := joinWrapped(payload); j != payload || joins != nil {
		t.Fatalf("joinWrapped of the bound row's payload took %d line breaks out", len(joins))
	}
	if n := testing.AllocsPerRun(20, func() { joinWrapped(payload) }); n != 0 {
		t.Fatalf("joinWrapped of a %d-byte payload it joins nowhere allocated %v times; want none, s itself", len(payload), n)
	}
}

// encodedTexts are runs b64Decode reads: each base64 variant's encoding of
// texts whose first three bytes hold a character of one, two, three and
// four bytes at each place (whole or cut by the third byte; U+FFFD, the
// character an invalid byte decodes as, among them), a control, a
// '+' or '/' in the first four characters of their encoding, and of
// random bytes; and random runs of the alphabets, '=' and line breaks.
func encodedTexts(r *rand.Rand) []string {
	var plain [][]byte
	for _, c := range []string{"a", "é", "€", "😀", "\x01", "\x7f", "\u0085", "\t", "\n", ">", "?", "\xff", "\xc3", "\xe2\x82", "\ufffd"} {
		for at := 0; at < 4; at++ {
			plain = append(plain, []byte(strings.Repeat("x", at)+c+"tail of it"), []byte(strings.Repeat("x", at)+c))
		}
	}
	plain = append(plain, []byte(">>>?"), []byte("???>text"), []byte("w12x1000"))
	for n := 0; n < 3000; n++ {
		b := make([]byte, r.IntN(24))
		for i := range b {
			if r.IntN(3) == 0 {
				b[i] = byte(r.IntN(256))
			} else {
				b[i] = byte(0x20 + r.IntN(0x5f))
			}
		}
		if r.IntN(4) == 0 {
			b = append(b, []byte("ü€😀")[:r.IntN(9)]...)
		}
		plain = append(plain, b)
	}
	var out []string
	for _, p := range plain {
		for _, enc := range b64Variants {
			out = append(out, enc.EncodeToString(p))
		}
	}
	const runBytes = "AZaz09w+/-_=\r\n"
	for n := 0; n < 20000; n++ {
		b := make([]byte, r.IntN(16))
		for i := range b {
			b[i] = runBytes[r.IntN(len(runBytes))]
		}
		out = append(out, string(b))
	}
	return out
}

// b64Decode gives what decoding each run whole gave (b64DecodeWhole), of
// every encoded text: it reads no text as none by its first four
// characters, those that begin with a character the third byte cuts and
// those whose four hold a '+' or '/' among them.
func TestB64DecodeIsTheWholeDecodesText(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	var told, cut, texts int
	for _, s := range encodedTexts(r) {
		got, gotOK := b64Decode(s)
		want, wantOK := b64DecodeWhole(s)
		if got != want || gotOK != wantOK {
			t.Fatalf("b64Decode(%q) = %q, %v; decoding it whole gave %q, %v", s, got, gotOK, want, wantOK)
		}
		if !b64MayBeText(s) {
			told++
		}
		if wantOK {
			texts++
			if b, ok := b64DecodeAny(s); ok && len(b) >= 3 && !utf8.FullRune(b[2:3]) && utf8.RuneStart(b[2]) {
				cut++
			}
		}
	}
	if told < 1000 || cut == 0 || texts < 1000 {
		t.Fatalf("the texts held %d told as no text by their first four characters, %d texts with a character cut by the third byte, %d texts; the row reads each kind", told, cut, texts)
	}
}

// A run that decodes to no text is told so from its first four characters
// and not decoded whole: decodeText of a word of the bound row's payload,
// whose bytes are no UTF-8, and of the encoding of a control, allocates
// nothing, nor b64Decode of four characters of either.
func TestARunThatDecodesToNoTextIsNotDecodedWhole(t *testing.T) {
	control := base64.StdEncoding.EncodeToString([]byte("\x01abcdef"))
	for _, s := range []string{"w12x1000", "w255x12345", control} {
		if _, ok := decodeText(s); ok {
			t.Fatalf("decodeText(%q) read text", s)
		}
		if n := testing.AllocsPerRun(20, func() { decodeText(s) }); n != 0 {
			t.Fatalf("decodeText(%q) allocated %v times; want none: it decodes a run whole that its first four characters tell is no text", s, n)
		}
		if n := testing.AllocsPerRun(20, func() { b64Decode(s[:4]) }); n != 0 {
			t.Fatalf("b64Decode(%q) allocated %v times; want none", s[:4], n)
		}
	}
}
