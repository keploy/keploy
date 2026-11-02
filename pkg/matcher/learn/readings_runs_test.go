package learn

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"math/rand/v2"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The patterns decodeText read a run's alphabet by before it read them
// byte by byte (isHexRun, isB32Run).
var (
	hexRunPattern   = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	b32RunPattern   = regexp.MustCompile(`^[A-Z2-7]+=*$`)
	b32LowerPattern = regexp.MustCompile(`^[a-z2-7]+=*$`)
)

// patternDecodeText is decodeText as it read a run's alphabets by their
// patterns.
func patternDecodeText(c string) (string, bool) {
	if hexRunPattern.MatchString(c) && len(c)%2 == 0 {
		if b, err := hex.DecodeString(c); err == nil && isASCIIText(b) {
			return string(b), true
		}
	}
	if b32RunPattern.MatchString(c) || b32LowerPattern.MatchString(c) {
		if b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(strings.ToUpper(c), "=")); err == nil && len(b) > 0 && isASCIIText(b) {
			return string(b), true
		}
	}
	return b64Decode(c)
}

// runEdges are the bytes at the edges of the alphabets decodeText reads:
// each class's first and last and those beside them, padding, the other
// base64 bytes, a separator, and bytes of a character of two (ſ) and of
// none (a stray continuation byte).
const runEdges = "0123456789:/@AFGZ[`afgz{=+-_. \n\xc5\xbf\x80"

// isHexRun and isB32Run say what their patterns say, and decodeText gives
// what it gave by them, of every text of up to three of runEdges' bytes, of
// random texts of four to forty of them, and of texts in each encoding
// decodeText reads: hex, base32 in either case, padded or not, and base64.
func TestEncodingRunsAreReadAsTheirPatterns(t *testing.T) {
	decoded := 0
	check := func(s string) {
		t.Helper()
		got, ok := decodeText(s)
		if want, wantOK := patternDecodeText(s); got != want || ok != wantOK {
			t.Fatalf("decodeText(%q) = %q, %v; by the patterns %q, %v", s, got, ok, want, wantOK)
		}
		if ok {
			decoded++
		}
		if got, want := isHexRun(s), hexRunPattern.MatchString(s); got != want {
			t.Fatalf("isHexRun(%q) = %v, its pattern %v", s, got, want)
		}
		if got, want := isB32Run(s, 'A', 'Z'), b32RunPattern.MatchString(s); got != want {
			t.Fatalf("isB32Run(%q, upper) = %v, its pattern %v", s, got, want)
		}
		if got, want := isB32Run(s, 'a', 'z'), b32LowerPattern.MatchString(s); got != want {
			t.Fatalf("isB32Run(%q, lower) = %v, its pattern %v", s, got, want)
		}
	}
	matched := map[string]int{}
	count := func(s string) {
		check(s)
		if hexRunPattern.MatchString(s) {
			matched["hex"]++
		}
		if b32RunPattern.MatchString(s) {
			matched["b32"]++
		}
		if b32LowerPattern.MatchString(s) {
			matched["b32 lower"]++
		}
	}
	b := []byte{}
	var walk func(n int)
	walk = func(n int) {
		count(string(b))
		if n == 0 {
			return
		}
		for i := 0; i < len(runEdges); i++ {
			b = append(b, runEdges[i])
			walk(n - 1)
			b = b[:len(b)-1]
		}
	}
	walk(3)
	r := rand.New(rand.NewPCG(1, 2))
	// Random texts, most drawn from one class with its padding so that a
	// long one matches at all, some from every edge.
	classes := []string{"0123456789abcdefABCDEF", "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567=", "abcdefghijklmnopqrstuvwxyz234567=", runEdges}
	for i := 0; i < 200000; i++ {
		c := classes[r.IntN(len(classes))]
		n := 4 + r.IntN(37)
		b = b[:0]
		for j := 0; j < n; j++ {
			if r.IntN(50) == 0 {
				b = append(b, runEdges[r.IntN(len(runEdges))])
				continue
			}
			b = append(b, c[r.IntN(len(c))])
		}
		count(string(b))
	}
	for _, text := range []string{"u-7", "tok-0c41b2", "user@example.com", "{\"sub\":\"u-7\"}", "hello, world"} {
		b := []byte(text)
		b32 := base32.StdEncoding.EncodeToString(b)
		for _, enc := range []string{hex.EncodeToString(b), strings.ToUpper(hex.EncodeToString(b)), b32, strings.ToLower(b32), strings.TrimRight(b32, "="), strings.ToLower(strings.TrimRight(b32, "=")), base64.StdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)} {
			count(enc)
			if _, ok := decodeText(enc); !ok {
				t.Fatalf("decodeText(%q) of %q: no text", enc, text)
			}
		}
	}
	// Texts that each pattern matches, long ones too, or the row would hold
	// the reads to nothing.
	for _, k := range []string{"hex", "b32", "b32 lower"} {
		if matched[k] < 1000 {
			t.Fatalf("%d texts the %s pattern matches: too few to tell", matched[k], k)
		}
	}
	t.Logf("texts matched: %v; %d decode to text", matched, decoded)
}

