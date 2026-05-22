package query

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func seedSymbols(t *testing.T, s *store.Store, syms []store.Symbol) {
	t.Helper()
	for _, sym := range syms {
		if err := s.UpsertSymbol(sym); err != nil {
			t.Fatalf("upsert %s: %v", sym.ID, err)
		}
	}
}

// --- Query type detection ---

func TestClassifyQuery(t *testing.T) {
	tests := []struct {
		task string
		want queryType
	}{
		// Precise: single compound identifiers
		{"CreateShipmentLeg", queryPrecise},
		{"shipmentLegSvc", queryPrecise},
		{"ValidateToken", queryPrecise},

		// Precise: dotted identifiers
		{"grpc.Dial", queryPrecise},
		{"status.Error", queryPrecise},
		{"codes.NotFound", queryPrecise},
		{"resourcename.Sprint", queryPrecise},

		// Discovery: multi-word natural language
		{"how does auth work", queryDiscovery},
		{"find rate limiting", queryDiscovery},
		{"who calls ValidateToken", queryDiscovery},
		{"CreateShipmentLeg ShipmentLeg service", queryDiscovery},

		// Discovery: single plain word (no compound/dot)
		{"auth", queryDiscovery},
		{"shipment", queryDiscovery},

		// Edge: single uppercase word without case transition
		{"API", queryDiscovery},
		{"ID", queryDiscovery},
	}

	for _, tt := range tests {
		t.Run(tt.task, func(t *testing.T) {
			got := classifyQuery(tt.task)
			if got != tt.want {
				label := map[queryType]string{queryPrecise: "precise", queryDiscovery: "discovery"}
				t.Errorf("classifyQuery(%q) = %s, want %s", tt.task, label[got], label[tt.want])
			}
		})
	}
}

// --- Compound identifier detection ---

