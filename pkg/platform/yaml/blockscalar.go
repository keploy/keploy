package yaml

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// blockscalar.go writes values that hold a string yaml.v3 cannot carry in a
// block scalar (models.YAMLBlockScalarUnsafe) so that they read back.
//
// yaml.v3 writes such a string as a literal block that does not load, or
// loads as another string. Its Node.Encode parses its own text back, so there
// the value failed to encode (a mock or test case was dropped); marshaled in
// one pass it was written unchecked, and the whole file stopped loading.
// Neither the encoder's value path nor a Node built by Node.Encode lets a
// caller pick one string's style, so a value that holds one is marshaled in
// flow style instead, where yaml.v3 double-quotes every string with a line
// break and the text parses back; the parsed tree is then restyled as the
// block document the value would have been, with those strings kept
// double-quoted (EncodeQuoted). Other values are written as before: the check
// (NeedsQuoting) reads the value's strings, and costs no text.

// NeedsQuoting reports whether v holds a string that yaml.v3, marshaling v,
// would write as a block scalar that does not read back
// (models.YAMLBlockScalarUnsafe): a string, a map key, a Node scalar left to
// choose its style, or what a MarshalYAML or MarshalText of v's returns. A
// value yaml.v3 cannot marshal (its MarshalYAML fails or panics) is reported
// false: marshaling it reports the failure.
func NeedsQuoting(v any) (needs bool) {
	defer func() {
		if recover() != nil {
			needs = false
		}
	}()
	return scanValue(reflect.ValueOf(v), 0)
}

// EncodeNode is n.Encode(v) for a v that may hold a string a block scalar
// cannot carry: such a v is encoded through EncodeQuoted, anything else as
// n.Encode encodes it.
func EncodeNode(n *yamlLib.Node, v any) error {
	if !NeedsQuoting(v) {
		return n.Encode(v)
	}
	q, err := EncodeQuoted(v)
	if err != nil {
		return err
	}
	*n = *q
	return nil
}

// quotedDoc makes yaml.v3 marshal V in flow style: a string with a line
// break is written double-quoted there, never as a block.
type quotedDoc struct {
	V any `yaml:"v,flow"`
}

