// Package indexer parses Go source code into symbols and call graph edges.
package indexer

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	callgraph "github.com/urechandro/go-callgraph"

	"github.com/urechandro/scout/store"
)

// CallGraphMethod selects the algorithm used to build call edges.
type CallGraphMethod string

const (
	// CallGraphAST uses AST-level name resolution (fast, imprecise for interfaces).
	CallGraphAST CallGraphMethod = "ast"
	// CallGraphCHA uses Class Hierarchy Analysis (conservative, includes all possible dispatch targets).
	CallGraphCHA CallGraphMethod = "cha"
	// CallGraphRTA uses Rapid Type Analysis (precise, tracks concrete types through the program).
	CallGraphRTA CallGraphMethod = "rta"
)

// Config controls what gets indexed.
type Config struct {
	// Dir is the root directory of the Go module to index.
	Dir string
	// Patterns are Go package patterns to load, e.g. []string{"./..."}.
	Patterns []string
	// ExcludeGenerated skips files with a "Code generated" header and common
	// generated file patterns (*.pb.go, *_gen.go) and directories (gen, vendor, generated).
	ExcludeGenerated bool
	// ExcludePaths skips packages whose import path contains any of these substrings.
	// e.g. []string{"cmd/localserver", "cmd/migration"}
	ExcludePaths []string
	// CallGraph selects the call graph algorithm for resolving call edges.
	// "rta" (default) or "cha". Empty string falls back to "rta".
	// AST-based resolution is always used for incremental (RunFiles) mode.
	CallGraph CallGraphMethod
	// IndexDeps indexes exported signatures from external dependency packages.
	IndexDeps bool
}

// Indexer parses Go packages and writes symbols and edges to a Store.
type Indexer struct {
	cfg          Config
	store        *store.Store
	fset         *token.FileSet
	useSSAEdges  bool // when true, indexFunc skips AST-based call edges
}

// New creates an Indexer for the given config and store.
func New(cfg Config, s *store.Store) *Indexer {
	return &Indexer{
		cfg:   cfg,
		store: s,
		fset:  token.NewFileSet(),
	}
}

// Run indexes all packages matching the configured patterns.
func (idx *Indexer) Run() error {
	method := idx.cfg.CallGraph
	if method == "" {
		method = CallGraphRTA
	}
	idx.useSSAEdges = method == CallGraphRTA || method == CallGraphCHA

	pkgs, err := idx.load()
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}

	// Collect non-excluded packages for symbol indexing.
	var indexable []*packages.Package
	var skippedPaths []string
	for _, pkg := range pkgs {
		if idx.isExcluded(pkg) {
			skippedPaths = append(skippedPaths, pkg.PkgPath)
			continue
		}
		indexable = append(indexable, pkg)
	}

	for _, pkg := range indexable {
		// Clear this package's implements edges before re-adding. Interface
		// satisfaction is computed fresh each pass, and a type that stopped
		// implementing an interface leaves both symbols intact — file-level
		// deletes never remove the stale edge. Same fix as the "calls" edge
		// clearing in buildCallGraph.
		if err := idx.store.DeleteEdgesByKindFromPackage("implements", pkg.PkgPath); err != nil {
			return fmt.Errorf("clear implements edges for %s: %w", pkg.PkgPath, err)
		}
		if err := idx.indexPackage(pkg); err != nil {
			return fmt.Errorf("index package %s: %w", pkg.PkgPath, err)
		}
	}

	log.Printf("indexed %d packages (%d skipped as generated)", len(indexable), len(skippedPaths))
	if len(skippedPaths) > 0 {
		log.Printf("skipped packages: %s", strings.Join(skippedPaths, ", "))
	}

	if idx.cfg.IndexDeps {
		if err := idx.indexDependencies(pkgs); err != nil {
			return fmt.Errorf("index dependencies: %w", err)
		}
	}

	// Build SSA call graph and write call edges to the store.
	if idx.useSSAEdges {
		if err := idx.buildCallGraph(pkgs, method); err != nil {
			return fmt.Errorf("build call graph (%s): %w", method, err)
		}
	}

	return nil
}