func TestIsCompoundIdent(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"CreateShipmentLeg", true},
		{"buildCallGraph", true},
		{"getBody", true},
		{"ID", false},       // too short transition-wise
		{"abc", false},      // all lowercase
		{"ABC", false},      // all uppercase
		{"ab", false},       // too short
		{"A", false},        // too short
		{"parseRPC", true},  // lowercase→uppercase transition
		{"FTS", false},      // all caps, no transition
		{"FTSQuery", false}, // no lowercase→uppercase transition (uppercase→uppercase)
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isCompoundIdent(tt.input)
			if got != tt.want {
				t.Errorf("isCompoundIdent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractCompoundIdents(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"CreateShipmentLeg service", []string{"CreateShipmentLeg"}},
		{"how does buildCallGraph work", []string{"buildCallGraph"}},
		{"find the auth middleware", nil},
		{"CreateShipmentLeg ShipmentLeg service", []string{"CreateShipmentLeg", "ShipmentLeg"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractCompoundIdents(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("extractCompoundIdents(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractCompoundIdents(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDecomposeIdentifier(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"CreateShipmentLeg", "create shipment leg"},
		{"buildCallGraph", "build call graph"},
		{"ID", "i d"},
		{"name", "name"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := decomposeIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("decomposeIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- Scoring functions ---

func TestKindWeight(t *testing.T) {
	tests := []struct {
		kind string
		want float64
	}{
		{"method", 0.3},
		{"func", 0.2},
		{"interface", 0.1},
		{"struct", -0.3},
		{"type", -0.2},
		{"rpc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			sym := store.Symbol{Kind: tt.kind}
			got := kindWeight(sym)
			if got != tt.want {
				t.Errorf("kindWeight(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestNameMatchBonus(t *testing.T) {
	tests := []struct {
		kind string
		want float64
	}{
		{"func", 1.0},
		{"method", 1.0},
		{"interface", 0.7},
		{"struct", 0.3},
		{"type", 0.3},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			sym := store.Symbol{Kind: tt.kind}
			got := nameMatchBonus(sym)
			if got != tt.want {
				t.Errorf("nameMatchBonus(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestImplBoost(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		pkg     string
		want    float64
	}{
		{"method in svc", "method", "myapp/svc", 0.5},
		{"func in server", "func", "myapp/server", 0.5},
		{"method in service", "method", "myapp/service", 0.5},
		{"method elsewhere", "method", "myapp/auth", 0},
		{"struct in svc", "struct", "myapp/svc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sym := store.Symbol{Kind: tt.kind, Package: tt.pkg}
			got := implBoost(sym)
			if got != tt.want {
				t.Errorf("implBoost(%s, %s) = %v, want %v", tt.kind, tt.pkg, got, tt.want)
			}
		})
	}
}

func TestGeneratedPenalty(t *testing.T) {
	tests := []struct {
		file string
		want float64
	}{
		{"shipment.pb.go", -0.6},
		{"shipment.pb.gw.go", -0.6},
		{"shipment.go", 0},
		{"shipment_test.go", 0},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			sym := store.Symbol{File: tt.file}
			got := generatedPenalty(sym)
			if got != tt.want {
				t.Errorf("generatedPenalty(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func TestTermCoverage(t *testing.T) {
	sym := store.Symbol{
		Name:      "CreateShipmentLeg",
		Package:   "myapp/svc",
		Signature: "func CreateShipmentLeg(req *CreateShipmentLegRequest) error",
	}
	decomposed := decomposeIdentifier(sym.Name)

	// Single term → always 0.
	got := termCoverage(sym, []string{"create"}, decomposed)
	if got != 0 {
		t.Errorf("single term should return 0, got %v", got)
	}

	// Two terms, both match → coverage = 1.0 → 1.0² × 1.5 = 1.5
	got = termCoverage(sym, []string{"create", "shipment"}, decomposed)
	if got != 1.5 {
		t.Errorf("two matching terms: got %v, want 1.5", got)
	}

	// Three terms, two match → coverage = 2/3 → (2/3)² × 1.5 ≈ 0.667
	got = termCoverage(sym, []string{"create", "shipment", "unrelated"}, decomposed)
	if got < 0.6 || got > 0.7 {
		t.Errorf("two of three terms: got %v, want ~0.667", got)
	}

	// No matches → 0.
	got = termCoverage(sym, []string{"unrelated", "other"}, decomposed)
	if got != 0 {
		t.Errorf("no matches should return 0, got %v", got)
	}
}

// --- FTS query building ---

func TestBuildFTSQuery(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"find the auth middleware", "find OR auth OR middleware"},
		{"CreateShipmentLeg", "createshipmentleg"},
		{"how do I add pagination", "pagination"},
		{"a the to", "a the to"}, // all stop words → falls through to raw
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := buildFTSQuery(tt.input)
			if got != tt.want {
				t.Errorf("buildFTSQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- Dedup ---

func TestDedupGenPath(t *testing.T) {
	scored := map[string]*SymbolSummary{
		"myapp/gen/backend/shipment.CreateShipment": {
			ID: "myapp/gen/backend/shipment.CreateShipment", Kind: "method",
			File: "/gen/backend/shipment.go", Score: 2.0,
		},
		"myapp/gen/frontend/shipment.CreateShipment": {
			ID: "myapp/gen/frontend/shipment.CreateShipment", Kind: "method",
			File: "/gen/frontend/shipment.go", Score: 1.5,
		},
		"myapp/svc.Server.CreateShipment": {
			ID: "myapp/svc.Server.CreateShipment", Kind: "method",
			File: "/svc/server.go", Score: 3.0,
		},
	}

	result := dedup(scored)

	// The non-gen symbol should always survive.
	if _, ok := result["myapp/svc.Server.CreateShipment"]; !ok {
		t.Error("non-gen symbol was removed")
	}
	// Of the two gen symbols, only backend should survive.
	if _, ok := result["myapp/gen/backend/shipment.CreateShipment"]; !ok {
		t.Error("backend gen symbol was removed")
	}
	if _, ok := result["myapp/gen/frontend/shipment.CreateShipment"]; ok {
		t.Error("frontend gen symbol should have been deduped")
	}
}

func TestDedupNameBoilerplate(t *testing.T) {
	// 4 symbols named "Validate" on different types → should collapse to 1.
	scored := map[string]*SymbolSummary{
		"myapp/shipment.ShipmentResource.Validate": {
			ID: "myapp/shipment.ShipmentResource.Validate", Kind: "method",
			File: "/shipment/resource.go", Score: 2.0,
		},
		"myapp/activity.ActivityResource.Validate": {
			ID: "myapp/activity.ActivityResource.Validate", Kind: "method",
			File: "/activity/resource.go", Score: 1.8,
		},
		"myapp/order.OrderResource.Validate": {
			ID: "myapp/order.OrderResource.Validate", Kind: "method",
			File: "/order/resource.go", Score: 1.5,
		},
		"myapp/route.RouteResource.Validate": {
			ID: "myapp/route.RouteResource.Validate", Kind: "method",
			File: "/route/resource.go", Score: 1.0,
		},
	}

	result := dedup(scored)

	if len(result) != 1 {
		t.Errorf("expected 1 symbol after name dedup, got %d", len(result))
	}
	// The highest-scored one should survive.
	survivor, ok := result["myapp/shipment.ShipmentResource.Validate"]
	if !ok {
		t.Error("highest-scored Validate was not kept")
		return
	}
	// Should have a penalty applied.
	if survivor.Score >= 2.0 {
		t.Errorf("expected penalty on survivor, score = %v", survivor.Score)
	}
}

// --- Integration: GetRelevantContext with exact name lookup ---

func TestGetRelevantContext_ExactNameLookup(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipmentLeg", Package: "myapp/svc",
			Name: "CreateShipmentLeg", Kind: "method",
			Signature: "func (s *Server) CreateShipmentLeg(ctx context.Context, req *CreateShipmentLegRequest) (*ShipmentLeg, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 50,
		},
		{
			ID: "myapp/model.ShipmentLeg", Package: "myapp/model",
			Name: "ShipmentLeg", Kind: "struct",
			Signature: "type ShipmentLeg struct",
			File: "/model/shipment.go", LineStart: 5, LineEnd: 20,
		},
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(token string) (*Claims, error)",
			File: "/auth/auth.go", LineStart: 1, LineEnd: 10,
		},
	})

	engine := New(s)
	resp, err := engine.GetRelevantContext(ContextRequest{
		Task:         "CreateShipmentLeg",
		BudgetTokens: 4000,
		Verbose:      true,
	})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}

	if len(resp.Symbols) == 0 {
		t.Fatal("expected symbols, got none")
	}

	// The method should be the top result (exact match + method kindWeight + implBoost).
	top := resp.Symbols[0]
	if top.ID != "myapp/svc.Server.CreateShipmentLeg" {
		t.Errorf("top result = %q, want myapp/svc.Server.CreateShipmentLeg", top.ID)
	}
	if top.Score < 3.0 {
		t.Errorf("exact match score = %v, want >= 3.0", top.Score)
	}

	// ValidateToken should NOT appear — it doesn't match the query.
	for _, sym := range resp.Symbols {
		if sym.ID == "myapp/auth.ValidateToken" {
			t.Error("ValidateToken should not appear for CreateShipmentLeg query")
		}
	}
}

func TestGetRelevantContext_FTSFallback(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(token string) (*Claims, error)",
			Docstring: "validates authentication tokens",
			File: "/auth/auth.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/auth.RefreshToken", Package: "myapp/auth",
			Name: "RefreshToken", Kind: "func",
			Signature: "func RefreshToken(token string) (string, error)",
			Docstring: "refreshes an expired authentication token",
			File: "/auth/refresh.go", LineStart: 1, LineEnd: 10,
		},
	})

	engine := New(s)
	resp, err := engine.GetRelevantContext(ContextRequest{
		Task:         "find auth token validation",
		BudgetTokens: 4000,
	})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}

	if len(resp.Symbols) == 0 {
		t.Fatal("expected FTS results, got none")
	}

	// Both auth symbols should appear.
	ids := map[string]bool{}
	for _, sym := range resp.Symbols {
		ids[sym.ID] = true
	}
	if !ids["myapp/auth.ValidateToken"] {
		t.Error("expected ValidateToken in FTS results")
	}
}

// --- GetUnimplemented ---

func TestGetUnimplemented_MissingRPC(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "shipment.v1.ShipmentService", Package: "shipment.v1",
			Name: "ShipmentService", Kind: "service",
			Signature: "service ShipmentService",
			File: "/proto/shipment.proto", LineStart: 5, LineEnd: 10,
		},
		{
			ID: "shipment.v1.ShipmentService.CreateShipment", Package: "shipment.v1",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			File: "/proto/shipment.proto", LineStart: 6, LineEnd: 6,
		},
		{
			ID: "shipment.v1.ShipmentService.GetShipment", Package: "shipment.v1",
			Name: "GetShipment", Kind: "rpc",
			Signature: "rpc GetShipment(GetShipmentRequest) returns (Shipment)",
			File: "/proto/shipment.proto", LineStart: 7, LineEnd: 7,
		},
		{
			ID: "shipment.v1.ShipmentService.DeleteShipment", Package: "shipment.v1",
			Name: "DeleteShipment", Kind: "rpc",
			Signature: "rpc DeleteShipment(DeleteShipmentRequest) returns (Empty)",
			File: "/proto/shipment.proto", LineStart: 8, LineEnd: 8,
		},
		// Only CreateShipment has a Go implementation.
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 50,
			Body: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error) {\n\treturn &Shipment{}, nil\n}",
		},
	})

	engine := New(s)
	resp, err := engine.GetUnimplemented("ShipmentService")
	if err != nil {
		t.Fatalf("GetUnimplemented: %v", err)
	}

	if resp.Service != "ShipmentService" {
		t.Errorf("Service = %q, want ShipmentService", resp.Service)
	}
	if resp.TotalRPCs != 3 {
		t.Errorf("TotalRPCs = %d, want 3", resp.TotalRPCs)
	}
	if resp.Implemented != 1 {
		t.Errorf("Implemented = %d, want 1", resp.Implemented)
	}
	if len(resp.Unimplemented) != 2 {
		t.Fatalf("Unimplemented = %d, want 2", len(resp.Unimplemented))
	}

	names := map[string]string{}
	for _, u := range resp.Unimplemented {
		names[u.Name] = u.Status
	}
	if names["GetShipment"] != "missing" {
		t.Errorf("GetShipment status = %q, want missing", names["GetShipment"])
	}
	if names["DeleteShipment"] != "missing" {
		t.Errorf("DeleteShipment status = %q, want missing", names["DeleteShipment"])
	}

	// Check request/response messages are parsed.
	for _, u := range resp.Unimplemented {
		if u.RequestMessage == "" {
			t.Errorf("%s: missing request message", u.Name)
		}
		if u.ResponseMessage == "" {
			t.Errorf("%s: missing response message", u.Name)
		}
	}
}

func TestGetUnimplemented_StubbedRPC(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()

	// Write a stub Go file so readLines can find it.
	stubBody := `func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error) {
	return nil, status.Errorf(codes.Unimplemented, "CreateShipment not implemented")
}`
	stubPath := filepath.Join(dir, "server.go")
	os.WriteFile(stubPath, []byte(stubBody), 0644)

	seedSymbols(t, s, []store.Symbol{
		{
			ID: "test.v1.TestService", Package: "test.v1",
			Name: "TestService", Kind: "service",
			Signature: "service TestService",
			File: "/proto/test.proto", LineStart: 1, LineEnd: 5,
		},
		{
			ID: "test.v1.TestService.CreateShipment", Package: "test.v1",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			File: "/proto/test.proto", LineStart: 2, LineEnd: 2,
		},
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error)",
			File: stubPath, LineStart: 1, LineEnd: 3,
			Body: fmt.Sprintf("/* %s:1-3 */", stubPath),
		},
	})

	engine := New(s)
	resp, err := engine.GetUnimplemented("TestService")
	if err != nil {
		t.Fatalf("GetUnimplemented: %v", err)
	}

	if len(resp.Unimplemented) != 1 {
		t.Fatalf("expected 1 unimplemented, got %d", len(resp.Unimplemented))
	}
	if resp.Unimplemented[0].Status != "stubbed" {
		t.Errorf("status = %q, want stubbed", resp.Unimplemented[0].Status)
	}
}

func TestGetUnimplemented_FullyImplemented(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "test.v1.Svc", Package: "test.v1",
			Name: "Svc", Kind: "service",
			Signature: "service Svc",
			File: "/proto/test.proto", LineStart: 1, LineEnd: 5,
		},
		{
			ID: "test.v1.Svc.Ping", Package: "test.v1",
			Name: "Ping", Kind: "rpc",
			Signature: "rpc Ping(PingRequest) returns (PingResponse)",
			File: "/proto/test.proto", LineStart: 2, LineEnd: 2,
		},
		{
			ID: "myapp/svc.Server.Ping", Package: "myapp/svc",
			Name: "Ping", Kind: "method",
			Signature: "func (s *Server) Ping(ctx context.Context, req *PingRequest) (*PingResponse, error)",
			File: "/svc/server.go", LineStart: 1, LineEnd: 5,
			Body: "func (s *Server) Ping(ctx context.Context, req *PingRequest) (*PingResponse, error) {\n\treturn &PingResponse{}, nil\n}",
		},
	})

	engine := New(s)
	resp, err := engine.GetUnimplemented("Svc")
	if err != nil {
		t.Fatalf("GetUnimplemented: %v", err)
	}

	if len(resp.Unimplemented) != 0 {
		t.Errorf("expected 0 unimplemented, got %d", len(resp.Unimplemented))
	}
	if resp.Implemented != 1 {
		t.Errorf("Implemented = %d, want 1", resp.Implemented)
	}
}

