package learn

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// b64SkipRecordings are the recordings this repository keeps: mocks of real
// wire traffic, test cases, mappings and reports, as they sit on disk.
var b64SkipRecordings = []string{
	"../../models/testdata",
	"../../platform/yaml/mapdb/testdata",
	"../../platform/yaml/mockdb/testdata",
	"../../platform/yaml/reportdb/testdata",
	"../../platform/yaml/testdb/testdata",
}

// b64SkipForms are s as the learn step meets it encoded: each of the
// encodings it decodes, a path's segments, a JWT's parts, a signed message
// glued by groups of both kinds and of each kind alone, base64 inside
// base64. The glued ones and the path are what the skips must not pass by:
// a run no text whole, and text in its parts.
func b64SkipForms(s string) []string {
	b := []byte(s)
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:16])
	std := base64.StdEncoding.EncodeToString(b)
	rawStd := base64.RawStdEncoding.EncodeToString(b)
	rawURL := base64.RawURLEncoding.EncodeToString(b)
	return []string{
		std,
		rawURL,
		hex.EncodeToString(b),
		"0x" + hex.EncodeToString(b),
		base32.StdEncoding.EncodeToString(b),
		"/v1/" + rawStd + "/" + digest + "/" + rawStd,
		rawURL + "." + rawURL + "." + digest,
		"v1_" + rawURL + "--" + digest,
		rawStd + "--" + digest,
		rawStd + "_" + digest,
		rawStd + "-" + digest,
		base64.StdEncoding.EncodeToString([]byte(std)),
	}
}

// b64SkipLiterals are the string literals of this package's tests: the
// payloads, headers, tokens and clock texts its rows are about.
func b64SkipLiterals(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*_test.go")
	if err != nil || len(names) == 0 {
		t.Fatalf("the package's test files: %v, %d found", err, len(names))
	}
	var out []string
	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
				if s, err := strconv.Unquote(l.Value); err == nil {
					out = append(out, s)
				}
			}
			return true
		})
	}
	return out
}

// b64SkipRecorded are the recordings' texts: each file whole and each of
// its lines, as a payload or a header is one value of a mock.
func b64SkipRecorded(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range b64SkipRecordings {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(path) {
			case ".yaml", ".json", ".golden":
			default:
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, string(b))
			out = append(out, strings.Split(string(b), "\n")...)
			return nil
		})
		if err != nil {
			t.Fatalf("recordings under %s: %v", root, err)
		}
	}
	return out
}

// b64SkipBoundPayload is the payload of the HTTP learn bound row, the one
// whose CPU the skips were made for: word-like runs, none of them encoded.
func b64SkipBoundPayload() string {
	var b strings.Builder
	b.WriteString(`{"blob":"`)
	for j := 0; b.Len() < 1<<16; j++ {
		b.WriteString("w7x" + strconv.Itoa(j) + " ")
	}
	b.WriteString(`"}`)
	return b.String()
}

// b64SkipClocks are the clocks s is read with: none, as a payload is, and
// empty, as a value with no clock read is; the clock read of s as a drifted
// value, where it has one; and its longest word alone, so that a run of
// that word is read as a clock of one word.
func b64SkipClocks(s string) []map[string]bool {
	clocks := []map[string]bool{nil, {}}
	if c := clockAt(s, s, nil, true, time.Time{}, time.UTC).rec; len(c) > 0 {
		clocks = append(clocks, c)
	}
	var longest string
	for _, w := range wordsBy(s, isWord) {
		if len(w) > len(longest) {
			longest = w
		}
	}
	if longest != "" {
		clocks = append(clocks, map[string]bool{longest: true})
	}
	return clocks
}

// b64Text passes by three reads whose answer it knows (b64Read): over the
// recordings this repository keeps, over the literals of this package's
// tests and each of them encoded as the learn step meets it, over the
// shapes and the random texts the run scan is held to, and over the bound
// row's payload, with no clock and with one, it gives what making those
// reads gives, byte for byte.
func TestB64TextPassesByOnlyReadsThatFindNothing(t *testing.T) {
	literals := b64SkipLiterals(t)
	recorded := b64SkipRecorded(t)
	texts := append(append(b64RunTexts(), literals...), recorded...)
	texts = append(texts, b64SkipBoundPayload())
	for _, s := range literals {
		texts = append(texts, b64SkipForms(s)...)
	}
	seen := make(map[string]bool, len(texts))
	found, clocked := 0, 0
	for _, s := range texts {
		if seen[s] {
			continue
		}
		seen[s] = true
		for i, clock := range b64SkipClocks(s) {
			got, want := b64Read(s, clock, false), b64Read(s, clock, true)
			if got != want {
				t.Fatalf("b64Text with clock %v of %.200q\n got %.400q\nwant %.400q (every read made)", clock, s, got, want)
			}
			if i == 0 && got != "" {
				found++
			}
			if i >= 2 {
				clocked++
			}
		}
	}
	t.Logf("%d texts (%d literals, %d recorded), %d decode to text, %d reads with a clock", len(seen), len(literals), len(recorded), found, clocked)
	// A corpus that decodes to nothing would hold the skips to nothing.
	if len(recorded) < 1000 || found < 10000 || clocked < 10000 {
		t.Fatalf("too little read to tell: %d recorded texts, %d decoded, %d reads with a clock", len(recorded), found, clocked)
	}
}
