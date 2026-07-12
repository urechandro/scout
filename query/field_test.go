package query

import (
	"strings"
	"testing"

	"github.com/urechandro/scout/store"
)

// seedFieldCorpus builds a miniature proto+Go cross-layer corpus: a message
// with fields, its generated getter, and hand-written code touching the
// field three different ways.
func seedFieldCorpus(t *testing.T, s *store.Store) {
	t.Helper()
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "planning.v1.ShipmentLeg", Kind: "message",
			Package: "planning.v1", Name: "ShipmentLeg",
			Signature: "message ShipmentLeg",
			File:      "/p/proto/leg.proto", LineStart: 5, LineEnd: 30,
		},
		{
			ID: "planning.v1.ShipmentLeg.pickup_time", Kind: "field",
			Package: "planning.v1", Name: "pickup_time",
			Signature: "google.protobuf.Timestamp pickup_time = 2",
			File:      "/p/proto/leg.proto", LineStart: 12, LineEnd: 12,
		},
		{
			ID: "planning.v1.ShipmentLeg.name", Kind: "field",
			Package: "planning.v1", Name: "name",
			Signature: "string name = 1",
			File:      "/p/proto/leg.proto", LineStart: 10, LineEnd: 10,
		},
		// Generated getter on the right message.
		{
			ID: "app/gen/planningv1.ShipmentLeg.GetPickupTime", Kind: "method",
			Package: "app/gen/planningv1", Name: "GetPickupTime",
			Signature: "func (x *ShipmentLeg) GetPickupTime() *timestamppb.Timestamp",
			File:      "/p/gen/planningv1/leg.pb.go", LineStart: 100, LineEnd: 105,
		},
		// Same-named getter on a different message: must be excluded.
		{
			ID: "app/gen/planningv1.Visit.GetPickupTime", Kind: "method",
			Package: "app/gen/planningv1", Name: "GetPickupTime",
			Signature: "func (x *Visit) GetPickupTime() *timestamppb.Timestamp",
			File:      "/p/gen/planningv1/visit.pb.go", LineStart: 300, LineEnd: 305,
		},
		// Hand-written code: getter call.
		{
			ID: "app/svc/legsvc.Server.ValidateLeg", Kind: "method",
			Package: "app/svc/legsvc", Name: "ValidateLeg",
			Signature: "func (s *Server) ValidateLeg(leg *planningv1.ShipmentLeg) error",
			File:      "/p/svc/legsvc/validate.go", LineStart: 10, LineEnd: 30,
			Body: "func (s *Server) ValidateLeg(leg *planningv1.ShipmentLeg) error {\n\tif leg.GetPickupTime() == nil { return errRequired }\n\treturn nil\n}",
		},
		// Hand-written code: composite-literal initialization, in a test file.
		{
			ID: "app/svc/legsvc.TestValidateLeg", Kind: "func",
			Package: "app/svc/legsvc", Name: "TestValidateLeg",
			Signature: "func TestValidateLeg(t *testing.T)",
			File:      "/p/svc/legsvc/validate_test.go", LineStart: 10, LineEnd: 30,
			Body: "func TestValidateLeg(t *testing.T) {\n\tleg := &planningv1.ShipmentLeg{PickupTime: ts}\n\t...\n}",
		},
		// A method that pages through legs — used by the ranking test.
		{
			ID: "app/svc/legsvc.Server.ListShipmentLegs", Kind: "method",
			Package: "app/svc/legsvc", Name: "ListShipmentLegs",
			Signature: "func (s *Server) ListShipmentLegs(ctx context.Context, req *planningv1.ListShipmentLegsRequest) (*planningv1.ListShipmentLegsResponse, error)",
			File:      "/p/svc/legsvc/list.go", LineStart: 40, LineEnd: 80,
		},
	})
}

// TestFieldImpact_DerivedNameFanOut: a proto field's blast radius is its
// declaring message, the generated getter on the right message (not the
// same-named getter on other messages), and code touching the field via
// getter call or literal initialization.
func TestFieldImpact_DerivedNameFanOut(t *testing.T) {
	s := newTestStore(t)
	seedFieldCorpus(t, s)
	e := New(s, Options{})

	resp, err := e.GetImpact("planning.v1.ShipmentLeg.pickup_time")
	if err != nil {
		t.Fatalf("GetImpact: %v", err)
	}

	all := map[string]bool{}
	for _, group := range [][]SymbolSummary{resp.Proto, resp.Generated, resp.Implementation, resp.Tests} {
		for _, s := range group {
			all[s.ID] = true
		}
	}

	for _, want := range []string{
		"planning.v1.ShipmentLeg",                      // declaring message
		"app/gen/planningv1.ShipmentLeg.GetPickupTime", // generated getter
		"app/svc/legsvc.Server.ValidateLeg",            // getter call
		"app/svc/legsvc.TestValidateLeg",               // literal init, test layer
	} {
		if !all[want] {
			t.Errorf("impact should include %s; got %v", want, keys(all))
		}
	}
	if all["app/gen/planningv1.Visit.GetPickupTime"] {
		t.Error("getter on a different message must be excluded")
	}
	if len(resp.Tests) == 0 {
		t.Error("literal-init usage in _test.go should classify into the tests layer")
	}
}

