package testdb

import (
	"io"
	"os"
	"testing"
)

// Flatten writes nothing to the process's stdout, whatever it is given: an
// embedded Replayer's stdout may be a log (a pod's), and the value Flatten
// is given is the application's. A value of a kind JSON does not decode to
// (an int, from another decoder) adds no key, as matcher.Flatten's does.
func TestFlattenWritesNothingToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = w
	got := Flatten(map[string]interface{}{"a": 42, "b": nil, "c": []interface{}{int64(7)}})
	os.Stdout = was
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if len(out) != 0 {
		t.Fatalf("Flatten wrote %q to stdout", out)
	}
	if _, ok := got["a"]; ok {
		t.Fatalf("Flatten = %v: an int added a key", got)
	}
	if v := got["b"]; len(v) != 1 || v[0] != "" {
		t.Fatalf("Flatten = %v: want a null as an empty value", got)
	}
}
