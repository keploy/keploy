package templates

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONLeaves_DocumentOrderPathsAndSpans(t *testing.T) {
	doc := `{"id":"usr_1", "n": 1234567, "items":[{"id":"a\"b"},true,null], "f": -1.5e3, "bare": {{int .n}}}`
	leaves, ok := JSONLeaves(doc)
	require.True(t, ok)
	var got []string
	for _, l := range leaves {
		got = append(got, l.DisplayPath()+"="+l.Raw(doc))
	}
	assert.Equal(t, []string{
		`id="usr_1"`, `n=1234567`, `items[0].id="a\"b"`, `items[1]=true`, `items[2]=null`, `f=-1.5e3`, `bare={{int .n}}`,
	}, got)
	assert.Equal(t, LeafBare, leaves[6].Kind)
	assert.Equal(t, `a"b`, leaves[2].Value(doc), "a string leaf's value is decoded")
	assert.True(t, leaves[2].InArray)
	assert.False(t, leaves[0].InArray)
}

func TestJSONLeaves_NotOneJSONDocument(t *testing.T) {
	// A document that is one placeholder alone is text: {{string .x}} stands
	// for the value as written, not for a JSON string.
	for _, doc := range []string{"", "plain text", `{"a":1} {"b":2}`, `{"a":}`, `{"a":"x`, "a=1&b=2", `{{string .x}} trailing`, `{{string .x}}`, ` {{int .n}} `} {
		_, ok := JSONLeaves(doc)
		assert.False(t, ok, "%q", doc)
	}
	for _, doc := range []string{`"just a string"`, `42`, `[]`, `{}`, ` {"a":[1,2]} `} {
		_, ok := JSONLeaves(doc)
		assert.True(t, ok, "%q", doc)
	}
}

func TestJSONEscape_NoHTMLEscaping(t *testing.T) {
	assert.Equal(t, `say \"hi\" <a&b>`, jsonEscape(`say "hi" <a&b>`))
	assert.Equal(t, `C:\\tmp\\new`, jsonEscape(`C:\tmp\new`))
	assert.Equal(t, `line1\nline2`, jsonEscape("line1\nline2"))
}

func TestJSONValueEqual(t *testing.T) {
	assert.True(t, jsonValueEqual(`"a"`, `"\u0061"`))
	assert.True(t, jsonValueEqual(`7654321`, `7654321`))
	assert.False(t, jsonValueEqual(`"42"`, `42`), "a string is not the number")
	assert.False(t, jsonValueEqual(`1`, `2`))
}

// A number is compared by its exact value: no digit of a number above 2^64 or
// of a long decimal is lost, and the same number written another way is
// equal.
func TestJSONValueEqual_NumbersAreExact(t *testing.T) {
	assert.False(t, jsonValueEqual(`123456789012345678901`, `123456789012345678902`))
	assert.False(t, jsonValueEqual(`0.1000000000000000000001`, `0.1000000000000000000002`))
	assert.True(t, jsonValueEqual(`1.50`, `1.5`))
	assert.True(t, jsonValueEqual(`1e2`, `100`))
	assert.True(t, jsonValueEqual(`-0`, `0`))
}

// Of a key an object repeats, the index holds the last, as every JSON reader
// keeps it.
func TestLeafIndex_ARepeatedKeyIsItsLastValue(t *testing.T) {
	doc := `{"id":"FIRST","id":"LAST"}`
	leaves, ok := JSONLeaves(doc)
	require.True(t, ok)
	idx := leafIndex(leaves)
	assert.Equal(t, "LAST", idx[leaves[0].PathKey()].Value(doc))
}

// A MEMBER A LATER MEMBER OF THE SAME NAME REPLACES is no value of the
// document: every leaf and key inside it, at any depth, is Shadowed, and a
// leaf counts only if it lies inside the last occurrence of every key on its
// path. The same name in another object, or in another element of an array,
// replaces nothing.
func TestJSONPlaces_AReplacedMemberIsShadowed(t *testing.T) {
	shadowed := func(doc string) (leaves, keys []string) {
		ls, ks, ok := JSONPlaces(doc)
		require.True(t, ok, doc)
		for _, l := range ls {
			if l.Shadowed {
				leaves = append(leaves, l.Raw(doc))
			}
		}
		for _, k := range ks {
			if k.Shadowed {
				keys = append(keys, k.DisplayPath())
			}
		}
		return leaves, keys
	}
	for _, c := range []struct {
		doc          string
		leaves, keys []string
	}{
		{`{"id":"a","id":{"x":"b"}}`, []string{`"a"`}, []string{"id"}},
		{`{"a":{"id":"X","k":1},"a":{}}`, []string{`"X"`, `1`}, []string{"a", "a.id", "a.k"}},
		{`{"id":1,"id":2,"id":3}`, []string{`1`, `2`}, []string{"id", "id"}},
		{`{"x":0,"id":1,"id":2,"y":3}`, []string{`1`}, []string{"id"}},
		{`[{"id":1,"id":2},{"id":3}]`, []string{`1`}, []string{"[0].id"}},
		{`{"a":{"id":1},"b":{"id":2}}`, nil, nil},
		{`{"a":[{"id":1}],"a":[{"id":2}]}`, []string{`1`}, []string{"a", "a[0].id"}},
	} {
		leaves, keys := shadowed(c.doc)
		assert.Equal(t, c.leaves, leaves, c.doc)
		assert.Equal(t, c.keys, keys, c.doc)
	}
	ls, _ := JSONLeaves(`{"id":"a","id":{"x":"b"}}`)
	_, atID := leafIndex(ls)["\x00kid"]
	assert.False(t, atID, "no scalar is the document's value at id")
}

