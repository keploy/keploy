package mocknoise

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"regexp/syntax"
	"runtime"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// correlationsByFullParse is CorrelationsFromMock as it was before the
// request was read first: it parses and flattens both bodies of every mock
// and indexes every answer value. The rows below hold the gated detector to
// the same correlations.
func correlationsByFullParse(m *models.Mock) []models.FieldCorrelation {
	if m == nil || m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
		return nil
	}
	reqFlat := flattenJSONBody(m.Spec.HTTPReq.Body)
	respFlat := flattenJSONBody(m.Spec.HTTPResp.Body)
	if len(reqFlat) == 0 || len(respFlat) == 0 {
		return nil
	}
	respByValue := make(map[string][]string, len(respFlat))
	for path, vals := range respFlat {
		for _, v := range vals {
			respByValue[v] = append(respByValue[v], "body."+path)
		}
	}
	var out []models.FieldCorrelation
	seen := make(map[string]struct{})
	for path, vals := range reqFlat {
		for _, v := range vals {
			class, ok := appRandomClass(v)
			if !ok {
				continue
			}
			respPaths, echoed := respByValue[v]
			if !echoed {
				continue
			}
			reqPath := "body." + path
			if _, dup := seen[reqPath]; dup {
				continue
			}
			seen[reqPath] = struct{}{}
			rp := append([]string(nil), respPaths...)
			sort.Strings(rp)
			out = append(out, models.FieldCorrelation{
				RequestPath:   reqPath,
				ResponsePaths: rp,
				RecordedValue: v,
				ValueClass:    class,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
	return out
}

// largeAnswer builds a JSON answer of at least size bytes: a list of items,
// each with a uuid of its own, followed by tail (a raw JSON fragment, or "").
func largeAnswer(size int, tail string) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"sku":"SKU-%08d","ref":"%08x-1111-4222-8333-%012x","tags":["red","blue"],"desc":"Lorem ipsum dolor sit amet"}`, i, i, i, i)
	}
	b.WriteString(`]`)
	b.WriteString(tail)
	b.WriteString(`}`)
	return b.String()
}

// TestAGatedDetectorFindsWhatAFullParseFinds holds the detector to the
// correlations a parse of every answer finds, on the existing fixtures and on
// the forms the raw-text gate must see through: a value with space around it,
// and an echo whose answer writes one of the value's characters as a \u
// escape (one row for each kind and edge of [0-9A-Za-z_-]). Each row runs
// many times, since the detector reads the written values in map order.
func TestAGatedDetectorFindsWhatAFullParseFinds(t *testing.T) {
	const (
		u1    = "550e8400-e29b-41d4-a716-446655440000"
		u2    = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		u3    = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
		nonce = "A0zZ9a_kQ7x-P2mN4rT8vB1c" // holds A, 0, z, Z, 9, a, _ and -
	)
	// escapeFirst writes the first c in nonce as a \u escape, as JSON allows.
	escapeFirst := func(c byte, hex string) string {
		i := strings.IndexByte(nonce, c)
		return nonce[:i] + `\u` + hex + nonce[i+1:]
	}
	for _, tc := range []struct {
		name, req, resp string
		want            int // correlations found, so a row cannot pass by finding none on both sides
	}{
		// The fixtures of TestCorrelationsFromMock.
		{"echoed uuid", `{"idempotencyKey":"` + u1 + `","amount":10}`, `{"status":"ok","idempotencyKey":"` + u1 + `"}`, 1},
		{"echoed long hex token", `{"token":"deadbeefcafe1234"}`, `{"echo":"deadbeefcafe1234"}`, 1},
		{"echoed mixed nonce", `{"nonce":"aZ09kQ7xP2mN4rT8vB1c"}`, `{"seen":"aZ09kQ7xP2mN4rT8vB1c"}`, 1},
		{"uuid in request only, not echoed", `{"idempotencyKey":"` + u1 + `"}`, `{"status":"ok"}`, 0},
		{"static enum echoed is not random", `{"status":"ACTIVE"}`, `{"status":"ACTIVE"}`, 0},
		{"small int echoed is not random", `{"id":42}`, `{"id":42}`, 0},
		{"long decimal id echoed is not a hex token", `{"ts":"1700000000000000"}`, `{"ts":"1700000000000000"}`, 0},
		{"non-JSON bodies", `not json`, `also not json`, 0},
		{"empty answer", `{"k":"` + u1 + `"}`, ``, 0},
		{"answer of space only", `{"k":"` + u1 + `"}`, " \n\t ", 0},

		// Several values, arrays, one value at two request paths and at two
		// answer paths, and an answer value the request never wrote.
		{"several values and paths",
			`{"ids":["` + u1 + `","` + u2 + `"],"key":"` + u1 + `","n":"` + nonce + `","amount":10}`,
			`{"a":[{"id":"` + u1 + `"},{"id":"` + u2 + `"}],"b":"` + u1 + `","c":"` + u3 + `","d":"` + nonce + `"}`, 3},
		{"two written, only the first echoed", `{"a":"` + u1 + `","b":"` + u2 + `"}`, `{"e":"` + u1 + `"}`, 1},
		{"two written, only the second echoed", `{"a":"` + u1 + `","b":"` + u2 + `"}`, `{"e":"` + u2 + `"}`, 1},
		{"value with space around it", `{"k":"\n` + u1 + ` "}`, `{"e":"\n` + u1 + ` "}`, 1},
		{"value with space around it, answered bare", `{"k":"\t` + u1 + `"}`, `{"e":"` + u1 + `"}`, 0},
		{"escaped space around a value, both sides", `{"k":"\u0020` + u1 + `"}`, `{"e":"\u0020` + u1 + `"}`, 1},
		{"escaped no-break space around a value, both sides", `{"k":"\u00a0` + u1 + `"}`, `{"e":"\u00a0` + u1 + `"}`, 1},
		{"answer is not JSON but holds the text", `{"k":"` + u1 + `"}`, `id=` + u1, 0},
		{"answer escapes a letter but holds no written value", `{"k":"` + u1 + `"}`, `{"e":"caf\u0065","f":"` + u2 + `"}`, 0},
		{"answer is a bare string", `{"k":"` + u1 + `"}`, `"` + u1 + `"`, 1},
		{"answer is a bare array", `{"k":"` + u1 + `"}`, `["` + u1 + `"]`, 1},
		{"answer with space around it", `{"k":"` + u1 + `"}`, "  \n {\"e\":\"" + u1 + "\"}\n ", 1},
		{"answer key written twice, the last wins", `{"k":"` + u1 + `"}`, `{"e":"x","e":"` + u1 + `"}`, 1},
		{"value inside a JSON string of JSON is not a value", `{"k":"` + u1 + `"}`, `{"e":"{\"id\":\"` + u1 + `\"}"}`, 0},
		{"large numbers are not values", `{"n":12345678901234567890123,"m":1e-7}`, `{"n":12345678901234567890123,"m":1e-7}`, 0},

		// One escaped character of the echoed value, each kind and edge.
		{"escaped A", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('A', "0041") + `"}`, 1},
		{"escaped Z", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('Z', "005A") + `"}`, 1},
		{"escaped Z, lowercase hex digits", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('Z', "005a") + `"}`, 1},
		{"escaped a", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('a', "0061") + `"}`, 1},
		{"escaped z", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('z', "007a") + `"}`, 1},
		{"escaped 0", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('0', "0030") + `"}`, 1},
		{"escaped 9", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('9', "0039") + `"}`, 1},
		{"escaped _", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('_', "005F") + `"}`, 1},
		{"escaped -", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('-', "002d") + `"}`, 1},
		{"request escapes, answer writes it plain", `{"n":"` + escapeFirst('A', "0041") + `"}`, `{"e":"` + nonce + `"}`, 1},
		{"escaped after a non-value escape", `{"n":"` + nonce + `"}`, `{"x":"\u003c","e":"` + escapeFirst('a', "0061") + `"}`, 1},
		{"escaped before a non-value escape", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('A', "0041") + `","x":"\u003c"}`, 1},
		{"escaped 3 bytes after an escaped backslash and a u", `{"n":"` + nonce + `"}`, `{"e":["\\u","` + escapeFirst('A', "0041") + `"]}`, 1},
		{"escaped right after an escaped backslash and a u", `{"n":"` + nonce + `"}`, `{"e":["x\\u","` + escapeFirst('A', "0041") + `"]}`, 1},
		{"escaped before an answer cut inside an escape", `{"n":"` + nonce + `"}`, `{"e":"` + escapeFirst('A', "0041") + `","x":"\u00`, 0},
		{"answer cut inside an escape", `{"n":"` + nonce + `"}`, `{"e":"\u00`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mockWithBodies(tc.req, tc.resp)
			want := correlationsByFullParse(m)
			if len(want) != tc.want {
				t.Fatalf("fixture: a full parse finds %d correlations, the row expects %d: %+v", len(want), tc.want, want)
			}
			for i := 0; i < 64; i++ {
				if got := CorrelationsFromMock(zap.NewNop(), m); !reflect.DeepEqual(got, want) {
					t.Fatalf("run %d: correlations differ from a full parse:\n got  %+v\n want %+v", i, got, want)
				}
			}
		})
	}
}

