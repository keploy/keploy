package contract

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// describeSchema renders a generated schema as a compact, deterministic string
// so a test can pin the whole inferred shape - nesting included - instead of
// reaching into one field. Notation: [x] is an array of x, {k:v} an object,
// `any` an untyped schema, and a trailing ? means nullable.
func describeSchema(s models.Schema) string {
	out := s.Type
	switch s.Type {
	case "":
		out = "any"
	case "array":
		inner := "<missing items>"
		if s.Items != nil {
			inner = describeSchema(*s.Items)
		}
		out = "[" + inner + "]"
	case "object":
		out = describeProps(s.Properties)
	}
	if s.Nullable {
		out += "?"
	}
	return out
}

// describeProperty is describeSchema for the map form every schema below the
// root is held in.
func describeProperty(p map[string]interface{}) string {
	typ, _ := p["type"].(string)
	out := typ
	switch typ {
	case "":
		out = "any"
	case "array":
		inner := "<missing items>"
		if items, ok := p["items"].(map[string]interface{}); ok {
			inner = describeProperty(items)
		}
		out = "[" + inner + "]"
	case "object":
		props, _ := p["properties"].(map[string]map[string]interface{})
		out = describeProps(props)
	}
	if p["nullable"] == true {
		out += "?"
	}
	return out
}

func describeProps(props map[string]map[string]interface{}) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+describeProperty(props[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// generatedSchemas runs a body through the real generation path as both a
// request and a response body, asserts the resulting document passes real
// kin-openapi validation, and returns the two inferred shapes.
//
// Both directions matter: request and response schemas are built by the same
// code but only the response carries an example, and it is the example that
// kin-openapi checks the schema against.
func generatedSchemas(t *testing.T, body string) (reqShape, respShape string) {
	t.Helper()
	svc := &contract{}

	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("POST", body, body))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI(%s) failed: %v", body, err)
	}
	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated contract for %s is not a valid OpenAPI document: %v", body, err)
	}

	post := oapi.Paths["/users"].Post
	return describeSchema(post.RequestBody.Content["application/json"].Schema),
		describeSchema(post.Responses["200"].Content["application/json"].Schema)
}

