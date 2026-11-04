package models

import (
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
		want string
	}{
		{"fields on one line", `1: {"y"} 2: {"z"}`, "m{1:l,2:l}"},
		{"a single-line nested block", `1: {7: 56}`, "m{1:l}"},
		{"nested length-delimited fields", "1: {\n  2: {\"x\"}\n  3: {4: {`ff`}}\n}", "m{1:l{2:l,3:l{4:l}}}"},
		{"bytes", "5: `deadbeef`", "m{5:l}"},
		{"a group", `6: !{1: {"g"}}`, "m{6:l{1:l}}"},
		{"a long-form marker and a remark", "1: long-form:1 0\n2: 1.5i64   # 0x3ff8000000000000i64\n3: {\"k\"}", "m{3:l}"},
		{"braces and a quote inside a string", `1: {"a } b \" {"} 2: {"c"}`, "m{1:l,2:l}"},
		{"an unterminated block", `1: {2: {"x"`, "m{1:l{2:l}}"},
		{"a stray '}' does not end the message", `1: {"a"} } 2: {"b"}`, "m{1:l,2:l}"},
	} {
		got := grpcMessagesShape([]GrpcLengthPrefixedMessage{{DecodedData: tc.data}})
		if got != tc.want {
			t.Errorf("%s: %q → %q, want %q", tc.name, tc.data, got, tc.want)
		}
	}
	if got := grpcMessagesShape(nil); got != "" {
		t.Errorf("no messages → %q, want none", got)
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
		if first, again := grpcMessagesShape(msgs), grpcMessagesShape(msgs); first != again {
			t.Fatalf("the shape differs between two reads: %q, then %q", first, again)
		}
	})
}

// TestGRPCShapeIsReadInFull checks that two messages that differ only deep
// down are two shapes: no depth is cut off.
func TestGRPCShapeIsReadInFull(t *testing.T) {
	const p = "/users.v1.Users/Get"
	nested := func(depth int, leaf string) string {
		return strings.Repeat("1: {", depth) + leaf + strings.Repeat("}", depth)
	}
	differentShape(t, "messages that differ 100 levels down",
		grpcCase(p, "0", nil, []string{nested(100, `2: {"x"}`)}),
		grpcCase(p, "0", nil, []string{nested(100, `3: {"x"}`)}))
}

// TestGRPCShapeOfADeepMessage checks that a message nested 1<<20 deep is read
// without exhausting the stack, its deepest field and the one after it kept.
func TestGRPCShapeOfADeepMessage(t *testing.T) {
	const p = "/users.v1.Users/Get"
	deep := func(leaf, after string) *TestCase {
		return grpcCase(p, "0", nil, []string{strings.Repeat("1: {", 1<<20) + leaf + strings.Repeat("}", 1<<20) + "\n" + after})
	}
	differentShape(t, "the deepest field", deep(`2: {"x"}`, `4: {"y"}`), deep(`3: {"x"}`, `4: {"y"}`))
	differentShape(t, "the field after", deep(`2: {"x"}`, `4: {"y"}`), deep(`2: {"x"}`, `5: {"y"}`))
}