// EncodeQuoted is the yaml.Node of v as yaml.v3 writes v, but with every
// string a block scalar cannot carry double-quoted. v is marshaled in flow
// style and parsed back, and the tree is set to the styles the block document
// has: block collections, and each scalar as yaml.v3 picks for it. (A field
// tagged ",flow" is written in block style here: it reads back the same.)
func EncodeQuoted(v any) (n *yamlLib.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			n, err = nil, fmt.Errorf("yaml: %v", r)
		}
	}()
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	if err := enc.Encode(quotedDoc{V: v}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	var doc yamlLib.Node
	if err := yamlLib.Unmarshal(buf.Bytes(), &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yamlLib.DocumentNode || len(doc.Content) != 1 || len(doc.Content[0].Content) != 2 {
		return nil, errors.New("yaml: the flow-style document did not parse back to one value")
	}
	root := doc.Content[0].Content[1]
	restyle(root)
	return root, nil
}

// restyle sets a tree parsed from flow style to the styles yaml.v3 writes
// in block style (encoder.stringv): block collections; a string with a line
// break a literal block unless a block cannot carry it, a YAML 1.1 bool or
// base-60 float double-quoted, and any other scalar left for the emitter to
// choose, as it chooses for a value.
//
// Flow style quotes a plain scalar that holds a flow indicator (, [ ] { } :),
// single-quoted when it can. Of what yaml.v3 writes plain, only a time.Time
// holds one and does not read back as a string; a string that reads as a
// timestamp is not written plain but double-quoted. So a single-quoted string
// that reads as a timestamp was a time, and is one again.
func restyle(n *yamlLib.Node) {
	switch n.Kind {
	case yamlLib.DocumentNode, yamlLib.SequenceNode, yamlLib.MappingNode:
		n.Style &^= yamlLib.FlowStyle
		for _, c := range n.Content {
			restyle(c)
		}
	case yamlLib.ScalarNode:
		single := n.Style&yamlLib.SingleQuotedStyle != 0
		n.Style &^= yamlLib.DoubleQuotedStyle | yamlLib.SingleQuotedStyle | yamlLib.LiteralStyle | yamlLib.FoldedStyle
		if n.Tag != "!!str" {
			return
		}
		switch {
		case models.YAMLBlockScalarUnsafe(n.Value), isOldBool(n.Value), isBase60Float(n.Value):
			n.Style |= yamlLib.DoubleQuotedStyle
		case single && n.Style&yamlLib.TaggedStyle == 0 && isTimestamp(n.Value):
			n.Tag = "!!timestamp"
		}
	}
}

// isTimestamp is yaml.v3's parseTimestamp (resolve.go): whether a plain
// scalar s reads back as a timestamp.
func isTimestamp(s string) bool {
	i := 0
	for ; i < len(s); i++ {
		if c := s[i]; c < '0' || c > '9' {
			break
		}
	}
	if i != 4 || i == len(s) || s[i] != '-' {
		return false
	}
	for _, f := range []string{"2006-1-2T15:4:5.999999999Z07:00", "2006-1-2t15:4:5.999999999Z07:00", "2006-1-2 15:4:5.999999999", "2006-1-2"} {
		if _, err := time.Parse(f, s); err == nil {
			return true
		}
	}
	return false
}

// isOldBool and isBase60Float are yaml.v3's (encode.go): strings it
// double-quotes so that a YAML 1.1 reader does not take them for a bool or a
// number.
func isOldBool(s string) bool {
	switch s {
	case "y", "Y", "yes", "Yes", "YES", "on", "On", "ON",
		"n", "N", "no", "No", "NO", "off", "Off", "OFF":
		return true
	}
	return false
}

var base60float = regexp.MustCompile(`^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$`)

func isBase60Float(s string) bool {
	if s == "" {
		return false
	}
	if c := s[0]; !(c == '+' || c == '-' || c >= '0' && c <= '9') || strings.IndexByte(s, ':') < 0 {
		return false
	}
	return base60float.MatchString(s)
}

// maxScanDepth bounds the scan on a value that refers to itself, which
// yaml.v3 cannot marshal either.
const maxScanDepth = 512

// scanKind is how the scan reads a value of a type, decided once per type.
type scanKind uint8

const (
	scanNone          scanKind = iota // writes no string
	scanWalk                          // by its kind: a string, or what it holds
	scanNodePtr                       // *yaml.Node
	scanNodeValue                     // yaml.Node
	scanMarshaler                     // what its MarshalYAML returns
	scanTextMarshaler                 // what its MarshalText returns
	scanLater                         // a type met again while it is being decided: taken to write strings
)

type typeScan struct {
	kind   scanKind
	fields []int // of a struct: the fields yaml.v3 marshals that can write a string
}

var (
	typeScans sync.Map // reflect.Type -> *typeScan

	nodeType          = reflect.TypeOf(yamlLib.Node{})
	nodePtrType       = reflect.TypeOf(&yamlLib.Node{})
	timeType          = reflect.TypeOf(time.Time{})
	timePtrType       = reflect.TypeOf(&time.Time{})
	durationType      = reflect.TypeOf(time.Duration(0))
	marshalerType     = reflect.TypeOf((*yamlLib.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

func scanOf(t reflect.Type) *typeScan {
	if s, ok := typeScans.Load(t); ok {
		return s.(*typeScan)
	}
	s := buildScan(t, map[reflect.Type]bool{})
	typeScans.Store(t, s)
	return s
}

// buildScan decides how to read a value of type t, in the order yaml.v3's
// marshal looks at a value.
func buildScan(t reflect.Type, visiting map[reflect.Type]bool) *typeScan {
	if s, ok := typeScans.Load(t); ok {
		return s.(*typeScan)
	}
	if visiting[t] {
		return &typeScan{kind: scanLater}
	}
	switch t {
	case nodePtrType:
		return &typeScan{kind: scanNodePtr}
	case nodeType:
		return &typeScan{kind: scanNodeValue}
	case timeType, timePtrType, durationType:
		return &typeScan{kind: scanNone}
	}
	if t.Kind() == reflect.Interface {
		return &typeScan{kind: scanWalk} // its dynamic value decides
	}
	if t.Implements(marshalerType) {
		return &typeScan{kind: scanMarshaler}
	}
	if t.Implements(textMarshalerType) {
		return &typeScan{kind: scanTextMarshaler}
	}
	visiting[t] = true
	defer delete(visiting, t)
	holds := func(e reflect.Type) bool { return buildScan(e, visiting).kind != scanNone }
	switch t.Kind() {
	case reflect.String:
		return &typeScan{kind: scanWalk}
	case reflect.Ptr, reflect.Slice, reflect.Array:
		if holds(t.Elem()) {
			return &typeScan{kind: scanWalk}
		}
	case reflect.Map:
		if holds(t.Key()) || holds(t.Elem()) {
			return &typeScan{kind: scanWalk}
		}
	case reflect.Struct:
		s := &typeScan{kind: scanNone}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" && !f.Anonymous {
				continue // unexported: yaml.v3 skips it
			}
			tag := f.Tag.Get("yaml")
			if tag == "" && !strings.Contains(string(f.Tag), ":") {
				tag = string(f.Tag)
			}
			if tag == "-" {
				continue
			}
			if holds(f.Type) {
				s.fields = append(s.fields, i)
			}
		}
		if len(s.fields) > 0 {
			s.kind = scanWalk
		}
		return s
	}
	return &typeScan{kind: scanNone}
}

// scanValue reports whether v writes a string a block scalar cannot carry.
func scanValue(v reflect.Value, depth int) bool {
	if !v.IsValid() || depth > maxScanDepth {
		return false
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return false // null, [] or {}
		}
	}
	s := scanOf(v.Type()) // never scanLater: that is only met inside a build
	switch s.kind {
	case scanNone:
		return false
	case scanNodePtr:
		if !v.CanInterface() {
			return false
		}
		return nodeNeedsQuoting(v.Interface().(*yamlLib.Node), depth)
	case scanNodeValue:
		if !v.CanInterface() {
			return false
		}
		n := v.Interface().(yamlLib.Node)
		return nodeNeedsQuoting(&n, depth)
	case scanMarshaler:
		if !v.CanInterface() {
			return false
		}
		out, err := v.Interface().(yamlLib.Marshaler).MarshalYAML()
		if err != nil || out == nil {
			return false
		}
		return scanValue(reflect.ValueOf(out), depth+1)
	case scanTextMarshaler:
		if !v.CanInterface() {
			return false
		}
		text, err := v.Interface().(encoding.TextMarshaler).MarshalText()
		return err == nil && models.YAMLBlockScalarUnsafe(string(text))
	}
	switch v.Kind() {
	case reflect.String:
		return models.YAMLBlockScalarUnsafe(v.String())
	case reflect.Ptr, reflect.Interface:
		return scanValue(v.Elem(), depth+1)
	case reflect.Struct:
		for _, i := range s.fields {
			if scanValue(v.Field(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if scanValue(it.Key(), depth+1) || scanValue(it.Value(), depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if scanValue(v.Index(i), depth+1) {
				return true
			}
		}
	}
	return false
}

// nodeNeedsQuoting reports whether a Node tree holds a scalar left to choose
// its style (or set to a block) that a block scalar cannot carry.
func nodeNeedsQuoting(n *yamlLib.Node, depth int) bool {
	if n == nil || depth > maxScanDepth {
		return false
	}
	switch n.Kind {
	case yamlLib.ScalarNode:
		return n.Style&(yamlLib.DoubleQuotedStyle|yamlLib.SingleQuotedStyle) == 0 && models.YAMLBlockScalarUnsafe(n.Value)
	case yamlLib.AliasNode:
		return false
	}
	for _, c := range n.Content {
		if nodeNeedsQuoting(c, depth+1) {
			return true
		}
	}
	return false
}
