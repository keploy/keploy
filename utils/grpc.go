package utils

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
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func GetProtoMessageDescriptor(ctx context.Context, logger *zap.Logger, pc models.ProtoConfig) (protoreflect.MessageDescriptor, []protoreflect.FileDescriptor, error) {
	if pc.RequestURI == "" {
		return nil, nil, fmt.Errorf("requestURI must be provided, eg:/service.DataService/GetComplexData")
	}

	protoPath := pc.ProtoFile
	protoDir := pc.ProtoDir
	protoInclude := pc.ProtoInclude
	grpcPath := pc.RequestURI

	// Auto-derive protoDir from gRPC path when protoInclude is available.
	// This enables multi-service scenarios where different requests need different proto directories.
	if len(protoInclude) > 0 && grpcPath != "" {
		derived, err := deriveProtoDirFromPath(grpcPath, protoInclude)
		if err == nil {
			protoDir = derived // Use derived directory, taking precedence over config
		}
		// If derivation fails, fall through to use config's protoDir
	}

	// Validate that we have at least one source of proto files
	if protoPath == "" && protoDir == "" {
		return nil, nil, fmt.Errorf("protoFile or protoDir must be provided (auto-derive from protoInclude also failed)")
	}

	// Normalize protoInclude roots to absolute.
	var absRoots []string
	for _, p := range protoInclude {
		absPath, err := mustAbs(p)
		if err != nil {
			return nil, nil, err
		}
		absRoots = append(absRoots, absPath)
	}

	// If -proto is given, ensure its directory is an include root.
	var absProto string
	if protoPath != "" {
		var err error
		absProto, err = mustAbs(protoPath)
		if err != nil {
			return nil, nil, err
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
			return nil, nil, err
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
			return nil, nil, fmt.Errorf("walking proto directory: %v", err)
		}
	}

	// If only -proto_dir was given and nothing got added (empty dir?), fail early.
	if len(compileNames) == 0 {
		return nil, nil, fmt.Errorf("no .proto files found to compile (proto=%q, proto_dir=%q)", protoPath, protoDir)
	}

	// Parse :path -> service + method
	svcFull, mName, err := ParseGRPCPath(grpcPath)
	if err != nil {
		return nil, nil, fmt.Errorf("parse :path: %v", err)
	}

	// Compile protos and locate the response type for the method
	mdOut, files, err := compileAndFindResponseDescriptor(compileNames, absRoots, svcFull, mName)
	if err != nil {
		return nil, nil, fmt.Errorf("find response descriptor: %v", err)
	}

	return mdOut, files, nil
}

// compileAndFindResponseDescriptor compiles all compileNames (+ imports via roots) and returns serviceFull.method Output desc.
// We avoid building a separate registry; instead we search the linked files directly.
func compileAndFindResponseDescriptor(compileNames []string, roots []string, serviceFull, method string) (protoreflect.MessageDescriptor, []protoreflect.FileDescriptor, error) {
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
		return nil, nil, fmt.Errorf("compile %v (relative to -I: %v): %w", compileNames, roots, err)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no files compiled for %v", compileNames)
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
				// Convert linker.Files to []protoreflect.FileDescriptor
				fileDescs := make([]protoreflect.FileDescriptor, len(files))
				for i, f := range files {
					fileDescs[i] = f
				}
				return m.Output(), fileDescs, nil
			}
		}
		return nil, nil, fmt.Errorf("method %q not found on service %q", method, serviceFull)
	}

	return nil, nil, fmt.Errorf("service %q not found in compiled set", serviceFull)
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