func TestGetUnimplemented_ServiceNotFound(t *testing.T) {
	s := newTestStore(t)
	engine := New(s)
	_, err := engine.GetUnimplemented("NonexistentService")
	if err == nil {
		t.Error("expected error for nonexistent service")
	}
}

// --- readLines ---

func TestReadLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	content := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := readLines(path, 2, 4)
	if err != nil {
		t.Fatalf("readLines: %v", err)
	}
	want := "line 2\nline 3\nline 4"
	if got != want {
		t.Errorf("readLines(2,4) = %q, want %q", got, want)
	}
}

// --- GetBody with references ---

func TestGetBody_References(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
			Body: `func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error) {
	if err := ValidateToken(ctx); err != nil {
		return nil, err
	}
	leg := BuildShipmentLeg(req)
	return s.repo.Save(ctx, leg)
}`,
		},
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(ctx context.Context) error",
			File: "/auth/auth.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/builder.BuildShipmentLeg", Package: "myapp/builder",
			Name: "BuildShipmentLeg", Kind: "func",
			Signature: "func BuildShipmentLeg(req *CreateShipmentRequest) *ShipmentLeg",
			File: "/builder/leg.go", LineStart: 1, LineEnd: 15,
		},
		{
			ID: "myapp/model.Shipment", Package: "myapp/model",
			Name: "Shipment", Kind: "struct",
			Signature: "type Shipment struct",
			File: "/model/shipment.go", LineStart: 1, LineEnd: 10,
		},
	})

	engine := New(s)
	resp, err := engine.GetBody("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}

	if len(resp.References) == 0 {
		t.Fatal("expected references, got none")
	}

	refIDs := map[string]bool{}
	for _, ref := range resp.References {
		refIDs[ref.ID] = true
	}

	if !refIDs["myapp/auth.ValidateToken"] {
		t.Error("expected ValidateToken in references")
	}
	if !refIDs["myapp/builder.BuildShipmentLeg"] {
		t.Error("expected BuildShipmentLeg in references")
	}

	// Self should not appear.
	if refIDs["myapp/svc.Server.CreateShipment"] {
		t.Error("self should not appear in references")
	}

	// References should have signatures but no bodies.
	for _, ref := range resp.References {
		if ref.Signature == "" {
			t.Errorf("reference %s has empty signature", ref.ID)
		}
		if ref.Why != "referenced" {
			t.Errorf("reference %s has why=%q, want 'referenced'", ref.ID, ref.Why)
		}
	}
}

