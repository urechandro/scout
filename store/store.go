// Package store manages the SQLite database for indexed symbols and graph edges.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Symbol represents a parsed Go symbol (function, type, method, etc).
type Symbol struct {
	ID        string // e.g. "github.com/myapp/auth.ValidateToken"
	Package   string // e.g. "github.com/myapp/auth"
	Name      string // e.g. "ValidateToken"
	Kind      string // "func", "type", "method", "var", "const", "interface"
	Signature string // e.g. "func ValidateToken(token string) (*Claims, error)"
	Docstring string
	File      string
	LineStart int
	LineEnd   int
	Body      string
}

// Edge represents a directed relationship between two symbols.
type Edge struct {
	FromID string
	ToID   string
	Kind   string // "calls", "implements", "uses_type", "imports"
}

// Convention is a documented architectural pattern loaded from conventions.yaml.
type Convention struct {
	Name        string   // unique slug, e.g. "transactional-outbox"
	Terms       []string // search terms that trigger this convention
	Description string   // what the pattern is and why it exists
	Structure   string   // pseudocode showing the repeating shape
	Examples    []string // symbol ID suffixes (fuzzy-matched at query time)
}

// Store manages symbol and edge persistence.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at the given path.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS symbols (
			id         TEXT PRIMARY KEY,
			package    TEXT NOT NULL,
			name       TEXT NOT NULL,
			kind       TEXT NOT NULL,
			signature  TEXT NOT NULL,
			docstring  TEXT NOT NULL DEFAULT '',
			file       TEXT NOT NULL,
			line_start INTEGER NOT NULL,
			line_end   INTEGER NOT NULL,
			body       TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_symbols_package ON symbols(package);
		CREATE INDEX IF NOT EXISTS idx_symbols_name    ON symbols(name);
		CREATE INDEX IF NOT EXISTS idx_symbols_file    ON symbols(file);

		CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
			id UNINDEXED,
			name,
			signature,
			docstring
		);

		CREATE TABLE IF NOT EXISTS edges (
			from_id TEXT NOT NULL,
			to_id   TEXT NOT NULL,
			kind    TEXT NOT NULL,
			PRIMARY KEY (from_id, to_id, kind)
		);

		CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id);
		CREATE INDEX IF NOT EXISTS idx_edges_to   ON edges(to_id);

		CREATE TABLE IF NOT EXISTS index_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS conventions (
			name        TEXT PRIMARY KEY,
			terms       TEXT NOT NULL,
			description TEXT NOT NULL,
			structure   TEXT NOT NULL DEFAULT '',
			examples    TEXT NOT NULL DEFAULT ''
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS conventions_fts USING fts5(
			name UNINDEXED,
			terms,
			description
		);
	`)
	if err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	return nil
}

// UpsertSymbol inserts or replaces a symbol.
func (s *Store) UpsertSymbol(sym Symbol) error {
	_, err := s.db.Exec(`
		INSERT INTO symbols (id, package, name, kind, signature, docstring, file, line_start, line_end, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			signature  = excluded.signature,
			docstring  = excluded.docstring,
			file       = excluded.file,
			line_start = excluded.line_start,
			line_end   = excluded.line_end,
			body       = excluded.body
	`,
		sym.ID, sym.Package, sym.Name, sym.Kind,
		sym.Signature, sym.Docstring, sym.File,
		sym.LineStart, sym.LineEnd, sym.Body,
	)
	if err != nil {
		return fmt.Errorf("upsert symbol %s: %w", sym.ID, err)
	}

	// Keep FTS index in sync. FTS5 virtual tables don't support ON CONFLICT,
	// so delete the old entry first then reinsert.
	_, err = s.db.Exec(`DELETE FROM symbols_fts WHERE id = ?`, sym.ID)
	if err != nil {
		return fmt.Errorf("delete fts for %s: %w", sym.ID, err)
	}
	// Store both the original name and its camelCase decomposition so that
	// searching "outbox" matches "addDispatchEventsToOutbox".
	ftsName := sym.Name + " " + decomposeIdentifier(sym.Name)
	_, err = s.db.Exec(`
    INSERT INTO symbols_fts (id, name, signature, docstring)
    VALUES (?, ?, ?, ?)
    `, sym.ID, ftsName, sym.Signature, sym.Docstring)
	if err != nil {
		return fmt.Errorf("insert fts for %s: %w", sym.ID, err)
	}

	return nil
}

// UpsertEdge inserts or ignores a graph edge.
func (s *Store) UpsertEdge(edge Edge) error {
	_, err := s.db.Exec(`
		INSERT INTO edges (from_id, to_id, kind)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, edge.FromID, edge.ToID, edge.Kind)
	if err != nil {
		return fmt.Errorf("upsert edge %s->%s: %w", edge.FromID, edge.ToID, err)
	}

	return nil
}

