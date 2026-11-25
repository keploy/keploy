package grpc

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

// These pin the default (non-decoded) gRPC body comparison, the one every
// replay uses when no proto files are given: in-cluster auto-replay always.
// The body there is protoscope text (field numbers, guessed types), compared
// after canonicalization.

// A noisy field is named by its field-number path on the protoscope body
// (body: ["8.2.1"] is field 1 of field 2 of field 8). Without it a gRPC
// response carrying a timestamp or a generated id can never be denoised on
// this path: noise used to be applied on the JSON path only, and a replay
// with no protos never takes that path.
func TestMatch_ProtoscopeBodyNoiseByFieldNumber(t *testing.T) {
	const recorded = "1: {\"order-1\"}\n8: {\n  2: {\n    1: 1795582799\n    2: 999999000\n  }\n  3: {\"Pending\"}\n}"

	t.Run("a listed field-number path is ignored", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"8.2.1"}})
		actual := grpcActual(strings.Replace(recorded, "1795582799", "1795590000", 1))
		_, res := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Fatalf("field 8.2.1 is noise, so only a noisy field differs; got a body mismatch")
		}
	})

	t.Run("a field outside the noisy path is still compared", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"8.2.1"}})
		actual := grpcActual(strings.Replace(recorded, "Pending", "Shipped", 1))
		_, res := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false)
		if decodedDataNormal(res) {
			t.Fatalf("field 8.3 is not noise and differs; got a match")
		}
	})

	t.Run("a noisy path covers every occurrence of a repeated field", func(t *testing.T) {
		rec := "4: {\n  1: 100\n  2: {\"a\"}\n}\n4: {\n  1: 200\n  2: {\"b\"}\n}"
		act := "4: {\n  1: 101\n  2: {\"a\"}\n}\n4: {\n  1: 201\n  2: {\"b\"}\n}"
		tc := grpcTestCase(rec, map[string][]string{"body": {"4.1"}})
		_, res := Match(tc, grpcActual(act), map[string]map[string][]string{}, true, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Fatalf("4.1 is noise in every element of repeated field 4; got a body mismatch")
		}
	})

	t.Run("global noise applies too", func(t *testing.T) {
		tc := grpcTestCase(recorded, nil)
		actual := grpcActual(strings.Replace(recorded, "1795582799", "1795590000", 1))
		global := map[string]map[string][]string{"body": {"8.2.1": {}}}
		_, res := Match(tc, actual, global, true, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Fatalf("8.2.1 is global noise; got a body mismatch")
		}
	})
}

// ignoreOrdering is the user's setting for every response body. The
// protoscope path read only the proven switch, so a user who asked for list
// order to matter (ignoreOrdering: false) had it ignored for gRPC without
// protos, while HTTP and decoded gRPC honoured it.
func TestMatch_ProtoscopeHonoursIgnoreOrdering(t *testing.T) {
	const rec = "3: {\"first\"}\n3: {\"second\"}"
	const act = "3: {\"second\"}\n3: {\"first\"}"

	t.Run("ignoreOrdering false: a reordered repeated field differs", func(t *testing.T) {
		_, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, false, zap.NewNop(), false)
		if decodedDataNormal(res) {
			t.Fatalf("ignoreOrdering is false and repeated field 3 came back reordered; got a match")
		}
	})

	t.Run("ignoreOrdering true: a reordered repeated field matches", func(t *testing.T) {
		_, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, true, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Fatalf("ignoreOrdering is true; got a mismatch for a reorder alone")
		}
	})

	t.Run("different fields in a different order always match", func(t *testing.T) {
		_, res := Match(grpcTestCase("1: 5\n2: {\"x\"}", nil), grpcActual("2: {\"x\"}\n1: 5"), map[string]map[string][]string{}, false, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Fatalf("protobuf gives the order of different fields no meaning; got a mismatch")
		}
	})
}