func TestGetBody_EmptyID_ReturnsError(t *testing.T) {
	s := newTestStore(t)
	// Seed a short symbol so that a wildcard LIKE '%' would return it if the
	// guard were absent — confirming the fix isn't a vacuous pass.
	seedSymbols(t, s, []store.Symbol{
		{ID: "proto.Check", Package: "proto", Name: "Check", Kind: "func", Signature: "func Check()", File: "check.go"},
	})

	engine := New(s)
	_, err := engine.GetBody("")
	if err == nil {
		t.Fatal("GetBody(\"\") returned nil error, want an error")
	}
}

func TestGetBody_NoReferences_EmptyBody(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/model.Shipment", Package: "myapp/model",
			Name: "Shipment", Kind: "struct",
			Signature: "type Shipment struct",
			File: "/model/shipment.go", LineStart: 1, LineEnd: 10,
			Body: "",
		},
	})

	engine := New(s)
	resp, err := engine.GetBody("myapp/model.Shipment")
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}

	if len(resp.References) != 0 {
		t.Errorf("expected no references for empty body, got %d", len(resp.References))
	}
}

// --- GetPattern with message bodies ---

func TestGetPattern_MessageBodies(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()

	reqBody := "message CreateShipmentRequest {\n  string name = 1;\n  string origin = 2;\n}"
	reqPath := filepath.Join(dir, "request.proto")
	os.WriteFile(reqPath, []byte(reqBody), 0644)

	respBody := "message Shipment {\n  string id = 1;\n  string name = 2;\n}"
	respPath := filepath.Join(dir, "response.proto")
	os.WriteFile(respPath, []byte(respBody), 0644)

	seedSymbols(t, s, []store.Symbol{
		{
			ID: "shipment.v1.ShipmentService.CreateShipment", Package: "shipment.v1",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			File: filepath.Join(dir, "shipment.proto"), LineStart: 1, LineEnd: 1,
		},
		{
			ID: "shipment.v1.CreateShipmentRequest", Package: "shipment.v1",
			Name: "CreateShipmentRequest", Kind: "message",
			Signature: "message CreateShipmentRequest",
			File: reqPath, LineStart: 1, LineEnd: 4,
			Body: fmt.Sprintf("/* %s:1-4 */", reqPath),
		},
		{
			ID: "shipment.v1.Shipment", Package: "shipment.v1",
			Name: "Shipment", Kind: "message",
			Signature: "message Shipment",
			File: respPath, LineStart: 1, LineEnd: 4,
			Body: fmt.Sprintf("/* %s:1-4 */", respPath),
		},
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
			Body: "func (s *Server) CreateShipment(ctx context.Context, req *CreateShipmentRequest) (*Shipment, error) {\n\treturn &Shipment{}, nil\n}",
		},
	})

	engine := New(s)
	resp, err := engine.GetPattern("CreateShipment")
	if err != nil {
		t.Fatalf("GetPattern: %v", err)
	}

	if resp.RequestMessage == nil {
		t.Fatal("expected request message, got nil")
	}
	if resp.RequestMessage.Body == "" {
		t.Error("request message body is empty")
	}
	if resp.RequestMessage.Body != reqBody {
		t.Errorf("request message body = %q, want %q", resp.RequestMessage.Body, reqBody)
	}

	if resp.ResponseMessage == nil {
		t.Fatal("expected response message, got nil")
	}
	if resp.ResponseMessage.Body == "" {
		t.Error("response message body is empty")
	}
	if resp.ResponseMessage.Body != respBody {
		t.Errorf("response message body = %q, want %q", resp.ResponseMessage.Body, respBody)
	}
}