// jsonString writes s as a JSON string. It escapes each character of the
// Basic Multilingual Plane as \uXXXX with probability p, with hex digits in
// either case, as JSON allows, and otherwise escapes only what JSON requires.
func jsonString(r *rand.Rand, s string, p float64) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c < 0x10000 && r.Float64() < p:
			h := fmt.Sprintf("%04x", c)
			if r.Intn(2) == 0 {
				h = strings.ToUpper(h)
			}
			b.WriteString(`\u` + h)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestAGatedDetectorMatchesAFullParseOnRandomMocks holds the detector to a
// full parse over random mocks: requests that write one to three values, some
// app-random and some not, with space around them; answers that echo some of
// them, with space added or dropped, beside other values and fragments that
// look like escapes, with any character written as a \u escape at rates up to
// 30%; and one answer in ten cut short. The seed is fixed, so a failure
// reproduces.
func TestAGatedDetectorMatchesAFullParseOnRandomMocks(t *testing.T) {
	const (
		iterations = 20000
		seed       = 1
		valueChars = "0123456789abcdefABCDEFghijkzZ_-"
	)
	r := rand.New(rand.NewSource(seed))
	value := func() string {
		switch r.Intn(4) {
		case 0:
			return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", r.Uint32(), r.Intn(65536), r.Intn(65536), r.Intn(65536), r.Int63n(1<<48))
		case 1:
			return fmt.Sprintf("%016x", r.Uint64()) + "ab"
		case 2:
			var b strings.Builder
			for i := 0; i < 20+r.Intn(5); i++ {
				b.WriteByte(valueChars[r.Intn(len(valueChars))])
			}
			return b.String() + "a1"
		default:
			return []string{"ACTIVE", "42", "hello world", "x", "1700000000000000"}[r.Intn(5)]
		}
	}
	pads := []string{"", "", "", " ", "\n", "\t", "\u00a0", " \n"}
	noise := []string{`\u`, `\u00`, `u0041`, "\u00e9", "<>", "\u65e5\u672c", `"`, `\`, `a\u0041`, `\u003c`, `\u0030`}
	found := 0
	for it := 0; it < iterations; it++ {
		vals := make([]string, 1+r.Intn(3))
		for i := range vals {
			vals[i] = pads[r.Intn(len(pads))] + value() + pads[r.Intn(len(pads))]
		}
		var req strings.Builder
		req.WriteString("{")
		for i, v := range vals {
			if i > 0 {
				req.WriteByte(',')
			}
			fmt.Fprintf(&req, `"k%d":%s`, i, jsonString(r, v, 0.02))
		}
		req.WriteString("}")

		var resp strings.Builder
		resp.WriteString(`{"arr":[`)
		p := []float64{0, 0, 0.01, 0.05, 0.3}[r.Intn(5)]
		for i, k := 0, 1+r.Intn(5); i < k; i++ {
			if i > 0 {
				resp.WriteByte(',')
			}
			var s string
			switch r.Intn(4) {
			case 0, 1: // an echo, sometimes with its space changed
				s = vals[r.Intn(len(vals))]
				if r.Intn(4) == 0 {
					s = strings.TrimSpace(s) + pads[r.Intn(len(pads))]
				}
			case 2:
				s = value()
			default:
				s = noise[r.Intn(len(noise))] + value()
			}
			resp.WriteString(jsonString(r, s, p))
		}
		resp.WriteString(`],"z":`)
		resp.WriteString(jsonString(r, noise[r.Intn(len(noise))], 0.2))
		resp.WriteString("}")
		body := resp.String()
		if r.Intn(10) == 0 {
			body = body[:r.Intn(len(body)+1)]
		}

		m := mockWithBodies(req.String(), body)
		want := correlationsByFullParse(m)
		if got := CorrelationsFromMock(zap.NewNop(), m); !reflect.DeepEqual(got, want) {
			t.Fatalf("mock %d differs from a full parse:\n request %s\n answer  %s\n got  %+v\n want %+v", it, req.String(), body, got, want)
		}
		found += len(want)
	}
	if found < iterations/10 {
		t.Fatalf("the generator made %d correlations in %d mocks: too few to test the gate", found, iterations)
	}
}

// TestEveryClassRegexMatchesOnlyValueChars holds the class regexes to
// isValueChar, which answerMayHold relies on: it parses uuidRe, hexRe and
// nonceRe and requires every character one of them can match, as a literal
// or in a class, case folds included, to be one isValueChar reports. So
// widening one of these regexes past those characters fails here, whatever
// character it adds. A line anchor, ^ or $ under (?m), fails too, since it
// lets a value hold a newline. The row parses each regex with Perl flags, as
// MustCompile does, so it takes ^ and $ as text anchors: a regex compiled
// with MustCompilePOSIX, whose ^ and $ are line anchors, is not caught. A
// regex that drops its ^ or $ is caught by the walks below, and a new class
// regex must be added to this list.
func TestEveryClassRegexMatchesOnlyValueChars(t *testing.T) {
	for _, re := range []*regexp.Regexp{uuidRe, hexRe, nonceRe} {
		tree, err := syntax.Parse(re.String(), syntax.Perl)
		if err != nil {
			t.Fatalf("parse %s: %v", re, err)
		}
		var walk func(n *syntax.Regexp)
		walk = func(n *syntax.Regexp) {
			switch n.Op {
			case syntax.OpLiteral:
				for _, r := range n.Rune {
					// Under (?i) a literal also matches each case fold of its rune.
					for f := r; ; {
						if !isValueChar(f) {
							t.Errorf("%s can match %U, which isValueChar does not report", re, f)
						}
						if n.Flags&syntax.FoldCase == 0 {
							break
						}
						if f = unicode.SimpleFold(f); f == r {
							break
						}
					}
				}
			case syntax.OpCharClass:
				// The parser has already added a class's case folds to its ranges.
				for i := 0; i+1 < len(n.Rune); i += 2 {
					for r := n.Rune[i]; r <= n.Rune[i+1]; r++ {
						if !isValueChar(r) {
							t.Errorf("%s can match %U, which isValueChar does not report", re, r)
							break
						}
					}
				}
			case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
				t.Errorf("%s can match any character", re)
			case syntax.OpBeginLine, syntax.OpEndLine:
				t.Errorf("%s has a line anchor, so it can match a value with a newline in it", re)
			case syntax.OpNoMatch, syntax.OpEmptyMatch,
				syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary,
				syntax.OpCapture, syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat,
				syntax.OpConcat, syntax.OpAlternate:
				// These match no character of their own; their parts are walked below.
			default:
				t.Errorf("%s has a node this row does not know: %v", re, n.Op)
			}
			for _, s := range n.Sub {
				walk(s)
			}
		}
		walk(tree)
	}
}

// TestEveryAcceptedValueIsMadeOfValueChars holds appRandomClass and
// isValueChar together, which answerMayHold relies on: every value a class
// accepts is, with the space around it trimmed, made of characters
// isValueChar reports, and each of those is written in JSON as itself or as
// one \u escape (no short escape such as the one for '/'). It walks the
// Basic Multilingual Plane, and the planes above it sparsely, putting each
// rune before, in the middle of and after a value of each class, and in place
// of its first or middle character. So a widening of appRandomClass's Go code
// to one more character at one of those places fails here, as does a class
// regex that loses its ^ or $ anchor.
func TestEveryAcceptedValueIsMadeOfValueChars(t *testing.T) {
	bases := acceptedBases(t)
	check := func(r rune) {
		c := string(r)
		if isValueChar(r) && (r >= 0x10000 || strings.ContainsRune("\"\\/\b\f\n\r\t", r)) {
			t.Errorf("isValueChar(%U): JSON writes it other than as itself or one \\u escape", r)
		}
		for _, b := range bases {
			mid := len(b) / 2
			for _, v := range []string{c + b, b[:mid] + c + b[mid:], b + c, c + b[1:], b[:mid] + c + b[mid+1:]} {
				if _, ok := appRandomClass(v); !ok {
					continue
				}
				for _, vr := range strings.TrimSpace(v) {
					if !isValueChar(vr) {
						t.Errorf("appRandomClass accepts %q, whose %U isValueChar does not report", v, vr)
					}
				}
			}
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0x10000 && r%0x101 != 0 {
			continue
		}
		if utf8.ValidRune(r) {
			check(r)
		}
	}
}

// acceptedBases returns a value of each app-random class, for the walks that
// change one and ask whether the class still accepts it.
func acceptedBases(t *testing.T) []string {
	t.Helper()
	bases := []string{
		"550e8400-e29b-41d4-a716-446655440000", // uuid
		"deadbeefcafe1234",                     // hex
		"aZ09kQ7xP2mN4rT8vB1c",                 // nonce
	}
	for _, b := range bases {
		if _, ok := appRandomClass(b); !ok {
			t.Fatalf("base %q is not accepted", b)
		}
	}
	return bases
}

// TestEveryValueWithTwoAddedCharsIsMadeOfValueChars is the walk above with
// two characters at once: it puts every pair of printable ASCII characters
// around a value of each class, and inside it at a third and two thirds of its
// length. Every value a class then accepts must still be made, once trimmed,
// of characters isValueChar reports.
func TestEveryValueWithTwoAddedCharsIsMadeOfValueChars(t *testing.T) {
	bases := acceptedBases(t)
	bad := 0
	for c1 := rune(0x20); c1 < 0x7f; c1++ {
		for c2 := rune(0x20); c2 < 0x7f; c2++ {
			a, z := string(c1), string(c2)
			for _, b := range bases {
				p, q := len(b)/3, 2*len(b)/3
				for _, v := range []string{a + b + z, b[:p] + a + b[p:q] + z + b[q:]} {
					if _, ok := appRandomClass(v); !ok {
						continue
					}
					for _, vr := range strings.TrimSpace(v) {
						if isValueChar(vr) {
							continue
						}
						if bad++; bad <= 3 {
							t.Errorf("appRandomClass accepts %q, whose %U isValueChar does not report", v, vr)
						}
					}
				}
			}
		}
	}
	if bad > 3 {
		t.Errorf("and %d more such characters", bad-3)
	}
}

// TestAValueOnlyInTheAnswerFindsNoCorrelation: an app-random value the answer
// carries but the request never wrote is not an echo, whether the request
// writes another such value or none.
func TestAValueOnlyInTheAnswerFindsNoCorrelation(t *testing.T) {
	const minted = "550e8400-e29b-41d4-a716-446655440000"
	for _, req := range []string{
		`{"amount":10}`,
		`{"key":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}`,
	} {
		m := mockWithBodies(req, `{"id":"`+minted+`","status":"created"}`)
		if got := CorrelationsFromMock(zap.NewNop(), m); len(got) != 0 {
			t.Fatalf("request %s: a value only the answer carries was correlated: %+v", req, got)
		}
	}
}

// TestALargeAnswerIsNotParsedAtIngest bounds what the detector allocates for
// a 2.7 MB answer it has no reason to parse: the request writes no app-random
// value (and the answer escapes a letter, so the request decides, not the
// escape scan), or writes one the answer's text does not hold, not even when
// it holds all of the value but its first or last character, or when its
// text ends inside an escape whose digits so far read as a value character.
// A parse and flatten of that answer allocates about a million times, and
// even a parse that fails at once copies the whole answer.
func TestALargeAnswerIsNotParsedAtIngest(t *testing.T) {
	const (
		size = 2_700_000
		key  = "9f1c2d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f"
	)
	keyed := `{"idempotencyKey":"` + key + `"}`
	for _, tc := range []struct{ name, req, resp string }{
		{"request writes no app-random value", `{"amount":10,"currency":"USD"}`, largeAnswer(size, `,"note":"caf\u0065 \u003c"`)},
		{"answer does not hold the written value", keyed, largeAnswer(size, `,"note":"a \u003c b"`)},
		{"answer holds the written value but its last character", keyed, largeAnswer(size, `,"note":"`+key[:len(key)-1]+`"`)},
		{"answer holds the written value but its first character", keyed, largeAnswer(size, `,"note":"`+key[1:]+`"`)},
		{"answer ends inside an escape that reads as a value character", keyed, largeAnswer(size, `,"note":"x`) + `\u41`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mockWithBodies(tc.req, tc.resp)
			allocs, bytes := allocatedPerRun(3, func() {
				if got := CorrelationsFromMock(zap.NewNop(), m); len(got) != 0 {
					t.Fatalf("want no correlation, got %+v", got)
				}
			})
			if allocs > 100 || bytes > 64<<10 {
				t.Fatalf("%d allocations of %d bytes for an answer that is never needed, want at most 100 and 64 KiB", allocs, bytes)
			}
		})
	}
}

