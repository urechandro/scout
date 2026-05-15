// Package protoindexer parses .proto files and writes services, RPCs,
// messages, and enums as symbols to the store.
package protoindexer

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/urechandro/scout/store"
)

// Config controls what gets indexed.
type Config struct {
	// Dir is the root directory to search for .proto files.
	Dir string
	// ExcludePaths skips files whose path contains any of these substrings.
	ExcludePaths []string
}

// Indexer parses .proto files and writes symbols to a Store.
type Indexer struct {
	cfg   Config
	store *store.Store
}

// New creates an Indexer for the given config and store.
func New(cfg Config, s *store.Store) *Indexer {
	return &Indexer{cfg: cfg, store: s}
}

// Run walks the configured directory and indexes all .proto files.
func (idx *Indexer) Run() error {
	var files []string
	err := filepath.WalkDir(idx.cfg.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != idx.cfg.Dir && (strings.HasPrefix(name, ".") || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".proto" {
			return nil
		}
		for _, excl := range idx.cfg.ExcludePaths {
			if strings.Contains(path, excl) {
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", idx.cfg.Dir, err)
	}

	var indexed int
	for _, f := range files {
		n, err := idx.indexFile(f)
		if err != nil {
			log.Printf("warning: proto %s: %v", f, err)
			continue
		}
		indexed += n
	}

	log.Printf("proto indexer: indexed %d symbols from %d files", indexed, len(files))
	return nil
}

// parseState tracks parser state while scanning a .proto file line by line.
type parseState struct {
	pkg        string // proto package declaration
	braceDepth int    // current nesting depth
	// current enclosing declaration at depth 1
	inService string
	inMessage string
	inEnum    string
	// index into syms slice for the current top-level block, so we can
	// update LineEnd when the closing brace is found.
	blockSymIdx int
	// comment lines accumulated before the next declaration
	pendingDoc []string
	// for multi-line rpc signatures
	rpcBuffer string
	inRPC     bool
}

func (idx *Indexer) indexFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var syms []store.Symbol
	state := &parseState{blockSymIdx: -1}
	lineNum := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Accumulate comments as pending docstring.
		if strings.HasPrefix(trimmed, "//") {
			state.pendingDoc = append(state.pendingDoc, strings.TrimSpace(strings.TrimPrefix(trimmed, "//")))
			continue
		}

		// Non-comment, non-empty line resets doc if it's not a declaration.
		doc := strings.Join(state.pendingDoc, "\n")

		// Track brace depth.
		opens := strings.Count(trimmed, "{")
		closes := strings.Count(trimmed, "}")

		// Package declaration.
		if strings.HasPrefix(trimmed, "package ") && state.braceDepth == 0 {
			state.pkg = strings.TrimSuffix(strings.TrimPrefix(trimmed, "package "), ";")
			state.pendingDoc = nil
			state.braceDepth += opens - closes
			continue
		}

		// Service declaration.
		if strings.HasPrefix(trimmed, "service ") && state.braceDepth == 0 {
			name := extractName(trimmed, "service ")
			sym := store.Symbol{
				ID:        qualifiedID(state.pkg, name),
				Package:   state.pkg,
				Name:      name,
				Kind:      "service",
				Signature: fmt.Sprintf("service %s", name),
				Docstring: doc,
				File:      path,
				LineStart: lineNum,
				LineEnd:   lineNum,
				Body:      fmt.Sprintf("/* %s:%d */", path, lineNum),
			}
			syms = append(syms, sym)
			state.inService = name
			state.inMessage = ""
			state.inEnum = ""
			state.blockSymIdx = len(syms) - 1
			state.pendingDoc = nil
			state.braceDepth += opens - closes
			continue
		}

		// Message declaration (top-level or nested).
		if strings.HasPrefix(trimmed, "message ") && state.braceDepth <= 1 {
			name := extractName(trimmed, "message ")
			qualName := name
			if state.inMessage != "" {
				qualName = state.inMessage + "." + name
			}
			sym := store.Symbol{
				ID:        qualifiedID(state.pkg, qualName),
				Package:   state.pkg,
				Name:      name,
				Kind:      "message",
				Signature: fmt.Sprintf("message %s", qualName),
				Docstring: doc,
				File:      path,
				LineStart: lineNum,
				LineEnd:   lineNum,
				Body:      fmt.Sprintf("/* %s:%d */", path, lineNum),
			}
			syms = append(syms, sym)
			if state.braceDepth == 0 {
				state.inMessage = qualName
				state.inService = ""
				state.inEnum = ""
				state.blockSymIdx = len(syms) - 1
			}
			state.pendingDoc = nil
			state.braceDepth += opens - closes
			continue
		}

		// Enum declaration.
		if strings.HasPrefix(trimmed, "enum ") && state.braceDepth <= 1 {
			name := extractName(trimmed, "enum ")
			qualName := name
			if state.inMessage != "" {
				qualName = state.inMessage + "." + name
			}
			sym := store.Symbol{
				ID:        qualifiedID(state.pkg, qualName),
				Package:   state.pkg,
				Name:      name,
				Kind:      "enum",
				Signature: fmt.Sprintf("enum %s", qualName),
				Docstring: doc,
				File:      path,
				LineStart: lineNum,
				LineEnd:   lineNum,
				Body:      fmt.Sprintf("/* %s:%d */", path, lineNum),
			}
			syms = append(syms, sym)
			if state.braceDepth == 0 {
				state.inEnum = qualName
				state.inService = ""
				state.inMessage = ""
				state.blockSymIdx = len(syms) - 1
			}
			state.pendingDoc = nil
			state.braceDepth += opens - closes
			continue
		}

		// RPC declaration inside a service (may span multiple lines).
		if state.braceDepth == 1 && state.inService != "" {
			if strings.HasPrefix(trimmed, "rpc ") || state.inRPC {
				state.rpcBuffer += " " + trimmed
				if strings.Contains(trimmed, ")") && (strings.Contains(state.rpcBuffer, "returns") && strings.Contains(state.rpcBuffer, ")")) {
					sig, reqMsg, respMsg := parseRPC(state.rpcBuffer)
					if sig != "" {
						rpcName := extractRPCName(state.rpcBuffer)
						sym := store.Symbol{
							ID:        qualifiedID(state.pkg, state.inService+"."+rpcName),
							Package:   state.pkg,
							Name:      rpcName,
							Kind:      "rpc",
							Signature: sig,
							Docstring: buildRPCDoc(doc, reqMsg, respMsg),
							File:      path,
							LineStart: lineNum,
							LineEnd:   lineNum,
							Body:      fmt.Sprintf("/* %s:%d */", path, lineNum),
						}
						syms = append(syms, sym)
					}
					state.rpcBuffer = ""
					state.inRPC = false
				} else {
					state.inRPC = true
				}
				state.pendingDoc = nil
				state.braceDepth += opens - closes
				continue
			}
		}

		// Reset pending doc on blank lines.
		if trimmed == "" {
			state.pendingDoc = nil
		}

		// Update brace depth and reset enclosing context when blocks close.
		prev := state.braceDepth
		state.braceDepth += opens - closes
		if state.braceDepth < prev && state.braceDepth == 0 {
			if state.blockSymIdx >= 0 && state.blockSymIdx < len(syms) {
				syms[state.blockSymIdx].LineEnd = lineNum
			}
			state.inService = ""
			state.inMessage = ""
			state.inEnum = ""
			state.blockSymIdx = -1
		}

		if trimmed != "" {
			state.pendingDoc = nil
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan: %w", err)
	}

	// Write symbols to store.
	for _, sym := range syms {
		if err := idx.store.UpsertSymbol(sym); err != nil {
			return 0, fmt.Errorf("upsert %s: %w", sym.ID, err)
		}
	}

	return len(syms), nil
}

func qualifiedID(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// extractName pulls the identifier from a declaration line.
// e.g. "service ShipmentService {" → "ShipmentService"
func extractName(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	name := strings.FieldsFunc(rest, func(r rune) bool {
		return r == ' ' || r == '{' || r == ';'
	})
	if len(name) > 0 {
		return name[0]
	}
	return ""
}

// parseRPC parses an RPC signature from a possibly multi-line buffer.
// Returns signature, request message, response message.
func parseRPC(buf string) (sig, req, resp string) {
	buf = strings.Join(strings.Fields(buf), " ")
	// Match: rpc Name(Req) returns (Resp)
	rpcIdx := strings.Index(buf, "rpc ")
	if rpcIdx < 0 {
		return "", "", ""
	}
	buf = buf[rpcIdx:]

	// Extract up to and including the returns clause.
	returnsIdx := strings.Index(buf, "returns")
	if returnsIdx < 0 {
		return "", "", ""
	}

	reqPart := buf[:returnsIdx]
	respPart := buf[returnsIdx:]

	req = extractParens(reqPart)
	resp = extractParens(respPart)

	name := extractRPCName(buf)
	if name == "" {
		return "", "", ""
	}

	return fmt.Sprintf("rpc %s(%s) returns (%s)", name, req, resp), req, resp
}

func extractRPCName(buf string) string {
	buf = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(buf), "rpc "))
	idx := strings.IndexAny(buf, "( ")
	if idx < 0 {
		return buf
	}
	return buf[:idx]
}

func extractParens(s string) string {
	open := strings.Index(s, "(")
	close := strings.Index(s, ")")
	if open < 0 || close < 0 || close <= open {
		return ""
	}
	return strings.TrimSpace(s[open+1 : close])
}

func buildRPCDoc(comment, req, resp string) string {
	parts := []string{}
	if comment != "" {
		parts = append(parts, comment)
	}
	if req != "" || resp != "" {
		parts = append(parts, fmt.Sprintf("Request: %s | Response: %s", req, resp))
	}
	return strings.Join(parts, "\n")
}