// --- GetImpact ---

func TestGetImpact_CrossLayerLinkage(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/proto.ShipmentService.CreateShipment", Package: "myapp/proto",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			File: "/proto/shipment.proto", LineStart: 5, LineEnd: 5,
		},
		{
			ID: "myapp/gen.CreateShipment", Package: "myapp/gen",
			Name: "CreateShipment", Kind: "method",
			Signature: "func CreateShipment(ctx, req) (*resp, error)",
			File: "/gen/shipment.pb.go", LineStart: 100, LineEnd: 120,
		},
		{
			ID: "myapp/svc_test.TestCreateShipment", Package: "myapp/svc_test",
			Name: "TestCreateShipment", Kind: "func",
			Signature: "func TestCreateShipment(t *testing.T)",
			File: "/svc/server_test.go", LineStart: 50, LineEnd: 80,
		},
	})

	// Edge: Go impl implements the RPC
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/proto.ShipmentService.CreateShipment", Kind: "implements"}); err != nil {
		t.Fatal(err)
	}
	// Edge: test calls the impl
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc_test.TestCreateShipment", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	resp, err := engine.GetImpact("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetImpact: %v", err)
	}

	if resp.Symbol.ID != "myapp/svc.Server.CreateShipment" {
		t.Errorf("target = %s, want myapp/svc.Server.CreateShipment", resp.Symbol.ID)
	}

	// Proto should appear via same-name lookup.
	if len(resp.Proto) == 0 {
		t.Error("expected proto layer hit for CreateShipment")
	}

	// Generated should appear via same-name lookup.
	if len(resp.Generated) == 0 {
		t.Error("expected generated layer hit for CreateShipment")
	}

	// Test should appear as caller.
	foundTest := false
	for _, s := range resp.Tests {
		if s.ID == "myapp/svc_test.TestCreateShipment" {
			foundTest = true
		}
	}
	if !foundTest {
		t.Error("expected test caller TestCreateShipment")
	}

	if resp.Total == 0 {
		t.Error("expected non-zero total")
	}
}

func TestGetImpact_RPCMessages(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/proto.ShipmentService.CreateShipment", Package: "myapp/proto",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (CreateShipmentResponse)",
			File: "/proto/shipment.proto", LineStart: 5, LineEnd: 5,
		},
		{
			ID: "myapp/proto.CreateShipmentRequest", Package: "myapp/proto",
			Name: "CreateShipmentRequest", Kind: "message",
			Signature: "message CreateShipmentRequest",
			File: "/proto/shipment.proto", LineStart: 10, LineEnd: 20,
		},
		{
			ID: "myapp/proto.CreateShipmentResponse", Package: "myapp/proto",
			Name: "CreateShipmentResponse", Kind: "message",
			Signature: "message CreateShipmentResponse",
			File: "/proto/shipment.proto", LineStart: 22, LineEnd: 30,
		},
	})

	engine := New(s)
	resp, err := engine.GetImpact("myapp/proto.ShipmentService.CreateShipment")
	if err != nil {
		t.Fatalf("GetImpact: %v", err)
	}

	// Both request and response messages should appear.
	allIDs := map[string]bool{}
	for _, s := range resp.Proto {
		allIDs[s.ID] = true
	}
	if !allIDs["myapp/proto.CreateShipmentRequest"] {
		t.Error("expected CreateShipmentRequest in impact")
	}
	if !allIDs["myapp/proto.CreateShipmentResponse"] {
		t.Error("expected CreateShipmentResponse in impact")
	}
}

