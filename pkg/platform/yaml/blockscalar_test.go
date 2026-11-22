package yaml

import (
	"bytes"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// Places a string sits in a recorded mock: a mapping value at each depth, a
// sequence item, a sequence in a sequence, a map value, an interface, a
// Node scalar, and a map key.
type bsInner struct {
	Query string            `yaml:"query"`
	Meta  map[string]string `yaml:"meta,omitempty"`
	List  []string          `yaml:"list,omitempty"`
	Any   any               `yaml:"any"`
	Bytes []byte            `yaml:"bytes,omitempty,flow"`
}

type bsPacket struct {
	Header  map[string]string `yaml:"header"`
	Message any               `yaml:"message"`
}

type bsDoc struct {
	Version string            `yaml:"version"`
	Top     string            `yaml:"top"`
	Spec    map[string]any    `yaml:"spec"`
	Reqs    []bsPacket        `yaml:"requests"`
	Node    *yamlLib.Node     `yaml:"node"`
	Keys    map[string]string `yaml:"keys"`
	At      time.Time         `yaml:"at"`
}

type bsBack struct {
	Version string         `yaml:"version"`
	Top     string         `yaml:"top"`
	Spec    map[string]any `yaml:"spec"`
	Reqs    []struct {
		Header  map[string]string `yaml:"header"`
		Message bsInner           `yaml:"message"`
	} `yaml:"requests"`
	Node string            `yaml:"node"`
	Keys map[string]string `yaml:"keys"`
	At   time.Time         `yaml:"at"`
}

func bsDocOf(s string) bsDoc {
	return bsDoc{
		Version: "v", Top: s,
		Spec: map[string]any{"body": s, "nested": []any{map[string]any{"data": s, "type": "utf-8"}, s}},
		Reqs: []bsPacket{{Header: map[string]string{"h": s}, Message: &bsInner{Query: s, Meta: map[string]string{"k": s},
			List: []string{s, "x"}, Any: []any{[]any{s}}, Bytes: []byte{1, 2}}}},
		Node: &yamlLib.Node{Kind: yamlLib.ScalarNode, Tag: "!!str", Value: s},
		Keys: map[string]string{s: "v"},
		At:   time.Date(2026, 9, 30, 8, 42, 54, 313873046, time.UTC),
	}
}

// bsWrite writes v as InsertMock does: in one pass, or through EncodeQuoted
// when it holds a string a block scalar cannot carry.
func bsWrite(t *testing.T, v any) []byte {
	t.Helper()
	if NeedsQuoting(v) {
		n, err := EncodeQuoted(v)
		if err != nil {
			t.Fatalf("EncodeQuoted: %v", err)
		}
		v = n
	}
	var b bytes.Buffer
	enc := yamlLib.NewEncoder(&b)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = enc.Close()
	return b.Bytes()
}

func bsReadsBack(t *testing.T, s string, text []byte) string {
	t.Helper()
	var back bsBack
	if err := yamlLib.Unmarshal(text, &back); err != nil {
		return "does not load: " + err.Error()
	}
	got := map[string]string{"top": back.Top, "node": back.Node, "key": ""}
	for k := range back.Keys {
		got["key"] = k
	}
	if b, ok := back.Spec["body"].(string); ok {
		got["body"] = b
	}
	if n, ok := back.Spec["nested"].([]any); ok && len(n) == 2 {
		m, _ := n[0].(map[string]any)
		got["nested.data"], _ = m["data"].(string)
		got["nested.item"], _ = n[1].(string)
	}
	if len(back.Reqs) == 1 {
		r := back.Reqs[0]
		got["header"], got["query"], got["meta"] = r.Header["h"], r.Message.Query, r.Message.Meta["k"]
		if len(r.Message.List) > 0 {
			got["list"] = r.Message.List[0]
		}
		if a, ok := r.Message.Any.([]any); ok && len(a) == 1 {
			if b, ok := a[0].([]any); ok && len(b) == 1 {
				got["any"], _ = b[0].(string)
			}
		}
		if !bytes.Equal(r.Message.Bytes, []byte{1, 2}) {
			return "the byte list reads back as " + string(r.Message.Bytes)
		}
	}
	for where, v := range got {
		if v != s {
			return where + " reads back as " + strings.ReplaceAll(v, "\n", `\n`)
		}
	}
	if !back.At.Equal(time.Date(2026, 9, 30, 8, 42, 54, 313873046, time.UTC)) {
		return "the timestamp reads back as " + back.At.String()
	}
	return ""
}

// Every string, wherever it sits in a value, reads back exactly: those a
// block scalar cannot carry double-quoted (EncodeQuoted), the rest written in
// one pass. yaml.v3's own literal block does not load, or loads another
// string, for 1 in 80 of these; models.YAMLBlockScalarUnsafe must name every
// one of them, or the one-pass write is not checked.
func TestEveryStringReadsBackWhereverItSits(t *testing.T) {
	t.Parallel()
	alpha := []string{"a", " ", "\t", "\n", "#", ":", "-", `"`, "'", "é", "\u2028", "\r", "|", "\u0085", "0", "\x00", "\x7f", "{", "&"}
	r := rand.New(rand.NewSource(7))
	seen := map[string]bool{}
	quoted := 0
	for len(seen) < 30000 {
		var sb strings.Builder
		for j, n := 0, 1+r.Intn(9); j < n; j++ {
			sb.WriteString(alpha[r.Intn(len(alpha))])
		}
		s := sb.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		d := bsDocOf(s)
		if NeedsQuoting(d) {
			quoted++
		}
		if why := bsReadsBack(t, s, bsWrite(t, d)); why != "" {
			t.Fatalf("%q: %s", s, why)
		}
	}
	if quoted == 0 {
		t.Fatal("no string was written quoted: the test does not reach EncodeQuoted")
	}
	t.Logf("%d strings, %d of them written through EncodeQuoted", len(seen), quoted)
}

// A value with no string a block scalar cannot carry is not sent through
// EncodeQuoted; and EncodeQuoted, given one, writes what the one-pass write
// does: the same bytes, but for a ",flow" field, which it writes in block
// style, and a Node scalar left to choose its style, which it writes as a
// string value (the same values).
func TestEncodeQuotedWritesTheOnePassDocument(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"plain", "123", "yes", "1:20", "", "a: b", "- x", "x\ny", "x\n\ty\n", "tab\there", " lead", "trail ", "é✓", "null", "true", "0x1F", "~"} {
		d := bsDocOf(s)
		d.Reqs[0].Message.(*bsInner).Bytes = nil // no ",flow" field
		// A hand-built Node scalar left to choose its style is written as
		// a string value is ("yes" double-quoted), where the one-pass write
		// leaves "yes" plain: it reads back the same.
		d.Node = nil
		if NeedsQuoting(d) {
			t.Fatalf("%q: a value a block scalar can carry is sent through EncodeQuoted", s)
		}
		var direct bytes.Buffer
		if err := yamlLib.NewEncoder(&direct).Encode(d); err != nil {
			t.Fatal(err)
		}
		n, err := EncodeQuoted(d)
		if err != nil {
			t.Fatal(err)
		}
		var viaNode bytes.Buffer
		if err := yamlLib.NewEncoder(&viaNode).Encode(n); err != nil {
			t.Fatal(err)
		}
		if direct.String() != viaNode.String() {
			t.Fatalf("%q: EncodeQuoted wrote\n%s\nthe one-pass write:\n%s", s, viaNode.String(), direct.String())
		}
	}
}

