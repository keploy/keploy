package models

import (
	"reflect"
	"testing"
)

type storeStub struct {
	reads   bool
	written int
}

func (s storeStub) Reads(*HTTPReq) bool { return s.reads }

func (s storeStub) Access(*Mock, []*Mock) HTTPStoreAccess {
	return HTTPStoreAccess{Written: make([]map[string]string, s.written), Read: map[string]string{"body.Item.Name.S": "Leads/c-1"}}
}

// No reader reads nothing; a registered reader is asked, and one Written
// entry per call or none is taken; a second reader, or a nil one, panics; the
// returned function removes the reader.
func TestHTTPStoreReader(t *testing.T) {
	read := &Mock{Spec: MockSpec{HTTPReq: &HTTPReq{}}}
	calls := []*Mock{{Name: "mock-0"}}
	if HTTPStoreReads(read.Spec.HTTPReq) {
		t.Fatal("no reader reads a call")
	}
	if _, ok := HTTPStoreAccessOf(read, calls); ok {
		t.Fatal("no reader states an access")
	}

	unregister := RegisterHTTPStore(storeStub{reads: true, written: 1})
	if !HTTPStoreReads(read.Spec.HTTPReq) || HTTPStoreReads(nil) {
		t.Fatal("the reader reads the read, and nothing reads a nil request")
	}
	a, ok := HTTPStoreAccessOf(read, calls)
	if !ok || len(a.Written) != 1 || !reflect.DeepEqual(a.Read, map[string]string{"body.Item.Name.S": "Leads/c-1"}) {
		t.Fatalf("access %+v %v, want the reader's", a, ok)
	}
	if _, ok := HTTPStoreAccessOf(&Mock{}, calls); ok {
		t.Fatal("a mock with no HTTP request is no read")
	}
	if _, ok := HTTPStoreAccessOf(nil, calls); ok {
		t.Fatal("a nil mock is no read")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a second reader registered")
			}
		}()
		RegisterHTTPStore(storeStub{})
	}()
	unregister()

	for _, s := range []storeStub{{reads: false, written: 1}, {reads: true, written: 0}, {reads: true, written: 2}} {
		unregister := RegisterHTTPStore(s)
		if _, ok := HTTPStoreAccessOf(read, calls); ok {
			t.Fatalf("reader %+v: an access taken from a reader that does not read the call, or marks other than one entry per call", s)
		}
		unregister()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a nil reader registered")
			}
		}()
		RegisterHTTPStore(nil)
	}()
}
