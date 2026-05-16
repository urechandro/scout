package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedSymbols(t *testing.T, s *Store, syms []Symbol) {
	t.Helper()
	for _, sym := range syms {
		if err := s.UpsertSymbol(sym); err != nil {
			t.Fatalf("upsert %s: %v", sym.ID, err)
		}
	}
}

func seedEdges(t *testing.T, s *Store, edges []Edge) {
	t.Helper()
	for _, e := range edges {
		if err := s.UpsertEdge(e); err != nil {
			t.Fatalf("upsert edge %s->%s: %v", e.FromID, e.ToID, err)
		}
	}
}

// --- Database creation ---

func TestNew_CreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.db")

	// SQLite creates intermediate dirs? No — but the file itself yes.
	// Use a direct path to ensure it works.
	path = filepath.Join(dir, "test.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Verify tables exist by doing basic operations.
	if err := s.UpsertSymbol(Symbol{ID: "test.Foo", Package: "test", Name: "Foo", Kind: "func", Signature: "func Foo()", File: "foo.go"}); err != nil {
		t.Fatalf("upsert after create: %v", err)
	}
	sym, err := s.GetSymbol("test.Foo")
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if sym.Name != "Foo" {
		t.Errorf("got name %q, want Foo", sym.Name)
	}
}

// --- UpsertSymbol ---

func TestUpsertSymbol_InsertAndUpdate(t *testing.T) {
	s := newTestStore(t)

	sym := Symbol{
		ID:        "pkg.Foo",
		Package:   "pkg",
		Name:      "Foo",
		Kind:      "func",
		Signature: "func Foo()",
		Docstring: "Foo does things.",
		File:      "foo.go",
		LineStart: 10,
		LineEnd:   20,
		Body:      "func Foo() {}",
	}
	if err := s.UpsertSymbol(sym); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetSymbol("pkg.Foo")
	if err != nil {
		t.Fatalf("get after insert: %v", err)
	}
	if got.Signature != "func Foo()" {
		t.Errorf("signature = %q, want %q", got.Signature, "func Foo()")
	}
	if got.LineStart != 10 || got.LineEnd != 20 {
		t.Errorf("lines = %d-%d, want 10-20", got.LineStart, got.LineEnd)
	}

	// Update the symbol.
	sym.Signature = "func Foo(ctx context.Context)"
	sym.LineEnd = 25
	if err := s.UpsertSymbol(sym); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err = s.GetSymbol("pkg.Foo")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Signature != "func Foo(ctx context.Context)" {
		t.Errorf("signature after update = %q", got.Signature)
	}
	if got.LineEnd != 25 {
		t.Errorf("line_end after update = %d, want 25", got.LineEnd)
	}
}

func TestUpsertSymbol_FTSSynced(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.ValidateToken", Package: "pkg", Name: "ValidateToken", Kind: "func", Signature: "func ValidateToken(token string) error", File: "auth.go"},
		{ID: "pkg.CreateUser", Package: "pkg", Name: "CreateUser", Kind: "func", Signature: "func CreateUser(name string) error", File: "user.go"},
	})

	// FTS should find ValidateToken by decomposed words.
	results, err := s.SearchFTS("validate", 10)
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if len(results) != 1 || results[0].ID != "pkg.ValidateToken" {
		t.Errorf("fts 'validate' got %d results, want 1 (ValidateToken)", len(results))
	}

	// Update the symbol and verify FTS stays in sync (old entry removed).
	if err := s.UpsertSymbol(Symbol{
		ID: "pkg.ValidateToken", Package: "pkg", Name: "ValidateToken", Kind: "func",
		Signature: "func ValidateToken(token string) (*Claims, error)", File: "auth.go",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	results, err = s.SearchFTS("validate", 10)
	if err != nil {
		t.Fatalf("fts after update: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("fts after update got %d results, want 1 (no duplicates)", len(results))
	}
}

// --- Edges ---

func TestUpsertEdge_AndGetRelated(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", File: "c.go"},
		{ID: "pkg.Iface", Package: "pkg", Name: "Iface", Kind: "interface", Signature: "type Iface interface", File: "iface.go"},
		{ID: "pkg.Impl", Package: "pkg", Name: "Impl", Kind: "method", Signature: "func (s *Server) Impl()", File: "impl.go"},
	})

	seedEdges(t, s, []Edge{
		{FromID: "pkg.A", ToID: "pkg.B", Kind: "calls"},
		{FromID: "pkg.A", ToID: "pkg.C", Kind: "calls"},
		{FromID: "pkg.Impl", ToID: "pkg.Iface", Kind: "implements"},
	})

	// GetCallers: B is called by A.
	callers, err := s.GetCallers("pkg.B")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}
	if len(callers) != 1 || callers[0].ID != "pkg.A" {
		t.Errorf("callers of B = %v, want [A]", symbolIDs(callers))
	}

	// GetCallees: A calls B and C.
	callees, err := s.GetCallees("pkg.A")
	if err != nil {
		t.Fatalf("GetCallees: %v", err)
	}
	if len(callees) != 2 {
		t.Errorf("callees of A = %v, want [B, C]", symbolIDs(callees))
	}

	// GetImplements: Impl implements Iface.
	impls, err := s.GetImplements("pkg.Impl")
	if err != nil {
		t.Fatalf("GetImplements: %v", err)
	}
	if len(impls) != 1 || impls[0].ID != "pkg.Iface" {
		t.Errorf("implements = %v, want [Iface]", symbolIDs(impls))
	}

	// GetImplementors: Iface is implemented by Impl.
	implementors, err := s.GetImplementors("pkg.Iface")
	if err != nil {
		t.Fatalf("GetImplementors: %v", err)
	}
	if len(implementors) != 1 || implementors[0].ID != "pkg.Impl" {
		t.Errorf("implementors = %v, want [Impl]", symbolIDs(implementors))
	}
}