// TestHTTPDocToOpenAPI_NestedAndUninformativeArrays covers the shapes that
// aborted `keploy contract generate` outright: the generator inferred an item
// schema from a single chosen element, so anything that element failed to
// reveal was either missing (an array of arrays got items:{type:array} with no
// nested items, which OpenAPI forbids) or wrong (an empty or null element made
// the item type default to "string", and the recorded example then violated the
// schema the generator had just written).
//
// Every case here produced an invalid document before the fold replaced
// sample-and-widen; generatedSchemas asserts validity, so these fail without
// the fix regardless of the shape assertions.
func TestHTTPDocToOpenAPI_NestedAndUninformativeArrays(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		// An array of arrays under an object key emitted items:{type: array}
		// with no nested items. This aborted generation for a HOMOGENEOUS body
		// too, so any object field holding an array of arrays was affected.
		{name: "homogeneous array of arrays under a key", body: `{"a":[[1,2]]}`, want: `{a:[[integer]]}`},
		{name: "mixed numeric array of arrays under a key", body: `{"a":[[1,1.5]]}`, want: `{a:[[number]]}`},
		{name: "widening across sibling inner arrays", body: `{"a":[[1],[2,3.5]]}`, want: `{a:[[number]]}`},
		{name: "three levels of nesting", body: `{"a":[[[1]]]}`, want: `{a:[[[integer]]]}`},
		{name: "array of arrays of objects", body: `{"a":[[{"x":1}]]}`, want: `{a:[[{x:integer}]]}`},
		{name: "array of arrays under a nested object", body: `{"a":{"b":[[1]]}}`, want: `{a:{b:[[integer]]}}`},
		{name: "array of arrays under an array root", body: `[{"v":[[1,1.5]]}]`, want: `[{v:[[number]]}]`},
		{name: "null beside an inner array", body: `{"a":[null,[1]]}`, want: `{a:[[integer]?]}`},

		// An empty first element revealed no item type, so the schema said
		// "string" and the example then failed validation.
		{name: "empty array before an informative one", body: `[[],[1]]`, want: `[[integer]]`},
		{name: "empty array then widening elements", body: `[[],[1],[1.5]]`, want: `[[number]]`},
		{name: "empty array before an object", body: `[[],[{"x":1}]]`, want: `[[{x:integer}]]`},
		{name: "empty array before a nested array", body: `[[],[[1]]]`, want: `[[[integer]]]`},
		// Uninformative at depth 2: no "first informative element" rule that
		// looks only at the top level can fix this one.
		{name: "uninformative at depth two", body: `[[[]],[[1]]]`, want: `[[[integer]]]`},
		// Uninformative through an object key rather than positionally.
		{name: "empty array under an object key", body: `[{"v":[]},{"v":[1]}]`, want: `[{v:[integer]}]`},
		{name: "empty array under a key one level deeper", body: `{"a":[{"v":[]},{"v":[1]}]}`, want: `{a:[{v:[integer]}]}`},
		// A null sample is exactly as uninformative as an empty array, and the
		// result must not depend on which element came first.
		{name: "null property before a typed one", body: `[{"v":null},{"v":1}]`, want: `[{v:integer?}]`},
		{name: "typed property before a null one", body: `[{"v":1},{"v":null}]`, want: `[{v:integer?}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReq, gotResp := generatedSchemas(t, tt.body)
			if gotReq != tt.want {
				t.Errorf("request schema = %s, want %s", gotReq, tt.want)
			}
			if gotResp != tt.want {
				t.Errorf("response schema = %s, want %s", gotResp, tt.want)
			}
		})
	}
}

// TestHTTPDocToOpenAPI_ConflictingElementKinds pins what happens when elements
// disagree on kind rather than merely on numeric width.
//
// OpenAPI 3.0 cannot say "string or integer" in a single type; oneOf is the
// honest encoding and is deliberately out of scope. An untyped schema is the
// only construct that accepts every kind, so that is what gets emitted - and
// unlike the previous behaviour of believing the first element, it produces a
// document the recorded example actually satisfies.
func TestHTTPDocToOpenAPI_ConflictingElementKinds(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "string then integer", body: `["x",1]`, want: `[any]`},
		{name: "integer then string", body: `[1,"x"]`, want: `[any]`},
		{name: "array then string", body: `[[],"x"]`, want: `[any]`},
		{name: "array then object", body: `[[],{}]`, want: `[any]`},
		{name: "conflict one level down", body: `[[1],["x"]]`, want: `[[any]]`},
		{name: "conflict under an object key", body: `{"a":[[1],["x"]]}`, want: `{a:[[any]]}`},
		// A conflict that also saw a null must stay nullable, or kin-openapi
		// rejects the null in the example: an untyped schema accepts every
		// kind but still refuses null unless nullable is set.
		{name: "null with conflicting kinds", body: `[null,"x",1]`, want: `[any?]`},
		{name: "null with every kind", body: `[null,1,"x",[2],{"a":1}]`, want: `[any?]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReq, gotResp := generatedSchemas(t, tt.body)
			if gotReq != tt.want {
				t.Errorf("request schema = %s, want %s", gotReq, tt.want)
			}
			if gotResp != tt.want {
				t.Errorf("response schema = %s, want %s", gotResp, tt.want)
			}
		})
	}
}

