package http

import (
	"fmt"
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
// sync.Map read and the weak pointer's load.
var parsedRecordings sync.Map // uintptr (the mock's address) -> *parsedRecording

// parsedRecording is one recording's parts and what they were parsed from:
// the recording (weakly), its recorded request, by identity (a copy or a
// re-decode of the mock gives it a new one; nothing changes one in place),
// and the container keyer's tally (its destination reads which recorded
// keys are ambiguous).
type parsedRecording struct {
	mock  weak.Pointer[models.Mock]
	req   *models.HTTPReq
	gen   uint64
	parts httpParts
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
	gen := containerkey.Default.RecordedGeneration()
	addr := uintptr(unsafe.Pointer(m))
	prev, _ := parsedRecordings.Load(addr)
	if p, ok := prev.(*parsedRecording); ok && p.mock.Value() == m && p.req == m.Spec.HTTPReq && p.gen == gen {
		if integrations.ExactViewVerify.Load() {
			verifyRecordedParts(m, p.parts)
		}
		return p.parts
	}
	recordedParsings.Add(1)
	p := &parsedRecording{mock: weak.Make(m), req: m.Spec.HTTPReq, gen: gen, parts: parseRecordedParts(m)}
	if old, _ := parsedRecordings.Swap(addr, p); old == nil || old.(*parsedRecording).mock != p.mock {
		// The recording's first entry: it goes with the recording, unless
		// another mock has the address by then.
		runtime.AddCleanup(m, forgetParsedRecording, parsedKey{addr: addr, mock: p.mock})
	}
	return p.parts
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
	if now := parseRecordedParts(m); !reflect.DeepEqual(now, kept) {
		panic(fmt.Sprintf("exact view verify: recorded parts of %s: kept %+v, parsed now %+v", m.Name, kept, now))
	}
}
