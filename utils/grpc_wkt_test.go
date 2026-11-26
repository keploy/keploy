package utils

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func writeProto(t *testing.T, dir, rel, text string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A --proto-dir holds only the user's own files: the well-known types they
// import (google/protobuf/*.proto) come with the compiler, as with protoc and
// as an uploaded schema compiles them.
func TestProtoDirResolvesWellKnownImports(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "shop/orders.proto", `syntax = "proto3";
package shop;
import "google/protobuf/timestamp.proto";
service Orders { rpc GetOrder(GetOrderRequest) returns (Order); }
message GetOrderRequest { string id = 1; }
message Order { string id = 1; google.protobuf.Timestamp created_at = 8; }
`)
	md, files, err := GetProtoMessageDescriptor(context.Background(), zap.NewNop(), models.ProtoConfig{
		ProtoDir:   dir,
		RequestURI: "/shop.Orders/GetOrder",
	})
	if err != nil {
		t.Fatalf("a proto importing a well-known type did not compile: %v", err)
	}
	if md.FullName() != "shop.Order" {
		t.Fatalf("response type = %s, want shop.Order", md.FullName())
	}
	f := md.Fields().ByName("created_at")
	if f == nil || f.Message() == nil || f.Message().FullName() != "google.protobuf.Timestamp" {
		t.Fatalf("created_at does not resolve to google.protobuf.Timestamp: %v", f)
	}
	if len(files) == 0 {
		t.Fatal("no files returned")
	}
}

// A file the user does give wins over the compiler's copy of the same path.
func TestProtoDirOwnImportWinsOverStandard(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "google/protobuf/timestamp.proto", `syntax = "proto3";
package google.protobuf;
message Timestamp { int64 seconds = 1; int32 nanos = 2; string vendored_marker = 3; }
`)
	writeProto(t, dir, "shop/orders.proto", `syntax = "proto3";
package shop;
import "google/protobuf/timestamp.proto";
service Orders { rpc GetOrder(GetOrderRequest) returns (Order); }
message GetOrderRequest { string id = 1; }
message Order { google.protobuf.Timestamp created_at = 8; }
`)
	md, _, err := GetProtoMessageDescriptor(context.Background(), zap.NewNop(), models.ProtoConfig{
		ProtoDir:   dir,
		RequestURI: "/shop.Orders/GetOrder",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ts := md.Fields().ByName("created_at").Message()
	if ts.Fields().ByName("vendored_marker") == nil {
		t.Fatal("the user's own google/protobuf/timestamp.proto was not the one used")
	}
}
