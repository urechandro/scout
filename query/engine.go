// Package query implements context retrieval: FTS search, graph expansion, and ranking.
package query

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urechandro/scout/store"
)

// ContextRequest is the input to GetRelevantContext.
type ContextRequest struct {
	// Task is a natural language description of what the model wants to do.
	Task string
	// BudgetTokens is the approximate token budget for the response.
	// Defaults to 1000 (precise) or 2000 (discovery) if zero.
	BudgetTokens int
	// MaxExpansionDepth controls graph hop count. 1 is usually right; 0 disables.
	MaxExpansionDepth int
	// Verbose includes docstrings, why, and score in results. Default (false)
	// returns compact pointers: id, kind, signature, file:line.
	Verbose bool
}

// SymbolSummary is a trimmed view of a symbol returned to the model.
// Full body is only fetched on demand via GetBody.
type SymbolSummary struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Signature string  `json:"signature"`
	Docstring string  `json:"docstring,omitempty"`
	File      string  `json:"file"`
	LineStart int     `json:"line_start"`
	LineEnd   int     `json:"line_end,omitempty"`
	Why       string  `json:"why,omitempty"`
	Score     float64 `json:"score,omitempty"`
}

// PackageHit summarizes how many symbols matched in a given package.
type PackageHit struct {
	Package string `json:"package"`
	Count   int    `json:"count"`
	Kinds   string `json:"kinds"`
}

// ContextResponse is returned by GetRelevantContext.
type ContextResponse struct {
	Packages  []PackageHit    `json:"packages,omitempty"`
	Symbols   []SymbolSummary `json:"symbols"`
	Truncated int             `json:"truncated"`
}

// Engine executes context queries against a Store.
type Engine struct {
	store *store.Store
}

// New creates a query Engine backed by the given store.
func New(s *store.Store) *Engine {
	return &Engine{store: s}
}

// queryType classifies a query as either a precise symbol lookup or a broad
// discovery question. Precise queries get a smaller token budget and skip FTS
// when Phase 1 produces hits.
type queryType int

const (
	queryDiscovery queryType = iota
	queryPrecise
)

// classifyQuery determines whether a query is a precise symbol lookup
// (single compound/dotted identifier like "grpc.Dial" or "CreateShipmentLeg")
// or a broader discovery question ("how does auth work").
func classifyQuery(task string) queryType {
	fields := strings.Fields(task)
	if len(fields) != 1 {
		return queryDiscovery
	}
	w := strings.TrimFunc(fields[0], func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.')
	})
	if isCompoundIdent(w) {
		return queryPrecise
	}
	if strings.Contains(w, ".") {
		return queryPrecise
	}
	return queryDiscovery
}

// GetRelevantContext is the primary MCP tool entry point.
func (e *Engine) GetRelevantContext(req ContextRequest) (*ContextResponse, error) {
	qtype := classifyQuery(req.Task)

	if req.BudgetTokens == 0 {
		switch qtype {
		case queryPrecise:
			req.BudgetTokens = 600
		default:
			req.BudgetTokens = 2000
		}
	}
	if req.MaxExpansionDepth == 0 {
		req.MaxExpansionDepth = 1
	}

	scored := make(map[string]*SymbolSummary)

	// Phase 1: exact name lookups for compound identifiers.
	// "CreateShipmentLeg ShipmentLeg service" → look up "CreateShipmentLeg"
	// and "ShipmentLeg" by name directly. These are high-confidence hits.
	compounds := extractCompoundIdents(req.Task)
	for _, name := range compounds {
		syms, err := e.store.GetByName(name)
		if err != nil {
			continue
		}
		for _, sym := range syms {
			score := 3.0 + kindWeight(sym) + implBoost(sym) + generatedPenalty(sym)
			s := toSummary(sym, score, "exact name match")
			scored[sym.ID] = &s
		}
	}

	// For precise queries, skip FTS entirely. When Phase 1 found hits we
	// already have high-confidence results. When it found nothing, falling
	// through to FTS would decompose the compound name into individual words
	// (e.g. "BatchDelete" → "batch OR delete") producing noisy, misleading
	// results. Better to return empty so the model knows the symbol doesn't
	// exist.
	skipFTS := qtype == queryPrecise

	// Phase 2: FTS for remaining discovery.
	if !skipFTS {
		ftsQuery := buildFTSQuery(req.Task)
		queryTerms := strings.Split(ftsQuery, " OR ")
		compoundParts := extractCompoundParts(req.Task)
		scoringTerms := append(queryTerms, compoundParts...)
		// Fetch extra to ensure source hits aren't crowded out by generated boilerplate.
		ftsLimit := 30 + len(queryTerms)*10
		hits, err := e.store.SearchFTS(ftsQuery, ftsLimit*3)
		if err != nil {
			return nil, fmt.Errorf("fts search: %w", err)
		}

		// Partition: score source hits first, then fill with generated up to a cap.
		var sourceHits, genHits []store.Symbol
		for _, sym := range hits {
			if isGenerated(sym.File) {
				genHits = append(genHits, sym)
			} else {
				sourceHits = append(sourceHits, sym)
			}
		}
		maxGen := 5
		if len(sourceHits) == 0 {
			maxGen = ftsLimit
		}
		if len(genHits) > maxGen {
			genHits = genHits[:maxGen]
		}
		filtered := append(sourceHits, genHits...)

		nameFreq := make(map[string]int, len(filtered))
		for _, sym := range filtered {
			nameFreq[strings.ToLower(sym.Name)]++
		}

		for i, sym := range filtered {
			if scored[sym.ID] != nil {
				continue
			}
			score := float64(len(filtered)-i) / float64(len(filtered))
			nameLower := strings.ToLower(sym.Name)
			decomposed := strings.ToLower(decomposeIdentifier(sym.Name))
			if nameFreq[nameLower] < 3 {
				for _, term := range scoringTerms {
					if nameLower == term {
						score += nameMatchBonus(sym)
						break
					}
				}
			}
			score += termCoverage(sym, scoringTerms, decomposed)
			score += kindWeight(sym)
			score += implBoost(sym)
			score += generatedPenalty(sym)
			s := toSummary(sym, score, "semantic match")
			scored[sym.ID] = &s
		}
	}

	scored = dedup(scored)

	if req.MaxExpansionDepth > 0 {
		expanded, err := e.expand(scored, req.MaxExpansionDepth)
		if err != nil {
			return nil, fmt.Errorf("graph expansion: %w", err)
		}
		for id, sym := range expanded {
			if _, exists := scored[id]; !exists {
				scored[id] = sym
			}
		}
	}

	ranked := rankSymbols(scored)
	ranked = prioritizeSource(ranked)
	kept, truncated := trimToBudget(ranked, req.BudgetTokens, req.Verbose)

	if !req.Verbose {
		for i := range kept {
			kept[i].Docstring = ""
			kept[i].Why = ""
			kept[i].Score = 0
		}
	}

	return &ContextResponse{
		Packages:  buildPackageSummary(kept),
		Symbols:   kept,
		Truncated: truncated,
	}, nil
}

