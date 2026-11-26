package protoschema

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const ordersProto = `syntax = "proto3";
package demo;
import "google/protobuf/timestamp.proto";
import "common/money.proto";
service Orders {
  rpc Read(ReadReq) returns (Order);
  rpc Watch(ReadReq) returns (stream Order);
}
message ReadReq { string id = 1; }
message Item { string sku = 1; common.Money price = 2; }
message Order {
  string id = 1;
  string status = 3;
  google.protobuf.Timestamp created_at = 8;
  repeated Item items = 4;
  map<string, Item> by_sku = 5;
}
`

const moneyProto = `syntax = "proto3";
package common;
message Money { int64 units = 1; string currency = 2; }
`

func compileOrders(t *testing.T) *Schema {
	t.Helper()
	c, err := Compile(context.Background(), map[string][]byte{
		"demo/orders.proto":  []byte(ordersProto),
		"common/money.proto": []byte(moneyProto),
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(c.DescriptorSet)
	if err != nil {
		t.Fatalf("the compiled set does not load on its own: %v", err)
	}
	return s
}

func TestCompileListsMethodsAndIsSelfContained(t *testing.T) {
	c, err := Compile(context.Background(), map[string][]byte{
		"demo/orders.proto":  []byte(ordersProto),
		"common/money.proto": []byte(moneyProto),
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range c.Methods {
		paths = append(paths, m.Path)
		if m.Path == "/demo.Orders/Watch" && !m.ServerStreaming {
			t.Errorf("Watch streams its response; got %+v", m)
		}
	}
	if strings.Join(paths, ",") != "/demo.Orders/Read,/demo.Orders/Watch" {
		t.Fatalf("methods = %v", paths)
	}
	// Loaded alone (well-known types included) it still lists them.
	s, err := Load(c.DescriptorSet)
	if err != nil || len(s.Methods()) != 2 {
		t.Fatalf("Load: %v, methods %v", err, s)
	}
}

func TestCompileNamesWhatFails(t *testing.T) {
	_, err := Compile(context.Background(), map[string][]byte{"demo/orders.proto": []byte(ordersProto)})
	var ce *CompileError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "common/money.proto") {
		t.Fatalf("a missing import must fail naming it; got %v", err)
	}
	_, err = Compile(context.Background(), map[string][]byte{"bad.proto": []byte("syntax = \"proto3\"; message {")})
	if !errors.As(err, &ce) {
		t.Fatalf("a syntax error must be a CompileError; got %v", err)
	}
	for _, p := range []string{"../escape.proto", "a/../../escape.proto"} {
		if _, err := Compile(context.Background(), map[string][]byte{p: []byte(moneyProto)}); err == nil || !strings.Contains(err.Error(), "leaves the upload's root") {
			t.Fatalf("%s: a path leaving the upload's root: %v", p, err)
		}
	}
	for _, p := range []string{"/abs.proto", "//abs.proto", `C:\x\a.proto`} {
		if _, err := Compile(context.Background(), map[string][]byte{p: []byte(moneyProto)}); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("%s: an absolute path: %v", p, err)
		}
	}
	if _, err := Compile(context.Background(), map[string][]byte{"a.proto": []byte(moneyProto), "./a.proto": []byte(moneyProto)}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("two names for one file: %v", err)
	}
	if _, err := Compile(context.Background(), map[string][]byte{"README.md": []byte("x")}); err == nil {
		t.Fatal("an upload with no .proto file was accepted")
	}
}

func TestMessageTypes(t *testing.T) {
	s := compileOrders(t)
	req, resp, err := s.MessageTypes("/demo.Orders/Read")
	if err != nil || req.FullName() != "demo.ReadReq" || resp.FullName() != "demo.Order" {
		t.Fatalf("Read: %v %v %v", req, resp, err)
	}
	if _, _, err := s.MessageTypes("/demo.Orders/Cancel"); !errors.Is(err, ErrNoMethod) {
		t.Fatalf("an undefined method: %v", err)
	}
	if _, _, err := s.MessageTypes("/other.Svc/Read"); !errors.Is(err, ErrNoMethod) {
		t.Fatalf("an undefined service: %v", err)
	}
}

func decodedMap(t *testing.T, d *Decoded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(d.JSON, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDecodeShowsFieldNames(t *testing.T) {
	s := compileOrders(t)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	body := "1: {\"o-1\"}\n3: {\"Pending\"}\n8: {\n  1: 1795582799\n}\n4: {\n  1: {\"sku-1\"}\n  2: {\n    1: 25\n    2: {\"USD\"}\n  }\n}"
	d, err := s.DecodeProtoscope(resp, body)
	if err != nil {
		t.Fatal(err)
	}
	m := decodedMap(t, d)
	if m["id"] != "o-1" || m["status"] != "Pending" {
		t.Fatalf("decoded = %s", d.JSON)
	}
	items := m["items"].([]any)
	price := items[0].(map[string]any)["price"].(map[string]any)
	if price["currency"] != "USD" || d.UnknownFields != 0 {
		t.Fatalf("nested message across files not decoded: %s", d.JSON)
	}
}

// A field the schema does not define is shown, never dropped, at the top and
// inside nested messages, list elements and map values.
func TestDecodeKeepsFieldsTheSchemaDoesNotDefine(t *testing.T) {
	s := compileOrders(t)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	body := "1: {\"o-1\"}\n9: {\"tenant-a\"}\n4: {\n  1: {\"sku-1\"}\n  7: 42\n}\n5: {\n  1: {\"k\"}\n  2: {\n    1: {\"sku-2\"}\n    6: {\"extra\"}\n  }\n}"
	d, err := s.DecodeProtoscope(resp, body)
	if err != nil {
		t.Fatal(err)
	}
	m := decodedMap(t, d)
	if m["#9"] != "tenant-a" {
		t.Errorf("top-level unknown field 9 missing: %s", d.JSON)
	}
	if it := m["items"].([]any)[0].(map[string]any); it["#7"] != float64(42) {
		t.Errorf("unknown field 7 in a list element missing: %s", d.JSON)
	}
	if v := m["by_sku"].(map[string]any)["k"].(map[string]any); v["#6"] != "extra" {
		t.Errorf("unknown field 6 in a map value missing: %s", d.JSON)
	}
	if d.UnknownFields != 3 {
		t.Errorf("UnknownFields = %d, want 3", d.UnknownFields)
	}
}

// A field whose wire type does not match the schema (a body of another type,
// or a schema of another version) is shown under its number and counted, not
// hidden: protobuf reads it as a field the schema does not define.
func TestDecodeShowsAFieldOfTheWrongWireType(t *testing.T) {
	s := compileOrders(t)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	d, err := s.DecodeProtoscope(resp, "1: 5")
	if err != nil {
		t.Fatal(err)
	}
	m := decodedMap(t, d)
	if _, named := m["id"]; named || m["#1"] != float64(5) || d.UnknownFields != 1 {
		t.Fatalf("field 1 (a string in the schema) sent as a varint must show as #1 and count; got %s (%d)", d.JSON, d.UnknownFields)
	}
}

func TestFromFileDescriptorsDecodesLikeACompiledSet(t *testing.T) {
	c, err := Compile(context.Background(), map[string][]byte{
		"demo/orders.proto":  []byte(ordersProto),
		"common/money.proto": []byte(moneyProto),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(c.DescriptorSet)
	var fds []protoreflect.FileDescriptor
	loaded.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool { fds = append(fds, fd); return true })
	s, err := FromFileDescriptors(fds)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := s.MessageTypes("/demo.Orders/Read")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.DecodeProtoscope(resp, "3: {\"Pending\"}\n9: 1")
	if err != nil || !strings.Contains(string(d.JSON), "Pending") || d.UnknownFields != 1 {
		t.Fatalf("decode: %v %s", err, d.JSON)
	}
}

// An undefined length-delimited field holding a nested message is shown as
// one, not as control characters.
func TestDecodeShowsAnUndefinedNestedMessage(t *testing.T) {
	s := compileOrders(t)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	d, err := s.DecodeProtoscope(resp, "1: {\"o-1\"}\n12: {\n  1: 5\n  2: 7\n}")
	if err != nil {
		t.Fatal(err)
	}
	m := decodedMap(t, d)
	nested, ok := m["#12"].(map[string]any)
	if !ok || nested["#1"] != float64(5) || nested["#2"] != float64(7) {
		t.Fatalf("#12 = %#v (%s)", m["#12"], d.JSON)
	}
}

// Nested unknown messages are opened to a bounded depth; deeper ones come
// back as base64, so an adversarial body cannot recurse without end.
func TestUnknownNestingIsBounded(t *testing.T) {
	b := []byte{0x08, 0x01} // field 1 = 1
	for i := 0; i < maxUnknownDepth+5; i++ {
		b = append(append([]byte{0x12}, protowire.AppendVarint(nil, uint64(len(b)))...), b...) // field 2 = {b}
	}
	v := unknownBytesValue(b)
	depth := 0
	for {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		depth++
		v = m["#2"]
	}
	if depth > maxUnknownDepth {
		t.Fatalf("opened %d levels, cap %d", depth, maxUnknownDepth)
	}
	if _, ok := v.(string); !ok {
		t.Fatalf("the level past the cap must be base64 text, got %T", v)
	}
}