// deriveProtoDirFromPath extracts the package from a gRPC path and searches protoInclude roots
// for a matching directory. This enables automatic proto directory resolution for multi-service
// scenarios where a single protoDir config is insufficient.
//
// It uses a multi-strategy approach following protobuf conventions:
//  1. Full package path: "homework.v1.Homework" → "homework/v1/"
//  2. First segment only: "homework.v1.Homework" → "homework/"
//
// Returns the first matching directory path, or an error if none found.
func deriveProtoDirFromPath(grpcPath string, protoIncludes []string) (string, error) {
	if grpcPath == "" || len(protoIncludes) == 0 {
		return "", fmt.Errorf("grpcPath and protoIncludes are required")
	}

	// Parse gRPC path to get service full name (e.g., "homework.v1.Homework")
	serviceFull, _, err := ParseGRPCPath(grpcPath)
	if err != nil {
		return "", fmt.Errorf("failed to parse gRPC path: %w", err)
	}

	parts := strings.Split(serviceFull, ".")
	if len(parts) == 0 {
		return "", fmt.Errorf("cannot extract package from service %q", serviceFull)
	}

	// Build search strategies in order of likelihood:
	// 1. Full package path (minus service name): "homework.v1.Homework" → "homework/v1"
	// 2. First segment only: "homework"
	var strategies []string

	// Strategy 1: Full package path (remove service name - last part)
	if len(parts) > 1 {
		packageParts := parts[:len(parts)-1] // ["homework", "v1"]
		strategies = append(strategies, filepath.Join(packageParts...))
	}

	// Strategy 2: First segment only
	strategies = append(strategies, parts[0])

	// Search protoInclude roots for matching directory
	for _, root := range protoIncludes {
		absRoot, err := mustAbs(root)
		if err != nil || absRoot == "" {
			continue
		}

		for _, strategy := range strategies {
			candidateDir := filepath.Join(absRoot, strategy)
			if info, err := os.Stat(candidateDir); err == nil && info.IsDir() {
				return candidateDir, nil
			}
		}
	}

	return "", fmt.Errorf("no proto directory found for service %q in protoInclude roots (tried: %v)", serviceFull, strategies)
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

// createTypeResolver creates a custom type resolver from compiled proto files.
// This resolver enables the protojson marshaler to resolve google.protobuf.Any type URLs
// like "type.googleapis.com/fuzz.Inner" to their actual message descriptors.
func createTypeResolver(files []protoreflect.FileDescriptor) *protoregistry.Types {
	types := &protoregistry.Types{}

	// Register all message types from all files
	for _, file := range files {
		registerMessagesFromFile(types, file)
	}

	return types
} // registerMessagesFromFile recursively registers all message types from a file descriptor
func registerMessagesFromFile(types *protoregistry.Types, file protoreflect.FileDescriptor) {
	messages := file.Messages()
	for i := 0; i < messages.Len(); i++ {
		msg := messages.Get(i)
		// Register the message type
		msgType := dynamicpb.NewMessageType(msg)
		types.RegisterMessage(msgType)

		// Recursively register nested messages
		registerNestedMessages(types, msg)
	}
}

// registerNestedMessages recursively registers nested message types
func registerNestedMessages(types *protoregistry.Types, msg protoreflect.MessageDescriptor) {
	nested := msg.Messages()
	for i := 0; i < nested.Len(); i++ {
		nestedMsg := nested.Get(i)
		msgType := dynamicpb.NewMessageType(nestedMsg)
		types.RegisterMessage(msgType)

		// Recursively register further nested messages
		registerNestedMessages(types, nestedMsg)
	}
}

// ProtoWireToJSON takes a MessageDescriptor, compiled files, and a wire-format []byte,
// and returns the JSON encoding ([]byte). The files parameter is crucial for resolving
// google.protobuf.Any types which require access to all message types in the compiled schema.
func ProtoWireToJSON(md protoreflect.MessageDescriptor, files []protoreflect.FileDescriptor, wire []byte) ([]byte, error) {
	// Create type resolver for Any type resolution - this fixes the error:
	// "proto: google.protobuf.Any: unable to resolve \"type.googleapis.com/fuzz.Inner\": not found"
	typeResolver := createTypeResolver(files)

	// Unmarshal into dynamic message
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(wire, msg); err != nil {
		return nil, err
	}

	// Marshal to JSON with custom type resolver
	actRespJson, err := protojson.MarshalOptions{
		Indent:          "  ",
		EmitUnpopulated: true,         // include false/0/""/empty fields
		UseProtoNames:   true,         // snake_case field names
		Resolver:        typeResolver, // Custom resolver for Any types
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
func ProtoTextToJSON(md protoreflect.MessageDescriptor, files []protoreflect.FileDescriptor, text string, logger *zap.Logger) ([]byte, bool) {

	if md == nil {
		LogError(logger, fmt.Errorf("message descriptor is nil"), "cannot convert grpc response to json")
		return nil, false
	}

	// Protoscope text -> raw protobuf wire
	wire, err := ProtoTextToWire(text)
	if err != nil {
		LogError(logger, err, "failed to convert protoscope text to raw protobuf wire, cannot convert grpc response to json")
		return nil, false
	}

	// wire -> JSON (use the shared WireToJSON you provided)
	j, err := ProtoWireToJSON(md, files, wire)
	if err != nil {
		// We don't know if it failed in unmarshal or marshal, so keep this generic.
		LogError(logger, err, "failed to convert wire to json, cannot convert grpc response to json")
		return nil, false
	}

	return j, true
}

// ProtoSchemaCache holds pre-compiled proto schemas for efficient reuse across multiple test cases.
// This avoids the overhead of re-compiling protos and re-creating type resolvers for each test case.
type ProtoSchemaCache struct {
	// Files contains all compiled file descriptors
	Files []protoreflect.FileDescriptor
	// OutputByMethod maps "package.Service/Method" to its output message descriptor
	OutputByMethod map[string]protoreflect.MessageDescriptor
	// TypeResolver is the pre-built type resolver for Any type resolution
	TypeResolver *protoregistry.Types
}

// BuildProtoSchemaCache compiles all proto files once and builds a lookup cache for method output descriptors.
// This function should be called once per test-set (before the testcase loop) to avoid repeated compilation.
//
// Parameters:
//   - logger: zap logger for error reporting
//   - pc: ProtoConfig containing protoFile, protoDir, and protoInclude settings
//   - grpcPaths: unique :path values from gRPC testcases (e.g., "/homework.v1.Homework/CreateHomework")
//
// The function:
//  1. Derives proto directories from gRPC paths (for multi-service support)
//  2. Collects all .proto files from all relevant directories
//  3. Compiles them once
//  4. Builds a map from "service.Full/Method" -> output MessageDescriptor
//  5. Pre-creates the type resolver for Any type resolution
func BuildProtoSchemaCache(logger *zap.Logger, pc models.ProtoConfig, grpcPaths []string) (*ProtoSchemaCache, error) {
	if len(grpcPaths) == 0 {
		return nil, fmt.Errorf("no gRPC paths provided")
	}

	protoPath := pc.ProtoFile
	protoDir := pc.ProtoDir
	protoInclude := pc.ProtoInclude

	// Collect all unique proto directories to compile from
	// Key: absolute directory path, Value: true (for dedup)
	protoDirsToCompile := make(map[string]bool)

	// If protoDir is explicitly configured, include it
	if protoDir != "" {
		absDir, err := mustAbs(protoDir)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path for protoDir: %w", err)
		}
		protoDirsToCompile[absDir] = true
	}

	// Auto-derive proto directories from gRPC paths when protoInclude is available
	if len(protoInclude) > 0 {
		for _, grpcPath := range grpcPaths {
			derived, err := deriveProtoDirFromPath(grpcPath, protoInclude)
			if err == nil && derived != "" {
				absDir, err := mustAbs(derived)
				if err == nil {
					protoDirsToCompile[absDir] = true
				}
			}
			// If derivation fails for a path, we continue with others - the explicit protoDir/protoFile will cover it
		}
	}

	// Validate that we have at least one source of proto files
	if protoPath == "" && len(protoDirsToCompile) == 0 {
		return nil, fmt.Errorf("protoFile or protoDir must be provided (auto-derive from protoInclude also failed)")
	}

	// Normalize protoInclude roots to absolute
	var absRoots []string
	for _, p := range protoInclude {
		absPath, err := mustAbs(p)
		if err != nil {
			return nil, err
		}
		absRoots = append(absRoots, absPath)
	}

	// If -proto is given, ensure its directory is an include root
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

	// Add all proto directories as include roots
	for dir := range protoDirsToCompile {
		if !containsDir(absRoots, dir) {
			absRoots = append(absRoots, dir)
		}
	}

	// Build compile list:
	// - If -proto provided, it goes first (priority)
	// - Add all .proto files from all collected directories
	compileNames := make([]string, 0, 64)
	seenCompile := map[string]bool{}

	// Helper to add a file by absolute path: convert to import-style rel to any -I root
	addFile := func(abs string) {
		rel := relToAny(abs, absRoots)
		if rel == "" {
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

	// 2) Walk all proto directories
	for dir := range protoDirsToCompile {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
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
			return nil, fmt.Errorf("walking proto directory %s: %v", dir, err)
		}
	}

	if len(compileNames) == 0 {
		return nil, fmt.Errorf("no .proto files found to compile")
	}

	// Compile all protos once
	c := &protocompile.Compiler{
		Resolver: &protocompile.SourceResolver{ImportPaths: absRoots},
		Reporter: reporter.NewReporter(
			func(e reporter.ErrorWithPos) error { return e },
			func(w reporter.ErrorWithPos) { /* optionally log warnings */ },
		),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	files, err := c.Compile(ctx, compileNames...)
	if err != nil {
		return nil, fmt.Errorf("compile %v (relative to -I: %v): %w", compileNames, absRoots, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files compiled for %v", compileNames)
	}

	// Convert linker.Files to []protoreflect.FileDescriptor
	fileDescs := make([]protoreflect.FileDescriptor, len(files))
	for i, f := range files {
		fileDescs[i] = f
	}

	// Build OutputByMethod map by iterating all services/methods in compiled files
	outputByMethod := make(map[string]protoreflect.MessageDescriptor)
	for _, fd := range files {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			sd := svcs.Get(i)
			svcFull := string(sd.FullName()) // e.g., "homework.v1.Homework"
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				key := svcFull + "/" + string(md.Name())
				outputByMethod[key] = md.Output()
			}
		}
	}

	// Pre-create the type resolver for Any type resolution
	typeResolver := createTypeResolver(fileDescs)

	return &ProtoSchemaCache{
		Files:          fileDescs,
		OutputByMethod: outputByMethod,
		TypeResolver:   typeResolver,
	}, nil
}

// ProtoTextToJSONCached converts Protoscope text to JSON using a pre-built cache.
// This is the fast-path version that should be used within testcase loops.
func ProtoTextToJSONCached(cache *ProtoSchemaCache, methodKey string, text string, logger *zap.Logger) ([]byte, bool) {
	if cache == nil {
		LogError(logger, fmt.Errorf("proto schema cache is nil"), "cannot convert grpc response to json")
		return nil, false
	}

	md, ok := cache.OutputByMethod[methodKey]
	if !ok || md == nil {
		LogError(logger, fmt.Errorf("method %q not found in proto schema cache", methodKey), "cannot convert grpc response to json")
		return nil, false
	}

	// Protoscope text -> raw protobuf wire
	wire, err := ProtoTextToWire(text)
	if err != nil {
		LogError(logger, err, "failed to convert protoscope text to raw protobuf wire, cannot convert grpc response to json")
		return nil, false
	}

	// wire -> JSON using cached type resolver
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(wire, msg); err != nil {
		LogError(logger, err, "failed to unmarshal wire to proto message")
		return nil, false
	}

	j, err := protojson.MarshalOptions{
		Indent:          "  ",
		EmitUnpopulated: true,
		UseProtoNames:   true,
		Resolver:        cache.TypeResolver,
	}.Marshal(msg)
	if err != nil {
		LogError(logger, err, "failed to marshal proto message to json")
		return nil, false
	}

	return j, true
}
