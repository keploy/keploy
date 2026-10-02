package wire

import (
	"testing"
)

func TestClientDeprecateEOFValue(t *testing.T) {
	// 1. Verify the Constant Value
	// The correct value for CLIENT_DEPRECATE_EOF is 0x00200000 (Bit 21)
	expected := uint32(0x00200000)

	if CLIENT_DEPRECATE_EOF != expected {
		t.Errorf("CLIENT_DEPRECATE_EOF is wrong! Expected 0x%x, got 0x%x", expected, CLIENT_DEPRECATE_EOF)
	}
}

func TestDeprecateEOFDetection(t *testing.T) {
	// 2. Verify detection when both sides support it
	ctx := &DecodeContext{
		ServerCaps: CLIENT_DEPRECATE_EOF,
		ClientCaps: CLIENT_DEPRECATE_EOF,
	}

	if !ctx.DeprecateEOF() {
		t.Error("DeprecateEOF() returned false, but both sides support the flag.")
	}

	// 3. Verify detection when only one side supports it (Should be False)
	ctxMixed := &DecodeContext{
		ServerCaps: CLIENT_DEPRECATE_EOF,
		ClientCaps: 0,
	}

	if ctxMixed.DeprecateEOF() {
		t.Error("DeprecateEOF() returned true, but Client does not support it.")
	}
}