// A body over the old 1 MiB canonicalization cap was compared as whitespace-
// normalized text, so field order suddenly mattered: the same message with
// its fields in another order failed only because it was large.
func TestMatch_ProtoscopeLargeBodyKeepsTheSameRules(t *testing.T) {
	big := strings.Repeat("x", 1<<20+16)
	rec := "1: {\"" + big + "\"}\n2: 5"
	act := "2: 5\n1: {\"" + big + "\"}"
	_, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, true, zap.NewNop(), false)
	if !decodedDataNormal(res) {
		t.Fatalf("a body over 1 MiB with its fields reordered must compare like a small one; got a mismatch")
	}
}

// A pattern is matched against the value as written in a pattern: a long
// string protoscope splits over quoted pieces is joined, a float's comment and
// width suffix are dropped.
func TestMatch_ProtoscopeNoisePatternsReadTheValue(t *testing.T) {
	long := func(id string) string {
		return "1: {\n  \"" + id[:60] + "\"\n  \"" + id[60:] + "\"\n}\n2: {\"keep\"}"
	}
	idA := "id-" + strings.Repeat("a", 140)
	idB := "id-" + strings.Repeat("b", 140)

	cases := []struct {
		name, rec, act string
		noise          map[string][]string
		match          bool
	}{
		{"a split long string matches its whole value", long(idA), long(idB), map[string][]string{"body.1": {"^id-"}}, true},
		{"a split long string not matching stays compared", long(idA), long("xx" + idB[2:]), map[string][]string{"body.1": {"^id-"}}, false},
		{"a float with a comment and suffix", "3: 0.5i32   # 0x3f000000i32\n2: {\"keep\"}", "3: 0.75i32   # 0x3f400000i32\n2: {\"keep\"}", map[string][]string{"body.3": {`^0\.\d+$`}}, true},
		{"a repeated element whose value matches", "4: {\n  1: {\"tok-1\"}\n}\n4: {\n  1: {\"tok-2\"}\n}", "4: {\n  1: {\"tok-9\"}\n}\n4: {\n  1: {\"tok-8\"}\n}", map[string][]string{"body.4.1": {"^tok-"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := grpcTestCase(c.rec, nil)
			global := map[string]map[string][]string{"body": c.noise}
			_, res := Match(tc, grpcActual(c.act), global, true, zap.NewNop(), false)
			if got := decodedDataNormal(res); got != c.match {
				t.Fatalf("match = %v, want %v", got, c.match)
			}
		})
	}
}

// A noise entry whose pattern does not compile ignores nothing: a typo must
// never widen noise to "always".
func TestMatch_ProtoscopeInvalidNoisePatternIgnoresNothing(t *testing.T) {
	global := map[string]map[string][]string{"body": {"body.1": {"[unclosed"}}}
	_, res := Match(grpcTestCase("1: {\"a\"}", nil), grpcActual("1: {\"b\"}"), global, true, zap.NewNop(), false)
	if decodedDataNormal(res) {
		t.Fatal("field 1 changed and its only noise pattern is invalid; it was ignored")
	}
}

// With order kept, a field the schema names as a map compares its entries in
// any order; another repeated field keeps its order.
func TestMatch_ProtoscopeMapEntriesHaveNoOrder(t *testing.T) {
	rec := "5: {\n  1: {\"k1\"}\n  2: 1\n}\n5: {\n  1: {\"k2\"}\n  2: 2\n}"
	act := "5: {\n  1: {\"k2\"}\n  2: 2\n}\n5: {\n  1: {\"k1\"}\n  2: 1\n}"
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, false, zap.NewNop(), false, WithUnorderedFields([]string{"5"})); !decodedDataNormal(res) {
		t.Fatal("map field 5's entries came back in another order; it failed")
	}
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, false, zap.NewNop(), false); decodedDataNormal(res) {
		t.Fatal("without the schema's word that field 5 is a map, its reorder must fail with order kept")
	}
}