func TestGetImpact_SymbolNotFound(t *testing.T) {
	s := newTestStore(t)
	engine := New(s)
	_, err := engine.GetImpact("nonexistent.Symbol")
	if err == nil {
		t.Error("expected error for non-existent symbol")
	}
}

// --- GetFlow ---

func TestGetFlow_WithCallersAndCallees(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
			Body: "func (s *Server) CreateShipment(ctx, req) (*resp, error) {\n\treturn nil, nil\n}",
		},
		{
			ID: "myapp/handler.HandleCreate", Package: "myapp/handler",
			Name: "HandleCreate", Kind: "func",
			Signature: "func HandleCreate()",
			File: "/handler/create.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/repo.Save", Package: "myapp/repo",
			Name: "Save", Kind: "method",
			Signature: "func (r *Repo) Save(ctx, obj) error",
			File: "/repo/repo.go", LineStart: 1, LineEnd: 15,
		},
	})

	// HandleCreate calls CreateShipment, CreateShipment calls Save.
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleCreate", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/repo.Save", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	resp, err := engine.GetFlow("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetFlow: %v", err)
	}

	if resp.Symbol.ID != "myapp/svc.Server.CreateShipment" {
		t.Errorf("symbol = %s, want CreateShipment", resp.Symbol.ID)
	}
	if resp.Symbol.Body == "" {
		t.Error("expected body on target symbol")
	}

	if len(resp.Callers) != 1 || resp.Callers[0].ID != "myapp/handler.HandleCreate" {
		t.Errorf("callers = %v, want [HandleCreate]", resp.Callers)
	}
	if len(resp.Callees) != 1 || resp.Callees[0].ID != "myapp/repo.Save" {
		t.Errorf("callees = %v, want [Save]", resp.Callees)
	}
}

func TestGetFlow_SymbolNotFound(t *testing.T) {
	s := newTestStore(t)
	engine := New(s)
	_, err := engine.GetFlow("nonexistent.Symbol")
	if err == nil {
		t.Error("expected error for non-existent symbol")
	}
}

// --- GetCallers ---

func TestGetCallers_DirectEdges(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/handler.HandleCreate", Package: "myapp/handler",
			Name: "HandleCreate", Kind: "func",
			Signature: "func HandleCreate()",
			File: "/handler/create.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/handler.HandleBatch", Package: "myapp/handler",
			Name: "HandleBatch", Kind: "func",
			Signature: "func HandleBatch()",
			File: "/handler/batch.go", LineStart: 1, LineEnd: 10,
		},
	})

	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleCreate", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleBatch", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	callers, err := engine.GetCallers("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}

	if len(callers) != 2 {
		t.Fatalf("expected 2 callers, got %d", len(callers))
	}

	ids := map[string]bool{}
	for _, c := range callers {
		ids[c.ID] = true
		if c.Why != "direct caller" {
			t.Errorf("caller %s has why=%q, want 'direct caller'", c.ID, c.Why)
		}
	}
	if !ids["myapp/handler.HandleCreate"] || !ids["myapp/handler.HandleBatch"] {
		t.Errorf("missing expected callers: %v", ids)
	}
}

func TestGetCallers_FallbackToImplements(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/proto.ShipmentService.CreateShipment", Package: "myapp/proto",
			Name: "CreateShipment", Kind: "rpc",
			Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)",
			File: "/proto/shipment.proto", LineStart: 5, LineEnd: 5,
		},
	})

	// Impl implements the RPC but has no direct callers.
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/proto.ShipmentService.CreateShipment", Kind: "implements"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	callers, err := engine.GetCallers("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}

	if len(callers) != 1 {
		t.Fatalf("expected 1 implements fallback, got %d", len(callers))
	}
	if callers[0].ID != "myapp/proto.ShipmentService.CreateShipment" {
		t.Errorf("fallback = %s, want proto RPC", callers[0].ID)
	}
	if callers[0].Why != "gRPC entry point: rpc CreateShipment(CreateShipmentRequest) returns (Shipment)" {
		t.Errorf("unexpected why: %s", callers[0].Why)
	}
}

func TestGetCallers_FallbackToBodyReference(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/util.NewClient", Package: "myapp/util",
			Name: "NewClient", Kind: "func",
			Signature: "func NewClient() *Client",
			File: "/util/client.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/main.setup", Package: "myapp/main",
			Name: "setup", Kind: "func",
			Signature: "func setup()",
			File: "/main.go", LineStart: 1, LineEnd: 20,
			Body: "func setup() {\n\tc := NewClient()\n\t_ = c\n}",
		},
	})

	engine := New(s)
	callers, err := engine.GetCallers("myapp/util.NewClient")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}

	if len(callers) != 1 {
		t.Fatalf("expected 1 body-reference caller, got %d", len(callers))
	}
	if callers[0].ID != "myapp/main.setup" {
		t.Errorf("body caller = %s, want main.setup", callers[0].ID)
	}
}

// --- GetCallees ---

