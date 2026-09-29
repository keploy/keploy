package rowscols

import (
	"context"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A column definition cut short, by a misframed read or by a hole in the
// captured stream, must be refused with an error. DecodeColumn read its
// fixed-length fields unchecked, and the panics ("slice bounds out of range
// [183:182]", "index out of range [1] with length 0") killed the MySQL
// recorder, and with it the recording of the rest of the connection.
func TestDecodeColumn_TruncatedDefinitionIsAnErrorNotAPanic(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	for _, col := range []*mysql.ColumnDefinition41{
		{Catalog: "def", Schema: "test", Table: "sessions", OrgTable: "sessions", Name: "email", OrgName: "email",
			FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 1020, Type: byte(mysql.FieldTypeVarString)},
		{Catalog: "def", Schema: "test", Table: "t", OrgTable: "t", Name: "id", OrgName: "id",
			FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong),
			DefaultValue: "a default value long enough to be cut inside"},
	} {
		full, err := EncodeColumn(ctx, logger, col)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := DecodeColumn(ctx, logger, full); err != nil {
			t.Fatalf("the whole definition of %q does not decode: %v", col.Name, err)
		}
		for cut := 0; cut < len(full); cut++ {
			t.Run(fmt.Sprintf("%s/%d_of_%d_bytes", col.Name, cut, len(full)), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("DecodeColumn panicked: %v", r)
					}
				}()
				// The header still declares the full length: the bytes are
				// missing, not the claim.
				if got, _, err := DecodeColumn(ctx, logger, full[:cut:cut]); err == nil {
					t.Fatalf("decoded a definition cut to %d of %d bytes as %+v", cut, len(full), got)
				}
			})
		}
	}
}

// A COM_FIELD_LIST column's default value survives decoding, however short.
// The length check compared the payload length with a position that counts
// the 4-byte header, so a default of up to 4 wire bytes (such as '0') was
// dropped.
func TestDecodeColumn_ShortDefaultValueIsKept(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	for _, def := range []string{"0", "ab", "a longer default value"} {
		col := &mysql.ColumnDefinition41{Catalog: "def", Schema: "s", Table: "t", OrgTable: "t", Name: "c", OrgName: "c",
			FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 4, Type: byte(mysql.FieldTypeLong), DefaultValue: def}
		b, err := EncodeColumn(ctx, logger, col)
		if err != nil {
			t.Fatal(err)
		}
		got, pos, err := DecodeColumn(ctx, logger, b)
		if err != nil || got.DefaultValue != def || pos != len(b) {
			t.Fatalf("default %q decoded as (%q, pos %d of %d, %v)", def, got.DefaultValue, pos, len(b), err)
		}
	}
}

// A text row cut short is refused, not read past its end.
func TestDecodeTextRow_TruncatedRowIsAnErrorNotAPanic(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	cols := []*mysql.ColumnDefinition41{{Name: "id", Type: byte(mysql.FieldTypeLongLong)}, {Name: "email", Type: byte(mysql.FieldTypeVarString)}}
	full := []byte{0, 0, 0, 3, 2, '4', '2', 0xfc, 3, 0, 'a', '@', 'b'}
	n := len(full) - 4
	full[0], full[1], full[2] = byte(n), byte(n>>8), byte(n>>16)
	if _, _, err := DecodeTextRow(ctx, logger, full, cols); err != nil {
		t.Fatalf("the whole row does not decode: %v", err)
	}
	for cut := 0; cut < len(full); cut++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("cut to %d bytes: DecodeTextRow panicked: %v", cut, r)
				}
			}()
			if row, _, err := DecodeTextRow(ctx, logger, full[:cut:cut], cols); err == nil {
				t.Fatalf("decoded a row cut to %d of %d bytes as %+v", cut, len(full), row.Values)
			}
		}()
	}
}
