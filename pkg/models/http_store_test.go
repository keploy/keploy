package models

import (
	"reflect"
	"testing"
)

type storeStub struct {
	reads   bool
	follows bool
	written int
}

func (s storeStub) Reads(*HTTPReq) bool { return s.reads }

func (s storeStub) Follows(_, _ *HTTPReq) bool { return s.follows }

func (s storeStub) Access(*Mock, []*Mock) HTTPStoreAccess {
	return HTTPStoreAccess{Written: make([]map[string]string, s.written), Read: map[string]string{"body.Item.Name.S": "Leads/c-1"}}
}

// accessOf is HTTPStoreAccessOf, failing the test where it panics: a read the
// hook cannot use, or no reader, is a call it answers with no access, never a
// crash of the learn step.
func accessOf(t *testing.T, read *Mock, calls []*Mock) (a HTTPStoreAccess, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HTTPStoreAccessOf(%+v) panicked: %v", read, r)
		}
	}()
	return HTTPStoreAccessOf(read, calls)
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
	if _, ok := accessOf(t, read, calls); ok {
		t.Fatal("no reader states an access")
	}
	if HTTPStoreFollows(read.Spec.HTTPReq, &HTTPReq{}) {
		t.Fatal("no reader follows a call")
	}

	unregister := RegisterHTTPStore(storeStub{reads: true, written: 1})
	if !HTTPStoreReads(read.Spec.HTTPReq) || HTTPStoreReads(nil) {
		t.Fatal("the reader reads the read, and nothing reads a nil request")
	}
	a, ok := HTTPStoreAccessOf(read, calls)
	if !ok || len(a.Written) != 1 || !reflect.DeepEqual(a.Read, map[string]string{"body.Item.Name.S": "Leads/c-1"}) {
		t.Fatalf("access %+v %v, want the reader's", a, ok)
	}
	if _, ok := accessOf(t, &Mock{}, calls); ok {
		t.Fatal("a mock with no HTTP request is no read")
	}
	if _, ok := accessOf(t, nil, calls); ok {
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

	// The reader decides which calls it follows; a nil request on either
	// side is followed by none, and the reader is not asked.
	for _, follows := range []bool{true, false} {
		unregister := RegisterHTTPStore(storeStub{follows: follows})
		if got := HTTPStoreFollows(&HTTPReq{}, &HTTPReq{}); got != follows {
			t.Fatalf("a reader that follows %v: HTTPStoreFollows = %v", follows, got)
		}
		unregister()
	}
	unregister = RegisterHTTPStore(storeStub{follows: true})
	if HTTPStoreFollows(nil, &HTTPReq{}) || HTTPStoreFollows(&HTTPReq{}, nil) {
		t.Fatal("a nil read or call is followed")
	}
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
