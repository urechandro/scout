package indexer

import (
	"go/types"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/urechandro/scout/store"
)

// indexDependencies extracts exported symbol signatures from external
// dependency packages and upserts them into the store. Only signatures
// are stored (no bodies, no docstrings) — enough for FTS discovery and
// get_body reference resolution without falling back to grep/Read.
func (idx *Indexer) indexDependencies(pkgs []*packages.Package) error {
	modulePath := DetectModulePath(idx.cfg.Dir)

	seen := make(map[string]bool)
	var deps []*types.Package

	for _, pkg := range pkgs {
		for _, imp := range pkg.Imports {
			if imp.Types == nil {
				continue
			}
			path := imp.Types.Path()
			if seen[path] {
				continue
			}
			seen[path] = true

			if modulePath != "" && strings.HasPrefix(path, modulePath) {
				continue
			}
			if !strings.Contains(path, ".") {
				continue
			}

			deps = append(deps, imp.Types)
		}
	}

	var total int
	for _, dep := range deps {
		n, err := idx.indexDepPackage(dep)
		if err != nil {
			log.Printf("warning: index dep %s: %v", dep.Path(), err)
			continue
		}
		total += n
	}

	log.Printf("indexed %d symbols from %d dependency packages", total, len(deps))
	return nil
}

func (idx *Indexer) indexDepPackage(tp *types.Package) (int, error) {
	scope := tp.Scope()
	var count int

	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}

		protoGen := obj.Pos().IsValid() && isProtoGenerated(idx.fset.Position(obj.Pos()).Filename)

		sym := idx.depSymbol(tp.Path(), obj)
		if err := idx.store.UpsertSymbol(sym); err != nil {
			return count, err
		}
		count++

		// Skip methods on proto-generated dep types — they're boilerplate
		// (Reset, ProtoMessage, Descriptor, etc.) and bloat the index.
		if protoGen {
			continue
		}

		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		for i := 0; i < named.NumMethods(); i++ {
			m := named.Method(i)
			if !m.Exported() {
				continue
			}
			msym := idx.depSymbol(tp.Path(), m)
			if err := idx.store.UpsertSymbol(msym); err != nil {
				return count, err
			}
			count++
		}
	}

	return count, nil
}

func (idx *Indexer) depSymbol(pkgPath string, obj types.Object) store.Symbol {
	kind := depObjKind(obj)

	var file string
	var lineStart, lineEnd int
	if pos := obj.Pos(); pos.IsValid() {
		p := idx.fset.Position(pos)
		file = p.Filename
		lineStart = p.Line
		lineEnd = p.Line
	}

	return store.Symbol{
		ID:        qualifiedID(pkgPath, obj),
		Package:   pkgPath,
		Name:      obj.Name(),
		Kind:      kind,
		Signature: types.ObjectString(obj, shortQualifier),
		File:      file,
		LineStart: lineStart,
		LineEnd:   lineEnd,
	}
}

func depObjKind(obj types.Object) string {
	switch o := obj.(type) {
	case *types.TypeName:
		switch o.Type().Underlying().(type) {
		case *types.Interface:
			return "interface"
		case *types.Struct:
			return "struct"
		default:
			return "type"
		}
	case *types.Func:
		sig := o.Type().(*types.Signature)
		if sig.Recv() != nil {
			return "method"
		}
		return "func"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	}
	return "type"
}

// DetectModulePath reads the module path from dir's go.mod. Returns "" when
// there is no go.mod or no module line — callers treat that as "unknown".
func DetectModulePath(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module"))
		}
	}
	return ""
}