// BodyResponse is returned by GetBody. It includes the symbol and optional
// disambiguation hints when the lookup was fuzzy.
type BodyResponse struct {
	*store.Symbol
	Hint       string          `json:"hint,omitempty"`
	OtherIDs   []string        `json:"other_ids,omitempty"`
	References []SymbolSummary `json:"references,omitempty"`
}

// GetBody returns the full source of a symbol.
// If the exact ID is not found, it falls back to suffix and name matching
// so that guessed or partial IDs still resolve.
func (e *Engine) GetBody(symbolID string) (*BodyResponse, error) {
	sym, candidates, err := e.store.FuzzyGetSymbol(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get body for %s: %w", symbolID, err)
	}

	// If body is a stale line reference (old index or proto-generated), read from file.
	if isLineRef(sym.Body) {
		if body, err := readLines(sym.File, sym.LineStart, sym.LineEnd); err == nil {
			sym.Body = body
		}
	}

	// Dep symbols have only a declaration line (LineStart == LineEnd).
	// Read the full declaration from the module cache on demand.
	if sym.LineStart == sym.LineEnd && sym.File != "" && sym.LineStart > 0 {
		if body, end, err := readDecl(sym.File, sym.LineStart); err == nil {
			sym.Body = body
			sym.LineEnd = end
		}
	}

	resp := &BodyResponse{Symbol: sym}

	// If there were multiple candidates, include hints so the model can
	// pick the right one next time.
	if len(candidates) > 1 {
		resp.Hint = fmt.Sprintf("Ambiguous: %d symbols matched. Showing first. Use an exact ID from other_ids for a specific one.", len(candidates))
		for _, c := range candidates[1:] {
			resp.OtherIDs = append(resp.OtherIDs, c.ID)
		}
	} else if sym.ID != symbolID {
		resp.Hint = fmt.Sprintf("Fuzzy match: requested %q, resolved to %q", symbolID, sym.ID)
	}

	resp.References = e.extractReferences(sym)

	return resp, nil
}

// extractReferences finds types and functions referenced in a symbol's body
// and returns their summaries (signatures only, no bodies).
func (e *Engine) extractReferences(sym *store.Symbol) []SymbolSummary {
	if sym.Body == "" {
		return nil
	}

	callNames := extractCallIdents(sym.Body)
	typeNames := extractTypeIdents(sym.Body)

	nameSet := make(map[string]bool, len(callNames)+len(typeNames))
	var names []string
	for _, n := range callNames {
		if !nameSet[n] {
			nameSet[n] = true
			names = append(names, n)
		}
	}
	for _, n := range typeNames {
		if !nameSet[n] {
			nameSet[n] = true
			names = append(names, n)
		}
	}

	if len(names) == 0 {
		return nil
	}

	seen := map[string]bool{sym.ID: true}
	var refs []SymbolSummary
	for _, name := range names {
		syms, err := e.store.GetByName(name)
		if err != nil {
			continue
		}
		for _, s := range syms {
			if seen[s.ID] || isGenerated(s.File) {
				continue
			}
			seen[s.ID] = true
			refs = append(refs, toSummary(s, 0, "referenced"))
		}
	}

	if len(refs) > 10 {
		refs = refs[:10]
	}
	return refs
}

func isGenerated(file string) bool {
	return strings.HasSuffix(file, ".pb.go") || strings.HasSuffix(file, ".pb.gw.go") ||
		strings.Contains(file, "/gen/")
}

// ImpactLayer classifies a symbol's position in the codebase stack.
type ImpactLayer string

const (
	LayerProto          ImpactLayer = "proto"
	LayerGenerated      ImpactLayer = "generated"
	LayerImplementation ImpactLayer = "implementation"
	LayerTest           ImpactLayer = "test"
)

// ImpactResponse is returned by GetImpact.
type ImpactResponse struct {
	Symbol         SymbolSummary   `json:"symbol"`
	Proto          []SymbolSummary `json:"proto,omitempty"`
	Generated      []SymbolSummary `json:"generated,omitempty"`
	Implementation []SymbolSummary `json:"implementation,omitempty"`
	Tests          []SymbolSummary `json:"tests,omitempty"`
	Total          int             `json:"total"`
}

// GetImpact traces all symbols affected if the given symbol changes.
// Unlike GetCallers (one hop, Go-only), this crosses proto↔Go boundaries,
// follows name-based linkage through generated code, and groups results by layer.
func (e *Engine) GetImpact(symbolID string) (*ImpactResponse, error) {
	resolvedID, err := e.resolveSymbolID(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get impact for %s: %w", symbolID, err)
	}

	sym, err := e.store.GetSymbol(resolvedID)
	if err != nil {
		return nil, fmt.Errorf("get impact for %s: %w", resolvedID, err)
	}

	resp := &ImpactResponse{
		Symbol: toSummary(*sym, 0, "target"),
	}

	visited := map[string]bool{resolvedID: true}
	var affected []SymbolSummary

	// Phase 1: find same-name symbols across layers (proto↔Go linkage).
	sameNameSyms, _ := e.store.GetByName(sym.Name)
	for _, s := range sameNameSyms {
		if visited[s.ID] {
			continue
		}
		visited[s.ID] = true
		why := "same name (cross-layer)"
		if isGenerated(s.File) {
			why = "generated code for " + sym.Name
		}
		affected = append(affected, toSummary(s, 0, why))
	}

	// Phase 2: if this is an RPC, find request/response messages.
	if sym.Kind == "rpc" {
		reqName, respName := parseRPCMessages(sym.Signature)
		for _, msgName := range []string{reqName, respName} {
			if msgName == "" {
				continue
			}
			msgs, _ := e.store.GetByName(msgName)
			for _, m := range msgs {
				if visited[m.ID] {
					continue
				}
				visited[m.ID] = true
				affected = append(affected, toSummary(m, 0, "request/response message"))
			}
		}
	}

	// Phase 3: if this implements something, include what it implements.
	impls, _ := e.store.GetImplements(resolvedID)
	for _, s := range impls {
		if visited[s.ID] {
			continue
		}
		visited[s.ID] = true
		why := "implements interface"
		if s.Kind == "rpc" {
			why = "implements RPC: " + s.Signature
		}
		affected = append(affected, toSummary(s, 0, why))
	}

	// Phase 4: if this is an interface or proto RPC, find implementors.
	if sym.Kind == "interface" || sym.Kind == "rpc" || sym.Kind == "service" {
		implementors, _ := e.store.GetImplementors(resolvedID)
		for _, s := range implementors {
			if visited[s.ID] {
				continue
			}
			visited[s.ID] = true
			affected = append(affected, toSummary(s, 0, "implements "+sym.Name))
		}
	}

	// Phase 5: collect callers of the target and all related symbols.
	// We trace callers for the original symbol plus any cross-layer matches.
	callerSeeds := []string{resolvedID}
	for _, s := range affected {
		if !isGenerated(s.File) && s.Kind != "rpc" && s.Kind != "message" && s.Kind != "service" && s.Kind != "enum" {
			callerSeeds = append(callerSeeds, s.ID)
		}
	}

	for _, seedID := range callerSeeds {
		callers, _ := e.store.GetCallers(seedID)
		for _, c := range callers {
			if visited[c.ID] {
				continue
			}
			visited[c.ID] = true
			affected = append(affected, toSummary(c, 0, "calls "+seedID))
		}
	}

	// Phase 6: body-reference fallback for symbols without call edges.
	bodyCallers, _ := e.store.GetCallersFromBody(sym.Name)
	for _, c := range bodyCallers {
		if visited[c.ID] {
			continue
		}
		visited[c.ID] = true
		affected = append(affected, toSummary(c, 0, "references "+sym.Name+" (heuristic)"))
	}

	// Classify into layers, deduping generated copies across /gen/ directories.
	var generated []SymbolSummary
	for _, s := range affected {
		layer := classifyLayer(s.File)
		switch layer {
		case LayerProto:
			resp.Proto = append(resp.Proto, s)
		case LayerGenerated:
			generated = append(generated, s)
		case LayerTest:
			resp.Tests = append(resp.Tests, s)
		default:
			resp.Implementation = append(resp.Implementation, s)
		}
	}
	resp.Generated = dedupGenerated(generated)

	resp.Total = len(resp.Proto) + len(resp.Generated) + len(resp.Implementation) + len(resp.Tests)
	return resp, nil
}