// DeleteEdgesByKind removes all edges of a given kind (e.g. "calls").
// Used to clear stale call edges before writing SSA-based ones.
func (s *Store) DeleteEdgesByKind(kind string) error {
	_, err := s.db.Exec(`DELETE FROM edges WHERE kind = ?`, kind)
	if err != nil {
		return fmt.Errorf("delete edges by kind %s: %w", kind, err)
	}
	return nil
}

// DeleteByFile removes all symbols (and their edges) from a given file.
// Used during incremental reindexing when a file changes.
func (s *Store) DeleteByFile(file string) error {
	rows, err := s.db.Query(`SELECT id FROM symbols WHERE file = ?`, file)
	if err != nil {
		return fmt.Errorf("query symbols for file %s: %w", file, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan symbol id: %w", err)
		}
		ids = append(ids, id)
	}

	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, id, id); err != nil {
			return fmt.Errorf("delete edges for %s: %w", id, err)
		}
		if _, err := s.db.Exec(`DELETE FROM symbols WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete symbol %s: %w", id, err)
		}
	}

	return nil
}

// SearchFTS performs full-text search across symbol names, signatures, and docstrings.
func (s *Store) SearchFTS(query string, limit int) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.package, s.name, s.kind, s.signature, s.docstring, s.file, s.line_start, s.line_end, s.body
		FROM symbols_fts f
		JOIN symbols s ON s.id = f.id
		WHERE symbols_fts MATCH ?
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search %q: %w", query, err)
	}
	defer rows.Close()

	return scanSymbols(rows)
}

// GetSymbol retrieves a single symbol by its fully-qualified ID.
func (s *Store) GetSymbol(id string) (*Symbol, error) {
	row := s.db.QueryRow(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE id = ?
	`, id)

	var sym Symbol
	err := row.Scan(
		&sym.ID, &sym.Package, &sym.Name, &sym.Kind,
		&sym.Signature, &sym.Docstring, &sym.File,
		&sym.LineStart, &sym.LineEnd, &sym.Body,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get symbol %s: %w", id, err)
	}

	return &sym, nil
}

// GetCallers returns all symbols that call the given symbol ID.
func (s *Store) GetCallers(symbolID string) ([]Symbol, error) {
	return s.getRelated(symbolID, "to_id", "from_id", "calls")
}

// GetCallees returns all symbols called by the given symbol ID.
func (s *Store) GetCallees(symbolID string) ([]Symbol, error) {
	return s.getRelated(symbolID, "from_id", "to_id", "calls")
}

// GetImplementors returns all types that implement the given interface ID.
func (s *Store) GetImplementors(symbolID string) ([]Symbol, error) {
	return s.getRelated(symbolID, "to_id", "from_id", "implements")
}

// GetImplements returns what a symbol implements (interface or proto RPC).
// Edge direction: GoMethod → Interface/RPC, so we match on from_id and return to_id.
func (s *Store) GetImplements(symbolID string) ([]Symbol, error) {
	return s.getRelated(symbolID, "from_id", "to_id", "implements")
}

