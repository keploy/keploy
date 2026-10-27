package models

import (
	"bytes"
	"encoding/gob"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// cellGobDecoders counts the gob decoders PostgresV3Cell.GobDecode built.
var cellGobDecoders atomic.Int64

// cellReadByGob, set by tests only, has each cell read by a gob decoder from
// its first message on, as GobDecode read every cell before cellStream: the
// reference cellStream's reads are compared with.
var cellReadByGob atomic.Bool

// cellStream reads the values PostgresV3Cell.GobEncode wrote into one cell's
// gob stream, one Decode per value, as a gob decoder over that stream reads
// them. GobEncode encodes each cell with a gob encoder of its own, so reading
// a cell built a gob decoder for it, and the cells of a recorded set are read
// whenever a test loads its mocks.
//
// A value of a type gob predefines (bool, the integers, the floats, []byte
// and string, whose wire ids gob's format fixes) is one message that defines
// no type, and is read here as gob reads it, bytes left in its message after
// the value included: gob drops them at its next Decode. The first message
// that is not such a value of the type asked for is read, with the rest of
// the stream, by a gob decoder: no message before it defined a type, so that
// decoder reads what one over the whole stream would. A time.Time or a UUID's
// [16]uint8 that is the whole rest of the stream is read here too when its
// type definition is the one this process's gob sends (cellKnownTypes).
type cellStream struct {
	b   []byte       // the stream from the next message on
	dec *gob.Decoder // once set, it reads the rest of the stream
}

// Decode reads the next value into e, a pointer, as gob.Decoder.Decode does.
func (s *cellStream) Decode(e any) error {
	if s.dec == nil && !cellReadByGob.Load() {
		if rest, ok := readCellValue(s.b, e); ok {
			s.b = rest
			return nil
		}
	}
	if s.dec == nil {
		cellGobDecoders.Add(1)
		s.dec = gob.NewDecoder(bytes.NewReader(s.b))
	}
	return s.dec.Decode(e)
}

// gob's predefined type ids (encoding/gob's format: "bool 1, int 2, uint 3,
// float 4, []byte 5, string 6").
const (
	gobIDBool   = 1
	gobIDInt    = 2
	gobIDUint   = 3
	gobIDFloat  = 4
	gobIDBytes  = 5
	gobIDString = 6
)

// gobTooBig is the smallest message size this reader leaves to gob: encoding/gob
// refuses a message of its tooBig or more, 1 GiB on 32-bit platforms and 8 GiB
// on 64-bit ones, so a message this size or larger is handed to gob, which
// reads or refuses it as it would have.
const gobTooBig = 1 << 30

// gobUint reads one of gob's unsigned integers off b, as gob's decodeUint does.
func gobUint(b []byte) (uint64, []byte, bool) {
	if len(b) == 0 {
		return 0, b, false
	}
	c := b[0]
	if c <= 0x7f {
		return uint64(c), b[1:], true
	}
	n := -int(int8(c))
	if n > 8 || len(b)-1 < n {
		return 0, b, false
	}
	var x uint64
	for _, d := range b[1 : 1+n] {
		x = x<<8 | uint64(d)
	}
	return x, b[1+n:], true
}

// gobInt reads one of gob's signed integers off b.
func gobInt(b []byte) (int64, []byte, bool) {
	u, rest, ok := gobUint(b)
	if !ok {
		return 0, b, false
	}
	if u&1 != 0 {
		return ^int64(u >> 1), rest, true
	}
	return int64(u >> 1), rest, true
}

// gobMessage splits the next message off b: its type id and its body after
// the singleton's field delta, which must be 0 for a value.
func gobMessage(b []byte) (id int64, body, rest []byte, ok bool) {
	n, b, ok := gobUint(b)
	if !ok || n == 0 || n >= gobTooBig || n > uint64(len(b)) {
		return 0, nil, nil, false
	}
	msg, rest := b[:n], b[n:]
	id, msg, ok = gobInt(msg)
	if !ok {
		return 0, nil, nil, false
	}
	return id, msg, rest, true
}

// readCellValue reads the value message at the head of b into e when it is
// exactly a value of e's type that cellStream reads (see there), and returns
// the stream after it. Otherwise it leaves e and b as they are.
func readCellValue(b []byte, e any) ([]byte, bool) {
	switch p := e.(type) {
	case *time.Time:
		return readKnownCellValue(b, cellKnownTypes().time, func(body []byte) bool {
			raw, ok := gobLengthPrefixed(body)
			if !ok {
				return false
			}
			var t time.Time
			if t.GobDecode(raw) != nil {
				return false
			}
			*p = t
			return true
		})
	case *[16]uint8:
		return readKnownCellValue(b, cellKnownTypes().uuid, func(body []byte) bool {
			n, body, ok := gobUint(body)
			if !ok || n != 16 {
				return false
			}
			var a [16]uint8
			for i := range a {
				var v uint64
				if v, body, ok = gobUint(body); !ok || v > math.MaxUint8 {
					return false
				}
				a[i] = uint8(v)
			}
			*p = a
			return true
		})
	}
	id, body, rest, ok := gobMessage(b)
	if !ok {
		return b, false
	}
	if body, ok = gobSingleton(body); !ok {
		return b, false
	}
	if !readPredefined(id, body, e) {
		return b, false
	}
	return rest, true
}

// gobSingleton is a value message's body after its field delta, which gob
// requires to be 0 for a value that is not a struct.
func gobSingleton(body []byte) ([]byte, bool) {
	delta, body, ok := gobUint(body)
	return body, ok && delta == 0
}

// gobLengthPrefixed is the bytes the length at the head of body counts, when
// body holds that many after it (as gob reads a string, a []byte or a
// GobEncoder's bytes, leaving any after them unread).
func gobLengthPrefixed(body []byte) ([]byte, bool) {
	n, body, ok := gobUint(body)
	if !ok || n > uint64(len(body)) {
		return nil, false
	}
	return body[:n], true
}

// readPredefined decodes body, the value of a message of type id, into e when
// e points to a type gob predefines and id is that type's, with gob's range
// checks. Otherwise it leaves e as it is.
func readPredefined(id int64, body []byte, e any) bool {
	switch p := e.(type) {
	case *[]byte:
		raw, ok := gobLengthPrefixed(body)
		if id != gobIDBytes || !ok {
			return false
		}
		// As gob fills a []byte: into its own array when it holds them,
		// so an empty value leaves a nil slice nil.
		if cap(*p) < len(raw) {
			*p = make([]byte, len(raw))
		} else {
			*p = (*p)[:len(raw)]
		}
		copy(*p, raw)
		return true
	case *string:
		raw, ok := gobLengthPrefixed(body)
		if id != gobIDString || !ok {
			return false
		}
		*p = string(raw)
		return true
	}
	var u uint64
	var ok bool
	if u, _, ok = gobUint(body); !ok {
		return false
	}
	switch p := e.(type) {
	case *bool:
		if id != gobIDBool {
			return false
		}
		*p = u != 0
	case *int64, *int32, *int16, *int8:
		if id != gobIDInt {
			return false
		}
		v := int64(u >> 1)
		if u&1 != 0 {
			v = ^int64(u >> 1)
		}
		switch p := p.(type) {
		case *int64:
			*p = v
		case *int32:
			if v < math.MinInt32 || v > math.MaxInt32 {
				return false
			}
			*p = int32(v)
		case *int16:
			if v < math.MinInt16 || v > math.MaxInt16 {
				return false
			}
			*p = int16(v)
		case *int8:
			if v < math.MinInt8 || v > math.MaxInt8 {
				return false
			}
			*p = int8(v)
		}
	case *uint64, *uint32, *uint16, *uint8:
		if id != gobIDUint {
			return false
		}
		switch p := p.(type) {
		case *uint64:
			*p = u
		case *uint32:
			if u > math.MaxUint32 {
				return false
			}
			*p = uint32(u)
		case *uint16:
			if u > math.MaxUint16 {
				return false
			}
			*p = uint16(u)
		case *uint8:
			if u > math.MaxUint8 {
				return false
			}
			*p = uint8(u)
		}
	case *float64, *float32:
		if id != gobIDFloat {
			return false
		}
		v := math.Float64frombits(bits.ReverseBytes64(u))
		switch p := p.(type) {
		case *float64:
			*p = v
		case *float32:
			// gob refuses a finite value float32 cannot hold.
			if av := math.Abs(v); math.MaxFloat32 < av && av <= math.MaxFloat64 {
				return false
			}
			*p = float32(v)
		}
	default:
		return false
	}
	return true
}

// cellKnownType is how this process's gob sends one type a cell holds: the
// messages defining it, as a new encoder sends them before its first value,
// and the id its values carry.
type cellKnownType struct {
	defs []byte
	id   int64
}

var (
	cellKnownOnce sync.Once
	cellKnown     struct{ time, uuid cellKnownType }
)

// cellKnownTypes are the definitions this process's gob sends for time.Time
// and [16]uint8. gob numbers a process's types in the order it first meets
// them, so the definitions name this process's ids: a cell another process
// encoded may carry other ids, and its stream is then read by gob.
func cellKnownTypes() *struct{ time, uuid cellKnownType } {
	cellKnownOnce.Do(func() {
		cellKnown.time = knownTypeOf(time.Time{})
		cellKnown.uuid = knownTypeOf([16]uint8{})
	})
	return &cellKnown
}

// knownTypeOf encodes v with a new encoder and splits what it sent into the
// definitions and the value's id. A split that is not definitions then one
// value leaves the type unknown (no defs), so its values are read by gob.
func knownTypeOf(v any) cellKnownType {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return cellKnownType{}
	}
	all := buf.Bytes()
	b := all
	for {
		id, _, rest, ok := gobMessage(b)
		if !ok {
			return cellKnownType{}
		}
		if id >= 0 {
			if len(rest) != 0 || len(b) == len(all) {
				return cellKnownType{}
			}
			return cellKnownType{defs: all[:len(all)-len(b)], id: id}
		}
		b = rest
	}
}

// readKnownCellValue reads a value of known type k that is all of b after
// k's definitions, its body read by read, and returns the empty rest.
func readKnownCellValue(b []byte, k cellKnownType, read func(body []byte) bool) ([]byte, bool) {
	if len(k.defs) == 0 || !bytes.HasPrefix(b, k.defs) {
		return b, false
	}
	id, body, rest, ok := gobMessage(b[len(k.defs):])
	if !ok || id != k.id || len(rest) != 0 {
		return b, false
	}
	if body, ok = gobSingleton(body); !ok || !read(body) {
		return b, false
	}
	return rest, true
}