// allocatedPerRun returns what f allocates per run, in allocations and in
// bytes, measured as testing.AllocsPerRun measures: one warm-up run, then
// runs runs with GOMAXPROCS at 1.
func allocatedPerRun(runs int, f func()) (allocs, bytes uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.Mallocs - before.Mallocs) / uint64(runs), (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// TestARequestIsNotParsedForAnEmptyAnswer bounds what the detector allocates
// for a 2.7 MB request that writes thousands of app-random values, when the
// answer has no body (a 204) or only space: no answer can echo, so the
// request is not parsed either.
func TestARequestIsNotParsedForAnEmptyAnswer(t *testing.T) {
	req := largeAnswer(2_700_000, "")
	for _, resp := range []string{"", " \r\n\t "} {
		m := mockWithBodies(req, resp)
		allocs := testing.AllocsPerRun(3, func() {
			if got := CorrelationsFromMock(zap.NewNop(), m); len(got) != 0 {
				t.Fatalf("want no correlation, got %+v", got)
			}
		})
		if allocs > 10 {
			t.Fatalf("answer %q: %.0f allocations, want at most 10", resp, allocs)
		}
	}
}

// TestAParsedAnswerKeepsOnlyWrittenValues bounds the parse that does happen:
// beyond parsing and flattening the answer, the detector keeps only the values
// the request wrote, not an index of every answer value.
func TestAParsedAnswerKeepsOnlyWrittenValues(t *testing.T) {
	const key = "9f1c2d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f"
	resp := largeAnswer(300_000, `,"echo":"`+key+`"`)
	m := mockWithBodies(`{"idempotencyKey":"`+key+`"}`, resp)
	parse := testing.AllocsPerRun(3, func() { _ = flattenJSONBody(resp) })
	allocs := testing.AllocsPerRun(3, func() {
		if got := CorrelationsFromMock(zap.NewNop(), m); len(got) != 1 {
			t.Fatalf("want the echo's correlation, got %+v", got)
		}
	})
	if allocs > parse+200 {
		t.Fatalf("%.0f allocations, want at most the answer's parse (%.0f) and 200", allocs, parse)
	}
}

// TestAnEchoInTheTailOfALargeAnswerIsFound: the raw-text checks read the whole
// answer, so a 2.7 MB answer whose only echo comes after its first two
// megabytes is parsed and the echo found, whether the answer writes the value
// as it is or writes one of its characters as a \u escape.
func TestAnEchoInTheTailOfALargeAnswerIsFound(t *testing.T) {
	const key = "9f1c2d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f"
	for _, tc := range []struct{ name, echo string }{
		{"written as it is", key},
		{"one character escaped", key[:1] + `\u` + "0066" + key[2:]}, // the f
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := largeAnswer(2_700_000, `,"echo":"`+tc.echo+`"`)
			if i, j := strings.Index(resp, tc.echo), strings.Index(resp, key[2:]); i < 2<<20 || j < 2<<20 {
				t.Fatalf("fixture: the echo is first at byte %d and the value's last characters at %d, want both past 2 MiB", i, j)
			}
			m := mockWithBodies(`{"idempotencyKey":"`+key+`"}`, resp)
			want := correlationsByFullParse(m)
			if len(want) != 1 {
				t.Fatalf("fixture: a full parse finds %d correlations, want the echo's: %+v", len(want), want)
			}
			if got := CorrelationsFromMock(zap.NewNop(), m); !reflect.DeepEqual(got, want) {
				t.Fatalf("correlations differ from a full parse:\n got  %+v\n want %+v", got, want)
			}
		})
	}
}
