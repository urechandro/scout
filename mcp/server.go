// Package mcp implements a Model Context Protocol server exposing codebase navigation tools.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

// jsonrpc message types.
const (
	methodInitialize       = "initialize"
	methodToolsList        = "tools/list"
	methodToolsCall        = "tools/call"
	methodPromptsList      = "prompts/list"
	methodPromptsGet       = "prompts/get"
	methodNotifInitialized = "notifications/initialized"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server is an MCP server that exposes codebase navigation tools over stdio.
type Server struct {
	engine *query.Engine
	store  *store.Store
	logger *slog.Logger
	in     *bufio.Scanner
	out    *json.Encoder
}

// New creates a Server backed by the given query engine and store.
func New(logger *slog.Logger, e *query.Engine, s *store.Store) *Server {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	return &Server{
		engine: e,
		store:  s,
		logger: logger,
		in:     scanner,
		out:    json.NewEncoder(os.Stdout),
	}
}

// Run reads JSON-RPC messages from stdin and writes responses to stdout.
func (s *Server) Run() error {
	s.logger.Info("mcp server started")

	for s.in.Scan() {
		line := s.in.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.logger.Warn("malformed request", "err", err)
			continue
		}

		s.logger.Debug("received request", "method", req.Method, "id", req.ID)

		resp := s.handle(req)
		if resp == nil {
			continue // Notification — no response needed.
		}

		if err := s.out.Encode(resp); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
	}

	if err := s.in.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("stdin read: %w", err)
	}

	return nil
}

func (s *Server) handle(req request) *response {
	switch req.Method {
	case methodInitialize:
		return s.handleInitialize(req)
	case methodNotifInitialized:
		return nil // Notification, no response.
	case methodToolsList:
		return s.handleToolsList(req)
	case methodToolsCall:
		return s.handleToolsCall(req)
	case methodPromptsList:
		return s.handlePromptsList(req)
	case methodPromptsGet:
		return s.handlePromptsGet(req)
	default:
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found"},
		}
	}
}

func (s *Server) handleInitialize(req request) *response {
	return &response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools":   map[string]any{},
				"prompts": map[string]any{},
			},
			"serverInfo": map[string]any{"name": "scout", "version": "0.1.0"},
		},
	}
}

