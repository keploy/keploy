package mockdb

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// A replay reads a test set's mocks twice: the read that sorts them into the
// agent's pools decodes every document (IndexTestSetMocks), and the stream to
// the agent reads each payload back (LoadPayload) as it sends it. Decoding
// the YAML is most of the CPU of both, the same work twice: on one throttled
// core a 1 GB mocks file costs some 18 CPU-seconds each way.
//
// So the first read keeps what it decoded, each mock gob-encoded, in a spill
// file beside the mock file, and the read-back decodes the gob, which is a
// fraction of the YAML's cost. Gob is what the stream sends: a mock read back
// from the spill is what the agent would get of the mock decoded from the
// file. The spill holds about what the mock file does, on disk; the heap
// holds a small entry per mock, as before.
//
// One gob encoder writes every mock, so each type is defined once, in the
// first mock that has it, and a later mock is only its value. A gob stream
// is read in order, though, and the mocks are read back in any order. So a
// mock whose encoding defined a type is a primer: it is spilled twice, as
// first encoded (which defines its types) and encoded again (its value
// alone), and one decoder reads the primers, in order, before the first
// mock it reads back. That decoder then knows every type in the spill, and
// reads any mock's value alone. A gob type is defined either in a message of
// its own or inside the value that first holds it (an interface's concrete
// type), so a primer is told by its length: encoded again, with nothing left
// to define, it is shorter.
//
// The spill is a cache, never the record. Any mock it cannot give back (it
// could not be written, its gob fails, a mock was too big to spill) is read
// back from the mock file, as it was before there was one. The spill file is
// removed from its directory as soon as it is made, where the system allows
// it (not Windows, where there is no spill), so a killed process leaves
// nothing behind; its space is freed when the source is collected. A spill is
// made only where its filesystem keeps room to spare once it is written
// (spillRoom).

// payloadSpill is a spill file of one mock file's mocks.
type payloadSpill struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
	// size is how many bytes have been written to f (through w).
	size int64
	// enc encodes every spilled mock, one after another, into encoded.
	enc     *gob.Encoder
	encoded bytes.Buffer
	// primers is where in f each primer's first encoding is, in order.
	primers []spillAt
	// at is where in f each spilled mock's value is, by the offset of its
	// document in the mock file (PayloadAt.Offset).
	at map[int64]spillAt
	// ready is set once every spilled mock is on disk; broken once anything
	// failed, after which nothing more is spilled or read back from f.
	ready, broken bool
	// dec decodes the mocks read back, from feed, once it has read the
	// primers (primed).
	dec    *gob.Decoder
	feed   bytes.Reader
	primed bool
	buf    []byte
	// hits counts the mocks read back from the spill.
	hits int
}