func TestUpsertEdge_Idempotent(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
	})

	edge := Edge{FromID: "pkg.A", ToID: "pkg.B", Kind: "calls"}
	if err := s.UpsertEdge(edge); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.UpsertEdge(edge); err != nil {
		t.Fatalf("second upsert (should be idempotent): %v", err)
	}

	callers, err := s.GetCallers("pkg.B")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}
	if len(callers) != 1 {
		t.Errorf("got %d callers after double insert, want 1", len(callers))
	}
}

// --- FuzzyGetSymbol ---

func TestFuzzyGetSymbol_ExactMatch(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "github.com/app/auth.ValidateToken", Package: "github.com/app/auth", Name: "ValidateToken", Kind: "func", Signature: "func ValidateToken()", File: "auth.go"},
	})

	sym, candidates, err := s.FuzzyGetSymbol("github.com/app/auth.ValidateToken")
	if err != nil {
		t.Fatalf("FuzzyGetSymbol: %v", err)
	}
	if sym.ID != "github.com/app/auth.ValidateToken" {
		t.Errorf("got ID %q", sym.ID)
	}
	if candidates != nil {
		t.Errorf("expected nil candidates for exact match, got %d", len(candidates))
	}
}

func TestFuzzyGetSymbol_SuffixMatch(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "github.com/app/auth.ValidateToken", Package: "github.com/app/auth", Name: "ValidateToken", Kind: "func", Signature: "func ValidateToken()", File: "auth.go"},
	})

	sym, candidates, err := s.FuzzyGetSymbol("auth.ValidateToken")
	if err != nil {
		t.Fatalf("FuzzyGetSymbol: %v", err)
	}
	if sym.ID != "github.com/app/auth.ValidateToken" {
		t.Errorf("got ID %q", sym.ID)
	}
	if candidates != nil {
		t.Errorf("expected nil candidates for unique suffix, got %d", len(candidates))
	}
}

func TestFuzzyGetSymbol_NameMatch_PrefersSvc(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "github.com/app/gen/api.CreateShipment", Package: "github.com/app/gen/api", Name: "CreateShipment", Kind: "method", Signature: "func CreateShipment()", File: "api.pb.go"},
		{ID: "github.com/app/svc.CreateShipment", Package: "github.com/app/svc", Name: "CreateShipment", Kind: "method", Signature: "func (s *Server) CreateShipment()", File: "shipment.go"},
	})

	sym, candidates, err := s.FuzzyGetSymbol("CreateShipment")
	if err != nil {
		t.Fatalf("FuzzyGetSymbol: %v", err)
	}
	if sym.ID != "github.com/app/svc.CreateShipment" {
		t.Errorf("got ID %q, want svc version", sym.ID)
	}
	if len(candidates) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(candidates))
	}
}

