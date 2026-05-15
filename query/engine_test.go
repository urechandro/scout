package query

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

func seedSymbols(t *testing.T, s *store.Store, syms []store.Symbol) {
	t.Helper()
	for _, sym := range syms {
		if err := s.UpsertSymbol(sym); err != nil {
			t.Fatalf("upsert %s: %v", sym.ID, err)
		}
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
