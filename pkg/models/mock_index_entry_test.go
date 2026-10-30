package models

import (
	"context"
	"testing"
)

// An index entry (IndexEntryFrom) is the mock as the read left it, unpooled,
// whose payload is read back from the source it is given, and shares nothing a
// replay changes in place with the mock it was copied from: its metadata, its
// async block and its origin are its own. A mock whose payload is in memory
// has no entry.
func TestIndexEntryFromSharesNothingAReplayChanges(t *testing.T) {
	recorded := payloadMocks()
	first := &fileSource{recorded: recorded}
	m := recorded["http"]()
	m.Spec.Async = &AsyncMeta{Lane: "lane-a", Seq: 3}
	m.Origin = &Origin{Clock: "proxy:run-1", Comm: "app"}
	m.DeriveLifetime()
	m.LeavePayloadOnDisk(first, PayloadAt{Name: "http"})
	m.MarkPooled()

	other := &fileSource{recorded: recorded}
	c := m.IndexEntryFrom(other)
	if c == nil || c == m {
		t.Fatalf("IndexEntryFrom gave %p for %p", c, m)
	}
	if c.Pooled() {
		t.Fatal("the entry is pooled: the mock it was copied from was")
	}
	if c.Name != m.Name || c.Kind != m.Kind || c.ConnectionID != m.ConnectionID || c.TestModeInfo != m.TestModeInfo || !c.PayloadOnDisk() {
		t.Fatalf("the entry is %+v; the mock %+v", c, m)
	}
	c.Spec.Metadata["type"] = "changed"
	c.Spec.Async.Lane = "changed"
	c.Spec.Async.Seq = 99
	c.Origin.Comm = "changed"
	if m.Spec.Metadata["type"] != "mocks" || m.Spec.Async.Lane != "lane-a" || m.Spec.Async.Seq != 3 || m.Origin.Comm != "app" {
		t.Fatalf("a change to the entry reached the mock it was copied from: metadata %v, async %+v, origin %+v", m.Spec.Metadata, *m.Spec.Async, *m.Origin)
	}

	if _, err := c.WithPayload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.reads != 0 || other.reads != 1 {
		t.Fatalf("the entry's payload was read back from the first source %d times and from its own %d; want its own once", first.reads, other.reads)
	}
	if _, err := m.WithPayload(context.Background()); err != nil || first.reads != 1 {
		t.Fatalf("the mock's own payload is no longer read back from its source: %v, reads %d", err, first.reads)
	}

	if e := recorded["dns"]().IndexEntryFrom(other); e != nil {
		t.Fatal("a mock whose payload is in memory has an index entry")
	}
}