func TestFuzzyGetSymbol_NotFound(t *testing.T) {
	s := newTestStore(t)

	_, _, err := s.FuzzyGetSymbol("nonexistent.Symbol")
	if err != ErrNotFound {
		t.Errorf("got err %v, want ErrNotFound", err)
	}
}

// --- SearchFTS ---

func TestSearchFTS_Basic(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.RateLimit", Package: "pkg", Name: "RateLimit", Kind: "func", Signature: "func RateLimit()", File: "rate.go"},
		{ID: "pkg.AuthMiddleware", Package: "pkg", Name: "AuthMiddleware", Kind: "func", Signature: "func AuthMiddleware()", File: "auth.go"},
		{ID: "pkg.CreateUser", Package: "pkg", Name: "CreateUser", Kind: "func", Signature: "func CreateUser()", File: "user.go"},
	})

	results, err := s.SearchFTS("rate", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(results) != 1 || results[0].ID != "pkg.RateLimit" {
		t.Errorf("got %v, want [RateLimit]", symbolIDs(results))
	}
}

func TestSearchFTSByKinds(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.Auth", Package: "pkg", Name: "Auth", Kind: "func", Signature: "func Auth()", Docstring: "Auth validates tokens", File: "auth.go"},
		{ID: "pkg.AuthConfig", Package: "pkg", Name: "AuthConfig", Kind: "struct", Signature: "type AuthConfig struct", Docstring: "AuthConfig holds auth settings", File: "auth.go"},
	})

	// Filter to structs only.
	results, err := s.SearchFTSByKinds("auth", []string{"struct"}, 10)
	if err != nil {
		t.Fatalf("SearchFTSByKinds: %v", err)
	}
	if len(results) != 1 || results[0].ID != "pkg.AuthConfig" {
		t.Errorf("got %v, want [AuthConfig]", symbolIDs(results))
	}
}

func TestSearchFTS_DecomposedName(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.addDispatchEventsToOutbox", Package: "pkg", Name: "addDispatchEventsToOutbox", Kind: "func", Signature: "func addDispatchEventsToOutbox()", File: "outbox.go"},
	})

	// "outbox" should match via the decomposed name.
	results, err := s.SearchFTS("outbox", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results for 'outbox', want 1", len(results))
	}
}

// --- DeleteByFile ---

func TestDeleteByFile(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "a.go"},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", File: "c.go"},
	})

	seedEdges(t, s, []Edge{
		{FromID: "pkg.A", ToID: "pkg.C", Kind: "calls"},
		{FromID: "pkg.C", ToID: "pkg.B", Kind: "calls"},
	})

	if err := s.DeleteByFile("a.go"); err != nil {
		t.Fatalf("DeleteByFile: %v", err)
	}

	// A and B should be gone.
	if _, err := s.GetSymbol("pkg.A"); err != ErrNotFound {
		t.Errorf("A should be deleted, got err=%v", err)
	}
	if _, err := s.GetSymbol("pkg.B"); err != ErrNotFound {
		t.Errorf("B should be deleted, got err=%v", err)
	}

	// C should remain.
	sym, err := s.GetSymbol("pkg.C")
	if err != nil {
		t.Fatalf("C should still exist: %v", err)
	}
	if sym.Name != "C" {
		t.Errorf("got %q", sym.Name)
	}

	// Edges involving A or B should be gone.
	callers, _ := s.GetCallers("pkg.C")
	if len(callers) != 0 {
		t.Errorf("callers of C after delete = %v, want none", symbolIDs(callers))
	}
	callees, _ := s.GetCallees("pkg.C")
	if len(callees) != 0 {
		t.Errorf("callees of C after delete = %v, want none", symbolIDs(callees))
	}
}

// --- DeleteEdgesByKind ---

func TestDeleteEdgesByKind(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
		{ID: "pkg.Iface", Package: "pkg", Name: "Iface", Kind: "interface", Signature: "type Iface interface", File: "iface.go"},
	})

	seedEdges(t, s, []Edge{
		{FromID: "pkg.A", ToID: "pkg.B", Kind: "calls"},
		{FromID: "pkg.A", ToID: "pkg.Iface", Kind: "implements"},
	})

	if err := s.DeleteEdgesByKind("calls"); err != nil {
		t.Fatalf("DeleteEdgesByKind: %v", err)
	}

	// Call edge gone.
	callees, _ := s.GetCallees("pkg.A")
	if len(callees) != 0 {
		t.Errorf("callees after delete calls = %v, want none", symbolIDs(callees))
	}

	// Implements edge should remain.
	impls, _ := s.GetImplements("pkg.A")
	if len(impls) != 1 {
		t.Errorf("implements after delete calls = %v, want [Iface]", symbolIDs(impls))
	}
}