func TestGetCallees_DirectEdges(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(ctx) error",
			File: "/auth/auth.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/repo.Save", Package: "myapp/repo",
			Name: "Save", Kind: "method",
			Signature: "func (r *Repo) Save(ctx, obj) error",
			File: "/repo/repo.go", LineStart: 1, LineEnd: 15,
		},
	})

	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/auth.ValidateToken", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/repo.Save", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	callees, err := engine.GetCallees("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetCallees: %v", err)
	}

	if len(callees) != 2 {
		t.Fatalf("expected 2 callees, got %d", len(callees))
	}

	ids := map[string]bool{}
	for _, c := range callees {
		ids[c.ID] = true
	}
	if !ids["myapp/auth.ValidateToken"] || !ids["myapp/repo.Save"] {
		t.Errorf("missing expected callees: %v", ids)
	}
}

func TestGetCallees_FallbackToBody(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
			Body: "func (s *Server) CreateShipment(ctx, req) (*resp, error) {\n\tValidateToken(ctx)\n\treturn nil, nil\n}",
		},
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(ctx) error",
			File: "/auth/auth.go", LineStart: 1, LineEnd: 10,
		},
	})

	// No call edges — should fall back to body extraction.
	engine := New(s)
	callees, err := engine.GetCallees("myapp/svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetCallees: %v", err)
	}

	if len(callees) == 0 {
		t.Fatal("expected body-fallback callees, got none")
	}

	found := false
	for _, c := range callees {
		if c.ID == "myapp/auth.ValidateToken" {
			found = true
		}
	}
	if !found {
		t.Error("expected ValidateToken in body-fallback callees")
	}
}

// --- GetConventions ---

func TestGetConventions_DocumentedConvention(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
	})

	if err := s.UpsertConvention(store.Convention{
		Name:        "transactional-outbox",
		Terms:       []string{"outbox", "event", "transactional"},
		Description: "Use a transactional outbox to publish domain events atomically.",
		Structure:   "1. Begin TX\n2. Write entity\n3. Write outbox row\n4. Commit",
		Examples:    []string{"svc.Server.CreateShipment"},
	}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	result, err := engine.GetConventions("transactional outbox")
	if err != nil {
		t.Fatalf("GetConventions: %v", err)
	}

	if result.Name != "transactional-outbox" {
		t.Errorf("name = %q, want transactional-outbox", result.Name)
	}
	if result.Description == "" {
		t.Error("expected description")
	}
	if result.Structure == "" {
		t.Error("expected structure")
	}
	if len(result.Examples) == 0 {
		t.Error("expected resolved examples")
	}
}

func TestGetConventions_FTSFallback(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/middleware.RateLimit", Package: "myapp/middleware",
			Name: "RateLimit", Kind: "func",
			Signature: "func RateLimit(next http.Handler) http.Handler",
			File: "/middleware/rate.go", LineStart: 1, LineEnd: 20,
		},
		{
			ID: "myapp/middleware.RateLimiter", Package: "myapp/middleware",
			Name: "RateLimiter", Kind: "struct",
			Signature: "type RateLimiter struct",
			File: "/middleware/rate.go", LineStart: 22, LineEnd: 30,
		},
	})

	engine := New(s)
	result, err := engine.GetConventions("rate limiting")
	if err != nil {
		t.Fatalf("GetConventions: %v", err)
	}

	if len(result.Examples) == 0 {
		t.Error("expected FTS fallback results")
	}
	if result.Hint == "" {
		t.Error("expected hint when falling back to FTS")
	}
}

func TestGetConventions_NoMatch(t *testing.T) {
	s := newTestStore(t)
	engine := New(s)
	result, err := engine.GetConventions("quantum computing")
	if err != nil {
		t.Fatalf("GetConventions: %v", err)
	}

	if result.Hint == "" {
		t.Error("expected hint when nothing matches")
	}
}

// --- Graph expansion ---

func TestExpand_OneHop(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/handler.HandleCreate", Package: "myapp/handler",
			Name: "HandleCreate", Kind: "func",
			Signature: "func HandleCreate()",
			File: "/handler/create.go", LineStart: 1, LineEnd: 10,
		},
		{
			ID: "myapp/repo.Save", Package: "myapp/repo",
			Name: "Save", Kind: "method",
			Signature: "func (r *Repo) Save(ctx, obj) error",
			File: "/repo/repo.go", LineStart: 1, LineEnd: 15,
		},
	})

	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleCreate", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/repo.Save", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	seeds := map[string]*SymbolSummary{
		"myapp/svc.Server.CreateShipment": {ID: "myapp/svc.Server.CreateShipment"},
	}
	expanded, err := engine.expand(seeds, 1)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if _, ok := expanded["myapp/handler.HandleCreate"]; !ok {
		t.Error("expected caller HandleCreate in expansion")
	}
	if _, ok := expanded["myapp/repo.Save"]; !ok {
		t.Error("expected callee Save in expansion")
	}
	// Seed should not be in result.
	if _, ok := expanded["myapp/svc.Server.CreateShipment"]; ok {
		t.Error("seed should not appear in expansion result")
	}
}

