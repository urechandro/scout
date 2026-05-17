package indexer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/urechandro/scout/store"
)

// RunFilesLight performs a fast AST-only reindex of the given files.
// It updates symbols (names, signatures, line numbers, bodies) without
// type-checking. Call edges are extracted heuristically from AST call
// expressions — accurate for same-package calls, best-effort for
// cross-package. This runs in <1ms per file.
//
// Use this for on-save updates where latency matters. Follow up with
// RunFiles for full type-checked accuracy when the dust settles.
func (idx *Indexer) RunFilesLight(files []string) error {
	for _, file := range files {
		if err := idx.store.DeleteByFile(file); err != nil {
			return fmt.Errorf("delete stale symbols for %s: %w", file, err)
		}
	}

	for _, file := range files {
		if err := idx.indexFileLight(file); err != nil {
			log.Printf("light reindex %s: %v", file, err)
		}
	}

	return nil
}

func (idx *Indexer) indexFileLight(file string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	pkgPath, err := idx.resolvePackagePath(file)
	if err != nil {
		return fmt.Errorf("resolve package path: %w", err)
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			idx.indexFuncLight(fset, d, file, pkgPath)
		case *ast.GenDecl:
			idx.indexGenDeclLight(fset, d, file, pkgPath)
		}
	}

	return nil
}

func (idx *Indexer) indexFuncLight(fset *token.FileSet, decl *ast.FuncDecl, file, pkgPath string) {
	if decl.Name == nil {
		return
	}

	name := decl.Name.Name
	kind := "func"
	var id string

	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = "method"
		recvName := extractReceiverName(decl.Recv.List[0].Type)
		id = fmt.Sprintf("%s.%s.%s", pkgPath, recvName, name)
	} else {
		id = fmt.Sprintf("%s.%s", pkgPath, name)
	}

	lineStart := fset.Position(decl.Pos()).Line
	lineEnd := fset.Position(decl.End()).Line

	sig := buildFuncSignature(decl, pkgPath)

	body := ""
	if !isProtoGenerated(file) {
		body, _ = readSourceLines(file, lineStart, lineEnd)
	}
	if body == "" {
		body = fmt.Sprintf("/* %s:%d-%d */", file, lineStart, lineEnd)
	}

	sym := store.Symbol{
		ID:        id,
		Package:   pkgPath,
		Name:      name,
		Kind:      kind,
		Signature: sig,
		Docstring: extractDoc(decl.Doc),
		File:      file,
		LineStart: lineStart,
		LineEnd:   lineEnd,
		Body:      body,
	}

	if err := idx.store.UpsertSymbol(sym); err != nil {
		log.Printf("light upsert func %s: %v", id, err)
		return
	}

	// Extract AST-level call edges from the function body.
	if decl.Body != nil {
		idx.indexCallsLight(decl.Body, id, pkgPath, file)
	}
}

func (idx *Indexer) indexGenDeclLight(fset *token.FileSet, decl *ast.GenDecl, file, pkgPath string) {
	for _, spec := range decl.Specs {
		typeSpec, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}

		name := typeSpec.Name.Name
		kind := "type"
		if _, ok := typeSpec.Type.(*ast.InterfaceType); ok {
			kind = "interface"
		} else if _, ok := typeSpec.Type.(*ast.StructType); ok {
			kind = "struct"
		}

		id := fmt.Sprintf("%s.%s", pkgPath, name)

		startPos := typeSpec.Pos()
		if decl.Lparen == 0 {
			startPos = decl.Pos()
		}
		lineStart := fset.Position(startPos).Line
		lineEnd := fset.Position(typeSpec.End()).Line

		sig := fmt.Sprintf("type %s.%s %s", filepath.Base(pkgPath), name, kind)

		body := ""
		if !isProtoGenerated(file) {
			body, _ = readSourceLines(file, lineStart, lineEnd)
		}
		if body == "" {
			body = fmt.Sprintf("/* %s:%d-%d */", file, lineStart, lineEnd)
		}

		sym := store.Symbol{
			ID:        id,
			Package:   pkgPath,
			Name:      name,
			Kind:      kind,
			Signature: sig,
			Docstring: extractDoc(decl.Doc),
			File:      file,
			LineStart: lineStart,
			LineEnd:   lineEnd,
			Body:      body,
		}

		if err := idx.store.UpsertSymbol(sym); err != nil {
			log.Printf("light upsert type %s: %v", id, err)
		}
	}
}

// indexCallsLight extracts call edges from AST call expressions without type info.
// For selector expressions (x.Method()), it looks up the callee by name in the store.
// For plain identifiers (funcName()), it assumes same-package.
func (idx *Indexer) indexCallsLight(body *ast.BlockStmt, callerID, pkgPath, _ string) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		var calleeID string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			// Same-package function call.
			calleeID = fmt.Sprintf("%s.%s", pkgPath, fn.Name)
		case *ast.SelectorExpr:
			// Could be pkg.Func or receiver.Method — try store lookup by name.
			calleeName := fn.Sel.Name
			syms, err := idx.store.GetByName(calleeName)
			if err == nil && len(syms) == 1 {
				calleeID = syms[0].ID
			}
			// Ambiguous or not found — skip rather than guess wrong.
		}

		if calleeID != "" && calleeID != callerID {
			_ = idx.store.UpsertEdge(store.Edge{
				FromID: callerID,
				ToID:   calleeID,
				Kind:   "calls",
			})
		}
		return true
	})
}

// resolvePackagePath determines the Go package path for a file by finding
// the nearest go.mod and computing the relative path.
func (idx *Indexer) resolvePackagePath(file string) (string, error) {
	dir := filepath.Dir(file)

	// Walk up to find go.mod.
	modDir := dir
	for {
		if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(modDir)
		if parent == modDir {
			return "", fmt.Errorf("no go.mod found above %s", file)
		}
		modDir = parent
	}

	// Read module path from go.mod.
	modFile, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return "", err
	}
	modulePath := ""
	for _, line := range strings.Split(string(modFile), "\n") {
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module"))
			break
		}
	}
	if modulePath == "" {
		return "", fmt.Errorf("no module directive in go.mod")
	}

	rel, err := filepath.Rel(modDir, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return modulePath, nil
	}
	return modulePath + "/" + filepath.ToSlash(rel), nil
}

// extractReceiverName extracts the type name from a method receiver AST node.
func extractReceiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return extractReceiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return extractReceiverName(t.X)
	case *ast.IndexListExpr:
		return extractReceiverName(t.X)
	}
	return "unknown"
}

// buildFuncSignature constructs a human-readable signature from AST.
func buildFuncSignature(decl *ast.FuncDecl, pkgPath string) string {
	pkg := filepath.Base(pkgPath)
	var b strings.Builder
	b.WriteString("func ")

	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		recvName := extractReceiverName(decl.Recv.List[0].Type)
		recvVar := "_"
		if len(decl.Recv.List[0].Names) > 0 {
			recvVar = decl.Recv.List[0].Names[0].Name
		}
		fmt.Fprintf(&b, "(%s %s) ", recvVar, recvName)
	}

	return b.String() + pkg + "." + decl.Name.Name + "(…)"
}
