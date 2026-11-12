package mysql_test

import (
	"bytes"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// A cached answer is served only to a connection of the framing it was
// encoded for: one of another framing gets its own bytes.
func TestWireCacheKeyedByFraming(t *testing.T) {
	var rs mysql.TextResultSet
	c := rs.WireCache()
	plain, ext := []byte("plain"), []byte("ext")
	if got := c.Store(0, plain); !bytes.Equal(got, plain) {
		t.Fatalf("Store returned %q", got)
	}
	if got := c.Load(mysql.FramingExtendedMetadata); got != nil {
		t.Fatalf("Load for another framing served %q", got)
	}
	if got := c.Store(mysql.FramingExtendedMetadata, ext); !bytes.Equal(got, ext) {
		t.Fatalf("Store for another framing returned %q, want its own bytes", got)
	}
	if got := c.Load(0); !bytes.Equal(got, plain) {
		t.Fatalf("Load(0) = %q after a store for another framing, want %q", got, plain)
	}
}