func (s *Server) handleToolsList(req request) *response {
	tools := []map[string]any{
		{
			"name": "get_relevant_context",
			"description": `Orient yourself before acting. Returns compact symbol pointers (id, kind, signature, file:line) — NOT full source, NOT docstrings by default.

Workflow: call this ONCE at the start of any task. Scan the returned IDs and signatures to identify which symbols matter, then call get_body only on the 1-2 symbols you will actually edit. Do NOT call it multiple times with overlapping queries.

If the result is empty or does not contain what you expected: accept that the symbol does not exist yet. Do NOT fall back to Glob, Read, or file search to verify — an empty result is the answer.

Use specific Go names when you know them ("ValidateToken", "ShipmentService"). Use domain terms when exploring ("rate limiting", "auth middleware"). The search matches against symbol names, signatures, and docstrings.

Returns compact pointers by default. Pass verbose=true only if you need docstrings to decide which symbol to expand. Each result includes an id you can pass to get_body, get_callers, or get_flow.

When results span multiple packages, the response includes a "packages" field summarizing hit counts per package — read this first to orient before drilling into individual symbols.`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "What you are trying to do. Use Go symbol names when known, domain terms when exploring. Examples: 'ValidateToken auth middleware', 'how are shipments created', 'rate limiting'.",
					},
					"budget_tokens": map[string]any{
						"type":        "integer",
						"description": "Max tokens for the response. Default: 600 (precise) or 2000 (discovery). Brief format is compact so this covers many symbols.",
					},
					"max_depth": map[string]any{
						"type":        "integer",
						"description": "How many hops to walk the call graph from FTS hits. Default 1. Use 2 if the first call missed implementations (e.g. found the interface but not the server method). Max 3.",
					},
					"verbose": map[string]any{
						"type":        "boolean",
						"description": "Include docstrings, match reason, and scores in results. Default false (compact format: id, kind, signature, file:line). Use verbose only when you need docstrings to decide which symbol to expand.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "get_body",
			"description": `Fetch the full source code of one symbol. Call this when you are about to read or edit a specific function/type.

Typical flow: get_relevant_context → pick 1-2 symbol IDs → get_body on each.

The response includes a "references" field with signatures of types and functions referenced in the body. This lets you understand dependencies without extra calls — only call get_body on a reference if you need its full source.

Accepts exact IDs (from previous tool results) or partial/guessed IDs — it will fuzzy-match by suffix then by name. For example, "Server.CreateShipment" resolves to the full ID. If multiple symbols match, the response includes other_ids for disambiguation.

Prefer IDs from get_relevant_context results over guessing. But if you must guess, use the shortest unambiguous suffix: "TypeName.MethodName" or just "FuncName".`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol_id": map[string]any{
						"type":        "string",
						"description": "Fully-qualified symbol ID from a previous tool result, e.g. github.com/myapp/auth.ValidateToken or github.com/myapp/svc.Server.CreateShipment.",
					},
				},
				"required": []string{"symbol_id"},
			},
		},
		{
			"name": "get_callers",
			"description": `List every function that calls this symbol. Built from whole-program analysis (RTA) so interface dispatch is resolved precisely — if Handler.ServeHTTP is called through the http.Handler interface, the actual callers appear here, not just the interface method.

Call this BEFORE changing a function's signature, return type, or behavior to understand the blast radius. Returns summaries (no bodies).`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol_id": map[string]any{
						"type":        "string",
						"description": "Fully-qualified symbol ID of the function/method whose callers you want.",
					},
				},
				"required": []string{"symbol_id"},
			},
		},
		{
			"name": "get_callees",
			"description": `List every function this symbol calls. Built from whole-program analysis (RTA) so interface calls resolve to concrete implementations, not just the interface method.

Call this to understand what a function depends on — useful before refactoring or when tracing data flow downstream.`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol_id": map[string]any{
						"type":        "string",
						"description": "Fully-qualified symbol ID of the function/method whose callees you want.",
					},
				},
				"required": []string{"symbol_id"},
			},
		},
		{
			"name": "get_flow",
			"description": `Get the full source of a symbol PLUS summaries of its callers and callees in one call. Use this instead of separate get_body + get_callers + get_callees when you want to understand a symbol in context.

Returns: callers (summaries) → symbol (full body) → callees (summaries). Call get_body on individual callers/callees only if you need their source too.

Best for: understanding how a function fits into the system before editing it.`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol_id": map[string]any{
						"type":        "string",
						"description": "Fully-qualified symbol ID of the central symbol to explore.",
					},
				},
				"required": []string{"symbol_id"},
			},
		},
		{
			"name": "get_pattern",
			"description": `Get one complete vertical slice showing how an operation is implemented end-to-end: proto RPC → request/response messages → Go method implementation, with full source bodies.

Use this BEFORE implementing a new RPC or feature. It gives you a concrete example to follow — copy the structure, rename, and adapt.

The task should describe the kind of operation: "create shipment", "update transport order", "list locations".`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "The kind of operation to find an example of, e.g. 'create shipment', 'batch update', 'get location by ID'.",
					},
				},
				"required": []string{"task"},
			},
		},
		{
			"name": "get_unimplemented",
			"description": `Find which RPCs in a proto service are missing or stubbed in the Go server implementation.

Call this BEFORE adding a new RPC to a service — it tells you exactly which RPCs still need implementing, with their proto signatures and request/response message names.

Accepts full or partial service names: "ShipmentService", "shipment.v1.ShipmentService", etc.

Returns: service name, total RPC count, and for each unimplemented RPC: the proto signature, file location, request/response message names, and whether it's completely missing or just stubbed (returns codes.Unimplemented).`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service": map[string]any{
						"type":        "string",
						"description": "Proto service name (full or partial). Examples: 'ShipmentService', 'shipment.v1.ShipmentService'.",
					},
				},
				"required": []string{"service"},
			},
		},
		{
			"name": "get_impact",
			"description": `Trace what breaks if you change this symbol. Unlike get_callers (one hop, Go-only), this crosses proto↔Go boundaries, follows name-based linkage through generated code, and finds affected tests.

Call this BEFORE renaming a symbol, changing a proto field, or modifying a type — it shows the full blast radius across layers: proto definitions, generated code, hand-written implementations, and tests.

Returns results grouped by layer: proto, generated, implementation, tests. Each entry includes a "why" explaining the connection.`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol_id": map[string]any{
						"type":        "string",
						"description": "Fully-qualified symbol ID of the symbol you plan to change.",
					},
				},
				"required": []string{"symbol_id"},
			},
		},
		{
			"name": "get_conventions",
			"description": `Find how a cross-cutting pattern is used across the codebase. Searches broadly across ALL symbol kinds (functions, methods, types, interfaces) — not just RPCs.

Returns up to 10 matching symbols ranked by relevance, with service-layer implementations boosted. Use get_body on specific results to see full source.

Good for: "outbox", "pagination", "validation", "error handling", "saga", "middleware", "retry". Any recurring pattern that spans multiple packages.`,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topic": map[string]any{
						"type":        "string",
						"description": "The convention or pattern to look up, e.g. 'mutation handlers', 'input validation', 'pagination', 'saga orchestration'.",
					},
				},
				"required": []string{"topic"},
			},
		},
	}

	return &response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": tools},
	}
}

