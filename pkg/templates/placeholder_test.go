package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func TestFind_TheGrammar(t *testing.T) {
	cases := []struct {
		text    string
		want    []Placeholder
		comment string
	}{
		{text: "{{string .id}}", want: []Placeholder{{Type: "string", Key: "id"}}},
		{text: "{{ int  .orderId }}", want: []Placeholder{{Type: "int", Key: "orderId"}}},
		{text: "{{.id}}", want: []Placeholder{{Key: "id"}}},
		{text: "{{capture string .id}}", want: []Placeholder{{Capture: true, Type: "string", Key: "id"}}},
		{text: "{{capture float .price}}", want: []Placeholder{{Capture: true, Type: "float", Key: "price"}}},
		{text: "{{capture .id}}", want: []Placeholder{{Capture: true, Key: "id"}}, comment: "found, so Check can refuse it by name"},
		{text: "{{string .secret.apiKey }}", want: []Placeholder{{Type: "string", Key: "secret.apiKey"}}},
		{text: "Bearer {{string .token}}; v={{int .v}}", want: []Placeholder{{Type: "string", Key: "token"}, {Type: "int", Key: "v"}}},
		{text: `{{\pi}} and {{u^2}} and {{ x }}`, want: nil, comment: "LaTeX and dot-less text are not placeholders"},
		{text: "{{ .Subscriber.Name }}", want: nil, comment: "a dotted key is not in the grammar"},
		{text: "{{string .user.id}}", want: nil, comment: "a dotted key is not in the grammar"},
		{text: "{{string .secret.a.b}}", want: nil, comment: "a secret is one name"},
		{text: "/users/%7B%7Bstring%20.id%7D%7D/x", want: []Placeholder{{Type: "string", Key: "id"}}, comment: "percent-encoded in a URL"},
		{text: "/q?x=%7b%7bint%20.n%7d%7d", want: []Placeholder{{Type: "int", Key: "n"}}},
		{text: "%7B%7Bnot a placeholder%7D%7D", want: nil},
		{text: "{{capture string .número}}", want: []Placeholder{{Capture: true, Type: "string", Key: "número"}}, comment: "a key is letters of any script"},
		{text: "{{string .secret.Contraseña }}", want: []Placeholder{{Type: "string", Key: "secret.Contraseña"}}},
		{text: "{{int .订单号2}}", want: []Placeholder{{Type: "int", Key: "订单号2"}}},
		{text: "{{string .2fa}}", want: nil, comment: "a key starts with a letter or '_'"},
	}
	for _, c := range cases {
		got := Find(c.text)
		require.Len(t, got, len(c.want), "%q %s", c.text, c.comment)
		for i := range got {
			assert.Equal(t, c.want[i].Capture, got[i].Capture, c.text)
			assert.Equal(t, c.want[i].Type, got[i].Type, c.text)
			assert.Equal(t, c.want[i].Key, got[i].Key, c.text)
			assert.Equal(t, got[i].Text != "", true)
		}
	}
}

func TestFind_SpansAndSecrets(t *testing.T) {
	s := "a=%7B%7Bstring%20.k%7D%7D&b={{int .n}}"
	ps := Find(s)
	require.Len(t, ps, 2)
	assert.Equal(t, "%7B%7Bstring%20.k%7D%7D", s[ps[0].Start:ps[0].End])
	assert.Equal(t, "{{string .k}}", ps[0].Text, "an encoded placeholder's Text is its decoded form")
	assert.Equal(t, "{{int .n}}", s[ps[1].Start:ps[1].End])

	name, ok := Find("{{string .secret.db_pass}}")[0].Secret()
	assert.True(t, ok)
	assert.Equal(t, "db_pass", name)
	_, ok = Find("{{string .secretive}}")[0].Secret()
	assert.False(t, ok)
}

func TestFormat(t *testing.T) {
	assert.Equal(t, "{{capture int .orderId}}", Format(true, TypeInt, "orderId"))
	assert.Equal(t, "{{string .id}}", Format(false, TypeString, "id"))
	assert.Equal(t, "{{.id}}", Format(false, "", "id"))
	// What Format writes, Find reads back as written.
	p := Find(Format(true, TypeFloat, "price"))[0]
	assert.Equal(t, Placeholder{Start: 0, End: len("{{capture float .price}}"), Capture: true, Type: TypeFloat, Key: "price", Text: "{{capture float .price}}"}, p)
}

// What Format writes with any key of the grammar, Find reads back: the key
// grammar is one for writers and reader.
func TestFind_ReadsBackWhatFormatWritesForAnyKey(t *testing.T) {
	for _, key := range []string{"id", "_x", "número", "Contraseña", "订单号", "ключ_2", "x٣", "secret.Contraseña"} {
		for _, capture := range []bool{false, true} {
			text := Format(capture, TypeString, key)
			ps := Find(`{"a":"` + text + `"}`)
			require.Len(t, ps, 1, text)
			assert.Equal(t, key, ps[0].Key)
			assert.Equal(t, capture, ps[0].Capture)
		}
	}
}

