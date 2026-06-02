package rowscols

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestEncodeTextRow_FewerValuesThanColumns guards the bounds-check fix at
// textRowPacket.go:60. Before the fix, a TextRow whose Values slice was
// shorter than the column list panicked the agent goroutine with
// "index out of range" inside EncodeTextRow, which the upstream
// recovery handler logged but which still closed the active MySQL
// connection. The MySQL fuzzer reproduced this against the
// "post-run-1" fixture (10 columns, 0 values).
func TestEncodeTextRow_FewerValuesThanColumns(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	columns := make([]*mysql.ColumnDefinition41, 10)
	for i := range columns {
		columns[i] = &mysql.ColumnDefinition41{
			Type: byte(mysql.FieldTypeVarString),
			Name: "c",
		}
	}

	tests := []struct {
		name string
		row  *mysql.TextRow
	}{
		{
			name: "row with no values",
			row: &mysql.TextRow{
				Header: mysql.Header{SequenceID: 1},
			},
		},
		{
			name: "row with fewer values than columns",
			row: &mysql.TextRow{
				Header: mysql.Header{SequenceID: 1},
				Values: []mysql.ColumnEntry{
					{Type: mysql.FieldTypeVarString, Name: "c", Value: "a"},
					{Type: mysql.FieldTypeVarString, Name: "c", Value: "b"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := EncodeTextRow(ctx, logger, tt.row, columns)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// header (4 bytes) + body. Each missing column is encoded as
			// NULL (0xfb), 1 byte. Each present string value uses
			// length-encoded form (1-byte length + payload).
			if len(out) < 4+len(columns) {
				t.Fatalf("encoded packet too short: got %d bytes, want >= %d", len(out), 4+len(columns))
			}
			// Trailing bytes for the missing columns must all be NULL markers.
			body := out[4:]
			for i := len(tt.row.Values); i < len(columns); i++ {
				idx := len(body) - (len(columns) - i)
				if idx < 0 || idx >= len(body) {
					t.Fatalf("body too short to inspect NULL marker at column %d", i)
				}
				if body[idx] != 0xfb {
					t.Fatalf("expected NULL marker (0xfb) at body offset %d for column %d, got 0x%x", idx, i, body[idx])
				}
			}
		})
	}
}