// dedupGenerated collapses symbols with the same name+kind across /gen/ directories,
// preferring the backend copy.
func dedupGenerated(syms []SymbolSummary) []SymbolSummary {
	type key struct{ name, kind string }
	groups := map[key][]SymbolSummary{}
	var order []key
	for _, s := range syms {
		name := s.ID
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		k := key{name, s.Kind}
		if _, exists := groups[k]; !exists {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	var out []SymbolSummary
	for _, k := range order {
		group := groups[k]
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		best := group[0]
		for _, s := range group[1:] {
			if strings.Contains(s.File, "backend") {
				best = s
				break
			}
		}
		best.Why += fmt.Sprintf(" (+%d copies)", len(group)-1)
		out = append(out, best)
	}
	return out
}

func classifyLayer(file string) ImpactLayer {
	if strings.HasSuffix(file, ".proto") {
		return LayerProto
	}
	if isGenerated(file) {
		return LayerGenerated
	}
	if strings.HasSuffix(file, "_test.go") {
		return LayerTest
	}
	return LayerImplementation
}

// GetCallers returns summaries of all symbols that call the given symbol.
// Falls back to fuzzy ID resolution if the exact ID is not found.
// If no call-graph callers exist, checks whether this symbol implements a
// proto RPC or interface and returns those as entry points instead of [].
func (e *Engine) GetCallers(symbolID string) ([]SymbolSummary, error) {
	resolvedID, err := e.resolveSymbolID(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get callers for %s: %w", symbolID, err)
	}

	callers, err := e.store.GetCallers(resolvedID)
	if err != nil {
		return nil, fmt.Errorf("get callers for %s: %w", resolvedID, err)
	}

	if len(callers) > 0 {
		summaries := make([]SymbolSummary, len(callers))
		for i, sym := range callers {
			summaries[i] = toSummary(sym, 0, "direct caller")
		}
		return summaries, nil
	}

	// Fallback 1: check if this symbol implements a proto RPC or interface.
	// This catches gRPC service methods where callers are external (gRPC framework).
	impls, err := e.store.GetImplements(resolvedID)
	if err == nil && len(impls) > 0 {
		var summaries []SymbolSummary
		for _, sym := range impls {
			why := "implements interface"
			if sym.Kind == "rpc" {
				why = fmt.Sprintf("gRPC entry point: %s", sym.Signature)
			}
			summaries = append(summaries, toSummary(sym, 0, why))
		}
		return summaries, nil
	}

	// Fallback 2: search for functions whose body references this symbol by name.
	// Catches constructors like New(), utility functions, etc.
	sym, _, err := e.store.FuzzyGetSymbol(resolvedID)
	if err != nil {
		return nil, nil
	}
	bodyCallers, err := e.store.GetCallersFromBody(sym.Name)
	if err != nil {
		return nil, nil
	}
	var summaries []SymbolSummary
	for _, caller := range bodyCallers {
		if caller.ID == resolvedID {
			continue // skip self
		}
		summaries = append(summaries, toSummary(caller, 0, "body reference (heuristic)"))
	}
	return summaries, nil
}

// GetCallees returns summaries of all symbols called by the given symbol.
// Falls back to fuzzy ID resolution if the exact ID is not found.
// If no call-graph edges exist, falls back to extracting identifiers from
// the function body and looking them up in the symbol table.
func (e *Engine) GetCallees(symbolID string) ([]SymbolSummary, error) {
	resolvedID, err := e.resolveSymbolID(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get callees for %s: %w", symbolID, err)
	}

	callees, err := e.store.GetCallees(resolvedID)
	if err != nil {
		return nil, fmt.Errorf("get callees for %s: %w", resolvedID, err)
	}

	if len(callees) > 0 {
		summaries := make([]SymbolSummary, len(callees))
		for i, sym := range callees {
			summaries[i] = toSummary(sym, 0, "direct callee")
		}
		return summaries, nil
	}

	// Fallback: extract identifiers from the function body and look them up.
	return e.calleesFromBody(resolvedID)
}

// calleesFromBody extracts exported identifiers from a symbol's body and
// matches them against the symbol table. This is a crude but useful fallback
// when RTA call edges are missing — it catches s.outbox.AddToOutbox,
// tx.InsertFoo, etc.
func (e *Engine) calleesFromBody(symbolID string) ([]SymbolSummary, error) {
	sym, err := e.store.GetSymbol(symbolID)
	if err != nil {
		return nil, nil
	}

	body := sym.Body
	if isLineRef(body) {
		body, err = readLines(sym.File, sym.LineStart, sym.LineEnd)
		if err != nil {
			return nil, nil
		}
	}

	// Extract identifiers that look like method/function calls.
	names := extractCallIdents(body)
	if len(names) == 0 {
		return nil, nil
	}

	// Look up each name in the symbol table, skipping self.
	seen := map[string]bool{symbolID: true}
	var summaries []SymbolSummary
	for _, name := range names {
		syms, err := e.store.GetByName(name)
		if err != nil {
			continue
		}
		for _, s := range syms {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			summaries = append(summaries, toSummary(s, 0, "body reference (heuristic)"))
		}
	}

	return summaries, nil
}

// extractCallIdents extracts exported Go identifiers from source code that
// look like function/method calls. Returns unique names.
func extractCallIdents(body string) []string {
	seen := map[string]bool{}
	var names []string

	// Match patterns like .MethodName( or package.FuncName(
	// We want the identifier immediately before '('.
	for i := 0; i < len(body); i++ {
		if body[i] != '(' {
			continue
		}
		// Walk backward to find the identifier before '('.
		end := i
		for end > 0 && (body[end-1] == ' ' || body[end-1] == '\t') {
			end--
		}
		start := end
		for start > 0 && isIdentChar(body[start-1]) {
			start--
		}
		if start == end {
			continue
		}
		name := body[start:end]
		// Only keep exported names (starts with uppercase).
		if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	return names
}

// extractTypeIdents extracts exported PascalCase identifiers that appear as
// type references (not call sites). Catches *TypeName, []TypeName, TypeName{},
// and TypeName in signatures/declarations.
func extractTypeIdents(body string) []string {
	seen := map[string]bool{}
	var names []string

	for i := 0; i < len(body); i++ {
		if !isUpperByte(body[i]) {
			continue
		}
		// Check preceding char: must be a type-context trigger or start of line.
		if i > 0 {
			prev := body[i-1]
			if isIdentChar(prev) {
				continue
			}
		}
		start := i
		end := i
		for end < len(body) && isIdentChar(body[end]) {
			end++
		}
		name := body[start:end]
		if len(name) < 3 || seen[name] {
			i = end - 1
			continue
		}
		// Skip Go keywords and common non-type identifiers.
		if isGoKeyword(name) {
			i = end - 1
			continue
		}
		// Must look like a type name: at least one lowercase letter (filters ALL_CAPS constants).
		hasLower := false
		for _, c := range name {
			if c >= 'a' && c <= 'z' {
				hasLower = true
				break
			}
		}
		if !hasLower {
			i = end - 1
			continue
		}
		seen[name] = true
		names = append(names, name)
		i = end - 1
	}

	return names
}

func isUpperByte(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

func isGoKeyword(s string) bool {
	switch s {
	case "Break", "Case", "Chan", "Const", "Continue", "Default", "Defer",
		"Else", "Fallthrough", "For", "Func", "Go", "Goto", "If",
		"Import", "Interface", "Map", "Package", "Range", "Return",
		"Select", "Struct", "Switch", "Type", "Var",
		"Context", "Server", "String", "Error":
		return true
	}
	return false
}

func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// resolveSymbolID resolves a potentially guessed/partial symbol ID to an exact one.
func (e *Engine) resolveSymbolID(symbolID string) (string, error) {
	// Try exact first (fast path).
	if _, err := e.store.GetSymbol(symbolID); err == nil {
		return symbolID, nil
	}
	// Fuzzy fallback.
	sym, _, err := e.store.FuzzyGetSymbol(symbolID)
	if err != nil {
		return "", err
	}
	return sym.ID, nil
}

// PatternSlice is a vertical slice of one RPC pattern: proto → messages → Go impl.
type PatternSlice struct {
	RPC             *SymbolWithBody `json:"rpc,omitempty"`
	RequestMessage  *SymbolWithBody `json:"request_message,omitempty"`
	ResponseMessage *SymbolWithBody `json:"response_message,omitempty"`
	Implementation  *SymbolWithBody `json:"implementation,omitempty"`
}

// SymbolWithBody is a symbol summary plus its full source body.
type SymbolWithBody struct {
	SymbolSummary
	Body string `json:"body,omitempty"`
}

// GetPattern returns a single end-to-end vertical slice for the best RPC matching the task.
// If no exact match, returns the nearest match with a hint rather than an error.
func (e *Engine) GetPattern(task string) (*PatternSlice, error) {
	slices, err := e.getPatternSlices(task, 1)
	if err != nil {
		return nil, err
	}
	if len(slices) > 0 {
		return slices[0], nil
	}

	// Graceful degradation: try broader search for any method matching the task.
	ftsQuery := buildFTSQuery(task)
	hits, err := e.store.SearchFTS(ftsQuery, 10)
	if err != nil || len(hits) == 0 {
		return nil, fmt.Errorf("no pattern found for %q — try get_relevant_context instead", task)
	}

	// Build a partial slice from the best hit.
	best := hits[0]
	body := best.Body
	if isLineRef(body) {
		body, _ = readLines(best.File, best.LineStart, best.LineEnd)
	}

	return &PatternSlice{
		Implementation: &SymbolWithBody{
			SymbolSummary: toSummary(best, 1.0, fmt.Sprintf("nearest match (no RPC found for %q)", task)),
			Body:          body,
		},
	}, nil
}

// ConventionResult is returned by GetConventions.
type ConventionResult struct {
	// Name is the convention slug (from conventions.yaml) or the search topic.
	Name string `json:"name"`
	// Description explains what the pattern is and why it exists.
	Description string `json:"description"`
	// Structure is pseudocode showing the repeating shape.
	Structure string `json:"structure,omitempty"`
	// Examples are resolved symbols that demonstrate the pattern, with summaries.
	Examples []SymbolSummary `json:"examples,omitempty"`
	// Hint provides guidance if results are partial or from fallback.
	Hint string `json:"hint,omitempty"`
}

// GetConventions looks up a documented convention by topic. It checks the
// conventions table first (populated from conventions.yaml), resolves example
// symbol IDs to real symbols, and falls back to FTS if no convention matches.
func (e *Engine) GetConventions(topic string) (*ConventionResult, error) {
	// 1. Try documented conventions (conventions.yaml → DB).
	ftsQuery := buildFTSQuery(topic)
	conventions, err := e.store.SearchConventions(ftsQuery)
	if err == nil && len(conventions) > 0 {
		c := conventions[0]
		result := &ConventionResult{
			Name:        c.Name,
			Description: c.Description,
			Structure:   c.Structure,
		}

		// Resolve example symbol IDs to real symbols via fuzzy lookup.
		for _, exampleID := range c.Examples {
			sym, _, err := e.store.FuzzyGetSymbol(exampleID)
			if err != nil {
				continue
			}
			result.Examples = append(result.Examples, toSummary(*sym, 1.0, "documented example"))
		}

		if len(conventions) > 1 {
			var others []string
			for _, other := range conventions[1:] {
				others = append(others, other.Name)
			}
			result.Hint = fmt.Sprintf("Also related: %s", strings.Join(others, ", "))
		}

		return result, nil
	}

	// 2. Fallback: FTS search across all symbols.
	hits, err := e.store.SearchFTS(ftsQuery, 30)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}

	if len(hits) == 0 {
		return &ConventionResult{
			Name: topic,
			Hint: fmt.Sprintf("No documented convention or symbols matched %q. Try different terms or use get_relevant_context.", topic),
		}, nil
	}

	// Score and sort: prefer svc implementations.
	type scored struct {
		sym   store.Symbol
		score float64
	}
	var items []scored
	for i, sym := range hits {
		s := float64(len(hits)-i) / float64(len(hits))
		s += implBoost(sym)
		s += generatedPenalty(sym)
		items = append(items, scored{sym: sym, score: s})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })

	if len(items) > 10 {
		items = items[:10]
	}

	var summaries []SymbolSummary
	for _, item := range items {
		summaries = append(summaries, toSummary(item.sym, item.score, "FTS match (no documented convention)"))
	}

	return &ConventionResult{
		Name:     topic,
		Examples: summaries,
		Hint:     fmt.Sprintf("No documented convention for %q — showing %d symbol matches from FTS. Add a conventions.yaml entry to get structured results.", topic, len(summaries)),
	}, nil
}

func (e *Engine) getPatternSlices(task string, limit int) ([]*PatternSlice, error) {
	type scored struct {
		sym   store.Symbol
		score float64
	}

	var candidates []scored

	// Phase 1: try exact name lookup for compound identifiers.
	// "create shipment leg" → look up "CreateShipmentLeg" as an RPC name.
	for _, name := range extractCompoundIdents(task) {
		if rpcs, err := e.store.GetByNameAndKind(name, "rpc"); err == nil {
			for _, r := range rpcs {
				candidates = append(candidates, scored{sym: r, score: 3.0})
			}
		}
		if len(candidates) == 0 {
			if methods, err := e.store.GetByName(name); err == nil {
				for _, m := range methods {
					s := implBoost(m) + generatedPenalty(m)
					candidates = append(candidates, scored{sym: m, score: s})
				}
			}
		}
	}

	// Phase 2: fall back to FTS if no exact match.
	if len(candidates) == 0 {
		ftsQuery := buildFTSQuery(task)
		queryTerms := strings.Split(ftsQuery, " OR ")

		scoreAndRank := func(hits []store.Symbol) []scored {
			var out []scored
			for _, h := range hits {
				decomposed := strings.ToLower(decomposeIdentifier(h.Name))
				s := termCoverage(h, queryTerms, decomposed) + implBoost(h) + generatedPenalty(h)
				out = append(out, scored{sym: h, score: s})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
			return out
		}

		rpcs, _ := e.store.SearchFTSByKinds(ftsQuery, []string{"rpc"}, 20)
		candidates = scoreAndRank(rpcs)

		if len(candidates) == 0 {
			methods, _ := e.store.SearchFTSByKinds(ftsQuery, []string{"method"}, 30)
			var svc, other []store.Symbol
			for _, m := range methods {
				pkg := strings.ToLower(m.Package)
				if strings.Contains(pkg, "svc") || strings.Contains(pkg, "server") || strings.Contains(pkg, "service") {
					svc = append(svc, m)
				} else {
					other = append(other, m)
				}
			}
			if len(svc) > 0 {
				candidates = scoreAndRank(svc)
			} else {
				candidates = scoreAndRank(other)
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	// get_pattern (limit=1) includes bodies; get_conventions (limit>1) returns summaries only.
	withBodies := limit == 1

	var slices []*PatternSlice
	for _, c := range candidates {
		slice, err := e.buildSlice(c.sym, withBodies)
		if err != nil {
			return nil, err
		}
		slices = append(slices, slice)
	}

	return slices, nil
}

// buildSlice builds a PatternSlice. withBodies controls whether full source is included.
func (e *Engine) buildSlice(rpc store.Symbol, withBodies bool) (*PatternSlice, error) {
	slice := &PatternSlice{}

	var rpcBody string
	if withBodies {
		rpcBody = rpc.Body
		if isLineRef(rpcBody) {
			rpcBody, _ = readLines(rpc.File, rpc.LineStart, rpc.LineEnd)
		}
	}
	slice.RPC = &SymbolWithBody{
		SymbolSummary: toSummary(rpc, 1.0, "rpc match"),
		Body:          rpcBody,
	}

	// Parse request/response message names from the RPC signature.
	reqName, respName := parseRPCMessages(rpc.Signature)

	if reqName != "" {
		if sym, err := e.store.GetByNameAndKind(reqName, "message"); err == nil && len(sym) > 0 {
			var body string
			if withBodies {
				body = sym[0].Body
				if isLineRef(body) {
					body, _ = readLines(sym[0].File, sym[0].LineStart, sym[0].LineEnd)
				}
			}
			slice.RequestMessage = &SymbolWithBody{
				SymbolSummary: toSummary(sym[0], 0, "request message"),
				Body:          body,
			}
		}
	}

	if respName != "" && respName != reqName {
		if sym, err := e.store.GetByNameAndKind(respName, "message"); err == nil && len(sym) > 0 {
			var body string
			if withBodies {
				body = sym[0].Body
				if isLineRef(body) {
					body, _ = readLines(sym[0].File, sym[0].LineStart, sym[0].LineEnd)
				}
			}
			slice.ResponseMessage = &SymbolWithBody{
				SymbolSummary: toSummary(sym[0], 0, "response message"),
				Body:          body,
			}
		}
	}

	// Find the Go implementation — a method with the same name.
	if methods, err := e.store.GetByNameAndKind(rpc.Name, "method"); err == nil {
		impl := preferSvcMethod(methods)
		if impl != nil {
			var implBody string
			if withBodies {
				implBody = impl.Body
				if isLineRef(implBody) {
					implBody, _ = readLines(impl.File, impl.LineStart, impl.LineEnd)
				}
			}
			slice.Implementation = &SymbolWithBody{
				SymbolSummary: toSummary(*impl, 0, "go implementation"),
				Body:          implBody,
			}
		}
	}

	return slice, nil
}

// parseRPCMessages extracts request and response message names from an RPC signature.
func parseRPCMessages(sig string) (req, resp string) {
	// "rpc Foo(RequestMsg) returns (ResponseMsg)"
	open := strings.Index(sig, "(")
	close := strings.Index(sig, ")")
	if open >= 0 && close > open {
		req = strings.TrimSpace(sig[open+1 : close])
	}
	returnsIdx := strings.Index(sig, "returns")
	if returnsIdx < 0 {
		return req, ""
	}
	rest := sig[returnsIdx:]
	open = strings.Index(rest, "(")
	close = strings.Index(rest, ")")
	if open >= 0 && close > open {
		resp = strings.TrimSpace(rest[open+1 : close])
	}
	return req, resp
}

// preferSvcMethod picks the best Go implementation candidate from a list of methods.
// Prefers methods in packages containing "svc".
func preferSvcMethod(methods []store.Symbol) *store.Symbol {
	for _, m := range methods {
		if strings.Contains(m.Package, "svc") {
			return &m
		}
	}
	if len(methods) > 0 {
		return &methods[0]
	}
	return nil
}

// FlowResponse is returned by GetFlow.
type FlowResponse struct {
	Callers []SymbolWithBody `json:"callers,omitempty"`
	Symbol  SymbolWithBody   `json:"symbol"`
	Callees []SymbolWithBody `json:"callees,omitempty"`
}

// GetFlow returns a connected subgraph around a symbol with full bodies,
// ordered as: callers → symbol → callees. Use this to understand how a
// subsystem works end-to-end without calling get_body individually on each symbol.
func (e *Engine) GetFlow(symbolID string) (*FlowResponse, error) {
	resolvedID, err := e.resolveSymbolID(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get symbol %s: %w", symbolID, err)
	}
	sym, err := e.store.GetSymbol(resolvedID)
	if err != nil {
		return nil, fmt.Errorf("get symbol %s: %w", resolvedID, err)
	}

	body := sym.Body
	if isLineRef(body) {
		body, _ = readLines(sym.File, sym.LineStart, sym.LineEnd)
	}
	resp := &FlowResponse{
		Symbol: SymbolWithBody{
			SymbolSummary: toSummary(*sym, 1.0, "target"),
			Body:          body,
		},
	}

	// Callers and callees are summaries only — use get_body if you need the source.
	callers, err := e.store.GetCallers(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get callers: %w", err)
	}
	for _, c := range callers {
		resp.Callers = append(resp.Callers, SymbolWithBody{
			SymbolSummary: toSummary(c, 0, "caller"),
		})
	}

	callees, err := e.store.GetCallees(symbolID)
	if err != nil {
		return nil, fmt.Errorf("get callees: %w", err)
	}
	for _, c := range callees {
		resp.Callees = append(resp.Callees, SymbolWithBody{
			SymbolSummary: toSummary(c, 0, "callee"),
		})
	}

	return resp, nil
}

// UnimplementedResponse is returned by GetUnimplemented.
type UnimplementedResponse struct {
	Service       string             `json:"service"`
	ServiceID     string             `json:"service_id"`
	TotalRPCs     int                `json:"total_rpcs"`
	Implemented   int                `json:"implemented"`
	Unimplemented []UnimplementedRPC `json:"unimplemented"`
	Hint          string             `json:"hint,omitempty"`
}

// UnimplementedRPC describes one proto RPC missing or stubbed in the Go server.
type UnimplementedRPC struct {
	Name            string `json:"name"`
	Signature       string `json:"signature"`
	File            string `json:"file"`
	LineStart       int    `json:"line_start"`
	RequestMessage  string `json:"request_message"`
	ResponseMessage string `json:"response_message"`
	Status          string `json:"status"` // "missing" or "stubbed"
}

// GetUnimplemented finds RPCs defined in a proto service that are missing or
// stubbed in the Go server implementation.
func (e *Engine) GetUnimplemented(service string) (*UnimplementedResponse, error) {
	// Phase 1: resolve the service symbol.
	svc, hint, err := e.resolveService(service)
	if err != nil {
		return nil, err
	}

	// Phase 2: get all RPCs for this service.
	rpcs, err := e.store.GetChildrenByIDPrefix(svc.ID, "rpc")
	if err != nil {
		return nil, fmt.Errorf("get rpcs for %s: %w", svc.ID, err)
	}

	resp := &UnimplementedResponse{
		Service:   svc.Name,
		ServiceID: svc.ID,
		TotalRPCs: len(rpcs),
		Hint:      hint,
	}

	// Phase 3: check each RPC for a Go implementation.
	for _, rpc := range rpcs {
		methods, _ := e.store.GetByNameAndKind(rpc.Name, "method")
		impl := preferSvcMethod(methods)

		reqMsg, respMsg := parseRPCMessages(rpc.Signature)

		if impl == nil {
			resp.Unimplemented = append(resp.Unimplemented, UnimplementedRPC{
				Name:            rpc.Name,
				Signature:       rpc.Signature,
				File:            rpc.File,
				LineStart:       rpc.LineStart,
				RequestMessage:  reqMsg,
				ResponseMessage: respMsg,
				Status:          "missing",
			})
			continue
		}

		// Check if the implementation is just a stub.
		body := impl.Body
		if isLineRef(body) {
			body, _ = readLines(impl.File, impl.LineStart, impl.LineEnd)
		}
		if isStubbedBody(body) {
			resp.Unimplemented = append(resp.Unimplemented, UnimplementedRPC{
				Name:            rpc.Name,
				Signature:       rpc.Signature,
				File:            rpc.File,
				LineStart:       rpc.LineStart,
				RequestMessage:  reqMsg,
				ResponseMessage: respMsg,
				Status:          "stubbed",
			})
			continue
		}

		resp.Implemented++
	}

	return resp, nil
}

// resolveService finds a proto service symbol by exact name, fuzzy match, or FTS.
func (e *Engine) resolveService(service string) (*store.Symbol, string, error) {
	// Try exact name match first.
	syms, err := e.store.GetByNameAndKind(service, "service")
	if err == nil && len(syms) == 1 {
		return &syms[0], "", nil
	}
	if err == nil && len(syms) > 1 {
		hint := fmt.Sprintf("Multiple services named %q. Showing first. Others:", service)
		for _, s := range syms[1:] {
			hint += " " + s.ID
		}
		return &syms[0], hint, nil
	}

	// Try fuzzy lookup.
	sym, _, err := e.store.FuzzyGetSymbol(service)
	if err == nil && sym.Kind == "service" {
		return sym, "", nil
	}

	// Try FTS.
	ftsQuery := buildFTSQuery(service)
	hits, err := e.store.SearchFTSByKinds(ftsQuery, []string{"service"}, 5)
	if err == nil && len(hits) > 0 {
		hint := ""
		if len(hits) > 1 {
			hint = fmt.Sprintf("Multiple services matched. Showing %q. Others:", hits[0].Name)
			for _, h := range hits[1:] {
				hint += " " + h.ID
			}
		}
		return &hits[0], hint, nil
	}

	return nil, "", fmt.Errorf("no service found matching %q", service)
}

// isStubbedBody checks whether a method body is just a gRPC unimplemented stub.
func isStubbedBody(body string) bool {
	return strings.Contains(body, "codes.Unimplemented")
}

func (e *Engine) expand(seeds map[string]*SymbolSummary, depth int) (map[string]*SymbolSummary, error) {
	result := make(map[string]*SymbolSummary)
	visited := make(map[string]bool, len(seeds))

	frontier := make([]string, 0, len(seeds))
	for id := range seeds {
		frontier = append(frontier, id)
		visited[id] = true
	}

	for d := 0; d < depth; d++ {
		var next []string

		for _, id := range frontier {
			callers, err := e.store.GetCallers(id)
			if err != nil {
				return nil, fmt.Errorf("get callers for %s: %w", id, err)
			}
			for _, sym := range callers {
				if visited[sym.ID] {
					continue
				}
				visited[sym.ID] = true
				next = append(next, sym.ID)
				score := 0.4 / float64(d+1)
				s := toSummary(sym, score, fmt.Sprintf("calls %s", id))
				result[sym.ID] = &s
			}

			callees, err := e.store.GetCallees(id)
			if err != nil {
				return nil, fmt.Errorf("get callees for %s: %w", id, err)
			}
			for _, sym := range callees {
				if visited[sym.ID] {
					continue
				}
				visited[sym.ID] = true
				next = append(next, sym.ID)
				score := 0.3 / float64(d+1)
				s := toSummary(sym, score, fmt.Sprintf("called by %s", id))
				result[sym.ID] = &s
			}
		}

		frontier = next
	}

	return result, nil
}

func rankSymbols(scored map[string]*SymbolSummary) []*SymbolSummary {
	ranked := make([]*SymbolSummary, 0, len(scored))
	for _, s := range scored {
		ranked = append(ranked, s)
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})

	return ranked
}

// prioritizeSource reorders ranked results so source symbols come before
// generated ones. Within each group the existing score order is preserved.
// This prevents generated boilerplate (e.g. _*_Handler gRPC stubs that all
// match "interceptor") from consuming the token budget before source hits.
func prioritizeSource(ranked []*SymbolSummary) []*SymbolSummary {
	var source, generated []*SymbolSummary
	for _, s := range ranked {
		if isGenerated(s.File) {
			generated = append(generated, s)
		} else {
			source = append(source, s)
		}
	}
	return append(source, generated...)
}

func trimToBudget(ranked []*SymbolSummary, budgetTokens int, verbose bool) ([]SymbolSummary, int) {
	// Rough estimate: 1 token ≈ 4 chars.
	used := 0
	var kept []SymbolSummary
	for _, s := range ranked {
		var cost int
		if verbose {
			cost = (len(s.Signature) + len(s.Docstring) + len(s.ID) + len(s.Why) + 60) / 4
		} else {
			// Brief: id + kind + signature + file:line ≈ much smaller
			cost = (len(s.ID) + len(s.Signature) + len(s.File) + 30) / 4
		}
		if used+cost > budgetTokens {
			return kept, len(ranked) - len(kept)
		}
		kept = append(kept, *s)
		used += cost
	}

	return kept, 0
}

func buildPackageSummary(symbols []SymbolSummary) []PackageHit {
	if len(symbols) <= 1 {
		return nil
	}

	type pkgInfo struct {
		count int
		kinds map[string]int
	}
	pkgs := map[string]*pkgInfo{}
	var order []string

	for _, s := range symbols {
		dir := filepath.Dir(s.File)
		info := pkgs[dir]
		if info == nil {
			info = &pkgInfo{kinds: map[string]int{}}
			pkgs[dir] = info
			order = append(order, dir)
		}
		info.count++
		info.kinds[s.Kind]++
	}

	if len(pkgs) <= 1 {
		return nil
	}

	prefix := commonPrefix(order)

	sort.Slice(order, func(i, j int) bool {
		return pkgs[order[i]].count > pkgs[order[j]].count
	})

	hits := make([]PackageHit, len(order))
	for i, dir := range order {
		info := pkgs[dir]
		short := strings.TrimPrefix(dir, prefix)
		if short == "" {
			short = "."
		}
		var kindParts []string
		for k, n := range info.kinds {
			if n > 1 {
				kindParts = append(kindParts, fmt.Sprintf("%d %ss", n, k))
			} else {
				kindParts = append(kindParts, k)
			}
		}
		sort.Strings(kindParts)
		hits[i] = PackageHit{
			Package: short,
			Count:   info.count,
			Kinds:   strings.Join(kindParts, ", "),
		}
	}

	return hits
}

func commonPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	prefix := paths[0]
	for _, p := range paths[1:] {
		for !strings.HasPrefix(p, prefix) {
			prefix = filepath.Dir(prefix)
			if prefix == "." || prefix == "/" {
				return ""
			}
		}
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

// dedup removes redundant symbols in two passes:
//  1. Gen-path dedup: when the same name+kind appears in multiple /gen/
//     directories, keep only the one in the preferred gen path (backend).
//  2. Name dedup: when 3+ symbols share a name, keep the highest-scored one.
func dedup(scored map[string]*SymbolSummary) map[string]*SymbolSummary {
	// Pass 1: collapse gen-path duplicates.
	type genKey struct{ name, kind string }
	genGroups := make(map[genKey][]*SymbolSummary)
	for _, s := range scored {
		if !strings.Contains(s.File, "/gen/") {
			continue
		}
		name := s.ID
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		k := genKey{name, s.Kind}
		genGroups[k] = append(genGroups[k], s)
	}
	for _, group := range genGroups {
		if len(group) < 2 {
			continue
		}
		// Prefer backend/internal/gen (the importable package).
		sort.Slice(group, func(i, j int) bool {
			iBackend := strings.Contains(group[i].File, "backend")
			jBackend := strings.Contains(group[j].File, "backend")
			if iBackend != jBackend {
				return iBackend
			}
			return group[i].Score > group[j].Score
		})
		for _, s := range group[1:] {
			delete(scored, s.ID)
		}
	}

	// Pass 2: collapse same-name boilerplate (e.g. Validate on every resource type).
	byName := make(map[string][]*SymbolSummary)
	for _, s := range scored {
		name := s.ID
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		byName[name] = append(byName[name], s)
	}
	for name, group := range byName {
		if len(group) < 3 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Score > group[j].Score })
		for _, s := range group[1:] {
			delete(scored, s.ID)
		}
		penalty := float64(len(group)-2) * 0.15
		group[0].Score -= penalty
		group[0].Why += fmt.Sprintf(" (+%d similar %s omitted)", len(group)-1, name)
	}
	return scored
}

// termCoverage rewards symbols whose name or signature matches multiple query
// terms. A symbol matching 3 of 4 terms ranks above one matching 1 of 4.
func termCoverage(sym store.Symbol, terms []string, decomposedName string) float64 {
	if len(terms) <= 1 {
		return 0
	}
	searchable := strings.ToLower(sym.Name + " " + decomposedName + " " + sym.Package + " " + sym.Signature + " " + sym.Docstring)
	matches := 0
	for _, term := range terms {
		if strings.Contains(searchable, term) {
			matches++
		}
	}
	if matches <= 1 {
		return 0
	}
	coverage := float64(matches) / float64(len(terms))
	return coverage * coverage * 1.5
}

// decomposeIdentifier splits a camelCase or PascalCase name into lowercase words.
func decomposeIdentifier(s string) string {
	var words []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			words = append(words, strings.ToLower(s[start:i]))
			start = i
		}
	}
	words = append(words, strings.ToLower(s[start:]))
	return strings.Join(words, " ")
}

// nameMatchBonus returns the score bonus when a symbol's name exactly matches
// a query term. Funcs/methods get a large bonus; structs/types get a small one
// since they're usually not what the caller wants to read.
func nameMatchBonus(sym store.Symbol) float64 {
	switch sym.Kind {
	case "func", "method":
		return 1.0
	case "interface":
		return 0.7
	default:
		return 0.3
	}
}

// kindWeight biases results toward behavior (funcs/methods) over structure
// (structs/types). Queries almost always ask "how does X work", not "what
// fields does X have".
func kindWeight(sym store.Symbol) float64 {
	switch sym.Kind {
	case "method":
		return 0.3
	case "func":
		return 0.2
	case "interface":
		return 0.1
	case "struct":
		return -0.3
	case "type", "const", "var":
		return -0.2
	default:
		return 0
	}
}

// implBoost returns a score bonus for symbols in server/svc packages.
func implBoost(sym store.Symbol) float64 {
	if sym.Kind != "method" && sym.Kind != "func" {
		return 0
	}
	pkg := strings.ToLower(sym.Package)
	if strings.Contains(pkg, "svc") ||
		strings.Contains(pkg, "server") ||
		strings.Contains(pkg, "service") {
		return 0.5
	}
	return 0
}

// generatedPenalty returns a score penalty for proto-generated files (.pb.go,
// .pb.gw.go). These are indexed for proto-go linking but should rank below
// hand-written code in get_relevant_context results — they are boilerplate
// the model rarely needs to read directly.
func generatedPenalty(sym store.Symbol) float64 {
	if strings.HasSuffix(sym.File, ".pb.go") || strings.HasSuffix(sym.File, ".pb.gw.go") {
		return -0.6
	}
	return 0
}

func toSummary(sym store.Symbol, score float64, why string) SymbolSummary {
	return SymbolSummary{
		ID:        sym.ID,
		Kind:      sym.Kind,
		Signature: sym.Signature,
		Docstring: sym.Docstring,
		File:      sym.File,
		LineStart: sym.LineStart,
		LineEnd:   sym.LineEnd,
		Why:       why,
		Score:     score,
	}
}

// readLines reads lines [start, end] (1-indexed, inclusive) from a file.
func readLines(path string, start, end int) (string, error) {
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

// readDecl reads a complete declaration (function, type, etc.) from a file
// starting at the given line. Uses brace counting to find the end. Returns
// the body text and the ending line number.
func readDecl(path string, startLine int) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	var lines []string
	depth := 0
	hasOpenBrace := false
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		if n < startLine {
			continue
		}
		line := scanner.Text()
		lines = append(lines, line)

		for _, ch := range line {
			if ch == '{' {
				depth++
				hasOpenBrace = true
			} else if ch == '}' {
				depth--
			}
		}

		if hasOpenBrace && depth <= 0 {
			return strings.Join(lines, "\n"), startLine + len(lines) - 1, scanner.Err()
		}

		// No braces on first line means it's a single-line declaration
		// (const, var, type alias). Return as-is.
		if len(lines) == 1 && !hasOpenBrace {
			return lines[0], startLine, nil
		}

		if len(lines) > 200 {
			break
		}
	}

	if len(lines) == 0 {
		return "", 0, fmt.Errorf("no lines read")
	}
	return strings.Join(lines, "\n"), startLine + len(lines) - 1, scanner.Err()
}

// isLineRef reports whether a stored body is a stale line reference from an old index.
func isLineRef(body string) bool {
	return body == "" || strings.HasPrefix(body, "/*")
}

// buildFTSQuery converts a natural language task into an FTS5 query.
// It strips stop words and ORs remaining terms.
func buildFTSQuery(task string) string {
	stop := map[string]bool{
		"a": true, "an": true, "the": true, "to": true, "for": true,
		"in": true, "on": true, "of": true, "and": true, "or": true,
		"is": true, "it": true, "add": true, "make": true, "how": true,
		"do": true, "i": true, "we": true, "my": true,
	}

	// Replace any non-alphanumeric character with a space so "field/prop" → "field prop".
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, task)

	words := strings.Fields(strings.ToLower(sanitized))
	var terms []string
	seen := make(map[string]bool)
	for _, w := range words {
		if w == "" || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, w)
	}

	if len(terms) == 0 {
		return task
	}

	return strings.Join(terms, " OR ")
}

