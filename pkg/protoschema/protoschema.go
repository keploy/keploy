// Package protoschema compiles .proto files into a descriptor set, loads one
// back, finds a gRPC method's message types, and decodes a recorded gRPC body
// to field-named JSON for display.
//
// It is display only. A gRPC verdict is always the field-number comparison of
// the recorded bodies (pkg/matcher/grpc); nothing here decides one. A field
// the descriptor does not define is shown as "#<number>", never dropped as
// protojson drops it, so a reader does not see two equal-looking bodies for a
// test that failed. One limit: inside google.protobuf.Any, Struct, Value and
// the wrapper types, protojson's own rendering is used, and an undefined field
// there is not shown.
package protoschema

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/reporter"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"go.keploy.io/server/v3/utils"
)

// Limits on what Compile accepts. They bound work on an upload, not what a
// real service's protos need.
const (
	MaxFiles      = 2000
	MaxTotalBytes = 20 << 20
)

// Method is one RPC a schema defines.
type Method struct {
	// Path is the gRPC :path, "/pkg.Service/Method".
	Path   string `json:"path"`
	Input  string `json:"input"`
	Output string `json:"output"`
	// ClientStreaming and ServerStreaming say whether either side streams.
	ClientStreaming bool `json:"client_streaming,omitempty"`
	ServerStreaming bool `json:"server_streaming,omitempty"`
}

// Compiled is a compiled schema: the descriptor set to store and the methods
// it defines.
type Compiled struct {
	// DescriptorSet is the serialized FileDescriptorSet, every file the
	// sources reach included (well-known types too), so it loads on its own.
	DescriptorSet []byte
	Methods       []Method
}

// CompileError is a compile failure that names what failed, for a CLI or UI
// to show as given.
type CompileError struct {
	Problems []string
}

func (e *CompileError) Error() string {
	return "the proto files do not compile: " + strings.Join(e.Problems, "; ")
}

// Compile compiles sources (path -> .proto contents, paths relative to the
// import root, as the files import one another) into a self-contained
// descriptor set. Every source is a root. Google's well-known types resolve
// without being supplied. A missing import or a syntax error fails with a
// CompileError naming each.
func Compile(ctx context.Context, sources map[string][]byte) (*Compiled, error) {
	if len(sources) == 0 {
		return nil, errors.New("no .proto files were given")
	}
	if len(sources) > MaxFiles {
		return nil, fmt.Errorf("%d files given; at most %d are accepted", len(sources), MaxFiles)
	}
	total := 0
	var roots []string
	clean := make(map[string][]byte, len(sources))
	for p, b := range sources {
		total += len(b)
		slashed := strings.ReplaceAll(p, "\\", "/")
		if strings.HasPrefix(slashed, "/") || (len(slashed) >= 2 && slashed[1] == ':') {
			return nil, fmt.Errorf("file path %q is absolute; give paths relative to the import root", p)
		}
		cp := path.Clean(slashed)
		if cp == "." || strings.HasPrefix(cp, "../") || cp == ".." {
			return nil, fmt.Errorf("file path %q leaves the upload's root", p)
		}
		if _, dup := clean[cp]; dup {
			return nil, fmt.Errorf("file path %q is given twice", cp)
		}
		clean[cp] = b
		if strings.HasSuffix(cp, ".proto") {
			roots = append(roots, cp)
		}
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("%d bytes given; at most %d are accepted", total, MaxTotalBytes)
	}
	if len(roots) == 0 {
		return nil, errors.New("none of the files given is a .proto file")
	}
	sort.Strings(roots)

	var problems []string
	comp := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) {
				b, ok := clean[path.Clean(p)]
				if !ok {
					return nil, fmt.Errorf("%s: not in the upload", p)
				}
				return io.NopCloser(bytes.NewReader(b)), nil
			},
		}),
		Reporter: reporter.NewReporter(func(err reporter.ErrorWithPos) error {
			problems = append(problems, err.Error())
			return nil
		}, nil),
		SourceInfoMode: protocompile.SourceInfoNone,
	}
	files, err := comp.Compile(ctx, roots...)
	if len(problems) > 0 {
		return nil, &CompileError{Problems: problems}
	}
	if err != nil {
		return nil, &CompileError{Problems: []string{err.Error()}}
	}

	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			add(imps.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	var methods []Method
	for _, f := range files {
		add(f)
		methods = append(methods, methodsOf(f)...)
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("serialize the compiled descriptors: %w", err)
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].Path < methods[j].Path })
	return &Compiled{DescriptorSet: b, Methods: methods}, nil
}

