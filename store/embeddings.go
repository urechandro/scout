package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
)

// EncodeEmbedding packs a float32 vector into a little-endian BLOB.
// Format is a tight 4-bytes-per-float layout — no header, length is derived
// from len(blob)/4. Matches what DecodeEmbedding expects.
func EncodeEmbedding(vec []float32) []byte {
	out := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

// DecodeEmbedding reverses EncodeEmbedding. Returns nil for empty input.
func DecodeEmbedding(blob []byte) []float32 {
	if len(blob) == 0 {
		return nil
	}
	n := len(blob) / 4
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out
}

// UpsertEmbedding writes a vector and its source model name to an existing
// symbol row. Does not create the symbol — callers should UpsertSymbol first.
func (s *Store) UpsertEmbedding(symbolID, model string, vec []float32) error {
	_, err := s.db.Exec(
		`UPDATE symbols SET embedding = ?, embedding_model = ? WHERE id = ?`,
		EncodeEmbedding(vec), model, symbolID,
	)
	if err != nil {
		return fmt.Errorf("upsert embedding for %s: %w", symbolID, err)
	}
	return nil
}

// EmbeddingRow is the (id, vector) pair returned by LoadEmbeddings for the
// in-memory vector slab the query engine cosines against.
type EmbeddingRow struct {
	ID     string
	Vector []float32
}

// LoadEmbeddings returns every symbol's stored vector for the given model.
// Rows whose embedding is NULL or was produced by a different model are skipped
// so the caller never compares vectors across model versions.
func (s *Store) LoadEmbeddings(model string) ([]EmbeddingRow, error) {
	rows, err := s.db.Query(
		`SELECT id, embedding FROM symbols WHERE embedding IS NOT NULL AND embedding_model = ?`,
		model,
	)
	if err != nil {
		return nil, fmt.Errorf("load embeddings for model %s: %w", model, err)
	}
	defer rows.Close()

	var out []EmbeddingRow
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		out = append(out, EmbeddingRow{ID: id, Vector: DecodeEmbedding(blob)})
	}
	return out, rows.Err()
}

// ListUnembedded returns symbols whose stored embedding is missing or was
// produced by a different model. Used by the indexer to incrementally fill
// in vectors without re-embedding the whole corpus.
func (s *Store) ListUnembedded(model string, limit int) ([]Symbol, error) {
	q := `
		SELECT id, package, name, kind, signature, docstring, file, line_start, line_end, body
		FROM symbols
		WHERE embedding IS NULL OR embedding_model != ?
	`
	args := []any{model}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list unembedded: %w", err)
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// EmbeddingSnapshot captures everything needed to restore a vector after a
// destructive reindex: the source text the vector was produced from (so we
// can verify it still matches), the model name (so we never mix vectors
// across model versions), and the vector itself.
type EmbeddingSnapshot struct {
	ID        string
	Name      string
	Signature string
	Docstring string
	Model     string
	Vector    []float32
}

// SnapshotEmbeddings reads every row that has a vector into memory. Called
// before `ResetIndex` so the embedder pass after the reindex can skip
// symbols whose source text didn't change. Holds the whole working set in
// RAM — at ~3KB per 768-dim vector, a 14k-symbol corpus is ~42MB, which is
// acceptable for a one-shot CLI but worth noting for very large repos.
func (s *Store) SnapshotEmbeddings() ([]EmbeddingSnapshot, error) {
	rows, err := s.db.Query(`
		SELECT id, name, signature, docstring, embedding_model, embedding
		FROM symbols
		WHERE embedding IS NOT NULL AND embedding_model != ''
	`)
	if err != nil {
		return nil, fmt.Errorf("snapshot embeddings: %w", err)
	}
	defer rows.Close()
	var out []EmbeddingSnapshot
	for rows.Next() {
		var snap EmbeddingSnapshot
		var blob []byte
		if err := rows.Scan(&snap.ID, &snap.Name, &snap.Signature, &snap.Docstring, &snap.Model, &blob); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		snap.Vector = DecodeEmbedding(blob)
		out = append(out, snap)
	}
	return out, rows.Err()
}

// RestoreEmbeddings writes snapshotted vectors back into the store, but only
// for symbols whose name+signature+docstring still match the snapshot.
// Returns the number of vectors actually restored. The rest stay missing,
// so the next embedder pass picks them up via `ListUnembedded`.
//
// All writes run in a single transaction — a partial restore would silently
// poison the embedder pass into assuming half the corpus is up to date.
func (s *Store) RestoreEmbeddings(snaps []EmbeddingSnapshot) (int, error) {
	if len(snaps) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin restore tx: %w", err)
	}
	check, err := tx.Prepare(`SELECT name, signature, docstring FROM symbols WHERE id = ?`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, fmt.Errorf("prepare check: %w", err)
	}
	defer check.Close()
	write, err := tx.Prepare(`UPDATE symbols SET embedding = ?, embedding_model = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, fmt.Errorf("prepare write: %w", err)
	}
	defer write.Close()

	restored := 0
	for _, snap := range snaps {
		var name, sig, doc string
		err := check.QueryRow(snap.ID).Scan(&name, &sig, &doc)
		if err == sql.ErrNoRows {
			// Symbol gone after reindex — skip, the vector is genuinely stale.
			continue
		}
		if err != nil {
			tx.Rollback() //nolint:errcheck
			return 0, fmt.Errorf("check %s: %w", snap.ID, err)
		}
		if name != snap.Name || sig != snap.Signature || doc != snap.Docstring {
			// Source text changed — let the embedder pass re-embed.
			continue
		}
		if _, err := write.Exec(EncodeEmbedding(snap.Vector), snap.Model, snap.ID); err != nil {
			tx.Rollback() //nolint:errcheck
			return 0, fmt.Errorf("restore %s: %w", snap.ID, err)
		}
		restored++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit restore: %w", err)
	}
	return restored, nil
}

// CountEmbeddings reports how many symbol rows have a vector for the given
// model. Useful for status output and tests.
func (s *Store) CountEmbeddings(model string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM symbols WHERE embedding IS NOT NULL AND embedding_model = ?`,
		model,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count embeddings: %w", err)
	}
	return n, nil
}