// TestFieldImpact_CommonFieldName: fields named "name" exist on nearly every
// message — impact must not degrade into a same-name sweep.
func TestFieldImpact_CommonFieldName(t *testing.T) {
	s := newTestStore(t)
	seedFieldCorpus(t, s)
	// A second message with its own "name" field and unrelated Name usage.
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "planning.v1.Visit.name", Kind: "field",
			Package: "planning.v1", Name: "name",
			Signature: "string name = 1",
			File:      "/p/proto/visit.proto", LineStart: 8, LineEnd: 8,
		},
	})
	e := New(s, Options{})

	resp, err := e.GetImpact("planning.v1.ShipmentLeg.name")
	if err != nil {
		t.Fatalf("GetImpact: %v", err)
	}
	for _, group := range [][]SymbolSummary{resp.Proto, resp.Generated, resp.Implementation, resp.Tests} {
		for _, sum := range group {
			if sum.ID == "planning.v1.Visit.name" {
				t.Error("sibling message's same-named field must not appear in impact")
			}
		}
	}
}

// TestDiscovery_FieldsRankBelowBehavior: the ranking guard. A discovery
// query touching field-adjacent vocabulary must surface messages/methods
// before the fields themselves.
func TestDiscovery_FieldsRankBelowBehavior(t *testing.T) {
	s := newTestStore(t)
	seedFieldCorpus(t, s)
	e := New(s, Options{})

	resp, err := e.GetRelevantContext(ContextRequest{Task: "shipment leg pickup time handling"})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if len(resp.Symbols) == 0 {
		t.Fatal("no results")
	}

	fieldRank, behaviorRank := -1, -1
	for i, sym := range resp.Symbols {
		if sym.Kind == "field" && fieldRank == -1 {
			fieldRank = i
		}
		if (sym.Kind == "method" || sym.Kind == "message") && behaviorRank == -1 {
			behaviorRank = i
		}
	}
	if fieldRank != -1 && behaviorRank != -1 && fieldRank < behaviorRank {
		ids := make([]string, len(resp.Symbols))
		for i, s := range resp.Symbols {
			ids[i] = s.Kind + ":" + s.ID
		}
		t.Errorf("field ranked above behavior symbols: %v", ids)
	}
}

// TestDiscovery_SnakeCaseFieldName reproduces the live Sonnet failure of
// 2026-07-12: three rephrasings of a field-name query returned zero field
// symbols because snake_case never qualified as an identifier — no Phase-1
// exact lookup, and FTS tokenization splits on "_" so the name bonus can't
// fire. Each of the three query shapes must now surface the field.
func TestDiscovery_SnakeCaseFieldName(t *testing.T) {
	s := newTestStore(t)
	seedFieldCorpus(t, s)
	// Two more messages carrying a same-named field, mirroring the live
	// index (Activity + two request messages all have reported_start_time).
	// The name-dedup pass keeps one of a 3+ group, so cross-term affinity
	// must make sure it keeps the one the query names.
	seedSymbols(t, s, []store.Symbol{
		{
			ID: "planning.v1.Visit.pickup_time", Kind: "field",
			Package: "planning.v1", Name: "pickup_time",
			Signature: "google.protobuf.Timestamp pickup_time = 4",
			File:      "/p/proto/visit.proto", LineStart: 14, LineEnd: 14,
		},
		{
			ID: "planning.v1.AdjustShipmentLegTimesRequest.pickup_time", Kind: "field",
			Package: "planning.v1", Name: "pickup_time",
			Signature: "google.protobuf.Timestamp pickup_time = 3",
			File:      "/p/proto/leg_service.proto", LineStart: 40, LineEnd: 40,
		},
	})
	e := New(s, Options{})

	// "Visit" mirrors the live failure shape: a plain capitalized word that
	// isCompoundIdent rejects, so only cross-term affinity over raw query
	// words can break the same-name tie. Run each query several times —
	// the original bug was a nondeterministic dedup tie-break that
	// surfaced the right field in ~1 of 5 calls.
	for _, q := range []string{
		"ShipmentLeg pickup_time",             // camelCase message + field
		"ShipmentLeg.pickup_time proto field", // dotted (discovery w/ extra words)
	} {
		// budget_tokens 1000 mirrors the live Sonnet run: at tight budgets
		// the field must survive the trim, not just make the ranked list.
		resp, err := e.GetRelevantContext(ContextRequest{Task: q, BudgetTokens: 1000})
		if err != nil {
			t.Fatalf("GetRelevantContext(%q): %v", q, err)
		}
		found := false
		for _, sym := range resp.Symbols {
			if sym.ID == "planning.v1.ShipmentLeg.pickup_time" {
				found = true
			}
		}
		if !found {
			ids := make([]string, len(resp.Symbols))
			for i, sym := range resp.Symbols {
				ids[i] = sym.ID
			}
			t.Errorf("query %q should surface the field; got %v", q, ids)
		}
	}

	// Plain capitalized message name (not camelCase): affinity must come
	// from raw query words. Repeat to catch tie-break nondeterminism.
	for i := 0; i < 5; i++ {
		resp, err := e.GetRelevantContext(ContextRequest{Task: "Visit pickup_time", BudgetTokens: 1000})
		if err != nil {
			t.Fatalf("GetRelevantContext(Visit): %v", err)
		}
		found := false
		for _, sym := range resp.Symbols {
			if sym.ID == "planning.v1.Visit.pickup_time" {
				found = true
			}
		}
		if !found {
			t.Fatalf("run %d: 'Visit pickup_time' should surface Visit's field", i)
		}
	}

	// A bare field name can't disambiguate between the three carriers, but
	// it must surface at least one of them (pre-fix it surfaced none).
	resp, err := e.GetRelevantContext(ContextRequest{Task: "pickup_time"})
	if err != nil {
		t.Fatalf("GetRelevantContext(bare): %v", err)
	}
	anyField := false
	for _, sym := range resp.Symbols {
		if sym.Kind == "field" && strings.HasSuffix(sym.ID, ".pickup_time") {
			anyField = true
		}
	}
	if !anyField {
		t.Error("bare field name should surface at least one pickup_time field")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
