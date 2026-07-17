package rowscols

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestDecodeColumn_ShortFixedBlock_NoPanic feeds a buffer that parses through
// the six length-encoded string fields but is truncated before the 13-byte
// fixed-field block. Before the bounds guard this indexed past the slice and
// panicked, crashing the async decoder goroutine and tearing down the whole
// connection's recording. It must now return an error and never panic.
func TestDecodeColumn_ShortFixedBlock_NoPanic(t *testing.T) {
	// 4-byte header + six empty length-encoded strings (0x00 each) = 10 bytes,
	// shorter than the 13-byte fixed-field block that follows (needs pos+13).
	b := []byte{
		0x06, 0x00, 0x00, 0x01, // header: payload_length=6, seq=1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // catalog/schema/table/org_table/name/org_name (all empty)
	}
	require.NotPanics(t, func() {
		col, _, err := DecodeColumn(context.Background(), zap.NewNop(), b)
		assert.Error(t, err, "a truncated fixed-field block must return an error")
		assert.Nil(t, col, "no partial column definition should be returned on error")
	})
}

// TestDecodeColumn_ShortFieldListDefault_NoPanic exercises the COM_FIELD_LIST
// branch: a packet whose PayloadLength claims more data after the fixed block,
// but whose buffer ends exactly at the fixed block so the length-encoded
// default-value prefix is missing. This path was unguarded and could index past
// the slice; it must now return an error and never panic.
func TestDecodeColumn_ShortFieldListDefault_NoPanic(t *testing.T) {
	b := []byte{
		0x1e, 0x00, 0x00, 0x01, // header: payload_length=30 (> pos), seq=1 → triggers field-list branch
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // six empty length-encoded strings
		0x0c,       // length of fixed-length fields
		0x21, 0x00, // character_set
		0x00, 0x00, 0x00, 0x00, // column_length
		0xfd,       // type
		0x00, 0x00, // flags
		0x00,       // decimals
		0x00, 0x00, // filler
		// buffer ends here: no default-value prefix, though PayloadLength claims one
	}
	require.NotPanics(t, func() {
		col, _, err := DecodeColumn(context.Background(), zap.NewNop(), b)
		assert.Error(t, err, "a field-list packet missing its default-value prefix must return an error")
		assert.Nil(t, col)
	})
}

// TestDecodeColumn_Valid confirms a well-formed column definition still decodes
// cleanly with the guards in place (no false positives).
func TestDecodeColumn_Valid(t *testing.T) {
	b := []byte{
		0x17, 0x00, 0x00, 0x01, // header: payload_length=23, seq=1 (== pos after fixed block → no field-list branch)
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // six empty length-encoded strings
		0x0c,       // length of fixed-length fields
		0x21, 0x00, // character_set = 33
		0x00, 0x01, 0x00, 0x00, // column_length = 256
		0xfd,       // type = MYSQL_TYPE_VAR_STRING
		0x00, 0x00, // flags
		0x00,       // decimals
		0x00, 0x00, // filler
	}
	col, _, err := DecodeColumn(context.Background(), zap.NewNop(), b)
	require.NoError(t, err)
	require.NotNil(t, col)
	assert.Equal(t, uint16(33), col.CharacterSet)
	assert.Equal(t, uint32(256), col.ColumnLength)
	assert.Equal(t, byte(0xfd), col.Type)
}