type spillAt struct {
	off int64
	n   int
	// doc is the CRC-32C of the document's bytes as decoded: a document
	// changed in place since is decoded again (and found changed), not
	// answered from the spill.
	doc uint32
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// maxSpilledMock is the largest gob encoding a mock is spilled as. A gob
// decoder refuses a message of 1 GiB or more; a mock that large is read back
// from the mock file.
const maxSpilledMock = 512 << 20

// maxKeptReadBuf is the largest read buffer a spill keeps for the next
// read-back; a bigger mock is read into one of its own, which is not held.
const maxKeptReadBuf = 1 << 20

// newPayloadSpill makes a spill file for the mock file at mockPath, of size
// bytes, in its directory, or returns nil when it cannot or there is not the
// room for it (spillRoom): the mocks are then read back from the mock file.
// The file is unlinked at once: owner is the source whose collection closes
// it, which frees its space.
func newPayloadSpill(owner *payloadSource, mockPath string, size int64) *payloadSpill {
	dir := filepath.Dir(mockPath)
	// The spill is about the size of the mock file: gob holds what YAML does.
	if !spillRoom(dir, size) {
		return nil
	}
	f, err := os.CreateTemp(dir, ".keploy-mock-payloads-*")
	if err != nil {
		return nil
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil
	}
	s := &payloadSpill{f: f, w: bufio.NewWriterSize(f, 1<<20), at: map[int64]spillAt{}}
	s.enc = gob.NewEncoder(&s.encoded)
	s.dec = gob.NewDecoder(&s.feed)
	runtime.AddCleanup(owner, func(f *os.File) { _ = f.Close() }, f)
	return s
}

// add spills mock, decoded from doc, the document at docOff of the mock
// file. It is called with the mock just decoded, before the read changes it.
// A mock too big to spill is left out (it is read back from the mock file).
// When a mock cannot be spilled whole (its encoding fails, a write fails, a
// primer is too big), nothing more is spilled or read back from the spill:
// what the encoder defined in it, a later mock does not define again.
func (s *payloadSpill) add(docOff int64, doc []byte, mock *models.Mock) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken || s.ready {
		return
	}
	s.encoded.Reset()
	if err := s.enc.Encode(mock); err != nil {
		s.fail()
		return
	}
	first := s.encoded.Len()
	if first > maxSpilledMock {
		// Whether it defined a type is not worth a second encoding.
		s.fail()
		return
	}
	if err := s.enc.Encode(mock); err != nil {
		s.fail()
		return
	}
	value := s.encoded.Len() - first
	if value < first {
		// It defined a type: a primer.
		at, ok := s.write(s.encoded.Bytes()[:first])
		if !ok {
			return
		}
		s.primers = append(s.primers, at)
	}
	at, ok := s.write(s.encoded.Bytes()[first:])
	if !ok {
		return
	}
	at.doc = crc32.Checksum(doc, castagnoli)
	s.at[docOff] = at
}

// write appends b to the spill file.
func (s *payloadSpill) write(b []byte) (spillAt, bool) {
	if _, err := s.w.Write(b); err != nil {
		s.fail()
		return spillAt{}, false
	}
	at := spillAt{off: s.size, n: len(b)}
	s.size += int64(len(b))
	return at, true
}

// finish ends the spill: every spilled mock is on disk, and can be read back.
func (s *payloadSpill) finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return
	}
	if err := s.w.Flush(); err != nil {
		s.fail()
		return
	}
	s.ready = true
	s.w, s.enc = nil, nil
	s.encoded = bytes.Buffer{}
}

// fail stops the spill: nothing is spilled or read back from it again.
func (s *payloadSpill) fail() {
	s.broken = true
	s.w, s.enc, s.dec = nil, nil, nil
	s.encoded = bytes.Buffer{}
	s.at, s.primers = nil, nil
}

// decode decodes the mock spilled at at with s.dec. A decoder that failed
// may be part-way through a type definition, so the spill is failed with it.
func (s *payloadSpill) decode(at spillAt) (*models.Mock, bool) {
	buf := make([]byte, at.n)
	if at.n <= maxKeptReadBuf {
		if cap(s.buf) < at.n {
			s.buf = make([]byte, maxKeptReadBuf)
		}
		buf = s.buf[:at.n]
	}
	if n, _ := s.f.ReadAt(buf, at.off); n != at.n {
		s.fail()
		return nil, false
	}
	s.feed.Reset(buf)
	var mock models.Mock
	if err := s.dec.Decode(&mock); err != nil || s.feed.Len() != 0 {
		s.fail()
		return nil, false
	}
	return &mock, true
}

// load reads back the mock spilled for the document at at, whose bytes in the
// mock file are now doc, or reports false when it has none, or the document's
// bytes are not the ones it spilled the mock of (the caller decodes doc then).
func (s *payloadSpill) load(at models.PayloadAt, doc []byte) (*models.Mock, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.broken {
		return nil, false
	}
	where, ok := s.at[at.Offset]
	if !ok || crc32.Checksum(doc, castagnoli) != where.doc {
		return nil, false
	}
	if !s.primed {
		for _, p := range s.primers {
			if _, ok := s.decode(p); !ok {
				return nil, false
			}
		}
		s.primed = true
	}
	mock, ok := s.decode(where)
	if !ok || mock.Name != at.Name {
		return nil, false
	}
	s.hits++
	return mock, true
}
