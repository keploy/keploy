package models

import (
	"testing"
	"unsafe"
)

// The transient fields a Mock carries from capture to record sit in padding:
// a Mock stays in the 640 B size class, so each one captured and held costs
// no more for them.
func TestAMockStaysInThe640ByteSizeClass(t *testing.T) {
	if n := unsafe.Sizeof(Mock{}); n > 640 {
		t.Fatalf("a Mock is %d B, past the 640 B size class: each one takes 704 B", n)
	}
}
