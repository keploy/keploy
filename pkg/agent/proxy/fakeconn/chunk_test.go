package fakeconn

import (
	"testing"
	"time"
)

func TestDirectionString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    Direction
		want string
	}{
		{FromClient, "client"},
		{FromDest, "dest"},
		{Direction(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("Direction(%d).String() = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestChunkIsZero(t *testing.T) {
	t.Parallel()
	var c Chunk
	if !c.IsZero() {
		t.Errorf("zero Chunk reported non-zero")
	}
	c.Bytes = []byte("x")
	if c.IsZero() {
		t.Errorf("Chunk with bytes reported zero")
	}
	c = Chunk{ReadAt: time.Now()}
	if c.IsZero() {
		t.Errorf("Chunk with ReadAt reported zero")
	}
}
