package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/protocolbuffers/protoscope"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type ProtoConfig struct {
	ProtoFile    string
	ProtoDir     string
	ProtoInclude []string
	RequestURI   string
}

func GetProtoMessageDescriptor(ctx context.Context, logger *zap.Logger, pc ProtoConfig) (protoreflect.MessageDescriptor, error) {
	if pc.ProtoFile == "" && pc.ProtoDir == "" {
		return nil, fmt.Errorf("protoFile or protoDir must be provided")
	}

	if pc.RequestURI == "" {
		return nil, fmt.Errorf("requestURI must be provided, eg:/service.DataService/GetComplexData")
	}

	protoPath := pc.ProtoFile
	protoDir := pc.ProtoDir
	protoInclude := pc.ProtoInclude
	grpcPath := pc.RequestURI

	// Normalize protoInclude roots to absolute.
	var absRoots []string
	for _, p := range protoInclude {
		absPath, err := mustAbs(p)
		if err != nil {
			return nil, err
		}
		absRoots = append(absRoots, absPath)
	}

	// If -proto is given, ensure its directory is an include root.
	var absProto string
	if protoPath != "" {
		var err error
		absProto, err = mustAbs(protoPath)
		if err != nil {
			return nil, err
		}
		protoDirOfFile := filepath.Dir(absProto)
		if !containsDir(absRoots, protoDirOfFile) {
			absRoots = append(absRoots, protoDirOfFile)
		}
	}

	// If -proto_dir is given, ensure it is an include root.
	var absProtoDir string
	if protoDir != "" {
		var err error
		absProtoDir, err = mustAbs(protoDir)
		if err != nil {
			return nil, err
		}
		if !containsDir(absRoots, absProtoDir) {
			absRoots = append(absRoots, absProtoDir)
		}
	}

	// Build compile list:
	// - If -proto provided, it goes first (priority).
	// - If -proto_dir provided, add all .proto files under it (dedup).
	compileNames := make([]string, 0, 64)
	seenCompile := map[string]bool{}

	// Helper to add a file by absolute path: convert to import-style rel to any -I root.
	addFile := func(abs string) {
		rel := relToAny(abs, absRoots)
		if rel == "" {
			// As a last resort, use base name; but with added roots we should have a rel.
			rel = filepath.ToSlash(filepath.Base(abs))
		}
		if !seenCompile[rel] {
			seenCompile[rel] = true
			compileNames = append(compileNames, rel)
		}
	}

	// 1) -proto (preferred)
	if absProto != "" {
		addFile(absProto)
	}

	// 2) -proto_dir (recursively add all .proto files)
	if absProtoDir != "" {
		err := filepath.WalkDir(absProtoDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".proto") {
				absPath, err := mustAbs(path)
				if err != nil {
					return err
				}
				addFile(absPath)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking proto directory: %v", err)
		}
	}

	// If only -proto_dir was given and nothing got added (empty dir?), fail early.
	if len(compileNames) == 0 {
		return nil, fmt.Errorf("no .proto files found to compile (proto=%q, proto_dir=%q)", protoPath, protoDir)
	}

	// Parse :path -> service + method
	svcFull, mName, err := utils.ParseGRPCPath(grpcPath)
	if err != nil {
		return nil, fmt.Errorf("parse :path: %v", err)
	}

	// Compile protos and locate the response type for the method
	mdOut, err := compileAndFindResponseDescriptor(compileNames, absRoots, svcFull, mName)
	if err != nil {
		return nil, fmt.Errorf("find response descriptor: %v", err)
	}

	return mdOut, nil
}

