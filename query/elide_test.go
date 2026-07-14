package query

import (
	"strings"
	"testing"

	"github.com/urechandro/scout/store"
)

func newElideEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	s := newTestStore(t)
	e := New(s, Options{
		ModulePrefix: "github.com/acme/svc",
		RootDir:      "/home/u/proj",
	})
	return e, s
}

func TestShortIDAndFile(t *testing.T) {
	e, _ := newElideEngine(t)

	tests := []struct {
		in, want string
	}{
		// Module-prefixed IDs get elided.
		{"github.com/acme/svc/internal/auth.ValidateToken", "internal/auth.ValidateToken"},
		// Already-elided IDs pass through (idempotent).
		{"internal/auth.ValidateToken", "internal/auth.ValidateToken"},
		// Dep symbols are untouched.
		{"google.golang.org/grpc.Dial", "google.golang.org/grpc.Dial"},
		// Root-package symbols (no slash after module) are untouched —
		// stripping down to ".Foo" would be ambiguous on resolution.
		{"github.com/acme/svc.Server", "github.com/acme/svc.Server"},
	}
	for _, tt := range tests {
		if got := e.shortID(tt.in); got != tt.want {
			t.Errorf("shortID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	if got := e.shortFile("/home/u/proj/internal/auth/auth.go"); got != "internal/auth/auth.go" {
		t.Errorf("shortFile = %q, want internal/auth/auth.go", got)
	}
	if got := e.shortFile("/home/u/go/pkg/mod/google.golang.org/grpc/dial.go"); got != "/home/u/go/pkg/mod/google.golang.org/grpc/dial.go" {
		t.Errorf("shortFile should not touch paths outside root, got %q", got)
	}
}

func TestElisionDisabledByDefault(t *testing.T) {
	s := newTestStore(t)
	e := New(s, Options{})

	id := "github.com/acme/svc/internal/auth.ValidateToken"
	if got := e.shortID(id); got != id {
		t.Errorf("shortID with no options should be a no-op, got %q", got)
	}
}

// TestElidedIDRoundTrip is the behavioral contract behind elision: every ID a
// response hands the model must resolve when passed back into a symbol_id
// parameter.
func TestElidedIDRoundTrip(t *testing.T) {
	e, s := newElideEngine(t)

	seedSymbols(t, s, []store.Symbol{
		{
			ID:        "github.com/acme/svc/internal/auth.ValidateToken",
			Package:   "github.com/acme/svc/internal/auth",
			Name:      "ValidateToken",
			Kind:      "func",
			Signature: "func ValidateToken(tok string) error",
			File:      "/home/u/proj/internal/auth/auth.go",
			LineStart: 10,
			LineEnd:   20,
			Body:      "func ValidateToken(tok string) error { return nil }",
		},
	})

	// GetBody with the elided form must resolve without a fuzzy-match hint.
	resp, err := e.GetBody("internal/auth.ValidateToken")
	if err != nil {
		t.Fatalf("GetBody(elided): %v", err)
	}
	if resp.Hint != "" {
		t.Errorf("elided ID should resolve exactly, got hint %q", resp.Hint)
	}
	// And the response itself renders elided — including Package, which
	// carries the same module prefix as the ID.
	if resp.ID != "internal/auth.ValidateToken" {
		t.Errorf("response ID = %q, want elided form", resp.ID)
	}
	if resp.File != "internal/auth/auth.go" {
		t.Errorf("response File = %q, want root-relative form", resp.File)
	}
	if resp.Package != "internal/auth" {
		t.Errorf("response Package = %q, want elided form", resp.Package)
	}

	// A fuzzy lookup's hint must not leak the full-length resolved ID.
	fuzzy, err := e.GetBody("ValidateToken")
	if err != nil {
		t.Fatalf("GetBody(fuzzy): %v", err)
	}
	if !strings.Contains(fuzzy.Hint, `resolved to "internal/auth.ValidateToken"`) {
		t.Errorf("fuzzy hint should show the elided ID, got %q", fuzzy.Hint)
	}
	if strings.Contains(fuzzy.Hint, "github.com/acme") {
		t.Errorf("fuzzy hint leaked full ID: %q", fuzzy.Hint)
	}

	// resolveSymbolID must accept both forms.
	for _, in := range []string{
		"internal/auth.ValidateToken",
		"github.com/acme/svc/internal/auth.ValidateToken",
	} {
		got, err := e.resolveSymbolID(in)
		if err != nil {
			t.Fatalf("resolveSymbolID(%q): %v", in, err)
		}
		if got != "github.com/acme/svc/internal/auth.ValidateToken" {
			t.Errorf("resolveSymbolID(%q) = %q, want full ID", in, got)
		}
	}
}

func TestGetRelevantContextRendersElided(t *testing.T) {
	e, s := newElideEngine(t)

	seedSymbols(t, s, []store.Symbol{
		{
			ID:        "github.com/acme/svc/internal/auth.ValidateToken",
			Package:   "github.com/acme/svc/internal/auth",
			Name:      "ValidateToken",
			Kind:      "func",
			Signature: "func ValidateToken(tok string) error",
			File:      "/home/u/proj/internal/auth/auth.go",
			LineStart: 10,
			LineEnd:   20,
		},
	})

	resp, err := e.GetRelevantContext(ContextRequest{Task: "ValidateToken"})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if len(resp.Symbols) != 1 {
		t.Fatalf("want 1 symbol, got %d", len(resp.Symbols))
	}
	if resp.Symbols[0].ID != "internal/auth.ValidateToken" {
		t.Errorf("ID = %q, want elided", resp.Symbols[0].ID)
	}
	if resp.Symbols[0].File != "internal/auth/auth.go" {
		t.Errorf("File = %q, want root-relative", resp.Symbols[0].File)
	}
}
