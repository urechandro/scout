package query

import (
	"strings"
	"testing"

	"github.com/urechandro/scout/store"
)

// seedTSBoundaryCorpus mirrors the platform-portal shape from the
// 2026-07-13 cross-repo trace: a Go resolver method, a generated GraphQL
// document const whose name embeds the RPC name, and the React component
// that passes the const to useMutation — no call edge in either direction.
func seedTSBoundaryCorpus(t *testing.T, s *store.Store) {
	t.Helper()
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "backend/internal/resolver.Mutation.ConfirmTransportOrder", Kind: "method",
			Package: "backend/internal/resolver", Name: "ConfirmTransportOrder",
			Signature: "func (r *Mutation) ConfirmTransportOrder(ctx context.Context, name string) (*planningv1.TransportOrder, error)",
			File:      "/p/backend/internal/resolver/mutation.go", LineStart: 370, LineEnd: 373,
		},
		{
			ID: "frontend/src/modules/planning/gen/graphql.TransportOrderSheet_ConfirmTransportOrderDocument", Kind: "const",
			Package: "frontend/src/modules/planning/gen/graphql", Name: "TransportOrderSheet_ConfirmTransportOrderDocument",
			Signature: "const TransportOrderSheet_ConfirmTransportOrderDocument: DocumentNode",
			File:      "/p/frontend/src/modules/planning/gen/graphql.ts", LineStart: 900, LineEnd: 905,
		},
		{
			ID: "frontend/src/modules/planning/pages/TransportOrdersPage/sheets/TransportOrderSheet/TransportOrderSheet.TransportOrderSheet", Kind: "func",
			Package: "frontend/src/modules/planning/pages/TransportOrdersPage/sheets/TransportOrderSheet/TransportOrderSheet", Name: "TransportOrderSheet",
			Signature: "TransportOrderSheet: React.FC",
			File:      "/p/frontend/src/modules/planning/pages/TransportOrdersPage/sheets/TransportOrderSheet/TransportOrderSheet.tsx",
			LineStart: 20, LineEnd: 180,
			Body: "TransportOrderSheet: React.FC = () => {\n  const [confirmTransportOrder] = useMutation(\n    TransportOrderSheet_ConfirmTransportOrderDocument,\n    { onCompleted: () => {} },\n  )\n}",
		},
	})
}

// TestEmbeddedIdentifierMatch: a query for the RPC name must surface the
// generated document const even though an exact-name hit (the Go resolver)
// also exists — the const's name embeds the identifier in a form neither
// exact lookup nor FTS tokenization can reach.
func TestEmbeddedIdentifierMatch(t *testing.T) {
	s := newTestStore(t)
	seedTSBoundaryCorpus(t, s)
	e := New(s, Options{})

	resp, err := e.GetRelevantContext(ContextRequest{Task: "ConfirmTransportOrder"})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}

	var exactRank, embeddedRank = -1, -1
	for i, sym := range resp.Symbols {
		switch sym.ID {
		case "backend/internal/resolver.Mutation.ConfirmTransportOrder":
			exactRank = i
		case "frontend/src/modules/planning/gen/graphql.TransportOrderSheet_ConfirmTransportOrderDocument":
			embeddedRank = i
		}
	}
	if exactRank == -1 {
		t.Fatal("exact-name hit missing")
	}
	if embeddedRank == -1 {
		ids := make([]string, len(resp.Symbols))
		for i, s := range resp.Symbols {
			ids[i] = s.ID
		}
		t.Fatalf("embedded-name document const missing; got %v", ids)
	}
	if embeddedRank < exactRank {
		t.Errorf("embedded match (rank %d) must not outrank exact match (rank %d)", embeddedRank, exactRank)
	}
}

// TestEmbeddedIdentifier_ShortIdentGate: short identifiers must not trigger
// the substring sweep.
func TestEmbeddedIdentifier_ShortIdentGate(t *testing.T) {
	s := newTestStore(t)
	seedTSBoundaryCorpus(t, s)
	e := New(s, Options{})

	// "getBody" is a compound ident but only 7 chars — below the gate.
	resp, err := e.GetRelevantContext(ContextRequest{Task: "getBody"})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	for _, sym := range resp.Symbols {
		if sym.Why == "name contains getBody" {
			t.Errorf("short ident triggered embedded matching: %s", sym.ID)
		}
	}
}

// TestGetCallers_NonCallBodyReference: the document const is passed as an
// argument to useMutation — no "name(" call shape exists, so the plain
// substring fallback must find the component.
func TestGetCallers_NonCallBodyReference(t *testing.T) {
	s := newTestStore(t)
	seedTSBoundaryCorpus(t, s)
	e := New(s, Options{})

	callers, err := e.GetCallers("TransportOrderSheet_ConfirmTransportOrderDocument")
	if err != nil {
		t.Fatalf("GetCallers: %v", err)
	}
	if len(callers) != 1 {
		t.Fatalf("want exactly the component as caller, got %d: %v", len(callers), callers)
	}
	if !strings.HasSuffix(callers[0].ID, ".TransportOrderSheet") {
		t.Errorf("caller = %s, want the TransportOrderSheet component", callers[0].ID)
	}
	if callers[0].Why != "body reference (non-call)" {
		t.Errorf("why = %q", callers[0].Why)
	}
}