// THE SCANNER READS A DOCUMENT AS JSON EXACTLY WHEN encoding/json DOES. A
// string with a raw control byte (a TAB) or a bad escape (\x) was read as
// JSON while the comparison, which asks json.Valid, read the same answer as
// text and, with compareAll off, never compared it: a use inside an array
// was then satisfied by any element. Without a placeholder, every document
// below, 300 random one-byte edits of each (a byte replaced, inserted or
// deleted, from a fixed seed), and documents nested to encoding/json's
// limit and one past it, scan exactly when json.Valid accepts them.
func TestScanJSON_AgreesWithJSONValid(t *testing.T) {
	seeds := []string{
		`{"orders":[{"id":"ord_lit","status":"pa\tid"},{"id":"ord_REC1"}]}`,
		"{\"s\":\"pa\tid\"}", "{\"s\":\"pa\x01id\"}", "{\"s\":\"a\x1fb\"}", "{\"s\":\"a\x7fb\"}",
		`{"s":"pa\xid"}`, `{"s":"éካ"}`, `{"s":"\u00e"}`, `{"s":"\u00eg"}`, `{"s":"\"\\\/\b\f\n\r\t"}`,
		`{"s":"\'"}`, `{"s":"\0"}`, `{"s":"\`, `"\u"`, `"\u12"`, "\"\xff\xfe\"", `{"s":"é"}`,
		`[1,-0,0.5,1e5,1E+5,1e-5,-1.25e3]`, `[01]`, `[1.]`, `[.5]`, `[+1]`, `[-]`, `[1e]`, `[1e+]`, `[0x1]`,
		`[true,false,null]`, `[truex]`, `[nul]`, `[True]`, `{"a":1,}`, `[1,]`, `{,}`, `{"a" 1}`, `{"a":1 "b":2}`,
		` {"a":[1,2]} `, "\t[1]\r\n", "\v[1]", "\xef\xbb\xbf[1]", `{"a":1}x`, `{} {}`, ``, ` `, `"x"`, `42`,
		`{"k\x01":1}`, "{\"k\t\":1}", `{"A":1}`,
	}
	agree := func(doc string) {
		t.Helper()
		_, scanned := scanJSON(doc)
		if valid := json.Valid([]byte(doc)); scanned != valid {
			t.Errorf("%q: scanned=%v, json.Valid=%v", doc, scanned, valid)
		}
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte("{}[]\",:\\/ \t\n\r\x00\x01\x1f\x7f0123456789.-+eEtrufalsnbxuAF\xff")
	for _, seed := range seeds {
		agree(seed)
		for n := 0; n < 300; n++ {
			b := []byte(seed)
			switch op := rng.Intn(3); {
			case op == 0 && len(b) > 0:
				b[rng.Intn(len(b))] = alphabet[rng.Intn(len(alphabet))]
			case op == 1:
				i := rng.Intn(len(b) + 1)
				b = append(b[:i], append([]byte{alphabet[rng.Intn(len(alphabet))]}, b[i:]...)...)
			case len(b) > 0:
				i := rng.Intn(len(b))
				b = append(b[:i], b[i+1:]...)
			}
			if !strings.Contains(string(b), "{{") {
				agree(string(b))
			}
		}
	}
	// encoding/json nests to 10000 and no deeper.
	agree(strings.Repeat("[", 10000) + strings.Repeat("]", 10000))
	agree(strings.Repeat("[", 10001) + strings.Repeat("]", 10001))
	agree(strings.Repeat(`{"a":`, 10000) + "1" + strings.Repeat("}", 10000))
}

// An answer is read as JSON only when json.Valid agrees: a placeholder an
// app answers is its text, never a value written outside a string, while an
// expected body may hold one.
func TestParseAnswer_ReadsAPlaceholderAsText(t *testing.T) {
	_, expected := parseJSON(`{"n":{{int .n}}}`)
	assert.True(t, expected, "an expected body reads a bare placeholder as a value")
	_, answer := parseAnswer(`{"n":{{int .n}}}`)
	assert.False(t, answer, "an answer holding a bare placeholder is not JSON")
	_, answer = parseAnswer(`{"n":"{{int .n}}"}`)
	assert.True(t, answer, "a placeholder inside a string is the app's text in a JSON string")
}
