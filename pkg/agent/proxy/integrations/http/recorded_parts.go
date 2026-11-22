package http

import (
	"fmt"
	"net/url"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// parsedRecordings holds each recording's parts (parseRecordedParts), by
// the recording's address, once parsed: the learn step reads every
// recording of the scope on every call, and parsing each again (its URL,
// query, header map, body copy, media type and destination) was most of a
// call's cost, growing with the set. An entry is the recording's only while
// its weak pointer still names it (an address the collector gave to another
// mock names a new one), and goes when the recording does (a cleanup on the
// mock: the store let it go, and no call holds it). A hit takes no lock: a
// sync.Map read and the weak pointers' loads.
//
// An entry reaches nothing of its recording's that grows with it: not its
// recorded request, nor the request's body (recordedParts gives the body
// from the recording at each read). The cleanup that drops an entry runs on
// the runtime's cleanup goroutines after the collection that freed the
// recording, and what the entry reaches is freed only by a collection after
// that: an entry that held the request held its body that long past the
// recording. A test's closed recordings stayed in the agent's heap beside the
// next test's whenever the cleanups ran late (a loaded box; keploy mock
// replay's TestMockReplayHoldsOneTestsRecordingsAtATime read 1.13 tests'
// recordings after a test's close where it holds under half a test's).
var parsedRecordings sync.Map // uintptr (the mock's address) -> *parsedRecording

// parsedRecording is one recording's parts and what they were parsed from:
// the recording and its recorded request, both weakly and by identity (a
// copy or a re-decode of the mock gives it a new request; nothing changes
// one in place), and the container keyer's tally (its destination reads
// which recorded keys are ambiguous). parts holds no body (recordedParts
// gives the recording's). strict is its URL as the strict comparison reads
// it (recordedURL), which parses a path-only URL as a request line, unlike
// the learn step's parts.
type parsedRecording struct {
	mock   weak.Pointer[models.Mock]
	req    weak.Pointer[models.HTTPReq]
	gen    uint64
	parts  httpParts
	strict strictURL
}

// strictURL is a recorded URL's path and query as recordedURL reads them.
type strictURL struct {
	path  string
	query url.Values
}

// strictURLOf is r's URL as the strict comparison reads it; none for a mock
// that recorded no request.
func strictURLOf(r *models.HTTPReq) strictURL {
	if r == nil {
		return strictURL{}
	}
	path, query := recordedURL(r)
	return strictURL{path: path, query: query}
}

// recordedParsings counts the recordings parsed for recordedParts: what the
// rows count to see that a call parses only what it has not seen.
var recordedParsings atomic.Int64

// recordedParts is the recorded request's parts, parsed once per recording
// (parsedRecordings) and read again only when its request or the keyer's
// tally changed. The parts are shared between calls: read only. In the
// exact view's verify mode (integrations.ExactViewVerify) each kept read is
// parsed again and a difference panics.
func recordedParts(m *models.Mock) httpParts {
	p := parsedOf(m).parts
	p.body = recordedBody(m.Spec.HTTPReq)
	return p
}

// recordedBody is r's body as the parts read it: the recorded body's own
// bytes, never a copy (parseRecordedParts).
func recordedBody(r *models.HTTPReq) []byte {
	return unsafe.Slice(unsafe.StringData(r.Body), len(r.Body))
}

// recordedStrictURL is the recorded request's path and query as the strict
// comparison reads them (recordedURL), parsed once per recording as
// recordedParts' are, and shared the same way: read only.
func recordedStrictURL(m *models.Mock) (string, url.Values) {
	s := parsedOf(m).strict
	return s.path, s.query
}

// parsedOf is m's entry in parsedRecordings, parsed now when it has none or
// its entry is stale.
func parsedOf(m *models.Mock) *parsedRecording {
	gen := containerkey.Default.RecordedGeneration()
	addr := uintptr(unsafe.Pointer(m))
	prev, _ := parsedRecordings.Load(addr)
	if p, ok := prev.(*parsedRecording); ok && p.mock.Value() == m && p.req.Value() == m.Spec.HTTPReq && p.gen == gen {
		if integrations.ExactViewVerify.Load() {
			verifyRecordedParts(m, p.parts)
			verifyStrictURL(m, p.strict)
		}
		return p
	}
	recordedParsings.Add(1)
	p := &parsedRecording{mock: weak.Make(m), req: weak.Make(m.Spec.HTTPReq), gen: gen, parts: keptParts(parseRecordedParts(m)), strict: strictURLOf(m.Spec.HTTPReq)}
	if old, _ := parsedRecordings.Swap(addr, p); old == nil || old.(*parsedRecording).mock != p.mock {
		// The recording's first entry: it goes with the recording, unless
		// another mock has the address by then.
		runtime.AddCleanup(m, forgetParsedRecording, parsedKey{addr: addr, mock: p.mock})
	}
	return p
}

// keptParts is p as an entry keeps it: without the body, which would hold
// the recorded request's body as long as the entry (parsedRecordings).
func keptParts(p httpParts) httpParts {
	p.body = nil
	return p
}

// parsedKey names a recording's entry for its cleanup.
type parsedKey struct {
	addr uintptr
	mock weak.Pointer[models.Mock]
}

// forgetParsedRecording drops a recording's entry once the recording is
// gone, unless a mock the collector placed at its address since has it.
func forgetParsedRecording(k parsedKey) {
	if v, ok := parsedRecordings.Load(k.addr); ok && v.(*parsedRecording).mock == k.mock {
		parsedRecordings.CompareAndDelete(k.addr, v)
	}
}

// verifyRecordedParts panics when kept, a recording's parts as recordedParts
// kept them, is not what parsing the recording now gives.
func verifyRecordedParts(m *models.Mock, kept httpParts) {
	if now := keptParts(parseRecordedParts(m)); !reflect.DeepEqual(now, kept) {
		panic(fmt.Sprintf("exact view verify: recorded parts of %s: kept %+v, parsed now %+v", m.Name, kept, now))
	}
}

// verifyStrictURL panics when kept, a recording's URL as parsedOf kept it for
// the strict comparison, is not what reading the recording now gives.
func verifyStrictURL(m *models.Mock, kept strictURL) {
	if now := strictURLOf(m.Spec.HTTPReq); !reflect.DeepEqual(now, kept) {
		panic(fmt.Sprintf("exact view verify: recorded strict URL of %s: kept %+v, read now %+v", m.Name, kept, now))
	}
}