// TestHTTPDocToOpenAPI_ShapesUnchanged is the regression guard for the fold.
//
// Replacing sample-and-widen with a fold over every element rewrote how every
// schema is inferred, not just the broken shapes. These bodies all generated
// valid contracts before, and every one of them must still produce exactly the
// same schema, or the fix silently rewrites contracts that were already right.
func TestHTTPDocToOpenAPI_ShapesUnchanged(t *testing.T) {
	tests := []struct{ body, want string }{
		{`[]`, `[string]`},
		{`[1,2,3]`, `[integer]`},
		{`[1,1.5]`, `[number]`},
		{`[1.5,1]`, `[number]`},
		{`["a","b"]`, `[string]`},
		{`[true]`, `[boolean]`},
		{`[[1,2]]`, `[[integer]]`},
		{`[[1],[1.5]]`, `[[number]]`},
		{`[[[1]]]`, `[[[integer]]]`},
		{`[[{"x":1}]]`, `[[{x:integer}]]`},
		{`[[]]`, `[[string]]`},
		{`[[],[]]`, `[[string]]`},
		{`[null]`, `[string?]`},
		{`[null,[1]]`, `[[integer]?]`},
		{`[null,1,1.5]`, `[number?]`},
		{`[[null,1],[1.5]]`, `[[number?]]`},
		{`[[null,1],[1.5,null]]`, `[[number?]]`},
		{`[null,{"id":1}]`, `[{id:integer}?]`},
		{`[{"id":1,"name":"Utkarsh"}]`, `[{id:integer,name:string}]`},
		{`[{"id":1,"deleted_at":null}]`, `[{deleted_at:string?,id:integer}]`},
		{`[{"a":1},{"a":1.5}]`, `[{a:number}]`},
		{`[{"a":{"b":1}},{"a":{"b":1.5}}]`, `[{a:{b:number}}]`},
		{`[{"v":[1]},{"v":[1.5]}]`, `[{v:[number]}]`},
		{`[{"v":[null,1]},{"v":[1.5]}]`, `[{v:[number?]}]`},
		// Only the keys the first object showed are described. OpenAPI allows
		// undeclared properties, so this stays valid; unioning the keys would
		// be better inference but is a separate change.
		{`[{"a":1},{"b":2}]`, `[{a:integer}]`},
		{`[{},{"x":1}]`, `[{}]`},
		{`[{"v":[]}]`, `[{v:[string]}]`},
		{`{"id":1}`, `{id:integer}`},
		{`{"count":0}`, `{count:integer}`},
		{`{"delta":-7}`, `{delta:integer}`},
		{`{"price":1.5}`, `{price:number}`},
		{`{"ratio":1.0}`, `{ratio:number}`},
		{`{"big":1e10}`, `{big:number}`},
		{`{"huge":9007199254740993}`, `{huge:integer}`},
		{`{"ok":true}`, `{ok:boolean}`},
		{`{"a":null}`, `{a:string?}`},
		{`{"a":{"b":null}}`, `{a:{b:string?}}`},
		{`{"tags":[null,"x"]}`, `{tags:[string?]}`},
		{`{"rows":[{"v":null}]}`, `{rows:[{v:string?}]}`},
		{`{"r":[{"v":[null,1]},{"v":[1.5]}]}`, `{r:[{v:[number?]}]}`},
		{`{"prices":[10,9.99]}`, `{prices:[number]}`},
		{`{"a":[]}`, `{a:[string]}`},
	}

	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			gotReq, gotResp := generatedSchemas(t, tt.body)
			if gotReq != tt.want {
				t.Errorf("request schema = %s, want %s", gotReq, tt.want)
			}
			if gotResp != tt.want {
				t.Errorf("response schema = %s, want %s", gotResp, tt.want)
			}
		})
	}
}

// TestItemSchemaFoldIsOrderIndependent asserts the property the fold is for:
// merging is commutative, so the inferred item schema cannot depend on which
// element the generator happened to look at first. Sample-and-widen could not
// have this property - it is why [{"v":1},{"v":null}] and its reverse used to
// disagree.
func TestItemSchemaFoldIsOrderIndependent(t *testing.T) {
	elements := []interface{}{
		nil,
		[]interface{}{},
		[]interface{}{int64(1)},
		[]interface{}{1.5},
		map[string]interface{}{"v": int64(2)},
	}

	forward := itemSchema(elements)
	finalizeSchema(forward)

	reversed := make([]interface{}, len(elements))
	for i, el := range elements {
		reversed[len(elements)-1-i] = el
	}
	backward := itemSchema(reversed)
	finalizeSchema(backward)

	if got, want := describeProperty(backward), describeProperty(forward); got != want {
		t.Errorf("reversed element order gave %s, want %s (same as forward order)", got, want)
	}
}

// TestFinalizeSchemaIsIdempotent guards the one-shot contract finalizeSchema
// documents: it must be safe to reach the same node twice (merged schemas share
// subtrees), and running it again must never turn a filled-in default into
// something else.
func TestFinalizeSchemaIsIdempotent(t *testing.T) {
	for _, body := range []interface{}{
		[]interface{}{},
		[]interface{}{nil, "x", int64(1)},
		[]interface{}{map[string]interface{}{"v": nil}},
	} {
		s := itemSchema(body.([]interface{}))
		finalizeSchema(s)
		once := describeProperty(s)
		finalizeSchema(s)
		if twice := describeProperty(s); twice != once {
			t.Errorf("finalizeSchema(%v) is not idempotent: once=%s twice=%s", body, once, twice)
		}
	}
}

// TestAnyTypeMarkerNeverEscapes is a belt-and-braces check that the internal
// conflict marker cannot reach a generated contract: it is not a legal OpenAPI
// type, so any leak would produce a document consumers cannot read.
func TestAnyTypeMarkerNeverEscapes(t *testing.T) {
	svc := &contract{}
	for _, body := range []string{`["x",1]`, `[[1],["x"]]`, `{"a":[[1],["x"]]}`, `[null,1,"x"]`} {
		oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("POST", body, body))
		if err != nil {
			t.Fatalf("HTTPDocToOpenAPI(%s): %v", body, err)
		}
		rendered := fmt.Sprintf("%#v", oapi)
		for _, marker := range []string{anyType, unknownType} {
			if strings.Contains(rendered, marker) {
				t.Errorf("internal type marker %q leaked into the generated contract for %s", marker, body)
			}
		}
	}
}