func methodsOf(fd protoreflect.FileDescriptor) []Method {
	var out []Method
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		s := svcs.Get(i)
		ms := s.Methods()
		for j := 0; j < ms.Len(); j++ {
			m := ms.Get(j)
			out = append(out, Method{
				Path:            "/" + string(s.FullName()) + "/" + string(m.Name()),
				Input:           string(m.Input().FullName()),
				Output:          string(m.Output().FullName()),
				ClientStreaming: m.IsStreamingClient(),
				ServerStreaming: m.IsStreamingServer(),
			})
		}
	}
	return out
}

// Schema is a loaded descriptor set.
type Schema struct {
	files *protoregistry.Files
	types *protoregistry.Types
}

// Load reads a descriptor set Compile produced (or one from `buf build -o` /
// `protoc --descriptor_set_out --include_imports`).
func Load(descriptorSet []byte) (*Schema, error) {
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(descriptorSet, set); err != nil {
		return nil, fmt.Errorf("read the descriptor set: %w", err)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("the descriptor set is not self-contained (build it with its imports included): %w", err)
	}
	types := &protoregistry.Types{}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		msgs := fd.Messages()
		for i := 0; i < msgs.Len(); i++ {
			registerMessage(types, msgs.Get(i))
		}
		return true
	})
	return &Schema{files: files, types: types}, nil
}

func registerMessage(types *protoregistry.Types, md protoreflect.MessageDescriptor) {
	_ = types.RegisterMessage(dynamicpb.NewMessageType(md))
	nested := md.Messages()
	for i := 0; i < nested.Len(); i++ {
		registerMessage(types, nested.Get(i))
	}
}

// Methods lists the RPCs the schema defines.
func (s *Schema) Methods() []Method {
	var out []Method
	s.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		out = append(out, methodsOf(fd)...)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ErrNoMethod says the schema does not define the method a body belongs to.
var ErrNoMethod = errors.New("the schema does not define this gRPC method")

// MessageTypes are the request and response types of the gRPC method at
// grpcPath ("/pkg.Service/Method").
func (s *Schema) MessageTypes(grpcPath string) (req, resp protoreflect.MessageDescriptor, err error) {
	p := strings.TrimPrefix(grpcPath, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return nil, nil, fmt.Errorf("%q is not a gRPC method path", grpcPath)
	}
	d, err := s.files.FindDescriptorByName(protoreflect.FullName(p[:i]))
	if err != nil {
		return nil, nil, ErrNoMethod
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil, ErrNoMethod
	}
	m := sd.Methods().ByName(protoreflect.Name(p[i+1:]))
	if m == nil {
		return nil, nil, ErrNoMethod
	}
	return m.Input(), m.Output(), nil
}

// Decoded is one body shown with field names.
type Decoded struct {
	// JSON is the message as JSON with proto field names. A field the schema
	// does not define is the key "#<number>".
	JSON json.RawMessage `json:"json"`
	// UnknownFields counts the fields the schema does not define, at any
	// depth: a schema older (or newer) than the body.
	UnknownFields int `json:"unknown_fields,omitempty"`
}

// DecodeProtoscope decodes a recorded body (protoscope text, as test cases
// store it) as a message of type md.
func (s *Schema) DecodeProtoscope(md protoreflect.MessageDescriptor, protoscope string) (*Decoded, error) {
	wire, err := utils.ProtoTextToWire(protoscope)
	if err != nil {
		return nil, fmt.Errorf("read the recorded body: %w", err)
	}
	return s.DecodeWire(md, wire)
}

// DecodeWire decodes protobuf wire bytes as a message of type md.
func (s *Schema) DecodeWire(md protoreflect.MessageDescriptor, wire []byte) (*Decoded, error) {
	msg := dynamicpb.NewMessage(md)
	if err := (proto.UnmarshalOptions{Resolver: s.types, DiscardUnknown: false}).Unmarshal(wire, msg); err != nil {
		return nil, fmt.Errorf("the body is not a %s: %w", md.FullName(), err)
	}
	j, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false, Resolver: s.types}.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", md.FullName(), err)
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	n := addUnknown(msg, obj)
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	return &Decoded{JSON: out, UnknownFields: n}, nil
}