func (s *Store) getRelated(symbolID, matchCol, returnCol, kind string) ([]Symbol, error) {
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT s.id, s.package, s.name, s.kind, s.signature, s.docstring, s.file, s.line_start, s.line_end, s.body
		FROM edges e
		JOIN symbols s ON s.id = e.%s
		WHERE e.%s = ? AND e.kind = ?
	`, returnCol, matchCol), symbolID, kind)
	if err != nil {
		return nil, fmt.Errorf("get related for %s: %w", symbolID, err)
	}
	defer rows.Close()

	return scanSymbols(rows)
}

// FuzzyGetSymbol tries progressively looser lookups for a symbol:
//  1. Exact ID match
//  2. ID suffix match (e.g. "svc.Server.CreateShipment" matches full qualified ID)
//  3. Name match (e.g. "CreateShipment" matches any symbol with that name)
//
// Returns the best match and all candidates (for disambiguation hints).
func (s *Store) FuzzyGetSymbol(query string) (*Symbol, []Symbol, error) {
	// 1. Exact match.
	sym, err := s.GetSymbol(query)
	if err == nil {
		return sym, nil, nil
	}

	// 2. Suffix match — the caller may have the right shape but wrong package prefix.
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE id LIKE ?
		ORDER BY length(id) ASC
		LIMIT 10
	`, "%"+query)
	if err != nil {
		return nil, nil, fmt.Errorf("suffix search %q: %w", query, err)
	}
	candidates, err := scanSymbols(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 1 {
		return &candidates[0], nil, nil
	}
	if len(candidates) > 1 {
		return &candidates[0], candidates, nil
	}

	// 3. Name match — extract the last component and search by name.
	// Fetch more candidates so we can pick the best one when multiple
	// symbols share the same name (common with generated vs hand-written code).
	name := query
	if idx := strings.LastIndex(query, "."); idx >= 0 {
		name = query[idx+1:]
	}
	rows, err = s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE name = ?
		LIMIT 20
	`, name)
	if err != nil {
		return nil, nil, fmt.Errorf("name search %q: %w", name, err)
	}
	candidates, err = scanSymbols(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 1 {
		return &candidates[0], nil, nil
	}
	if len(candidates) > 1 {
		best := pickBestCandidate(candidates, query)
		return best, candidates, nil
	}

	return nil, nil, ErrNotFound
}

// pickBestCandidate selects the most relevant symbol from a list of name-match
// candidates. It prefers candidates whose package path shares the most overlap
// with the original query string (the caller often provides a partially-qualified
// ID), and penalises symbols from generated packages.
func pickBestCandidate(candidates []Symbol, query string) *Symbol {
	queryLower := strings.ToLower(query)
	best := &candidates[0]
	bestScore := -1.0

	for i := range candidates {
		c := &candidates[i]
		score := 0.0

		// Penalise generated packages — the caller almost never wants these.
		pkgLower := strings.ToLower(c.Package)
		if strings.Contains(pkgLower, "/gen/") || strings.Contains(pkgLower, "unimplemented") {
			score -= 2.0
		}

		// Reward packages whose path appears verbatim in the query.
		// Longer overlap = better match.
		if strings.Contains(queryLower, pkgLower) {
			score += float64(len(c.Package))
		}

		// Reward hand-written svc/server/service packages.
		if strings.Contains(pkgLower, "svc") || strings.Contains(pkgLower, "server") || strings.Contains(pkgLower, "service") {
			score += 1.0
		}

		if score > bestScore {
			bestScore = score
			best = c
		}
	}

	return best
}

// GetCallersFromBody finds functions/methods whose stored body contains the
// given name followed by '('. This is a heuristic fallback for when call graph
// edges are missing — it catches callers like `svc.New(...)` or `Foo(...)`.
func (s *Store) GetCallersFromBody(name string) ([]Symbol, error) {
	// Search for "Name(" in body text. The % wildcards make this a substring match.
	pattern := "%" + name + "(%"
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols
		WHERE (kind = 'func' OR kind = 'method') AND body LIKE ?
		LIMIT 15
	`, pattern)
	if err != nil {
		return nil, fmt.Errorf("body callers search %q: %w", name, err)
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetByName returns all symbols with the given name (any kind).
func (s *Store) GetByName(name string) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE name = ?
		LIMIT 10
	`, name)
	if err != nil {
		return nil, fmt.Errorf("get by name %s: %w", name, err)
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetByNameAndKind returns all symbols with the given name and kind.
func (s *Store) GetByNameAndKind(name, kind string) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE name = ? AND kind = ?
	`, name, kind)
	if err != nil {
		return nil, fmt.Errorf("get by name/kind %s/%s: %w", name, kind, err)
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetByKind returns all symbols with the given kind.
func (s *Store) GetByKind(kind string) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE kind = ?
	`, kind)
	if err != nil {
		return nil, fmt.Errorf("get by kind %s: %w", kind, err)
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetByFile returns all symbols defined in a given file.
func (s *Store) GetByFile(file string) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols WHERE file = ?
		ORDER BY line_start
	`, file)
	if err != nil {
		return nil, fmt.Errorf("get symbols by file %s: %w", file, err)
	}
	defer rows.Close()

	return scanSymbols(rows)
}

