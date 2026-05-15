// Package query implements context retrieval: FTS search, graph expansion, and ranking.
package query

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/urechandro/scout/store"
)

// ContextRequest is the input to GetRelevantContext.
type ContextRequest struct {
	// Task is a natural language description of what the model wants to do.
	Task string
	// BudgetTokens is the approximate token budget for the response.
	// Defaults to 6000 if zero.
	BudgetTokens int
	// MaxExpansionDepth controls graph hop count. 1 is usually right; 0 disables.
	MaxExpansionDepth int
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
	LineEnd   int     `json:"line_end"`
	Why       string  `json:"why"`
	Score     float64 `json:"score"`
}

// ContextResponse is returned by GetRelevantContext.
type ContextResponse struct {
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

// GetRelevantContext is the primary MCP tool entry point.
func (e *Engine) GetRelevantContext(req ContextRequest) (*ContextResponse, error) {
	if req.BudgetTokens == 0 {
		req.BudgetTokens = 6000
	}
	if req.MaxExpansionDepth == 0 {
		req.MaxExpansionDepth = 1
	}

	ftsQuery := buildFTSQuery(req.Task)
	queryTerms := strings.Split(ftsQuery, " OR ")
	ftsLimit := 30 + len(queryTerms)*10
	hits, err := e.store.SearchFTS(ftsQuery, ftsLimit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}

	scored := make(map[string]*SymbolSummary, len(hits))
	for i, sym := range hits {
		score := float64(len(hits)-i) / float64(len(hits))
		nameLower := strings.ToLower(sym.Name)
		decomposed := strings.ToLower(decomposeIdentifier(sym.Name))
		for _, term := range queryTerms {
			if nameLower == term {
				score += nameMatchBonus(sym)
				break
			}
		}
		score += termCoverage(sym, queryTerms, decomposed)
		score += kindWeight(sym)
		score += implBoost(sym)
		score += generatedPenalty(sym)
		s := toSummary(sym, score, "semantic match")
		scored[sym.ID] = &s
	}

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
	kept, truncated := trimToBudget(ranked, req.BudgetTokens)

	return &ContextResponse{
		Symbols:   kept,
		Truncated: truncated,
	}, nil
}

// BodyResponse is returned by GetBody. It includes the symbol and optional
// disambiguation hints when the lookup was fuzzy.
type BodyResponse struct {
	*store.Symbol
	Hint       string   `json:"hint,omitempty"`
	OtherIDs   []string `json:"other_ids,omitempty"`
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

	return resp, nil
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
	// Search for RPC symbols matching the task.
	ftsQuery := buildFTSQuery(task)
	hits, err := e.store.SearchFTS(ftsQuery, 20)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}

	// Prefer RPC hits; fall back to method hits in svc packages.
	var rpcs, svcMethods, otherMethods []store.Symbol
	for _, h := range hits {
		switch h.Kind {
		case "rpc":
			rpcs = append(rpcs, h)
		case "method":
			if strings.Contains(h.Package, "svc") || strings.Contains(h.Package, "server") || strings.Contains(h.Package, "service") {
				svcMethods = append(svcMethods, h)
			} else {
				otherMethods = append(otherMethods, h)
			}
		}
	}

	// Cascade: RPCs → svc methods → other methods.
	candidates := rpcs
	if len(candidates) == 0 {
		candidates = svcMethods
	}
	if len(candidates) == 0 {
		candidates = otherMethods
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	// get_pattern (limit=1) includes bodies; get_conventions (limit>1) returns summaries only.
	withBodies := limit == 1

	var slices []*PatternSlice
	for _, rpc := range candidates {
		slice, err := e.buildSlice(rpc, withBodies)
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
			slice.RequestMessage = &SymbolWithBody{
				SymbolSummary: toSummary(sym[0], 0, "request message"),
			}
		}
	}

	if respName != "" && respName != reqName {
		if sym, err := e.store.GetByNameAndKind(respName, "message"); err == nil && len(sym) > 0 {
			slice.ResponseMessage = &SymbolWithBody{
				SymbolSummary: toSummary(sym[0], 0, "response message"),
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

func trimToBudget(ranked []*SymbolSummary, budgetTokens int) ([]SymbolSummary, int) {
	// Rough estimate: 1 token ≈ 4 chars. Summaries are ~150-300 chars each.
	used := 0
	var kept []SymbolSummary
	for _, s := range ranked {
		cost := (len(s.Signature) + len(s.Docstring) + len(s.ID) + len(s.Why) + 60) / 4
		if used+cost > budgetTokens {
			return kept, len(ranked) - len(kept)
		}
		kept = append(kept, *s)
		used += cost
	}

	return kept, 0
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

// isLineRef reports whether a stored body is a stale line reference from an old index.
func isLineRef(body string) bool {
	return body == "" || strings.HasPrefix(body, "/*")
}

// buildFTSQuery converts a natural language task into an FTS5 query.
// It strips common stop words and ORs the remaining terms.
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