func (s *Server) handlePromptsList(req request) *response {
	return &response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"prompts": []map[string]any{
				{
					"name":        "usage",
					"description": "How to use scout tools effectively — the right call sequence, what to avoid, and when to use each tool.",
				},
			},
		},
	}
}

func (s *Server) handlePromptsGet(req request) *response {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name != "usage" {
		return s.errResponse(req.ID, -32602, "unknown prompt")
	}

	return &response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"description": "How to use scout tools effectively.",
			"messages": []map[string]any{
				{
					"role": "user",
					"content": map[string]any{
						"type": "text",
						"text": usagePrompt,
					},
				},
			},
		},
	}
}

// usagePrompt is the system-level guidance injected into Claude Code via the
// MCP prompts capability. It replaces the need for a CLAUDE.md section.
const usagePrompt = `# Scout — codebase navigation

Scout is connected. Use its tools to orient before acting. This is faster and
more token-efficient than reading files directly.

## The right flow for any task

1. **get_relevant_context once** — orient, confirm what exists, find the closest
   reference. Do not call it again with a rephrased query hoping for different results.
   **Empty result = the symbol does not exist. Stop. Do not verify with Glob, Read,
   or any file search. An empty result IS the answer.**
2. **get_conventions** — before implementing any pattern (RPC handler, event handler,
   pagination, auth, outbox, transactions). Do not guess at patterns.
3. **get_body on the specific symbols you are about to use as a template or edit**
   — and only those. Not for browsing.
4. **Then act.**

## Rules

- Do not call get_relevant_context more than once per task unless the scope changes.
- Do not use Read to open Go files. If you know a file path, derive the symbol ID
  from context results and call get_body instead. Reading whole files defeats the index.
- Proto files may be read with Read if they are not in the scout index.
- Before changing a function signature, call get_impact to understand the full blast
  radius across proto, generated code, implementations, and tests.

## Tools

| Tool | When |
|---|---|
| get_relevant_context(task) | Start of every task |
| get_conventions(topic) | Before implementing any pattern |
| get_body(symbol_id) | When about to read or edit a specific symbol |
| get_impact(symbol_id) | Before renaming, changing a type, or modifying a proto field |
| get_callers(symbol_id) | Before changing a signature (Go-only, single hop) |
| get_callees(symbol_id) | To understand what a symbol depends on |
| get_flow(symbol_id) | To understand a symbol in context (body + callers + callees) |
| get_pattern(task) | To get a full example slice (proto → messages → Go impl) |
| get_unimplemented(service) | Before adding a new RPC — shows what's missing or stubbed |
`

