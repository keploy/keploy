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
// A record that sends a definition is a lesson: it is kept as written, and
// the record stored is the value encoded again, now with every type it uses
// already sent, so it carries no definition. A definition need not come in a
// message of its own: gob writes the definition of a type first held in an
// interface value inside another interface value into the outer value's
// message (a MySQL parameter's map[string]interface{} inside a packet), so
// a record that is one gob message may still teach a type. The store tells
// by length instead: every value is encoded twice, and the first encode is
// kept as a lesson when it is longer than the second. It is longer exactly
// when it sent a definition, as the second sends none and, with no
// definition in it, a value's gob length is the value's alone (map entries
// come in any order, each at its own length; the one GobEncode among the
// stored types, PostgresV3Cell's, writes a value the same way each time).
// A kept decoder first reads the lessons in the order they were written,
// decoding their values and dropping them, and then reads any record. What
// it decodes is what a new decoder decodes from the record written alone,
// into a new value each time, so every read is a copy no other caller holds.
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

// encodeLocked returns v encoded for the store with no type definition in
// it, or, once the stream cannot go on (freshFrom), as a gob stream of its
// own. offset is where the store writes next, and store keeps a lesson's
// bytes there. Caller holds the store's mu.
func (g *gobRecords) encodeLocked(v any, resp bool, offset int64, store func([]byte) (int64, int, error)) ([]byte, error) {
	if g.freshFrom == math.MaxInt64 {
		if g.enc == nil {
			g.enc = gob.NewEncoder(&g.encBuf)
		}
		first, err := g.encodeOnce(v)
		var second []byte
		if err == nil {
			second, err = g.encodeOnce(v)
		}
		if err == nil {
			if len(first) == len(second) {
				return second, nil
			}
			off, n, err := store(first)
			if err != nil {
				g.freshFrom, g.enc = offset, nil
				return nil, err
			}
			g.lessons = append(g.lessons, lesson{off: off, n: n, resp: resp})
			return second, nil
		}
		// The encoder may have sent a definition no lesson holds.
		g.freshFrom, g.enc = offset, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeOnce returns v as the store's encoder writes it next.
func (g *gobRecords) encodeOnce(v any) ([]byte, error) {
	g.encBuf.Reset()
	if err := g.enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.Clone(g.encBuf.Bytes()), nil
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
