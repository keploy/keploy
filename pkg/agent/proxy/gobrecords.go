package proxy

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
)

// gobRecords encodes what a DiskMocks stores as one gob stream, and decodes
// it with decoders that live as long as the store.
//
// A new gob.Decoder compiles a decoding engine for every type it meets, and
// that compile was most of what a read cost while each record was a gob
// stream of its own with a decoder of its own. So the store writes every
// record with one gob.Encoder, which sends each type's definition once, in
// the first record that uses it, and keeps decoders that have read those
// definitions.
//
// A record that sends a definition (gob may split such a record's value
// around it) is a lesson: it is kept as written, and the record is encoded
// again, now with every type it uses already sent, as one message of its
// value alone. Every stored record is such a message. A kept decoder first
// reads the lessons in the order they were written, decoding their values
// and dropping them, and then reads any record. What it decodes is what a
// new decoder decodes from the record written alone, into a new value each
// time, so every read is a copy no other caller holds.
//
// A failed encode, or a lesson the store failed to keep, can leave the
// encoder having sent a definition no lesson holds, so from then on the store
// writes each record as a gob stream of its own, read by a new decoder, as
// before (freshFrom).
type gobRecords struct {
	// Writer side; the store's mu guards it.
	enc     *gob.Encoder
	encBuf  bytes.Buffer
	lessons []lesson
	// freshFrom is the offset from which every record is a gob stream of its
	// own: math.MaxInt64 until an encode fails.
	freshFrom int64

	// Reader side.
	mu    sync.Mutex
	free  []*keptDecoder
	built atomic.Int64 // gob decoders made (DecodersBuilt)
}

// lesson locates a record as first written: the type definitions it sent
// and its value. resp marks a spilled response's.
type lesson struct {
	off  int64
	n    int
	resp bool
}

// maxKeptDecoders bounds the idle kept decoders: one per reader in flight is
// enough, and a burst above it builds decoders that are then dropped.
const maxKeptDecoders = 32

// keptDecoder is a gob.Decoder kept across records, with how many of the
// store's lessons it has read.
type keptDecoder struct {
	dec  *gob.Decoder
	in   bytes.Reader
	read int
}

// errGobRecord reports a record a kept decoder did not read exactly.
var errGobRecord = errors.New("gob record: not read whole")

func newGobRecords() gobRecords { return gobRecords{freshFrom: math.MaxInt64} }

// encodeLocked returns v encoded for the store as one message of its value
// alone, or, once the stream cannot go on (freshFrom), as a gob stream of its
// own. offset is where the store writes next, and store keeps a lesson's
// bytes there. Caller holds the store's mu.
func (g *gobRecords) encodeLocked(v any, resp bool, offset int64, store func([]byte) (int64, int, error)) ([]byte, error) {
	if g.freshFrom == math.MaxInt64 {
		if g.enc == nil {
			g.enc = gob.NewEncoder(&g.encBuf)
		}
		for try := 0; try < 2; try++ {
			g.encBuf.Reset()
			if err := g.enc.Encode(v); err != nil {
				break
			}
			out := g.encBuf.Bytes()
			if oneValueMessage(out) {
				return bytes.Clone(out), nil
			}
			if try == 1 {
				break
			}
			off, n, err := store(bytes.Clone(out))
			if err != nil {
				g.freshFrom, g.enc = offset, nil
				return nil, err
			}
			g.lessons = append(g.lessons, lesson{off: off, n: n, resp: resp})
			offset = off + int64(n)
		}
		g.freshFrom, g.enc = offset, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// oneValueMessage reports whether b, one Encode's output, is a single gob
// message: its value alone, as a type definition always comes in a message
// of its own before the value's.
func oneValueMessage(b []byte) bool {
	n, w, ok := gobUint(b)
	return ok && uint64(len(b)-w) == n
}

// gobUint reads an unsigned integer as gob writes it: one byte below 0x80,
// else the negated byte count and then the bytes, big-endian.
func gobUint(b []byte) (x uint64, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	c := b[0]
	if c <= 0x7f {
		return uint64(c), 1, true
	}
	k := -int(int8(c))
	if k < 1 || k > 8 || len(b) < 1+k {
		return 0, 0, false
	}
	for _, d := range b[1 : 1+k] {
		x = x<<8 | uint64(d)
	}
	return x, 1 + k, true
}

// decodeRecord decodes the record rec stored at off into a new T.
func decodeRecord[T any](d *DiskMocks, off int64, rec []byte) (*T, error) {
	g := &d.records
	d.mu.Lock()
	lessons, freshFrom := g.lessons, g.freshFrom
	d.mu.Unlock()
	v := new(T)
	if off >= freshFrom {
		g.built.Add(1)
		if err := gob.NewDecoder(bytes.NewReader(rec)).Decode(v); err != nil {
			return nil, err
		}
		return v, nil
	}
	k := g.get()
	if err := k.learn(d, lessons); err != nil {
		return nil, err
	}
	if err := k.decode(rec, v); err != nil {
		return nil, err
	}
	g.put(k)
	return v, nil
}

func (g *gobRecords) get() *keptDecoder {
	g.mu.Lock()
	if n := len(g.free); n > 0 {
		k := g.free[n-1]
		g.free = g.free[:n-1]
		g.mu.Unlock()
		return k
	}
	g.mu.Unlock()
	g.built.Add(1)
	k := &keptDecoder{}
	k.dec = gob.NewDecoder(&k.in)
	return k
}

func (g *gobRecords) put(k *keptDecoder) {
	g.mu.Lock()
	if len(g.free) < maxKeptDecoders {
		g.free = append(g.free, k)
	}
	g.mu.Unlock()
}

// learn has the decoder read the lessons it has not read, in order.
func (k *keptDecoder) learn(d *DiskMocks, lessons []lesson) error {
	for ; k.read < len(lessons); k.read++ {
		l := lessons[k.read]
		b, err := d.bytesAt(l.off, l.n)
		if err != nil {
			return err
		}
		if l.resp {
			err = k.decode(b, new(spilledResponse))
		} else {
			err = k.decode(b, new(models.Mock))
		}
		if err != nil {
			return fmt.Errorf("gob record: read the type definitions at %d: %w", l.off, err)
		}
	}
	return nil
}

// decode decodes b, read whole, into v. On an error the caller drops k.
func (k *keptDecoder) decode(b []byte, v any) error {
	k.in.Reset(b)
	if err := k.dec.Decode(v); err != nil {
		return err
	}
	if k.in.Len() != 0 {
		return errGobRecord
	}
	return nil
}
