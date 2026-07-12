package query

import (
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

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
