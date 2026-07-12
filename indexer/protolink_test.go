package indexer

import (
	"path/filepath"
	"testing"

	"github.com/urechandro/scout/store"
)

func newLinkStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *store.Store, syms ...store.Symbol) {
	t.Helper()
	for _, sym := range syms {
		if err := s.UpsertSymbol(sym); err != nil {
			t.Fatalf("upsert %s: %v", sym.ID, err)
		}
	}
}

// TestLinkProtoToGo_RebuildsStaleEdges is the regression test for the
// add-only staleness bug: when the preferred implementation for an RPC
// changes, the old edge's endpoints both still exist, so nothing else
// ever deletes it. LinkProtoToGo must rebuild, not accumulate.
func TestLinkProtoToGo_RebuildsStaleEdges(t *testing.T) {
	s := newLinkStore(t)

	rpc := store.Symbol{
		ID: "proto/planning/v1.PlanningService.ConfirmOrder", Kind: "rpc",
		Package: "proto/planning/v1", Name: "ConfirmOrder",
		Signature: "rpc ConfirmOrder(ConfirmOrderRequest) returns (ConfirmOrderResponse)",
		File:      "/p/planning.proto", LineStart: 1, LineEnd: 2,
	}
	oldImpl := store.Symbol{
		ID: "app/handlers.Handler.ConfirmOrder", Kind: "method",
		Package: "app/handlers", Name: "ConfirmOrder",
		Signature: "func (h *Handler) ConfirmOrder(...)",
		File:      "/p/handlers/h.go", LineStart: 1, LineEnd: 2,
	}
	svcImpl := store.Symbol{
		ID: "app/svc/ordersvc.Server.ConfirmOrder", Kind: "method",
		Package: "app/svc/ordersvc", Name: "ConfirmOrder",
		Signature: "func (s *Server) ConfirmOrder(...)",
		File:      "/p/svc/s.go", LineStart: 1, LineEnd: 2,
	}
	// An unrelated Go interface edge that must survive the rebuild.
	iface := store.Symbol{
		ID: "app/repo.Repository", Kind: "interface",
		Package: "app/repo", Name: "Repository",
		Signature: "type Repository interface{...}",
		File:      "/p/repo/r.go", LineStart: 1, LineEnd: 2,
	}
	ifaceImpl := store.Symbol{
		ID: "app/repo.SQLRepository", Kind: "struct",
		Package: "app/repo", Name: "SQLRepository",
		Signature: "type SQLRepository struct{...}",
		File:      "/p/repo/sql.go", LineStart: 1, LineEnd: 2,
	}
	seed(t, s, rpc, oldImpl, svcImpl, iface, ifaceImpl)

	// Simulate the stale state: an earlier pass linked oldImpl (no svc
	// package existed yet), plus the Go interface edge.
	for _, e := range []store.Edge{
		{FromID: oldImpl.ID, ToID: rpc.ID, Kind: "implements"},
		{FromID: ifaceImpl.ID, ToID: iface.ID, Kind: "implements"},
	} {
		if err := s.UpsertEdge(e); err != nil {
			t.Fatalf("seed edge: %v", err)
		}
	}

	linked, err := LinkProtoToGo(s)
	if err != nil {
		t.Fatalf("LinkProtoToGo: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1", linked)
	}

	impls, err := s.GetImplementors(rpc.ID)
	if err != nil {
		t.Fatalf("GetImplementors: %v", err)
	}
	if len(impls) != 1 {
		ids := make([]string, len(impls))
		for i, im := range impls {
			ids[i] = im.ID
		}
		t.Fatalf("rpc should have exactly 1 implementor after rebuild, got %d: %v", len(impls), ids)
	}
	if impls[0].ID != svcImpl.ID {
		t.Errorf("implementor = %s, want svc-package method %s", impls[0].ID, svcImpl.ID)
	}

	// The Go interface edge is not rpc-targeted and must be untouched.
	ifaceImpls, err := s.GetImplementors(iface.ID)
	if err != nil {
		t.Fatalf("GetImplementors(iface): %v", err)
	}
	if len(ifaceImpls) != 1 || ifaceImpls[0].ID != ifaceImpl.ID {
		t.Errorf("interface edge should survive rpc relink, got %v", ifaceImpls)
	}
}

// TestDeleteEdgesByKindFromPackage_ExactPackageScope guards the package
// matching used by the incremental reindex: "pkg." must not sweep up
// subpackages the pass isn't re-indexing.
func TestDeleteEdgesByKindFromPackage_ExactPackageScope(t *testing.T) {
	s := newLinkStore(t)

	inPkg := store.Symbol{
		ID: "app/repo.SQLRepository", Kind: "struct", Package: "app/repo",
		Name: "SQLRepository", Signature: "type SQLRepository struct{...}",
		File: "/p/repo/sql.go", LineStart: 1, LineEnd: 2,
	}
	inSubPkg := store.Symbol{
		ID: "app/repo/postgres.Repo", Kind: "struct", Package: "app/repo/postgres",
		Name: "Repo", Signature: "type Repo struct{...}",
		File: "/p/repo/postgres/r.go", LineStart: 1, LineEnd: 2,
	}
	iface := store.Symbol{
		ID: "app/repo.Repository", Kind: "interface", Package: "app/repo",
		Name: "Repository", Signature: "type Repository interface{...}",
		File: "/p/repo/r.go", LineStart: 1, LineEnd: 2,
	}
	seed(t, s, inPkg, inSubPkg, iface)

	for _, e := range []store.Edge{
		{FromID: inPkg.ID, ToID: iface.ID, Kind: "implements"},
		{FromID: inSubPkg.ID, ToID: iface.ID, Kind: "implements"},
	} {
		if err := s.UpsertEdge(e); err != nil {
			t.Fatalf("seed edge: %v", err)
		}
	}

	if err := s.DeleteEdgesByKindFromPackage("implements", "app/repo"); err != nil {
		t.Fatalf("DeleteEdgesByKindFromPackage: %v", err)
	}

	impls, err := s.GetImplementors(iface.ID)
	if err != nil {
		t.Fatalf("GetImplementors: %v", err)
	}
	if len(impls) != 1 || impls[0].ID != inSubPkg.ID {
		t.Errorf("only the exact-package edge should be deleted; want subpackage edge to survive, got %v", impls)
	}
}
