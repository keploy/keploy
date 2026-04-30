package models

import (
	"bytes"
	"encoding/gob"
	"strings"
	"testing"
)

// TestGobDecode_NegativeLength_EveryLengthPrefixedTag is a regression
// test for the panic / unbounded-allocation surface where a hand-edited
// or corrupted gob blob carries a negative int32 length-prefix on a
// length-prefixed type. The slice arm (cellTagSlice = 13) was hardened
// in an earlier review pass; this test pins the symmetric guards on
// TSVector / Hstore / Multirange / JSONB so a future revert can't
// silently re-open the panic surface.
//
// Each case writes a single tag byte followed by a gob-encoded int32
// of -1, then asserts that GobDecode returns a typed error containing
// the substring "invalid negative length" rather than panicking via
// runtime.makeslice / make().
func TestGobDecode_NegativeLength_EveryLengthPrefixedTag(t *testing.T) {
	cases := []struct {
		name string
		tag  byte
	}{
		{"slice", cellTagSlice},
		{"jsonb_map", cellTagJSONB},
		{"tsvector", cellTagTSVector},
		{"hstore", cellTagHstore},
		{"multirange", cellTagMultirange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			buf.WriteByte(tc.tag)
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(int32(-1)); err != nil {
				t.Fatalf("encode negative length: %v", err)
			}
			var c PostgresV3Cell
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("GobDecode panicked instead of returning typed error: %v", r)
				}
			}()
			err := c.GobDecode(buf.Bytes())
			if err == nil {
				t.Fatalf("expected error for negative length on tag %d, got nil", tc.tag)
			}
			if !strings.Contains(err.Error(), "invalid negative length") {
				t.Fatalf("error message does not flag the bounds violation: %v", err)
			}
		})
	}
}

// TestGobDecode_TSVector_NegativePositionCount pins the inner
// length-prefix on TSVector positions (the per-lexeme `pn` field).
// The outer length is 1 (one lexeme), the lexeme word decodes
// cleanly, and the inner position count is -1 — must be rejected
// without panicking through make([]TSVectorPosition, -1).
func TestGobDecode_TSVector_NegativePositionCount(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(cellTagTSVector)
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(int32(1)); err != nil {
		t.Fatalf("encode lexeme count: %v", err)
	}
	if err := enc.Encode("word"); err != nil {
		t.Fatalf("encode word: %v", err)
	}
	if err := enc.Encode(int32(-1)); err != nil {
		t.Fatalf("encode negative position count: %v", err)
	}
	var c PostgresV3Cell
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GobDecode panicked instead of returning typed error: %v", r)
		}
	}()
	err := c.GobDecode(buf.Bytes())
	if err == nil {
		t.Fatalf("expected error for negative position count, got nil")
	}
	if !strings.Contains(err.Error(), "invalid negative length") {
		t.Fatalf("error message does not flag the bounds violation: %v", err)
	}
}