// compileAndFindResponseDescriptor compiles all compileNames (+ imports via roots) and returns serviceFull.method Output desc.
// We avoid building a separate registry; instead we search the linked files directly.
func compileAndFindResponseDescriptor(compileNames []string, roots []string, serviceFull, method string) (protoreflect.MessageDescriptor, error) {
	c := &protocompile.Compiler{
		Resolver: &protocompile.SourceResolver{ImportPaths: roots},
		Reporter: reporter.NewReporter(
			func(e reporter.ErrorWithPos) error { return e }, // errors
			func(w reporter.ErrorWithPos) { /* optionally log warnings */ },
		),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	files, err := c.Compile(ctx, compileNames...)
	if err != nil {
		return nil, fmt.Errorf("compile %v (relative to -I: %v): %w", compileNames, roots, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files compiled for %v", compileNames)
	}

	// Directly search the linked files for the service, then the method.
	full := protoreflect.FullName(serviceFull)
	for _, f := range files {
		d := f.FindDescriptorByName(full)
		if d == nil {
			continue
		}

		sd, ok := d.(protoreflect.ServiceDescriptor)
		if !ok {
			continue
		}

		for i := range sd.Methods().Len() {
			m := sd.Methods().Get(i)
			if string(m.Name()) == method {
				return m.Output(), nil
			}
		}
		return nil, fmt.Errorf("method %q not found on service %q", method, serviceFull)
	}

	return nil, fmt.Errorf("service %q not found in compiled set", serviceFull)
}

func mustAbs(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	a, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("abs(%q): %v", p, err)
	}
	return filepath.Clean(a), nil
}

func containsDir(dirs []string, dir string) bool {
	dir = filepath.Clean(dir)
	for _, d := range dirs {
		if filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}

// relToAny returns an import-style path (forward slashes) for abs under any root; "" if none match.
func relToAny(abs string, roots []string) string {
	abs = filepath.Clean(abs)
	for _, r := range roots {
		r = filepath.Clean(r)
		if rel, err := filepath.Rel(r, abs); err == nil && !strings.HasPrefix(rel, "..") && !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return filepath.ToSlash(rel)
		}
	}
	return ""
}

// ProtoTextToWire turns Protoscope text into wire bytes using the library (no exec).
func ProtoTextToWire(text string) ([]byte, error) {
	sc := protoscope.NewScanner(text) // expects string
	b, err := sc.Exec()
	if err != nil {
		return nil, fmt.Errorf("can't convert proto text to wire: %w", err)
	}
	return b, nil
}

// ProtoWireToJSON takes a MessageDescriptor and a wire-format []byte,
// and returns the JSON encoding ([]byte).
func ProtoWireToJSON(md protoreflect.MessageDescriptor, wire []byte) ([]byte, error) {
	// Unmarshal into dynamic message
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(wire, msg); err != nil {
		return nil, err
	}

	// Marshal to JSON with your options
	actRespJson, err := protojson.MarshalOptions{
		Indent:          "  ",
		EmitUnpopulated: true, // include false/0/""/empty fields
		UseProtoNames:   true, // snake_case field names
	}.Marshal(msg)
	if err != nil {
		return nil, err
	}

	return actRespJson, nil
}

// ProtoTextToJSON converts a Protoscope text payload to JSON via:
//
//	Protoscope text -> wire bytes (ProtoTextToWire) -> JSON (WireToJSON).
//
// It preserves your logging style and returns (jsonBytes, ok).
func ProtoTextToJSON(md protoreflect.MessageDescriptor, text string, logger *zap.Logger) ([]byte, bool) {

	if md == nil {
		utils.LogError(logger, fmt.Errorf("message descriptor is nil"), "cannot convert grpc response to json")
		return nil, false
	}

	// Protoscope text -> raw protobuf wire
	wire, err := ProtoTextToWire(text)
	if err != nil {
		utils.LogError(logger, err, "failed to convert protoscope text to raw protobuf wire, cannot convert grpc response to json")
		return nil, false
	}

	// wire -> JSON (use the shared WireToJSON you provided)
	j, err := ProtoWireToJSON(md, wire)
	if err != nil {
		// We don't know if it failed in unmarshal or marshal, so keep this generic.
		utils.LogError(logger, err, "failed to convert wire to json, cannot convert grpc response to json")
		return nil, false
	}

	return j, true
}