// --- GetByName / GetByNameAndKind ---

func TestGetByName(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "a.Foo", Package: "a", Name: "Foo", Kind: "func", Signature: "func Foo()", File: "a.go"},
		{ID: "b.Foo", Package: "b", Name: "Foo", Kind: "method", Signature: "func (s *S) Foo()", File: "b.go"},
		{ID: "c.Bar", Package: "c", Name: "Bar", Kind: "func", Signature: "func Bar()", File: "c.go"},
	})

	results, err := s.GetByName("Foo")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

func TestGetByNameAndKind(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "a.Foo", Package: "a", Name: "Foo", Kind: "func", Signature: "func Foo()", File: "a.go"},
		{ID: "b.Foo", Package: "b", Name: "Foo", Kind: "method", Signature: "func (s *S) Foo()", File: "b.go"},
	})

	results, err := s.GetByNameAndKind("Foo", "method")
	if err != nil {
		t.Fatalf("GetByNameAndKind: %v", err)
	}
	if len(results) != 1 || results[0].ID != "b.Foo" {
		t.Errorf("got %v, want [b.Foo]", symbolIDs(results))
	}
}

// --- GetCallersFromBody ---

func TestGetCallersFromBody(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.Handler", Package: "pkg", Name: "Handler", Kind: "func", Signature: "func Handler()", File: "handler.go",
			Body: "func Handler() {\n\tresult := svc.New(cfg)\n\tresult.Run()\n}"},
		{ID: "pkg.Other", Package: "pkg", Name: "Other", Kind: "func", Signature: "func Other()", File: "other.go",
			Body: "func Other() {\n\tfmt.Println(\"hello\")\n}"},
	})

	results, err := s.GetCallersFromBody("New")
	if err != nil {
		t.Fatalf("GetCallersFromBody: %v", err)
	}
	if len(results) != 1 || results[0].ID != "pkg.Handler" {
		t.Errorf("got %v, want [Handler]", symbolIDs(results))
	}
}

// --- GetChildrenByIDPrefix ---

func TestGetChildrenByIDPrefix(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "proto.ShipmentService", Package: "proto", Name: "ShipmentService", Kind: "service", Signature: "service ShipmentService", File: "shipment.proto"},
		{ID: "proto.ShipmentService.CreateShipment", Package: "proto", Name: "CreateShipment", Kind: "rpc", Signature: "rpc CreateShipment(CreateShipmentRequest) returns (Shipment)", File: "shipment.proto"},
		{ID: "proto.ShipmentService.GetShipment", Package: "proto", Name: "GetShipment", Kind: "rpc", Signature: "rpc GetShipment(GetShipmentRequest) returns (Shipment)", File: "shipment.proto"},
		{ID: "proto.OtherService.Delete", Package: "proto", Name: "Delete", Kind: "rpc", Signature: "rpc Delete()", File: "other.proto"},
	})

	rpcs, err := s.GetChildrenByIDPrefix("proto.ShipmentService", "rpc")
	if err != nil {
		t.Fatalf("GetChildrenByIDPrefix: %v", err)
	}
	if len(rpcs) != 2 {
		t.Errorf("got %d RPCs, want 2", len(rpcs))
	}
}

// --- Conventions ---