// RunFiles re-indexes only the specified files (for incremental updates).
func (idx *Indexer) RunFiles(files []string) error {
	// Reload affected packages first so we only delete stale symbols when we
	// know the package loaded successfully. Deleting before load means a compile
	// error would permanently erase symbols until the next successful save.
	pkgs, err := idx.load()
	if err != nil {
		return fmt.Errorf("load packages for incremental update: %w", err)
	}

	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	for _, pkg := range pkgs {
		// Only re-index packages that contain at least one changed file.
		affected := false
		for _, f := range pkg.GoFiles {
			if fileSet[f] {
				affected = true
				break
			}
		}
		if !affected {
			continue
		}

		// Skip packages that failed to type-check — TypesInfo is nil when the
		// package has errors. Don't delete existing symbols in this case so that
		// the index stays usable until the compile error is fixed.
		if pkg.TypesInfo == nil {
			log.Printf("watcher: skipping package %s (type-check failed, keeping stale symbols)", pkg.PkgPath)
			continue
		}

		// Delete stale symbols for the changed files in this package.
		for _, f := range pkg.GoFiles {
			if fileSet[f] {
				if err := idx.store.DeleteByFile(f); err != nil {
					return fmt.Errorf("delete stale symbols for %s: %w", f, err)
				}
			}
		}

		// Implements edges are cross-file within the package: removing a
		// method in the changed file can un-implement an interface for a
		// type declared elsewhere, and DeleteByFile won't touch that edge.
		// Clear the package's implements edges; indexPackage re-adds the
		// ones that still hold. (Proto→Go rpc links from this package are
		// cleared too — callers re-run LinkProtoToGo after RunFiles.)
		if err := idx.store.DeleteEdgesByKindFromPackage("implements", pkg.PkgPath); err != nil {
			return fmt.Errorf("clear implements edges for %s: %w", pkg.PkgPath, err)
		}

		if err := idx.indexPackage(pkg); err != nil {
			return fmt.Errorf("re-index package %s: %w", pkg.PkgPath, err)
		}
	}

	return nil
}

func (idx *Indexer) load() ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode:  callgraph.LoadMode,
		Dir:   idx.cfg.Dir,
		Fset:  idx.fset,
		Tests: true, // Include _test.go — needed for RTA roots and test symbol indexing.
	}

	patterns := idx.cfg.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	// Log a summary of what was found.
	var withErrors, noSyntax int
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			withErrors++
			if !idx.isExcludedPath(pkg.PkgPath) {
				for _, e := range pkg.Errors {
					log.Printf("warning: package %s: %v", pkg.PkgPath, e)
				}
			}
		}
		if len(pkg.Syntax) == 0 {
			noSyntax++
		}
	}
	log.Printf("packages.Load returned %d packages (%d with errors, %d with no syntax)", len(pkgs), withErrors, noSyntax)

	return pkgs, nil
}

func (idx *Indexer) indexPackage(pkg *packages.Package) error {
	for _, file := range pkg.Syntax {
		pos := idx.fset.File(file.Pos())
		if pos == nil {
			continue
		}
		filename := pos.Name()

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if err := idx.indexFunc(pkg, d, filename); err != nil {
					return fmt.Errorf("index func in %s: %w", filename, err)
				}
			case *ast.GenDecl:
				if err := idx.indexGenDecl(pkg, d, filename); err != nil {
					return fmt.Errorf("index gen decl in %s: %w", filename, err)
				}
			}
		}
	}

	return nil
}

func (idx *Indexer) indexFunc(pkg *packages.Package, decl *ast.FuncDecl, file string) error {
	if decl.Name == nil {
		return nil
	}
	if pkg.TypesInfo == nil {
		return nil
	}

	obj := pkg.TypesInfo.Defs[decl.Name]
	if obj == nil {
		return nil
	}

	sym := store.Symbol{
		ID:        qualifiedID(pkg.PkgPath, obj),
		Package:   pkg.PkgPath,
		Name:      decl.Name.Name,
		Kind:      funcKind(decl),
		Signature: types.ObjectString(obj, shortQualifier),
		Docstring: extractDoc(decl.Doc),
		File:      file,
		LineStart: idx.fset.Position(decl.Pos()).Line,
		LineEnd:   idx.fset.Position(decl.End()).Line,
		Body:      idx.extractBody(decl),
	}

	if err := idx.store.UpsertSymbol(sym); err != nil {
		return fmt.Errorf("upsert func symbol %s: %w", sym.ID, err)
	}

	// Index calls made inside this function (AST-based fallback).
	// Skipped during full index when SSA call graph will provide precise edges.
	if !idx.useSSAEdges {
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			calleeID := idx.resolveCall(pkg, callExpr)
			if calleeID == "" {
				return true
			}

			edge := store.Edge{
				FromID: sym.ID,
				ToID:   calleeID,
				Kind:   "calls",
			}
			// Best-effort — ignore errors for unresolved calls.
			_ = idx.store.UpsertEdge(edge)

			return true
		})
	}

	return nil
}

