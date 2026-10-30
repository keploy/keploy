package logleak

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Find finds content in each encoding it lists, and nothing in a log that
// gives only its length.
func TestFindFindsEachEncodingAndNothingElse(t *testing.T) {
	content := []byte("token=sk_live_9fQ2xLp7&email=ada@example.com")
	for name, e := range Encodings(content) {
		// Each encoding cut to an inner stretch: Find must not need the whole.
		inner := e[len(e)/4 : len(e)/4+windowOf(len(e), len(content))+3]
		log := `{"msg":"x","v":"` + inner + `"}`
		found := Find(t, log, content)
		if len(found) == 0 {
			t.Errorf("%s: Find missed %q in %s", name, inner, log)
		}
	}
	if found := Find(t, `{"msg":"read the request","bytes":47}`, content); len(found) != 0 {
		t.Errorf("Find found content in a log that gives its length only: %v", found)
	}
}

// The encodings Find looks in are a fixed list, and each that another cannot
// stand in for is found from a log that holds it alone, computed here, not by
// Encodings: a dropped encoding cannot drop its own case. A path-escaped
// value with a space in every other byte holds no window of any other
// encoding, and base64 from byte 1 or 2 shares no character run with base64
// from byte 0.
func TestFindLooksInAFixedListOfEncodings(t *testing.T) {
	var names []string
	for name := range Encodings([]byte("token=sk_live_9fQ2xLp7")) {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"%q of []byte", "%v", "HEX", "dump text", "hex", "json", "path-escape", "query-escape", "quoted", "raw", "spaced HEX", "spaced hex"}
	for _, b := range []string{"rawstd", "rawurl", "std", "url"} {
		for shift := 0; shift < 3; shift++ {
			want = append(want, fmt.Sprintf("base64 %s from byte %d", b, shift))
		}
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("Encodings names %q, want %q", names, want)
	}

	spaced := []byte("a b/c d/e f/g h/i j/k")
	content := []byte("token=sk_live_9fQ2xLp7&email=ada@example.com")
	for _, c := range []struct {
		name    string
		content []byte
		log     string
	}{
		{"path-escape", spaced, url.PathEscape(string(spaced))},
		{"base64 std from byte 1", content, base64.StdEncoding.EncodeToString(content[1:])},
		{"base64 std from byte 2", content, base64.StdEncoding.EncodeToString(content[2:])},
		{"base64 rawurl from byte 1", content, base64.RawURLEncoding.EncodeToString(content[1:])},
		{"base64 rawurl from byte 2", content, base64.RawURLEncoding.EncodeToString(content[2:])},
	} {
		found := Find(t, `{"msg":"x","v":"`+c.log+`"}`, c.content)
		if !slices.ContainsFunc(found, func(f string) bool { return strings.HasPrefix(f, c.name+": ") }) {
			t.Errorf("Find did not find %q by its %s: found %v", c.log, c.name, found)
		}
	}
}

// Find reports content, not the frame an encoding puts around it: a log
// holding a hex.Dump's offset ("00000000") or a run of padding spaces holds no
// content. A hex.Dump of the content is found by its columns.
func TestFindReportsNoFrameAndNoEmptyContent(t *testing.T) {
	content := []byte("token=sk_live_9fQ2xLp7&email=ada@example.com")
	for _, log := range []string{
		`{"msg":"x","traceID":"00000000000000000000000000000000"}`,
		"x\t        \t        indented        text",
	} {
		if found := Find(t, log, content); len(found) != 0 {
			t.Errorf("Find found content in %q: %v", log, found)
		}
	}
	if found := Find(t, `{"msg":"x","dump":"`+hex.Dump(content)+`"}`, content); len(found) == 0 {
		t.Error("Find missed a hex.Dump of the content")
	}
}

// Logger renders each entry twice, as JSON and as console text.
func TestLoggerRendersJSONAndConsole(t *testing.T) {
	logger, out := Logger(nil)
	logger.Debug("planted line")
	if got := strings.Count(out.String(), "planted line"); got != 2 {
		t.Fatalf("the line is rendered %d times, want 2 (JSON and console): %s", got, out.String())
	}
}

// fatalOnly is a test's t whose Fatalf is recorded, not acted on, so a row
// can see Find refuse content.
type fatalOnly struct {
	testing.TB
	fatal string
}

func (f *fatalOnly) Fatalf(format string, args ...any) { f.fatal = fmt.Sprintf(format, args...) }

// Find refuses, by failing the row's setup, content a log can hold by chance:
// under Window bytes (empty too), or with a byte that is not printable ASCII,
// whose hex.Dump text is '.', as a log's dotted text is. It looked for each,
// and a log of "........" held binary content's text column.
func TestFindRefusesContentALogHoldsByChance(t *testing.T) {
	const dotted = `{"msg":"waiting........done","ok":"abc"}`
	for name, content := range map[string][]byte{
		"empty":              nil,
		"under Window bytes": []byte("abc"),
		"one byte short":     []byte("sk_live"),
		"a NUL":              []byte("sk_live_\x009fQ2xLp7"),
		"binary":             {0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09},
		"a newline":          []byte("line one\nline two"),
		"a byte over 126":    []byte("caf\xc3\xa9 au lait"),
	} {
		f := &fatalOnly{TB: t}
		if found := Find(f, dotted, content); f.fatal == "" || len(found) != 0 {
			t.Errorf("%s: Find did not refuse %q (found %v)", name, content, found)
		}
	}
	f := &fatalOnly{TB: t}
	if Find(f, `{"msg":"x"}`, []byte("sk_live_")); f.fatal != "" {
		t.Errorf("Find refused printable content of Window bytes: %s", f.fatal)
	}
}

// A run of an encoding that writes a byte as several characters holds Window
// bytes of content, not Window characters: a log's own decimal or hex list
// that shares two or three bytes with the content is no leak, while the
// content's own spaced hex, in either case, is one.
func TestFindLooksForWindowBytesOfContentInEachEncoding(t *testing.T) {
	content := []byte("token=sk_live_9fQ2xLp7")
	// "en=s" is 101 110 61 115: four bytes of the content, under Window.
	for _, log := range []string{`{"msg":"x","v":"[101 110 61 115]"}`, `{"msg":"x","v":"65 6e 3d 73"}`, `{"msg":"x","v":"65 6E 3D 73"}`, `{"msg":"x","v":"656e3d73"}`} {
		if found := Find(t, log, content); len(found) != 0 {
			t.Errorf("Find found content in %s, which holds 4 of its bytes: %v", log, found)
		}
	}
	for _, f := range []string{"% x", "% X", "%v"} {
		log := `{"msg":"x","v":"` + fmt.Sprintf(f, content[2:2+Window]) + `"}`
		if found := Find(t, log, content); len(found) == 0 {
			t.Errorf("Find missed %d bytes of content written by %s in %s", Window, f, log)
		}
	}
}
