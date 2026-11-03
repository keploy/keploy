package models

import (
	"sync/atomic"
)

// An HTTP API can be a store: an application writes a record with one call
// and reads it back with a later one (DynamoDB's JSON API: PutItem and
// UpdateItem write an item, GetItem, Query and Scan read it). keploy answers
// the read with its recorded answer, which holds what the recorded run wrote.
// Where this run wrote other values (a time, an id the application made),
// the learn step answers the read with them instead, as it does for a Mongo
// read: a reader registered here (enterprise's DynamoDB reader) states which
// values each call stored into which record, and which leaves of a read's
// answer such a record holds. keploy links a read to a write only where the
// reader states the link, and never guesses it from the values.
//
// With no reader registered, no call is read so, and every read is answered
// as recorded.

// HTTPStoreAccess is what a registered reader found in a test's calls and in
// the read that follows them. Leaves are named by the learn step's leaf
// paths ("body.Item.Name.S", "body.Items[0].Name.S"), records by the
// reader's names for them (a table and an item's key).
type HTTPStoreAccess struct {
	// Written holds, for each call the reader was given (by its index), the
	// leaves of its request whose value the call stored into a record: leaf
	// path to the record's name. A call that stored nothing the reader can
	// state holds none.
	Written []map[string]string
	// Read holds the leaves of the read's recorded answer that hold a value
	// of a record only the calls given can have written since the record's
	// value is known (written whole in the test, or by an attribute a call
	// given set): leaf path to the record's name. A leaf it does not hold is
	// answered as recorded.
	Read map[string]string
	// Unread says why leaves of the read's answer that a record holds are
	// answered as recorded though a call given wrote the record (a write
	// outside what the reader reads): leaf path to the reason, for a person.
	Unread map[string]string
}

// HTTPStoreReader reads an HTTP API's calls as a store's writes and reads.
type HTTPStoreReader interface {
	// Reads reports whether r reads records back (a call Access may name
	// leaves of): asked before the test's calls are read, so a call that
	// reads none costs nothing more.
	Reads(r *HTTPReq) bool
	// Follows reports, from the two requests alone, whether Access, asked of
	// read, may read call as a write or a stop: asked of each call served
	// earlier in the test before its recorded answer is loaded, so a call it
	// does not follow (another API, another table) is never loaded for the
	// read and is not given to Access. It must follow every call Access may
	// mark or stop on; following one more only costs a load.
	Follows(read, call *HTTPReq) bool
	// Access reads calls, the HTTP calls served earlier in the test that
	// Follows takes for read, in the order they were served, each with its
	// recorded request and answer, then read, a call Reads takes, with its
	// recorded answer. A call it does not read is no write and no read.
	Access(read *Mock, calls []*Mock) HTTPStoreAccess
}

// httpStoreReader is the registered reader, nil when there is none.
var httpStoreReader atomic.Pointer[HTTPStoreReader]

// RegisterHTTPStore registers r as the reader of the records HTTP calls
// write and read back. Call it from an init function. There is one reader:
// a nil one, or a second, panics. The returned function removes it (tests).
func RegisterHTTPStore(r HTTPStoreReader) (unregister func()) {
	if r == nil {
		panic("models: RegisterHTTPStore with a nil reader")
	}
	p := &r
	if !httpStoreReader.CompareAndSwap(nil, p) {
		panic("models: a second HTTP store reader registered")
	}
	return func() { httpStoreReader.CompareAndSwap(p, nil) }
}

// HTTPStoreReads reports whether a reader is registered and reads r as a
// call that reads records back (HTTPStoreReader.Reads).
func HTTPStoreReads(r *HTTPReq) bool {
	p := httpStoreReader.Load()
	return p != nil && r != nil && (*p).Reads(r)
}

// HTTPStoreFollows reports whether a reader is registered and follows call
// for a read of read (HTTPStoreReader.Follows).
func HTTPStoreFollows(read, call *HTTPReq) bool {
	p := httpStoreReader.Load()
	return p != nil && read != nil && call != nil && (*p).Follows(read, call)
}

// HTTPStoreAccessOf is what the registered reader reads in calls and read
// (HTTPStoreReader.Access); false when no reader is registered, read has no
// HTTP request, the reader does not read it as a read, or it gives other
// than one Written entry per call (which call a mark is of is then unknown,
// and nothing is read back).
func HTTPStoreAccessOf(read *Mock, calls []*Mock) (HTTPStoreAccess, bool) {
	p := httpStoreReader.Load()
	if p == nil || read == nil || read.Spec.HTTPReq == nil || !(*p).Reads(read.Spec.HTTPReq) {
		return HTTPStoreAccess{}, false
	}
	a := (*p).Access(read, calls)
	if len(a.Written) != len(calls) {
		return HTTPStoreAccess{}, false
	}
	return a, true
}
