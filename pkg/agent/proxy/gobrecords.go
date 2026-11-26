package proxy

import (
	"bytes"
	"encoding"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"reflect"
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
// already sent, so it carries no definition. A definition can arrive either as
// a top-level gob type-definition message (opening with a negative type id) or
// nested inside an interface value's message when a previously unseen concrete
// type sits inside an interface value (such as a MySQL parameter's
// map[string]interface{} inside a packet).
//
// To avoid encoding every steady-state mock twice, encodeLocked tracks which
// concrete types have already been seen inside interface values (walking only
// struct/slice/map subtrees whose static type can reach an interface) and
// checks whether the encoded output opened with a top-level type-definition
// message. Steady-state records introduce no new interface concrete types and
// carry no top-level type definitions, so they encode exactly once. Only the
// rare records that introduce a new type perform a second encode to separate
// the lesson from the definition-free record.
//
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
	enc       *gob.Encoder
	encBuf    bytes.Buffer
	lessons   []lesson
	seenTypes map[reflect.Type]struct{}
	// freshFrom is the offset from which every record is a gob stream of its
	// own: math.MaxInt64 until an encode fails.
	freshFrom int64

	// Reader side.
	mu      sync.Mutex
	free    []*keptDecoder
	built   atomic.Int64 // gob decoders made (DecodersBuilt)
	encodes atomic.Int64 // encodeOnce calls made (Encodes)
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

var (
	gobEncoderType      = reflect.TypeOf((*gob.GobEncoder)(nil)).Elem()
	binaryMarshalerType = reflect.TypeOf((*encoding.BinaryMarshaler)(nil)).Elem()

	typeHasIfaceCache sync.Map // reflect.Type -> bool
	ifaceFieldsCache  sync.Map // reflect.Type -> []int (exported struct fields that can reach an interface)
)

// implementsExternalGob reports whether gob encodes t via GobEncoder or
// BinaryMarshaler rather than descending into its fields. (encoding/gob does
// not treat encoding.TextMarshaler as an external gob encoder.)
func implementsExternalGob(t reflect.Type) bool {
	if t.Implements(gobEncoderType) || t.Implements(binaryMarshalerType) {
		return true
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(gobEncoderType) || pt.Implements(binaryMarshalerType)
}

// typeHasInterface reports whether values of static type t can contain any
// interface value that encoding/gob inspects. Cached per reflect.Type.
func typeHasInterface(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if v, ok := typeHasIfaceCache.Load(t); ok {
		return v.(bool)
	}
	res := computeTypeHasInterface(t, make(map[reflect.Type]bool))
	typeHasIfaceCache.Store(t, res)
	return res
}

func computeTypeHasInterface(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if t == nil {
		return false
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Interface {
		return true
	}
	if implementsExternalGob(t) {
		return false
	}
	if visiting[t] {
		return false
	}
	visiting[t] = true
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.IsExported() && computeTypeHasInterface(f.Type, visiting) {
				return true
			}
		}
		return false
	case reflect.Slice, reflect.Array:
		return computeTypeHasInterface(t.Elem(), visiting)
	case reflect.Map:
		return computeTypeHasInterface(t.Key(), visiting) || computeTypeHasInterface(t.Elem(), visiting)
	default:
		return false
	}
}

// ifaceStructFields returns the indices of exported fields on struct type t
// whose static type can reach an interface. Cached per reflect.Type.
func ifaceStructFields(t reflect.Type) []int {
	if v, ok := ifaceFieldsCache.Load(t); ok {
		return v.([]int)
	}
	var fields []int
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && typeHasInterface(f.Type) {
			fields = append(fields, i)
		}
	}
	ifaceFieldsCache.Store(t, fields)
	return fields
}