// A value whose MarshalYAML returns the string, or a Node left to choose its
// style, is looked into; a Node already quoted is not taken as a block.
func TestNeedsQuotingLooksIntoMarshalersAndNodes(t *testing.T) {
	t.Parallel()
	bad := "\tx\ny"
	cases := []struct {
		v    any
		want bool
	}{
		{bsMarshaler{s: bad}, true},
		{&bsMarshaler{s: bad}, true},
		{bsMarshaler{s: "fine\ntoo"}, false},
		{bsText{s: bad}, true},
		{&yamlLib.Node{Kind: yamlLib.ScalarNode, Value: bad}, true},
		{&yamlLib.Node{Kind: yamlLib.ScalarNode, Value: bad, Style: yamlLib.DoubleQuotedStyle}, false},
		{&yamlLib.Node{Kind: yamlLib.SequenceNode, Content: []*yamlLib.Node{{Kind: yamlLib.ScalarNode, Value: bad, Style: yamlLib.LiteralStyle}}}, true},
		{map[string]int{bad: 1}, true},
		{struct {
			Skip string `yaml:"-"`
		}{bad}, false},
		{struct{ hidden string }{bad}, false},
		{[]byte(bad), false},
		{(*bsInner)(nil), false},
		{bsFails{}, false},
	}
	for i, c := range cases {
		if got := NeedsQuoting(c.v); got != c.want {
			t.Errorf("case %d (%T): NeedsQuoting = %v, want %v", i, c.v, got, c.want)
		}
	}
	// A type that holds itself.
	type self struct {
		S    string `yaml:"s"`
		Next *self  `yaml:"next"`
	}
	if !NeedsQuoting(&self{S: "ok", Next: &self{S: bad}}) {
		t.Error("a string two levels down a self-referring type is missed")
	}
}

type bsMarshaler struct{ s string }

func (m bsMarshaler) MarshalYAML() (any, error) { return map[string]string{"v": m.s}, nil }

type bsText struct{ s string }

func (m bsText) MarshalText() ([]byte, error) { return []byte(m.s), nil }

type bsFails struct{}

func (bsFails) MarshalYAML() (any, error) { panic("cannot") }

// EncodeNode is Node.Encode for what it could encode, and encodes what
// Node.Encode could not: it parsed back a block scalar that does not load.
func TestEncodeNodeEncodesWhatNodeEncodeCouldNot(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"\tleading tab\nx", "\n\thello", " a\nb"} {
		v := map[string]any{"list": []any{map[string]string{"q": s}}}
		var plain yamlLib.Node
		plainErr := plain.Encode(v)
		var n yamlLib.Node
		if err := EncodeNode(&n, v); err != nil {
			t.Fatalf("%q: EncodeNode: %v", s, err)
		}
		out, err := yamlLib.Marshal(&n)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string][]map[string]string
		if err := yamlLib.Unmarshal(out, &back); err != nil || back["list"][0]["q"] != s {
			t.Fatalf("%q: read back %q (%v); Node.Encode said %v", s, back["list"][0]["q"], err, plainErr)
		}
	}
	// What Node.Encode can encode, EncodeNode encodes the same.
	v := map[string]any{"a": []any{"x\n\ty", 1, true, "123"}}
	var a, b yamlLib.Node
	if err := a.Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := EncodeNode(&b, v); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("EncodeNode differs from Node.Encode on a value it can encode")
	}
}

func TestYAMLBlockScalarUnsafe(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"":                     false,
		"single line":          false,
		"\tsingle":             false,
		"SELECT\n\tid\nFROM t": false,
		"x\n\n\ty":             false,
		"\tleading tab\nx":     true,
		"\n\thello":            true,
		" leading space\nx":    true,
		"\n":                   true,
		"\u2028a\nb":           true,
		"\x80\n bad utf-8":     false,
	} {
		if got := models.YAMLBlockScalarUnsafe(s); got != want {
			t.Errorf("YAMLBlockScalarUnsafe(%q) = %v, want %v", s, got, want)
		}
	}
}