// Placeholder-shaped text whose key is not a key of the grammar (a dotted
// key, a '-', a leading digit) is text: Find leaves it, FindOutsideGrammar
// names it so Check can warn. A secret is in the grammar.
func TestFindOutsideGrammar(t *testing.T) {
	text := `{"a":"{{ .Subscriber.Name }}","b":"{{string .user.id}}","c":"{{string .secret.k}}","d":"{{string .id}}","e":"{{capture string .user-id}}","f":"{{int .2fa}}","g":"{{string .secret.a.b}}"}`
	got := FindOutsideGrammar(text)
	var keys []string
	for _, p := range got {
		keys = append(keys, p.Key)
	}
	assert.Equal(t, []string{"Subscriber.Name", "user.id", "user-id", "2fa", "secret.a.b"}, keys)
	assert.Equal(t, TypeString, got[1].Type)
	assert.True(t, got[2].Capture)
	for _, p := range Find(text) {
		assert.True(t, IsKey(p.Key), "Find found %q, outside the grammar", p.Text)
	}
	assert.True(t, IsKey("número_2"))
	assert.False(t, IsKey("user-id"))
	assert.False(t, IsKey("2fa"))
}

func TestContains_FindsTheEncodedFormInAnyCase(t *testing.T) {
	assert.True(t, Contains("/u/%7b%7Bstring%20.id%7D%7d"))
	assert.False(t, Contains("/u/%7B%22a%22%7D 100% off"))
	assert.False(t, Contains("no placeholder here, 50%"))
	assert.False(t, hasEncodedOpen("%7B%7"))
	assert.True(t, hasEncodedOpen("x%%7B%7B"))
}

// FIRST HELD IS A TEXT'S FIRST TEMPLATE TEXT, a secret excepted: what keploy
// templatize reads, over each file of a set, to leave a set that holds any as
// it is. It is any "{{", optional capture and type, '.', no brace up to "}}":
// a placeholder of the grammar, of a key defined or not, percent-encoded too;
// text outside the grammar; an expression, whether an earlier keploy rendered
// it or not; a bare dot. The leftmost of the text, with where it starts. A
// secret keploy sanitize wrote is none, however it is spaced or encoded; a
// capture of a secret, which sanitize never writes, is. Text no keploy
// renders (no '.' after the braces) is none.
func TestFirstHeld(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       string
		at         int
	}{
		{"plain text", `{"id":"u_1"}`, "", 0},
		{"a use", `{"id":"{{string .id}}"}`, "{{string .id}}", 7},
		{"a capture", `id={{capture int .n}};`, "{{capture int .n}}", 3},
		{"secrets only", `http://h/users?key={{string .secret.k}} Bearer {{ string .secret.tok }} {{.secret.x}}`, "", 0},
		{"secrets percent-encoded", `http://h/%7B%7Bstring%20.secret.k%7D%7D`, "", 0},
		{"the leftmost, past a secret", `http://h/{{string .secret.k}}/{{.a.b}}/{{string .x}}`, "{{.a.b}}", 30},
		{"a use before text outside the grammar", `{{string .x}} {{.a.b}}`, "{{string .x}}", 0},
		{"untyped text", `Hi {{ .Name }}`, "{{ .Name }}", 3},
		{"an expression an earlier keploy rendered", `/users/{{ .id | print }}`, "{{ .id | print }}", 7},
		{"an expression that holds a double quote", `{{ float .price | printf "%.2f" }} {{string .id}}`, `{{ float .price | printf "%.2f" }}`, 0},
		{"a bare dot", `{{#items}}{{.}}{{/items}}`, "{{.}}", 10},
		{"a secret's expression, which is no secret", `http://h/{{ .secret.k | urlquery }}`, "{{ .secret.k | urlquery }}", 9},
		{"a capture of a secret, which sanitize never writes", `{"t":"{{capture string .secret.tok}}"}`, "{{capture string .secret.tok}}", 6},
		{"percent-encoded in a URL", `http://h/users/%7B%7Bstring%20.id%7D%7D`, "{{string .id}}", 15},
		{"a capture of a key outside the grammar", `X-Id: {{capture string .a.b}}`, "{{capture string .a.b}}", 6},
		{"a capture with no type", `{{capture .id}}`, "{{capture .id}}", 0},
		{"split over two lines of a file", "a: {{string\n    .id}}", "{{string\n    .id}}", 3},
		{"braces no keploy renders", `{"t":"{{name}} {{\\pi}} {{ }} {{u^2}} {{stringy .x}}"}`, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, at, ok := FirstHeld(c.text)
			assert.Equal(t, c.want != "", ok)
			assert.Equal(t, c.want, text)
			assert.Equal(t, c.at, at)
		})
	}
}

// EachString visits every string a value holds, a map's keys too, in a fixed
// order, each with its path: what templatize reads config.yaml's template
// with, and the set check a test case's assertions.
func TestEachString(t *testing.T) {
	var got []string
	EachString(map[string]interface{}{
		"b": []interface{}{"x", 2, map[interface{}]interface{}{"k": "v"}},
		"a": "one",
		"n": nil,
		"m": map[models.AssertionType]interface{}{"z": []string{"p"}},
	}, func(path, text string) { got = append(got, path+"="+text) })
	assert.Equal(t, []string{"a=a", "a=one", "b=b", "b[0]=x", "b[2].k=k", "b[2].k=v", "m=m", "m.z=z", "m.z[0]=p", "n=n"}, got)

	// A document a store decodes into structs, as bson.D's {Key, Value}
	// elements: each exported field, by its name.
	type elem struct {
		Key   string
		Value interface{}
		note  string
	}
	got = nil
	EachString(map[string]interface{}{"header_equal": []elem{{Key: "X-User", Value: "{{string .id}}", note: "unexported"}}},
		func(path, text string) { got = append(got, path+"="+text) })
	assert.Equal(t, []string{"header_equal=header_equal", "header_equal[0].Key=X-User", "header_equal[0].Value={{string .id}}"}, got)
}
