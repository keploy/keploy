package learn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

// RefusalSink takes a note of each word Decide refuses (SetRefusalSink).
// Refused is called once per refused word, as Decide returns, on the
// goroutine that called Decide; it must not block, and it may be called
// from several goroutines at once.
type RefusalSink interface {
	Refused(RefusalNote)
}

// RefusalNote is a refused word as a sink may keep it, beyond the process
// that decided it: which check refused it and where it stands, by shape,
// and each run's value by its shape and a salted hash. It holds no value:
// the hashes are keyed with a secret made anew in each process (refusalKey)
// that never leaves it, so two notes of one process say whether two values
// were equal, and nothing else of them.
type RefusalNote struct {
	Kind  RefusalKind `json:"kind"`
	Check string      `json:"check"`
	Test  string      `json:"test,omitempty"`
	Mock  string      `json:"mock,omitempty"`
	// Field is the field's shape: its path with each array position
	// written [] (body.items[].id).
	Field string `json:"field"`
	Word  int    `json:"word,omitempty"`
	// Places are the refusal's places ("mock-N <path>"), each path as
	// Field writes it.
	Places []string `json:"places,omitempty"`
	// Recorded and Full are the word as recorded and as record's full
	// round sent it.
	Recorded ValueShape `json:"recorded"`
	Full     ValueShape `json:"full"`
}

// ValueShape is what a value looks like, never the value: the type of the
// leaf it stands in, its length in bytes, the classes of the characters it
// holds (digit, lower, upper, space, punct, other: one past ASCII), in
// that order and joined by "+", and an HMAC-SHA256 of it under this
// process's refusal key, in hex.
type ValueShape struct {
	Type    string `json:"type,omitempty"`
	Len     int    `json:"len"`
	Classes string `json:"classes,omitempty"`
	Hash    string `json:"hash"`
}

type sinkBox struct{ s RefusalSink }

var refusalSink atomic.Pointer[sinkBox]

// SetRefusalSink makes s the sink Decide notes each refused word to; nil
// sets none, as at start.
func SetRefusalSink(s RefusalSink) {
	if s == nil {
		refusalSink.Store(nil)
		return
	}
	refusalSink.Store(&sinkBox{s})
}

// refusalKey is this process's key for ValueShape.Hash: random, made at its
// first use, and never written anywhere.
var refusalKey = sync.OnceValue(func() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
})

// noteRefused notes each word of ds Decide refused to the sink, if one is
// set. A decision's Recorded and Full are the values themselves (only its
// Refusal hides a secret's), so each hash is of the value as the run held it.
func noteRefused(in Input, ds []Decision) {
	b := refusalSink.Load()
	if b == nil {
		return
	}
	for _, d := range ds {
		// A decision carries a Refusal exactly where its word is refused:
		// dc.learned, which makes every other outcome, sets none.
		if d.Refusal != nil {
			b.s.Refused(refusalNote(in, d))
		}
	}
}

func refusalNote(in Input, d Decision) RefusalNote {
	rf := d.Refusal
	typ := typeName(in.Rec.Leaves[rf.Field].Type)
	n := RefusalNote{
		Kind: rf.Kind, Check: rf.Check, Test: rf.Test, Mock: rf.Mock, Field: pathShape(rf.Field), Word: rf.Word,
		Recorded: shapeOf(typ, d.Recorded), Full: shapeOf(typ, d.Full),
	}
	for _, p := range rf.Places {
		n.Places = append(n.Places, pathShape(p))
	}
	return n
}

var arrayPosition = regexp.MustCompile(`\[[0-9]+\]`)

// pathShape is path with each array position written [].
func pathShape(path string) string { return arrayPosition.ReplaceAllString(path, "[]") }

func typeName(t Type) string {
	switch t {
	case String:
		return "string"
	case Number:
		return "number"
	case Bool:
		return "bool"
	case Null:
		return "null"
	case EmptyObj:
		return "empty-object"
	case EmptyArray:
		return "empty-array"
	case Keys:
		return "keys"
	case Instant:
		return "instant"
	}
	return ""
}

var classNames = [...]string{"digit", "lower", "upper", "space", "punct", "other"}

func shapeOf(typ, v string) ValueShape {
	var has [len(classNames)]bool
	for _, r := range v {
		switch {
		case r > unicode.MaxASCII:
			has[5] = true
		case r >= '0' && r <= '9':
			has[0] = true
		case r >= 'a' && r <= 'z':
			has[1] = true
		case r >= 'A' && r <= 'Z':
			has[2] = true
		case unicode.IsSpace(r):
			has[3] = true
		default:
			has[4] = true
		}
	}
	var cs []string
	for i, ok := range has {
		if ok {
			cs = append(cs, classNames[i])
		}
	}
	m := hmac.New(sha256.New, refusalKey())
	m.Write([]byte(v))
	return ValueShape{Type: typ, Len: len(v), Classes: strings.Join(cs, "+"), Hash: hex.EncodeToString(m.Sum(nil))}
}