func (idx *Indexer) indexGenDecl(pkg *packages.Package, decl *ast.GenDecl, file string) error {
	if pkg.TypesInfo == nil {
		return nil
	}
	for _, spec := range decl.Specs {
		typeSpec, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}

		obj := pkg.TypesInfo.Defs[typeSpec.Name]
		if obj == nil {
			continue
		}

		kind := "type"
		if _, ok := typeSpec.Type.(*ast.InterfaceType); ok {
			kind = "interface"
		} else if _, ok := typeSpec.Type.(*ast.StructType); ok {
			kind = "struct"
		}

		// Use the GenDecl position for standalone declarations (includes
		// doc comment and the "type" keyword), TypeSpec for grouped ones.
		startPos := typeSpec.Pos()
		if decl.Lparen == 0 { // not a grouped type(...) block
			startPos = decl.Pos()
		}

		lineStart := idx.fset.Position(startPos).Line
		lineEnd := idx.fset.Position(typeSpec.End()).Line

		sym := store.Symbol{
			ID:        qualifiedID(pkg.PkgPath, obj),
			Package:   pkg.PkgPath,
			Name:      typeSpec.Name.Name,
			Kind:      kind,
			Signature: types.ObjectString(obj, shortQualifier),
			Docstring: extractDoc(decl.Doc),
			File:      file,
			LineStart: lineStart,
			LineEnd:   lineEnd,
			Body:      idx.extractTypeBody(file, lineStart, lineEnd),
		}

		if err := idx.store.UpsertSymbol(sym); err != nil {
			return fmt.Errorf("upsert type symbol %s: %w", sym.ID, err)
		}

		// Index interface implementations.
		if kind == "interface" {
			idx.indexImplementors(pkg, obj)
		}
	}

	return nil
}

func (idx *Indexer) indexImplementors(pkg *packages.Package, iface types.Object) {
	ifaceType, ok := iface.Type().Underlying().(*types.Interface)
	if !ok {
		return
	}

	// Walk all known objects in this package and check if they implement the interface.
	for _, obj := range pkg.TypesInfo.Defs {
		if obj == nil {
			continue
		}

		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}

		if types.Implements(named, ifaceType) || types.Implements(types.NewPointer(named), ifaceType) {
			edge := store.Edge{
				FromID: qualifiedID(pkg.PkgPath, obj),
				ToID:   qualifiedID(pkg.PkgPath, iface),
				Kind:   "implements",
			}
			_ = idx.store.UpsertEdge(edge)
		}
	}
}

func (idx *Indexer) resolveCall(pkg *packages.Package, call *ast.CallExpr) string {
	if pkg.TypesInfo == nil {
		return ""
	}

	var ident *ast.Ident

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		ident = fn
	case *ast.SelectorExpr:
		ident = fn.Sel
	default:
		return ""
	}

	obj := pkg.TypesInfo.Uses[ident]
	if obj == nil {
		return ""
	}

	if obj.Pkg() == nil {
		return "" // Built-in.
	}

	return qualifiedID(obj.Pkg().Path(), obj)
}

func (idx *Indexer) extractBody(decl *ast.FuncDecl) string {
	if decl.Body == nil {
		return ""
	}

	start := idx.fset.Position(decl.Pos())
	end := idx.fset.Position(decl.End())

	// Skip full body storage for proto-generated files — they're large
	// boilerplate. We still index signatures and edges.
	if isProtoGenerated(start.Filename) {
		return fmt.Sprintf("/* %s:%d-%d */", start.Filename, start.Line, end.Line)
	}

	body, err := readSourceLines(start.Filename, start.Line, end.Line)
	if err != nil {
		// Fall back to line reference if file is unreadable.
		return fmt.Sprintf("/* %s:%d-%d */", start.Filename, start.Line, end.Line)
	}
	return body
}

func (idx *Indexer) extractTypeBody(file string, lineStart, lineEnd int) string {
	if isProtoGenerated(file) {
		return fmt.Sprintf("/* %s:%d-%d */", file, lineStart, lineEnd)
	}
	body, err := readSourceLines(file, lineStart, lineEnd)
	if err != nil {
		return fmt.Sprintf("/* %s:%d-%d */", file, lineStart, lineEnd)
	}
	return body
}

func readSourceLines(path string, start, end int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		if n > end {
			break
		}
		if n >= start {
			lines = append(lines, scanner.Text())
		}
	}
	return strings.Join(lines, "\n"), scanner.Err()
}

// shortQualifier returns just the package name (not full path) for type signatures,
// keeping signatures readable and token-efficient.
func shortQualifier(pkg *types.Package) string {
	return pkg.Name()
}

// qualifiedID builds a stable fully-qualified ID for a symbol.
// Delegates to the shared go-callgraph library for consistent IDs across tools.
func qualifiedID(pkgPath string, obj types.Object) string {
	return callgraph.QualifiedObjID(pkgPath, obj)
}