func (s *Server) handleToolsCall(req request) *response {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errResponse(req.ID, -32602, "invalid params")
	}

	s.logger.Info("tool call", "tool", params.Name)

	var (
		result any
		err    error
	)

	switch params.Name {
	case "get_relevant_context":
		result, err = s.callGetRelevantContext(params.Arguments)
	case "get_body":
		result, err = s.callGetBody(params.Arguments)
	case "get_callers":
		result, err = s.callGetCallers(params.Arguments)
	case "get_callees":
		result, err = s.callGetCallees(params.Arguments)
	case "get_flow":
		result, err = s.callGetFlow(params.Arguments)
	case "get_pattern":
		result, err = s.callGetPattern(params.Arguments)
	case "get_unimplemented":
		result, err = s.callGetUnimplemented(params.Arguments)
	case "get_conventions":
		result, err = s.callGetConventions(params.Arguments)
	case "get_impact":
		result, err = s.callGetImpact(params.Arguments)
	default:
		return s.errResponse(req.ID, -32601, fmt.Sprintf("unknown tool: %s", params.Name))
	}

	if err != nil {
		s.logger.Warn("tool error", "tool", params.Name, "err", err)
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: toolResult{
				IsError: true,
				Content: []toolContent{{Type: "text", Text: err.Error()}},
			},
		}
	}

	text, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return s.errResponse(req.ID, -32603, "marshal result")
	}

	estimatedTokens := len(text) / 4
	s.logger.Info("tool response", "tool", params.Name, "bytes", len(text), "estimated_tokens", estimatedTokens)

	return &response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: toolResult{
			Content: []toolContent{{Type: "text", Text: string(text)}},
		},
	}
}

func (s *Server) callGetRelevantContext(args json.RawMessage) (any, error) {
	var params struct {
		Query        string `json:"query"`
		BudgetTokens int    `json:"budget_tokens"`
		MaxDepth     int    `json:"max_depth"`
		Verbose      bool   `json:"verbose"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Query == "" {
		return nil, fmt.Errorf("query is required")
	}

	depth := params.MaxDepth
	if depth == 0 {
		depth = 1
	}
	if depth > 3 {
		depth = 3
	}

	return s.engine.GetRelevantContext(query.ContextRequest{
		Task:              params.Query,
		BudgetTokens:      params.BudgetTokens,
		MaxExpansionDepth: depth,
		Verbose:           params.Verbose,
	})
}

func (s *Server) callGetBody(args json.RawMessage) (any, error) {
	var params struct {
		SymbolID string `json:"symbol_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	return s.engine.GetBody(params.SymbolID)
}

func (s *Server) callGetCallers(args json.RawMessage) (any, error) {
	var params struct {
		SymbolID string `json:"symbol_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	return s.engine.GetCallers(params.SymbolID)
}

func (s *Server) callGetCallees(args json.RawMessage) (any, error) {
	var params struct {
		SymbolID string `json:"symbol_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	return s.engine.GetCallees(params.SymbolID)
}

func (s *Server) callGetFlow(args json.RawMessage) (any, error) {
	var params struct {
		SymbolID string `json:"symbol_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return s.engine.GetFlow(params.SymbolID)
}

func (s *Server) callGetPattern(args json.RawMessage) (any, error) {
	var params struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return s.engine.GetPattern(params.Task)
}

func (s *Server) callGetUnimplemented(args json.RawMessage) (any, error) {
	var params struct {
		Service string `json:"service"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Service == "" {
		return nil, fmt.Errorf("service is required")
	}
	return s.engine.GetUnimplemented(params.Service)
}

func (s *Server) callGetConventions(args json.RawMessage) (any, error) {
	var params struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return s.engine.GetConventions(params.Topic)
}

func (s *Server) callGetImpact(args json.RawMessage) (any, error) {
	var params struct {
		SymbolID string `json:"symbol_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.SymbolID == "" {
		return nil, fmt.Errorf("symbol_id is required")
	}
	return s.engine.GetImpact(params.SymbolID)
}

func (s *Server) errResponse(id any, code int, msg string) *response {
	return &response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	}
}
