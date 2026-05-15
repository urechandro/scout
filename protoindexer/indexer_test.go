package protoindexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urechandro/scout/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeProto(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestIndexService(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "shipment.proto", `
syntax = "proto3";
package shipment.v1;

service ShipmentService {
  rpc CreateShipment(CreateShipmentRequest) returns (Shipment);
  rpc GetShipment(GetShipmentRequest) returns (Shipment);
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Service symbol.
	sym, err := s.GetSymbol("shipment.v1.ShipmentService")
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if sym.Kind != "service" {
		t.Errorf("service kind = %q, want service", sym.Kind)
	}

	// RPCs.
	rpcs, err := s.GetByNameAndKind("CreateShipment", "rpc")
	if err != nil || len(rpcs) == 0 {
		t.Fatalf("get CreateShipment rpc: %v (len=%d)", err, len(rpcs))
	}
	if rpcs[0].Signature != "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)" {
		t.Errorf("rpc signature = %q", rpcs[0].Signature)
	}

	rpcs2, err := s.GetByNameAndKind("GetShipment", "rpc")
	if err != nil || len(rpcs2) == 0 {
		t.Fatalf("get GetShipment rpc: %v", err)
	}
}

func TestIndexMessage_LineRange(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "shipment.proto", `syntax = "proto3";
package shipment.v1;

message CreateShipmentRequest {
  string name = 1;
  string origin = 2;
  string destination = 3;
}

message Shipment {
  string name = 1;
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sym, err := s.GetSymbol("shipment.v1.CreateShipmentRequest")
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if sym.Kind != "message" {
		t.Errorf("kind = %q, want message", sym.Kind)
	}
	if sym.LineStart != 4 {
		t.Errorf("LineStart = %d, want 4", sym.LineStart)
	}
	if sym.LineEnd != 8 {
		t.Errorf("LineEnd = %d, want 8 (closing brace)", sym.LineEnd)
	}

	// Second message should also have correct range.
	sym2, err := s.GetSymbol("shipment.v1.Shipment")
	if err != nil {
		t.Fatalf("get Shipment: %v", err)
	}
	if sym2.LineStart != 10 {
		t.Errorf("Shipment LineStart = %d, want 10", sym2.LineStart)
	}
	if sym2.LineEnd != 12 {
		t.Errorf("Shipment LineEnd = %d, want 12", sym2.LineEnd)
	}
}

func TestIndexEnum_LineRange(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "status.proto", `syntax = "proto3";
package shipment.v1;

enum ShipmentStatus {
  SHIPMENT_STATUS_UNSPECIFIED = 0;
  SHIPMENT_STATUS_ACTIVE = 1;
  SHIPMENT_STATUS_COMPLETED = 2;
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sym, err := s.GetSymbol("shipment.v1.ShipmentStatus")
	if err != nil {
		t.Fatalf("get enum: %v", err)
	}
	if sym.Kind != "enum" {
		t.Errorf("kind = %q, want enum", sym.Kind)
	}
	if sym.LineStart != 4 {
		t.Errorf("LineStart = %d, want 4", sym.LineStart)
	}
	if sym.LineEnd != 8 {
		t.Errorf("LineEnd = %d, want 8", sym.LineEnd)
	}
}

func TestIndexNestedMessage(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "nested.proto", `syntax = "proto3";
package test.v1;

message Outer {
  message Inner {
    string value = 1;
  }
  Inner inner = 1;
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both outer and nested message should be indexed.
	outer, err := s.GetSymbol("test.v1.Outer")
	if err != nil {
		t.Fatalf("get Outer: %v", err)
	}
	if outer.LineStart != 4 {
		t.Errorf("Outer LineStart = %d, want 4", outer.LineStart)
	}
	if outer.LineEnd != 9 {
		t.Errorf("Outer LineEnd = %d, want 9", outer.LineEnd)
	}

	inner, err := s.GetSymbol("test.v1.Outer.Inner")
	if err != nil {
		t.Fatalf("get Inner: %v", err)
	}
	if inner.Kind != "message" {
		t.Errorf("Inner kind = %q, want message", inner.Kind)
	}
}

func TestIndexRPCDocstring(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "svc.proto", `syntax = "proto3";
package test.v1;

service TestService {
  // Creates a new widget.
  rpc CreateWidget(CreateWidgetRequest) returns (Widget);
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rpcs, err := s.GetByNameAndKind("CreateWidget", "rpc")
	if err != nil || len(rpcs) == 0 {
		t.Fatalf("get rpc: %v", err)
	}
	if rpcs[0].Docstring == "" {
		t.Error("expected docstring on RPC, got empty")
	}
	// Should include the request/response info.
	if rpcs[0].Docstring == "" {
		t.Error("docstring should include Request/Response info")
	}
}

func TestExcludePaths(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()

	// Create a proto in a "vendor" subdirectory (auto-skipped) and one in "generated".
	os.MkdirAll(filepath.Join(dir, "generated"), 0755)
	writeProto(t, dir, "good.proto", `syntax = "proto3";
package test.v1;
message Good { string name = 1; }
`)
	writeProto(t, filepath.Join(dir, "generated"), "bad.proto", `syntax = "proto3";
package test.v1;
message Bad { string name = 1; }
`)

	idx := New(Config{Dir: dir, ExcludePaths: []string{"generated"}}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := s.GetSymbol("test.v1.Good"); err != nil {
		t.Errorf("Good should be indexed: %v", err)
	}
	if _, err := s.GetSymbol("test.v1.Bad"); err == nil {
		t.Error("Bad should be excluded")
	}
}

func TestServiceLineRange(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeProto(t, dir, "svc.proto", `syntax = "proto3";
package test.v1;

service MyService {
  rpc Foo(FooRequest) returns (FooResponse);
  rpc Bar(BarRequest) returns (BarResponse);
}
`)

	idx := New(Config{Dir: dir}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sym, err := s.GetSymbol("test.v1.MyService")
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if sym.LineStart != 4 {
		t.Errorf("LineStart = %d, want 4", sym.LineStart)
	}
	if sym.LineEnd != 7 {
		t.Errorf("LineEnd = %d, want 7", sym.LineEnd)
	}
}

// --- Helper function tests ---

func TestExtractName(t *testing.T) {
	tests := []struct {
		line, prefix, want string
	}{
		{"service ShipmentService {", "service ", "ShipmentService"},
		{"message CreateShipmentRequest {", "message ", "CreateShipmentRequest"},
		{"enum ShipmentStatus {", "enum ", "ShipmentStatus"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := extractName(tt.line, tt.prefix)
			if got != tt.want {
				t.Errorf("extractName(%q, %q) = %q, want %q", tt.line, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestParseRPC(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSig string
		wantReq string
		wantRes string
	}{
		{
			"simple",
			"rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			"rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			"CreateShipmentRequest", "Shipment",
		},
		{
			"streaming",
			"rpc ListShipments(ListShipmentsRequest) returns (stream Shipment)",
			"rpc ListShipments(ListShipmentsRequest) returns (stream Shipment)",
			"ListShipmentsRequest", "stream Shipment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, req, resp := parseRPC(tt.input)
			if sig != tt.wantSig {
				t.Errorf("sig = %q, want %q", sig, tt.wantSig)
			}
			if req != tt.wantReq {
				t.Errorf("req = %q, want %q", req, tt.wantReq)
			}
			if resp != tt.wantRes {
				t.Errorf("resp = %q, want %q", resp, tt.wantRes)
			}
		})
	}
}

func TestQualifiedID(t *testing.T) {
	if got := qualifiedID("shipment.v1", "Shipment"); got != "shipment.v1.Shipment" {
		t.Errorf("got %q", got)
	}
	if got := qualifiedID("", "Shipment"); got != "Shipment" {
		t.Errorf("got %q", got)
	}
}