// isExcluded reports whether a package should be skipped based on config.
func (idx *Indexer) isExcluded(pkg *packages.Package) bool {
	if idx.cfg.ExcludeGenerated && isGeneratedPackage(pkg) {
		return true
	}
	return idx.isExcludedPath(pkg.PkgPath)
}

// isExcludedPath reports whether a package path matches any exclusion pattern.
func (idx *Indexer) isExcludedPath(pkgPath string) bool {
	for _, excl := range idx.cfg.ExcludePaths {
		if strings.Contains(pkgPath, excl) {
			return true
		}
	}
	return false
}

// isGeneratedPackage reports whether all files in a package are generated,
// based on directory name and file-level heuristics.
func isGeneratedPackage(pkg *packages.Package) bool {
	for _, f := range pkg.GoFiles {
		if !isGeneratedFile(f) {
			return false
		}
	}
	return len(pkg.GoFiles) > 0
}

// isGeneratedFile reports whether a file should be excluded as generated code.
// Proto-generated files (.pb.go, .pb.gw.go) are NOT excluded — they define
// the gRPC service interfaces needed to link proto RPCs to Go implementations.
func isGeneratedFile(path string) bool {
	base := filepath.Base(path)

	// Proto-generated files are always kept — they define gRPC service interfaces
	// needed to link proto RPCs to Go implementations. Check before directory
	// scan so that files under proto/gen/ are not skipped.
	if strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, ".pb.gw.go") {
		return false
	}

	// Check directory components.
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
		switch part {
		case "vendor", "gen", "generated":
			return true
		}
	}

	// Check filename suffixes.
	if strings.HasSuffix(base, "_gen.go") ||
		strings.HasSuffix(base, "_generated.go") {
		return true
	}

	// Check for "Code generated" comment in first 5 lines.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for i := 0; i < 5 && scanner.Scan(); i++ {
		if strings.Contains(scanner.Text(), "Code generated") {
			return true
		}
	}

	return false
}

// isProtoGenerated reports whether a file is a protobuf-generated Go file.
// Used to skip storing full bodies (large boilerplate) while still indexing signatures.
func isProtoGenerated(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, ".pb.gw.go")
}

func funcKind(decl *ast.FuncDecl) string {
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		return "method"
	}

	return "func"
}

func extractDoc(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}

	var lines []string
	for _, c := range cg.List {
		text := strings.TrimPrefix(c.Text, "//")
		text = strings.TrimPrefix(text, "/*")
		text = strings.TrimSuffix(text, "*/")
		lines = append(lines, strings.TrimSpace(text))
	}

	return strings.Join(lines, "\n")
}

// buildCallGraph constructs an SSA-based call graph from loaded packages using
// the go-callgraph library, and writes call edges to the store.
// This replaces AST-level call resolution with whole-program analysis that
// correctly resolves interface dispatch.
func (idx *Indexer) buildCallGraph(pkgs []*packages.Package, method CallGraphMethod) error {
	var m callgraph.Method
	switch method {
	case CallGraphRTA:
		m = callgraph.RTA
	default:
		m = callgraph.CHA
	}

	log.Printf("running %s call graph analysis…", strings.ToUpper(string(method)))
	g, err := callgraph.BuildFromPackages(pkgs, m)
	if err != nil {
		return fmt.Errorf("build call graph: %w", err)
	}

	// Only write edges originating from indexed (non-excluded) packages.
	indexedPkgs := make(map[string]bool, len(pkgs))
	var indexedPkgPaths []string
	for _, pkg := range pkgs {
		if !idx.isExcluded(pkg) {
			indexedPkgs[pkg.PkgPath] = true
			indexedPkgPaths = append(indexedPkgPaths, pkg.PkgPath)
		}
	}

	// Clear stale call edges for this module's packages only, preserving edges
	// from other modules in multi-module repos indexed with --root.
	if err := idx.store.DeleteEdgesByKindAndPackages("calls", indexedPkgPaths); err != nil {
		return fmt.Errorf("clear old call edges: %w", err)
	}

	var written int
	g.ForEachEdgeInPackages(indexedPkgs, func(e callgraph.EdgeInfo) bool {
		fromID := callgraph.QualifiedID(e.Caller)
		toID := callgraph.QualifiedID(e.Callee)
		if fromID == "" || toID == "" {
			return true
		}
		if err := idx.store.UpsertEdge(store.Edge{FromID: fromID, ToID: toID, Kind: "calls"}); err != nil {
			log.Printf("warning: upsert call edge %s → %s: %v", fromID, toID, err)
		}
		written++
		return true
	})

	log.Printf("call graph: wrote %d call edges (%s)", written, strings.ToUpper(string(method)))
	return nil
}