// addUnknown puts every field msg carries but its descriptor does not define
// into obj as "#<number>", recursing into the message fields obj already
// holds. It returns how many it added.
func addUnknown(msg protoreflect.Message, obj map[string]any) int {
	if obj == nil {
		return 0
	}
	n := 0
	for num, vals := range unknownFields(msg.GetUnknown()) {
		n += len(vals)
		key := "#" + strconv.Itoa(int(num))
		if len(vals) == 1 {
			obj[key] = vals[0]
		} else {
			obj[key] = vals
		}
	}
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil {
			return true
		}
		child, ok := obj[fd.TextName()]
		if !ok {
			return true
		}
		switch {
		case fd.IsMap():
			cm, ok := child.(map[string]any)
			if !ok || fd.MapValue().Message() == nil {
				return true
			}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if o, ok := cm[k.String()].(map[string]any); ok {
					n += addUnknown(mv.Message(), o)
				}
				return true
			})
		case fd.IsList():
			arr, ok := child.([]any)
			if !ok {
				return true
			}
			l := v.List()
			for i := 0; i < l.Len() && i < len(arr); i++ {
				if o, ok := arr[i].(map[string]any); ok {
					n += addUnknown(l.Get(i).Message(), o)
				}
			}
		default:
			if o, ok := child.(map[string]any); ok {
				n += addUnknown(v.Message(), o)
			}
		}
		return true
	})
	return n
}

// unknownFields reads raw unknown-field bytes into values by field number:
// a varint as a number, fixed32/64 as their unsigned value, a length-
// delimited field as text when it is UTF-8 and else base64, a group as its
// raw bytes in base64.
func unknownFields(raw []byte) map[protowire.Number][]any {
	return unknownFieldsAt(raw, 0)
}

func unknownFieldsAt(raw []byte, depth int) map[protowire.Number][]any {
	out := map[protowire.Number][]any{}
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			out[0] = append(out[0], base64.StdEncoding.EncodeToString(raw))
			return out
		}
		raw = raw[n:]
		var v any
		switch typ {
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(raw)
			n, v = m, x
		case protowire.Fixed32Type:
			x, m := protowire.ConsumeFixed32(raw)
			n, v = m, x
		case protowire.Fixed64Type:
			x, m := protowire.ConsumeFixed64(raw)
			n, v = m, x
		case protowire.BytesType:
			b, m := protowire.ConsumeBytes(raw)
			n = m
			if m >= 0 {
				v = unknownBytesValueAt(b, depth)
			}
		default:
			m := protowire.ConsumeFieldValue(num, typ, raw)
			n = m
			if m >= 0 {
				v = base64.StdEncoding.EncodeToString(raw[:m])
			}
		}
		if n < 0 {
			out[num] = append(out[num], base64.StdEncoding.EncodeToString(raw))
			return out
		}
		raw = raw[n:]
		out[num] = append(out[num], v)
	}
	return out
}

// FromFileDescriptors builds a Schema from already-compiled files (a run's
// local --proto-dir, say), their imports included.
func FromFileDescriptors(fds []protoreflect.FileDescriptor) (*Schema, error) {
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if fd == nil || seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			add(imps.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	for _, fd := range fds {
		add(fd)
	}
	b, err := proto.Marshal(set)
	if err != nil {
		return nil, err
	}
	return Load(b)
}

// unknownBytesValue shows a length-delimited field the schema does not
// define: printable text as a string, bytes that parse whole as protobuf
// fields as a nested object of "#<number>" keys, anything else as base64.
func unknownBytesValue(b []byte) any {
	return unknownBytesValueAt(b, 0)
}

// maxUnknownDepth bounds how deep nested unknown messages are opened; deeper
// ones are shown as base64, so an adversarial body cannot recurse without end.
const maxUnknownDepth = 100

func unknownBytesValueAt(b []byte, depth int) any {
	if utf8.Valid(b) && printable(b) {
		return string(b)
	}
	if depth >= maxUnknownDepth {
		return base64.StdEncoding.EncodeToString(b)
	}
	if nested, ok := parseWholeMessageAt(b, depth+1); ok {
		return nested
	}
	return base64.StdEncoding.EncodeToString(b)
}

func printable(b []byte) bool {
	for _, r := range string(b) {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

// parseWholeMessage reads b as protobuf fields, all of it, into "#<number>"
// keys; ok is false when b is not (exactly) a run of fields.
func parseWholeMessageAt(b []byte, depth int) (map[string]any, bool) {
	if len(b) == 0 {
		return nil, false
	}
	rest := b
	for len(rest) > 0 {
		_, typ, n := protowire.ConsumeTag(rest)
		if n < 0 || typ == protowire.StartGroupType || typ == protowire.EndGroupType {
			return nil, false
		}
		m := protowire.ConsumeFieldValue(0, typ, rest[n:])
		if m < 0 {
			return nil, false
		}
		rest = rest[n+m:]
	}
	out := map[string]any{}
	for num, vals := range unknownFieldsAt(b, depth) {
		key := "#" + strconv.Itoa(int(num))
		if len(vals) == 1 {
			out[key] = vals[0]
		} else {
			out[key] = vals
		}
	}
	return out, true
}