func TestConventions_CRUD(t *testing.T) {
	s := newTestStore(t)

	conv := Convention{
		Name:        "transactional-outbox",
		Terms:       []string{"outbox", "event", "dispatch"},
		Description: "Use transactional outbox for reliable event dispatch.",
		Structure:   "1. Begin TX\n2. Write entity\n3. Write outbox event\n4. Commit",
		Examples:    []string{"svc.CreateShipment", "svc.UpdateOrder"},
	}

	if err := s.UpsertConvention(conv); err != nil {
		t.Fatalf("UpsertConvention: %v", err)
	}

	// Get by name.
	got, err := s.GetConvention("transactional-outbox")
	if err != nil {
		t.Fatalf("GetConvention: %v", err)
	}
	if got.Description != conv.Description {
		t.Errorf("description mismatch")
	}
	if len(got.Terms) != 3 || got.Terms[0] != "outbox" {
		t.Errorf("terms = %v", got.Terms)
	}
	if len(got.Examples) != 2 {
		t.Errorf("examples = %v", got.Examples)
	}

	// Search by FTS.
	results, err := s.SearchConventions("outbox")
	if err != nil {
		t.Fatalf("SearchConventions: %v", err)
	}
	if len(results) != 1 || results[0].Name != "transactional-outbox" {
		t.Errorf("search got %v", results)
	}

	// AllConventions.
	all, err := s.AllConventions()
	if err != nil {
		t.Fatalf("AllConventions: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("all = %d, want 1", len(all))
	}

	// Update.
	conv.Description = "Updated description."
	if err := s.UpsertConvention(conv); err != nil {
		t.Fatalf("update convention: %v", err)
	}
	got, _ = s.GetConvention("transactional-outbox")
	if got.Description != "Updated description." {
		t.Errorf("description after update = %q", got.Description)
	}
}

func TestGetConvention_NotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetConvention("nonexistent")
	if err != ErrNotFound {
		t.Errorf("got err %v, want ErrNotFound", err)
	}
}

// --- Meta ---

func TestMeta_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetMeta("last_index", "2024-01-01"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	val, err := s.GetMeta("last_index")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "2024-01-01" {
		t.Errorf("got %q, want 2024-01-01", val)
	}

	// Update.
	if err := s.SetMeta("last_index", "2024-06-15"); err != nil {
		t.Fatalf("SetMeta update: %v", err)
	}
	val, _ = s.GetMeta("last_index")
	if val != "2024-06-15" {
		t.Errorf("got %q after update", val)
	}
}

func TestGetMeta_NotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetMeta("nonexistent")
	if err != ErrNotFound {
		t.Errorf("got err %v, want ErrNotFound", err)
	}
}

// --- decomposeIdentifier ---

func TestDecomposeIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"CreateShipment", "create shipment"},
		{"addDispatchEventsToOutbox", "add dispatch events to outbox"},
		{"BatchMutateActivities", "batch mutate activities"},
		{"ID", "i d"},
		{"simple", "simple"},
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

// --- GetByFile / GetByKind ---

func TestGetByFile(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "shared.go", LineStart: 20},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "shared.go", LineStart: 10},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", File: "other.go"},
	})

	results, err := s.GetByFile("shared.go")
	if err != nil {
		t.Fatalf("GetByFile: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d, want 2", len(results))
	}
	// Should be ordered by line_start.
	if results[0].ID != "pkg.B" {
		t.Errorf("first result = %s, want B (line 10)", results[0].ID)
	}
}

func TestGetByKind(t *testing.T) {
	s := newTestStore(t)

	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "interface", Signature: "type B interface", File: "b.go"},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", File: "c.go"},
	})

	results, err := s.GetByKind("interface")
	if err != nil {
		t.Fatalf("GetByKind: %v", err)
	}
	if len(results) != 1 || results[0].ID != "pkg.B" {
		t.Errorf("got %v, want [B]", symbolIDs(results))
	}
}

// --- pickBestCandidate ---

func TestPickBestCandidate(t *testing.T) {
	candidates := []Symbol{
		{ID: "github.com/app/gen/api.Create", Package: "github.com/app/gen/api", Name: "Create", Kind: "method"},
		{ID: "github.com/app/svc.Create", Package: "github.com/app/svc", Name: "Create", Kind: "method"},
		{ID: "github.com/app/other.Create", Package: "github.com/app/other", Name: "Create", Kind: "method"},
	}

	best := pickBestCandidate(candidates, "Create")
	if best.ID != "github.com/app/svc.Create" {
		t.Errorf("got %q, want svc version (svc bonus)", best.ID)
	}
}

func TestPickBestCandidate_PenalizesGenerated(t *testing.T) {
	candidates := []Symbol{
		{ID: "github.com/app/gen/api.Foo", Package: "github.com/app/gen/api", Name: "Foo", Kind: "method"},
		{ID: "github.com/app/util.Foo", Package: "github.com/app/util", Name: "Foo", Kind: "method"},
	}

	best := pickBestCandidate(candidates, "Foo")
	if best.ID != "github.com/app/util.Foo" {
		t.Errorf("got %q, want util version (gen penalized)", best.ID)
	}
}

// --- helpers ---

func symbolIDs(syms []Symbol) []string {
	ids := make([]string, len(syms))
	for i, s := range syms {
		ids[i] = s.ID
	}
	return ids
}