func TestExpand_ZeroDepth(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/repo.Save", Package: "myapp/repo",
			Name: "Save", Kind: "method",
			Signature: "func (r *Repo) Save(ctx, obj) error",
			File: "/repo/repo.go", LineStart: 1, LineEnd: 15,
		},
	})

	if err := s.UpsertEdge(store.Edge{FromID: "myapp/svc.Server.CreateShipment", ToID: "myapp/repo.Save", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	seeds := map[string]*SymbolSummary{
		"myapp/svc.Server.CreateShipment": {ID: "myapp/svc.Server.CreateShipment"},
	}
	expanded, err := engine.expand(seeds, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(expanded) != 0 {
		t.Errorf("expand(depth=0) should return empty, got %d", len(expanded))
	}
}

func TestExpand_CallerScore(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/handler.HandleCreate", Package: "myapp/handler",
			Name: "HandleCreate", Kind: "func",
			Signature: "func HandleCreate()",
			File: "/handler/create.go", LineStart: 1, LineEnd: 10,
		},
	})

	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleCreate", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	seeds := map[string]*SymbolSummary{
		"myapp/svc.Server.CreateShipment": {ID: "myapp/svc.Server.CreateShipment"},
	}
	expanded, err := engine.expand(seeds, 1)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	caller := expanded["myapp/handler.HandleCreate"]
	if caller == nil {
		t.Fatal("expected HandleCreate in expansion")
	}
	// Caller at depth 1 should have score 0.4/1 = 0.4
	if caller.Score != 0.4 {
		t.Errorf("caller score = %f, want 0.4", caller.Score)
	}
}

// --- classifyLayer ---

func TestClassifyLayer(t *testing.T) {
	tests := []struct {
		file string
		want ImpactLayer
	}{
		{"/proto/shipment.proto", LayerProto},
		{"/gen/shipment.pb.go", LayerGenerated},
		{"/gen/shipment.pb.gw.go", LayerGenerated},
		{"/svc/server.go", LayerImplementation},
		{"/svc/server_test.go", LayerTest},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := classifyLayer(tt.file)
			if got != tt.want {
				t.Errorf("classifyLayer(%q) = %q, want %q", tt.file, got, tt.want)
			}
		})
	}
}

// --- dedupGenerated ---

func TestDedupGenerated_CollapsesCopies(t *testing.T) {
	syms := []SymbolSummary{
		{ID: "myapp/gen/frontend.CreateShipment", Kind: "method", File: "/gen/frontend/shipment.pb.go", Why: "generated code"},
		{ID: "myapp/gen/backend.CreateShipment", Kind: "method", File: "/gen/backend/shipment.pb.go", Why: "generated code"},
		{ID: "myapp/gen/frontend.Shipment", Kind: "struct", File: "/gen/frontend/shipment.pb.go", Why: "generated code"},
	}

	result := dedupGenerated(syms)
	if len(result) != 2 {
		t.Fatalf("expected 2 after dedup (method + struct), got %d: %v", len(result), result)
	}

	// The backend copy should be preferred for the method.
	for _, s := range result {
		if s.Kind == "method" {
			if s.ID != "myapp/gen/backend.CreateShipment" {
				t.Errorf("expected backend copy, got %s", s.ID)
			}
			if !strings.Contains(s.Why, "+1 copies") {
				t.Errorf("expected dedup annotation in Why, got %q", s.Why)
			}
		}
	}
}


// --- parseRPCMessages ---

func TestParseRPCMessages(t *testing.T) {
	tests := []struct {
		sig      string
		wantReq  string
		wantResp string
	}{
		{"rpc CreateShipment(CreateShipmentRequest) returns (Shipment)", "CreateShipmentRequest", "Shipment"},
		{"rpc ListLegs(ListLegsRequest) returns (ListLegsResponse)", "ListLegsRequest", "ListLegsResponse"},
		{"not an RPC signature", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.sig, func(t *testing.T) {
			req, resp := parseRPCMessages(tt.sig)
			if req != tt.wantReq {
				t.Errorf("req = %q, want %q", req, tt.wantReq)
			}
			if resp != tt.wantResp {
				t.Errorf("resp = %q, want %q", resp, tt.wantResp)
			}
		})
	}
}

// --- Fuzzy resolution in tools ---

func TestGetCallers_FuzzyResolution(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "myapp/svc.Server.CreateShipment", Package: "myapp/svc",
			Name: "CreateShipment", Kind: "method",
			Signature: "func (s *Server) CreateShipment(ctx, req) (*resp, error)",
			File: "/svc/server.go", LineStart: 10, LineEnd: 30,
		},
		{
			ID: "myapp/handler.HandleCreate", Package: "myapp/handler",
			Name: "HandleCreate", Kind: "func",
			Signature: "func HandleCreate()",
			File: "/handler/create.go", LineStart: 1, LineEnd: 10,
		},
	})
	if err := s.UpsertEdge(store.Edge{FromID: "myapp/handler.HandleCreate", ToID: "myapp/svc.Server.CreateShipment", Kind: "calls"}); err != nil {
		t.Fatal(err)
	}

	engine := New(s)
	// Use a suffix that should resolve via fuzzy lookup.
	callers, err := engine.GetCallers("svc.Server.CreateShipment")
	if err != nil {
		t.Fatalf("GetCallers (fuzzy): %v", err)
	}
	if len(callers) != 1 {
		t.Fatalf("expected 1 caller via fuzzy resolution, got %d", len(callers))
	}
}

func TestIsLineRef(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"/* file.go:42 */", true},
		{"/* file.go:42-67 */", true},
		{"func main() {}", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isLineRef(tt.input)
			if got != tt.want {
				t.Errorf("isLineRef(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
