package util

import (
	"bytes"
	"io"
	"testing"
)

func TestPrefixReaderDropsPrefixAfterConsumption(t *testing.T) {
	r := NewPrefixReader([]byte("prefix"), bytes.NewBufferString("suffix"))

	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read from PrefixReader: %v", err)
	}

	if got, want := string(buf), "prefixsuffix"; got != want {
		t.Fatalf("unexpected data read. got %q want %q", got, want)
	}

	if r.prefix != nil {
		t.Fatal("expected prefix backing slice to be released after read")
	}
}