// SetMeta stores a metadata key/value pair (e.g. last index time).
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO index_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}

	return nil
}

// GetMeta retrieves a metadata value by key.
func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM index_meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get meta %s: %w", key, err)
	}

	return value, nil
}

// UpsertConvention inserts or replaces a convention.
func (s *Store) UpsertConvention(c Convention) error {
	terms := strings.Join(c.Terms, ",")
	examples := strings.Join(c.Examples, ",")

	_, err := s.db.Exec(`
		INSERT INTO conventions (name, terms, description, structure, examples)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			terms       = excluded.terms,
			description = excluded.description,
			structure   = excluded.structure,
			examples    = excluded.examples
	`, c.Name, terms, c.Description, c.Structure, examples)
	if err != nil {
		return fmt.Errorf("upsert convention %s: %w", c.Name, err)
	}

	// Keep FTS in sync.
	_, _ = s.db.Exec(`DELETE FROM conventions_fts WHERE name = ?`, c.Name)
	_, err = s.db.Exec(`
		INSERT INTO conventions_fts (name, terms, description)
		VALUES (?, ?, ?)
	`, c.Name, terms, c.Description)
	if err != nil {
		return fmt.Errorf("insert conventions fts for %s: %w", c.Name, err)
	}

	return nil
}

// SearchConventions searches conventions by term using FTS.
func (s *Store) SearchConventions(query string) ([]Convention, error) {
	rows, err := s.db.Query(`
		SELECT c.name, c.terms, c.description, c.structure, c.examples
		FROM conventions_fts f
		JOIN conventions c ON c.name = f.name
		WHERE conventions_fts MATCH ?
		LIMIT 5
	`, query)
	if err != nil {
		return nil, fmt.Errorf("search conventions %q: %w", query, err)
	}
	defer rows.Close()

	return scanConventions(rows)
}

// GetConvention retrieves a single convention by name.
func (s *Store) GetConvention(name string) (*Convention, error) {
	row := s.db.QueryRow(`
		SELECT name, terms, description, structure, examples
		FROM conventions WHERE name = ?
	`, name)

	var c Convention
	var terms, examples string
	err := row.Scan(&c.Name, &terms, &c.Description, &c.Structure, &examples)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get convention %s: %w", name, err)
	}
	c.Terms = splitNonEmpty(terms, ",")
	c.Examples = splitNonEmpty(examples, ",")
	return &c, nil
}

// AllConventions returns all conventions in the store.
func (s *Store) AllConventions() ([]Convention, error) {
	rows, err := s.db.Query(`
		SELECT name, terms, description, structure, examples
		FROM conventions ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list conventions: %w", err)
	}
	defer rows.Close()

	return scanConventions(rows)
}

func scanConventions(rows *sql.Rows) ([]Convention, error) {
	var conventions []Convention
	for rows.Next() {
		var c Convention
		var terms, examples string
		if err := rows.Scan(&c.Name, &terms, &c.Description, &c.Structure, &examples); err != nil {
			return nil, fmt.Errorf("scan convention: %w", err)
		}
		c.Terms = splitNonEmpty(terms, ",")
		c.Examples = splitNonEmpty(examples, ",")
		conventions = append(conventions, c)
	}
	return conventions, nil
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// decomposeIdentifier splits a camelCase or PascalCase identifier into lowercase words.
// e.g. "addDispatchEventsToOutbox" → "add dispatch events to outbox"
// e.g. "BatchMutateActivities" → "batch mutate activities"
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

// ErrNotFound is returned when a requested symbol or key does not exist.
var ErrNotFound = fmt.Errorf("not found")

func scanSymbols(rows *sql.Rows) ([]Symbol, error) {
	var symbols []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(
			&sym.ID, &sym.Package, &sym.Name, &sym.Kind,
			&sym.Signature, &sym.Docstring, &sym.File,
			&sym.LineStart, &sym.LineEnd, &sym.Body,
		); err != nil {
			return nil, fmt.Errorf("scan symbol: %w", err)
		}
		symbols = append(symbols, sym)
	}

	return symbols, nil
}