// A set built once reads the same as the list it was built from.
func TestMatch_ProtoscopeUnorderedFieldSet(t *testing.T) {
	rec := "5: {\n  1: {\"k1\"}\n  2: 1\n}\n5: {\n  1: {\"k2\"}\n  2: 2\n}"
	act := "5: {\n  1: {\"k2\"}\n  2: 2\n}\n5: {\n  1: {\"k1\"}\n  2: 1\n}"
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, false, zap.NewNop(), false, WithUnorderedFieldSet(map[string]bool{"5": true})); !decodedDataNormal(res) {
		t.Fatal("map field 5's entries came back in another order; it failed")
	}
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(act), map[string]map[string][]string{}, false, zap.NewNop(), false, WithUnorderedFieldSet(map[string]bool{"6": true})); decodedDataNormal(res) {
		t.Fatal("field 5 is not in the set; its reorder must fail with order kept")
	}
}

// A body difference read through a schema is graded as a JSON one: a changed
// value is not a broken schema.
func TestMatch_ProtoscopeDifferenceGradedThroughTheDecoder(t *testing.T) {
	decode := func(e, a string) (string, string, bool) {
		return `{"status":"Pending"}`, `{"status":"Shipped"}`, true
	}
	_, withDecoder := Match(grpcTestCase("3: {\"Pending\"}", nil), grpcActual("3: {\"Shipped\"}"), map[string]map[string][]string{}, true, zap.NewNop(), false, WithAssessmentDecoder(decode))
	_, without := Match(grpcTestCase("3: {\"Pending\"}", nil), grpcActual("3: {\"Shipped\"}"), map[string]map[string][]string{}, true, zap.NewNop(), false)
	if withDecoder.FailureInfo.Risk == without.FailureInfo.Risk {
		t.Fatalf("grading did not use the decoded bodies: %v and %v", withDecoder.FailureInfo.Risk, without.FailureInfo.Risk)
	}
	if decodedDataNormal(withDecoder) {
		t.Fatal("the decoder must not change the verdict")
	}
}

// A nested map path ("2.4") is sorted, while the same field number at the top
// level keeps its order.
func TestMatch_ProtoscopeUnorderedPathsAreNested(t *testing.T) {
	rec := "2: {\n  4: {\"a\"}\n  4: {\"b\"}\n}\n4: {\"x\"}\n4: {\"y\"}"
	nestedSwapped := "2: {\n  4: {\"b\"}\n  4: {\"a\"}\n}\n4: {\"x\"}\n4: {\"y\"}"
	topSwapped := "2: {\n  4: {\"a\"}\n  4: {\"b\"}\n}\n4: {\"y\"}\n4: {\"x\"}"
	opt := WithUnorderedFields([]string{"2.4"})
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(nestedSwapped), map[string]map[string][]string{}, false, zap.NewNop(), false, opt); !decodedDataNormal(res) {
		t.Fatal("map 2.4's entries reordered; it failed")
	}
	if _, res := Match(grpcTestCase(rec, nil), grpcActual(topSwapped), map[string]map[string][]string{}, false, zap.NewNop(), false, opt); decodedDataNormal(res) {
		t.Fatal("top-level field 4 is not the map 2.4; its reorder must fail with order kept")
	}
}

// With a decoder, a reorder alone keeps its own reason, not a value change.
func TestMatch_ProtoscopeReorderKeepsItsReasonWithADecoder(t *testing.T) {
	decode := func(e, a string) (string, string, bool) { return `{"v":["a","b"]}`, `{"v":["b","a"]}`, true }
	_, res := Match(grpcTestCase("3: {\"a\"}\n3: {\"b\"}", nil), grpcActual("3: {\"b\"}\n3: {\"a\"}"), map[string]map[string][]string{}, false, zap.NewNop(), false, WithAssessmentDecoder(decode))
	if a := res.FailureInfo.Assessment; a == nil || len(a.Reasons) == 0 || !strings.Contains(a.Reasons[0], "another order") {
		t.Fatalf("a reorder must say so; got %+v", res.FailureInfo.Assessment)
	}
}