// extractCompoundIdents returns raw compound identifiers from the task string
// (e.g. "CreateShipmentLeg" from "CreateShipmentLeg ShipmentLeg service").
// These are used for exact name lookups in the symbol table.
func extractCompoundIdents(task string) []string {
	var idents []string
	seen := make(map[string]bool)
	for _, w := range strings.Fields(task) {
		// Strip punctuation from edges.
		w = strings.TrimFunc(w, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.')
		})
		if isCompoundIdent(w) && !seen[w] {
			seen[w] = true
			idents = append(idents, w)
		}
		// Dotted identifiers like "grpc.Dial" or "status.Error": extract
		// the name part after the last dot for exact name lookup.
		if dot := strings.LastIndex(w, "."); dot >= 0 && dot < len(w)-1 {
			name := w[dot+1:]
			if len(name) >= 2 && name[0] >= 'A' && name[0] <= 'Z' && !seen[name] {
				seen[name] = true
				idents = append(idents, name)
			}
		}
	}
	return idents
}

// extractCompoundParts returns the decomposed words from any compound
// identifiers (PascalCase/camelCase) in the task string. These are used
// for scoring (termCoverage) but not added to the FTS query to avoid
// broadening results.
func extractCompoundParts(task string) []string {
	var parts []string
	seen := make(map[string]bool)
	for _, w := range strings.Fields(task) {
		if isCompoundIdent(w) {
			for _, part := range strings.Fields(decomposeIdentifier(w)) {
				if !seen[part] {
					seen[part] = true
					parts = append(parts, part)
				}
			}
		}
	}
	return parts
}

// isCompoundIdent reports whether s looks like a PascalCase or camelCase
// identifier with at least two words (e.g. "CreateShipmentLeg", "buildCallGraph").
func isCompoundIdent(s string) bool {
	if len(s) < 3 {
		return false
	}
	transitions := 0
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			transitions++
		}
	}
	return transitions > 0
}