// boundRowPayload is a payload as the HTTP bound row's recordings answer
// (TestLearnMatchReadsTheRecordingsBeforeACallWithinTheSetsBudget): size
// bytes of words of ten characters or so, "w255x" and a count, each a run
// of the base64 alphabet that decodes to no text.
func boundRowPayload(size int) string {
	var b strings.Builder
	b.WriteString(`{"blob":"`)
	for j := 0; b.Len() < size; j++ {
		b.WriteString("w255x" + strconv.Itoa(j) + " ")
	}
	b.WriteString(`"}`)
	return b.String()
}

// b64Text keeps no run that decodes to nothing (b64Read's seen), decodes
// none whole that its first four characters tell is no text
// (b64MayBeText), and copies no text it joins nowhere (joinWrapped):
// reading a payload of 1 MiB of runs that are none allocates less than the
// payload's size (about a third of it). Decoding each run whole left about
// seven times its size, and keeping every run tried as well took about
// fourteen.
func TestB64TextKeepsNoRunThatDecodesToNothing(t *testing.T) {
	payload := boundRowPayload(1 << 20)
	if got := b64Text(payload, nil); got != "" {
		t.Fatalf("b64Text of the bound row's payload = %.200q, want nothing: none of its runs is text", got)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b64Text(payload, nil)
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("b64Text of %d bytes of runs that are no text allocated %d bytes", len(payload), alloc)
	if alloc > uint64(len(payload)) {
		t.Fatalf("b64Text of %d bytes of runs that are no text allocated %d bytes, more than the payload: it keeps the runs that decoded to nothing, decodes whole the runs that are no text, or copies the payload", len(payload), alloc)
	}
}

// everyVariant is b64DecodeAny as it tried each of b64Variants in turn,
// whatever s's length and padding.
func everyVariant(s string) ([]byte, bool) {
	for _, enc := range b64Variants {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// b64DecodeAny gives what trying every variant gave (everyVariant), of
// every text of up to five of b64Edges' bytes (each length's four places in
// a quantum, padding at each place, and line breaks the decoder skips), of
// random texts of up to forty, and of each variant's encodings of random
// bytes, padded or not.
func TestB64DecodeAnyIsEveryVariantTried(t *testing.T) {
	const b64Edges = "Aa0+/-_=\r\n"
	same := func(s string) {
		t.Helper()
		b, ok := b64DecodeAny(s)
		wb, wok := everyVariant(s)
		if ok != wok || string(b) != string(wb) {
			t.Fatalf("b64DecodeAny(%q) = %q, %v; trying every variant gives %q, %v", s, b, ok, wb, wok)
		}
	}
	var decoded int
	var each func(prefix string, n int)
	each = func(prefix string, n int) {
		same(prefix)
		if _, ok := everyVariant(prefix); ok {
			decoded++
		}
		if n == 0 {
			return
		}
		for i := 0; i < len(b64Edges); i++ {
			each(prefix+b64Edges[i:i+1], n-1)
		}
	}
	each("", 5)
	r := rand.New(rand.NewPCG(20261102, 2))
	for i := 0; i < 100000; i++ {
		b := make([]byte, r.IntN(41))
		for j := range b {
			b[j] = b64Edges[r.IntN(len(b64Edges))]
		}
		same(string(b))
		raw := make([]byte, r.IntN(30))
		for j := range raw {
			raw[j] = byte(r.Uint32())
		}
		for _, enc := range b64Variants {
			e := enc.EncodeToString(raw)
			same(e)
			same(e + "=")
			if len(e) > 1 {
				same(e[:len(e)-1])
				same(e[:len(e)/2] + "\n" + e[len(e)/2:])
			}
		}
	}
	if decoded < 1000 {
		t.Fatalf("%d of the edge texts decode; want the texts to reach the decodes too", decoded)
	}
}

// b64DecodeAny tries no variant a run's length rules out, each of which
// failed with a buffer of its decode allocated: of a run of nine (a last
// quantum of one character, which no variant decodes) or of seven with
// padding, at its end or its start (no whole quanta, and a raw variant
// reads no '=') it allocates nothing, and of a run of ten (raw only) one
// buffer, its decode.
func TestB64DecodeAnyTriesNoVariantItsLengthRulesOut(t *testing.T) {
	for _, c := range []struct {
		run    string
		allocs float64
	}{{"w255x1234", 0}, {"w255x1=", 0}, {"=w255x1", 0}, {"w255x12345", 1}} {
		if got := testing.AllocsPerRun(100, func() { b64DecodeAny(c.run) }); got > c.allocs {
			t.Errorf("b64DecodeAny(%q) allocated %v times; want at most %v: it tries a variant its length rules out", c.run, got, c.allocs)
		}
	}
}
