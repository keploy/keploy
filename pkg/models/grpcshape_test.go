package models

import (
	"reflect"
	"strings"
	"testing"
)

func grpcCase(path, status string, req, resp []string) *TestCase {
	tc := &TestCase{Kind: GRPC_EXPORT}
	tc.GrpcReq.Headers.PseudoHeaders = map[string]string{":path": path}
	var reqMsgs, respMsgs []GrpcLengthPrefixedMessage
	for _, d := range req {
		reqMsgs = append(reqMsgs, GrpcLengthPrefixedMessage{DecodedData: d})
	}
	for _, d := range resp {
		respMsgs = append(respMsgs, GrpcLengthPrefixedMessage{DecodedData: d})
	}
	tc.GrpcReq.SetMessages(reqMsgs)
	tc.GrpcResp.SetMessages(respMsgs)
	tc.GrpcResp.Trailers.OrdinaryHeaders = map[string]string{"grpc-status": status}
	return tc
}

func TestGRPCShape(t *testing.T) {
	const p = "/users.v1.Users/Get"
	sameShape(t, "values",
		grpcCase(p, "0", []string{`1: {"alice"}`}, []string{`1: {1: {"a"} 2: {"a@x"}}`}),
		grpcCase(p, "0", []string{`1: {"bob"}`}, []string{`1: {1: {"b"} 2: {"b@y"}}`}))
	differentShape(t, "a nested message's fields",
		grpcCase(p, "0", nil, []string{`1: {1: {"alice"} 2: {"a@x"}}`}),
		grpcCase(p, "0", nil, []string{`1: {1: {"alice"}}`}))
	sameShape(t, "scalars do not count (proto3 omits defaults)",
		grpcCase(p, "0", nil, []string{"1: {\"a\"}\n2: 5\n3: 1.5i32   # 0x3fc00000i32"}),
		grpcCase(p, "0", nil, []string{`1: {"a"}`}))
	differentShape(t, "status",
		grpcCase(p, "0", nil, []string{`1: {"a"}`}), grpcCase(p, "5", nil, []string{`1: {"a"}`}))
	differentShape(t, "path",
		grpcCase(p, "0", nil, nil), grpcCase("/users.v1.Users/List", "0", nil, nil))
	sameShape(t, "a stream's length and order",
		grpcCase(p, "0", nil, []string{`1: {"a"}`, `2: {"b"}`, `1: {"c"}`}),
		grpcCase(p, "0", nil, []string{`2: {"b"}`, `1: {"a"}`}))
	differentShape(t, "a field only in a later stream message",
		grpcCase(p, "0", nil, []string{`1: {"a"}`, `1: {"a"} 3: {"x"}`}),
		grpcCase(p, "0", nil, []string{`1: {"a"}`}))
	differentShape(t, "no message and one empty message",
		grpcCase(p, "0", nil, nil), grpcCase(p, "0", nil, []string{""}))
	sameShape(t, "a missing grpc-status reads as 0",
		grpcCase(p, "", nil, nil), grpcCase(p, "0", nil, nil))
}

func TestGRPCMessagesShapeParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want []string
	}{
		{"fields on one line", `1: {"y"} 2: {"z"}`, []string{"$.1:l", "$.2:l", "$:m"}},
		{"a single-line nested block", `1: {7: 56}`, []string{"$.1:l", "$:m"}},
		{"nested length-delimited fields", "1: {\n  2: {\"x\"}\n  3: {4: {`ff`}}\n}", []string{"$.1.2:l", "$.1.3.4:l", "$.1.3:l", "$.1:l", "$:m"}},
		{"bytes", "5: `deadbeef`", []string{"$.5:l", "$:m"}},
		{"a group", `6: !{1: {"g"}}`, []string{"$.6.1:l", "$.6:l", "$:m"}},
		{"a long-form marker and a remark", "1: long-form:1 0\n2: 1.5i64   # 0x3ff8000000000000i64\n3: {\"k\"}", []string{"$.3:l", "$:m"}},
		{"braces and a quote inside a string", `1: {"a } b \" {"} 2: {"c"}`, []string{"$.1:l", "$.2:l", "$:m"}},
		{"an unterminated block", `1: {2: {"x"`, []string{"$.1.2:l", "$.1:l", "$:m"}},
	} {
		got := grpcMessagesShape([]GrpcLengthPrefixedMessage{{DecodedData: tc.data}})
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q → %v, want %v", tc.name, tc.data, got, tc.want)
		}
	}
	if got := grpcMessagesShape(nil); got != nil {
		t.Errorf("no messages → %v, want none", got)
	}
}

// FuzzGRPCMessagesShape checks that any protoscope text gives one shape,
// without a panic or a loop.
func FuzzGRPCMessagesShape(f *testing.F) {
	for _, s := range []string{"1: {2: {\"x\"} 3: `ff`}\n4: 5", `1: {"a } b"}`, "1: !{", "#\n}", `{{{`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		msgs := []GrpcLengthPrefixedMessage{{DecodedData: data}}
		if !reflect.DeepEqual(grpcMessagesShape(msgs), grpcMessagesShape(msgs)) {
			t.Fatal("the shape differs between two reads")
		}
	})
}

func TestGRPCMessagesShapeOfADeepMessage(t *testing.T) {
	deep := strings.Repeat("1: {", 1<<20) + strings.Repeat("}", 1<<20) + "\n2: {\"after\"}"
	got := grpcMessagesShape([]GrpcLengthPrefixedMessage{{DecodedData: deep}})
	if len(got) != protoscopeMaxDepth+3 {
		t.Fatalf("%d entries for a message nested past protoscopeMaxDepth, want %d", len(got), protoscopeMaxDepth+3)
	}
	if got[len(got)-2] != "$.2:l" {
		t.Fatalf("the field after the deep one: %v", got[len(got)-3:])
	}
}
