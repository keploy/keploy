package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScanJSONReadsWhatEncodingJSONReads checks that the scanner takes a
// document for one JSON value exactly when encoding/json does, and writes
// its shape.
func TestScanJSONReadsWhatEncodingJSONReads(t *testing.T) {
	for _, c := range []struct{ doc, shape string }{
		{`{}`, "o"},
		{`[]`, "a"},
		{` {"a" : [ 1 , "x" , true , null , {"b":-0.5e+10} ] } `, "o{a:a[osnbz{b:n}]}"},
		{`"x"`, "s"},
		{`-0`, "n"},
		{`1e400`, "n"},
		{`{"a":1,"a":"x"}`, "o{a:sn}"},
		{`{"\u0061":1,"a":2}`, "o{a:n}"},
		{`{"\ud83d\ude00":1}`, `o{"😀":n}`},
		{`{"\ud83d":1}`, `o{"�":n}`},
		{`{"a\/b\"c\\d\n":1}`, `o{"a/b\"c\\d\n":n}`},
		{`{"a":1}x`, ""},
		{`{"a":1} {}`, ""},
		{`{"a":1,}`, ""},
		{`[1,]`, ""},
		{`[01]`, ""},
		{`[1.]`, ""},
		{`[.5]`, ""},
		{`[1e]`, ""},
		{`[-]`, ""},
		{`[tru]`, ""},
		{`[truex]`, ""},
		{`{"a"}`, ""},
		{`{a:1}`, ""},
		{`["\x"]`, ""},
		{`["\u12"]`, ""},
		{"[\"a\tb\"]", ""},
		{`[`, ""},
		{``, ""},
	} {
		tree, _, ok := scanJSON(c.doc, roleNone)
		if valid := json.Valid([]byte(c.doc)); ok != valid {
			t.Errorf("scanJSON(%q) ok = %v, encoding/json says %v", c.doc, ok, valid)
		}
		if ok && tree.String() != c.shape {
			t.Errorf("scanJSON(%q) = %q, want %q", c.doc, tree.String(), c.shape)
		}
	}
}

// TestScanJSONKeepsInvalidUTF8 checks that a member name keeps a byte that is
// not valid UTF-8, where encoding/json writes U+FFFD.
func TestScanJSONKeepsInvalidUTF8(t *testing.T) {
	a, _, _ := scanJSON("{\"\xff\":1}", roleNone)
	b, _, _ := scanJSON(`{"\ufffd":1}`, roleNone)
	c, _, _ := scanJSON("{\"\\n\xff\":1}", roleNone)
	if a.String() == b.String() || a.String() != `o{"\xff":n}` || c.String() != `o{"\n\xff":n}` {
		t.Fatalf("invalid UTF-8 names: %q, %q, %q", a.String(), b.String(), c.String())
	}
}

// TestJSONRequestOperation checks what a request's JSON body says of the
// operation it runs, as encoding/json would decode it into a map.
func TestJSONRequestOperation(t *testing.T) {
	for _, c := range []struct{ body, op string }{
		{`{"jsonrpc":"2.0","method":"a"}`, `jsonrpc:"a"`},
		{`{"jsonrpc":"2.0","method":"a","method":1}`, ""},
		{`{"jsonrpc":1,"method":"a"}`, ""},
		{`[{"jsonrpc":"2.0","method":"b"},{"jsonrpc":"2.0","method":"a"},{"jsonrpc":"2.0","method":"b"}]`, `jsonrpc:"a","b"`},
		{`[{"jsonrpc":"2.0","method":"a"},1]`, ""},
		{`[]`, ""},
		{`{"query":"{a}","operationName":"Q"}`, `graphql:query {a}#"Q"`},
		{`{"query":"{a}","operationName":"Q","operationName":null}`, "graphql:query {a}"},
		{`{"query":null,"extensions":{"persistedQuery":{"sha256Hash":"h"}}}`, `graphql:persisted:"h"`},
		{`{"extensions":{"persistedQuery":{"sha256Hash":"h"}},"extensions":{}}`, ""},
		{`{"extensions":{"persistedQuery":{"sha256Hash":"h"},"persistedQuery":1}}`, ""},
		{`{"query":"red shoes","extensions":{"persistedQuery":{"sha256Hash":"h"}}}`, ""},
		{`{"variables":{"query":"{a}"}}`, ""},
		{`[{"query":"{b}"},{"query":"{a}"},{"query":"{b}"}]`, "graphql:query {a};graphql:query {b}"},
	} {
		_, req, ok := scanJSON(c.body, roleEnvelope)
		if !ok {
			t.Fatalf("scanJSON(%q) failed", c.body)
		}
		if got := requestOperation("application/json", []byte(c.body), req, ""); got != c.op {
			t.Errorf("operation of %s = %q, want %q", c.body, got, c.op)
		}
	}
}

// FuzzScanJSON checks that the scanner takes a document for JSON exactly
// when encoding/json does, without a panic, nesting below encoding/json's
// depth limit.
func FuzzScanJSON(f *testing.F) {
	for _, s := range []string{`{"a":[1,"x",true,null,{"b":-0.5e+10}]}`, `[1,2`, `"\ud83d\ude00"`, "{\"\xff\":1}", `  0  `, `[[[]]]`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		if strings.Count(doc, "[")+strings.Count(doc, "{") > 9000 {
			t.Skip("deeper than encoding/json reads")
		}
		tree, _, ok := scanJSON(doc, roleEnvelope)
		if valid := json.Valid([]byte(doc)); ok != valid {
			t.Fatalf("scanJSON(%q) ok = %v, encoding/json says %v", doc, ok, valid)
		}
		if again, _, _ := scanJSON(doc, roleEnvelope); ok && tree.String() != again.String() {
			t.Fatalf("the shape differs between two reads: %q, then %q", tree.String(), again.String())
		}
	})
}
