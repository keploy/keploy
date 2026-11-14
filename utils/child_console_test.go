package utils

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// brokenWriter fails every write.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }

// TeeWriter writes each write to both writers, a failing one keeping it from
// neither, and is dst itself with no tee.
func TestTeeWriterWritesToBothWriters(t *testing.T) {
	var dst bytes.Buffer
	if w := TeeWriter(&dst, nil); w != io.Writer(&dst) {
		t.Fatalf("TeeWriter(dst, nil) = %T; want dst itself", w)
	}
	var tee bytes.Buffer
	for _, c := range []struct {
		name     string
		dst, tee io.Writer
		got      []*bytes.Buffer
	}{
		{"both writers", &dst, &tee, []*bytes.Buffer{&dst, &tee}},
		{"a failing tee", &dst, brokenWriter{}, []*bytes.Buffer{&dst}},
		{"a failing dst", brokenWriter{}, &tee, []*bytes.Buffer{&tee}},
	} {
		dst.Reset()
		tee.Reset()
		n, err := TeeWriter(c.dst, c.tee).Write([]byte("port is already allocated"))
		if n != 25 || err != nil {
			t.Fatalf("%s: Write = %d, %v; want 25, nil", c.name, n, err)
		}
		for _, b := range c.got {
			if b.String() != "port is already allocated" {
				t.Fatalf("%s: a writer got %q; want the write", c.name, b.String())
			}
		}
	}
}