// walkInterfaces collects any concrete types held inside non-nil interface
// values reachable from rv that are not yet recorded in seen. Subtrees whose
// static type cannot contain an interface are skipped in O(1).
func walkInterfaces(rv reflect.Value, seen map[reflect.Type]struct{}, pending *[]reflect.Type) {
	if !rv.IsValid() {
		return
	}
	t := rv.Type()
	if t.Kind() == reflect.Interface {
		if rv.IsNil() {
			return
		}
		elem := rv.Elem()
		et := elem.Type()
		if _, ok := seen[et]; !ok {
			*pending = append(*pending, et)
		}
		if typeHasInterface(et) {
			walkInterfaces(elem, seen, pending)
		}
		return
	}
	if !typeHasInterface(t) {
		return
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if !rv.IsNil() {
			walkInterfaces(rv.Elem(), seen, pending)
		}
	case reflect.Struct:
		for _, idx := range ifaceStructFields(t) {
			walkInterfaces(rv.Field(idx), seen, pending)
		}
	case reflect.Slice, reflect.Array:
		n := rv.Len()
		for i := 0; i < n; i++ {
			walkInterfaces(rv.Index(i), seen, pending)
		}
	case reflect.Map:
		if rv.Len() == 0 {
			return
		}
		keyHas := typeHasInterface(t.Key())
		valHas := typeHasInterface(t.Elem())
		iter := rv.MapRange()
		for iter.Next() {
			if keyHas {
				walkInterfaces(iter.Key(), seen, pending)
			}
			if valHas {
				walkInterfaces(iter.Value(), seen, pending)
			}
		}
	}
}

// hasTopLevelTypeDef reports whether b carries any top-level gob type
// definition message (or unexpected multi-message framing). A single value
// message starts with a positive gob signed type id (even encoded uint) and
// consumes all of b.
func hasTopLevelTypeDef(b []byte) bool {
	size, rest, ok := gobReadUint(b)
	if !ok || size == 0 || size != uint64(len(rest)) {
		return true
	}
	id, _, ok := gobReadUint(rest)
	if !ok {
		return true
	}
	// gob encodes signed ints as uints with the sign in bit 0 (odd = negative).
	return id&1 == 1
}

// gobReadUint reads one gob unsigned integer off the front of b.
func gobReadUint(b []byte) (uint64, []byte, bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	if b[0] < 0x80 {
		return uint64(b[0]), b[1:], true
	}
	n := -int(int8(b[0]))
	if n > 8 || len(b) < 1+n {
		return 0, nil, false
	}
	var x uint64
	for _, c := range b[1 : 1+n] {
		x = x<<8 | uint64(c)
	}
	return x, b[1+n:], true
}

// encodeLocked returns v encoded for the store with no type definition in
// it, or, once the stream cannot go on (freshFrom), as a gob stream of its
// own. offset is where the store writes next, and store keeps a lesson's
// bytes there. Caller holds the store's mu.
func (g *gobRecords) encodeLocked(v any, resp bool, offset int64, store func([]byte) (int64, int, error)) ([]byte, error) {
	if g.freshFrom == math.MaxInt64 {
		if g.enc == nil {
			g.enc = gob.NewEncoder(&g.encBuf)
			g.seenTypes = make(map[reflect.Type]struct{})
		}
		var pending []reflect.Type
		walkInterfaces(reflect.ValueOf(v), g.seenTypes, &pending)

		first, err := g.encodeOnce(v)
		if err == nil {
			if len(pending) == 0 && !hasTopLevelTypeDef(first) {
				return first, nil
			}
			var second []byte
			second, err = g.encodeOnce(v)
			if err == nil {
				for _, pt := range pending {
					g.seenTypes[pt] = struct{}{}
				}
				if len(first) == len(second) {
					return second, nil
				}
				off, n, storeErr := store(first)
				if storeErr != nil {
					g.freshFrom, g.enc, g.seenTypes = offset, nil, nil
					return nil, storeErr
				}
				g.lessons = append(g.lessons, lesson{off: off, n: n, resp: resp})
				return second, nil
			}
		}
		// The encoder may have sent a definition no lesson holds.
		g.freshFrom, g.enc, g.seenTypes = offset, nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeOnce returns v as the store's encoder writes it next.
func (g *gobRecords) encodeOnce(v any) ([]byte, error) {
	g.encodes.Add(1)
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
