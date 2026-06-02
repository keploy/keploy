package rowscols

import (
	"bytes"
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestEncodeBinaryRow_BlobBase64LookalikeText is a regression test for the
// bug where a BLOB / length-encoded string value that happens to be valid
// base64 (e.g. "high") was silently base64-DECODED into garbage bytes
// ("high" -> 0x86 0x28 0x21 -> "�(!") instead of being written as its
// raw bytes. The recorded value is correct on disk; the corruption was
// purely in the replay-side encoder's "try base64, else raw" heuristic.
//
// See EncodeBinaryRow: BLOB-column string values must be written verbatim;
// genuine binary round-trips through YAML !!binary as []byte and is handled
// by the []byte branch, so the string branch is always plain text.
func TestEncodeBinaryRow_BlobBase64LookalikeText(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	cases := []struct {
		name  string
		value string
	}{
		{name: "valid-base64 plaintext (the original bug)", value: "high"},
		{name: "another base64-lookalike", value: "data"},
		{name: "ordinary text", value: "low"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			columns := []*mysql.ColumnDefinition41{
				{Name: "priority", Type: byte(mysql.FieldTypeBLOB)},
			}
			row := &mysql.BinaryRow{
				Header: mysql.Header{SequenceID: 1},
				Values: []mysql.ColumnEntry{
					{Type: mysql.FieldTypeBLOB, Name: "priority", Value: tc.value},
				},
				RowNullBuffer: []byte{0x00}, // 1 column, none null
			}

			got, err := EncodeBinaryRow(ctx, logger, row, columns)
			if err != nil {
				t.Fatalf("EncodeBinaryRow error: %v", err)
			}

			// The raw value must appear as a length-encoded string:
			// <len><bytes>. For these short values len fits in one byte.
			wantLenEnc := append([]byte{byte(len(tc.value))}, []byte(tc.value)...)
			if !bytes.Contains(got, wantLenEnc) {
				t.Fatalf("encoded row missing raw length-encoded value %q.\n got: % x", tc.value, got)
			}

			// And it must NOT have been base64-decoded. For "high" the bad
			// path produced 0x86 0x28 0x21 — assert that garbage is absent.
			if tc.value == "high" {
				bad := []byte{0x86, 0x28, 0x21}
				if bytes.Contains(got, bad) {
					t.Fatalf("regression: %q was base64-decoded to garbage % x in % x", tc.value, bad, got)
				}
			}
		})
	}
}
