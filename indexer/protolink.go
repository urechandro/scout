package indexer

import (
	"fmt"
	"log"
	"strings"

	"github.com/urechandro/scout/store"
)

// LinkProtoToGo creates "implements" edges from Go server methods to their
// proto RPCs by matching names. When multiple methods share a name, the one
// in a package containing "svc" wins (matches the convention for service
// implementation packages). Individual edge upsert failures are logged but
// do not fail the call. Returns the number of edges linked.
func LinkProtoToGo(s *store.Store) (int, error) {
	// Rebuild, don't accumulate: clear every rpc-targeted implements edge
	// first. All RPCs are relinked below, so anything not re-added was
	// stale (e.g. the preferred impl moved to a svc package — the old
	// edge's endpoints both still exist, so file-level deletes never
	// cascade it away).
	if err := s.DeleteEdgesToSymbolKind("implements", "rpc"); err != nil {
		return 0, fmt.Errorf("clear stale rpc links: %w", err)
	}

	rpcs, err := s.GetByKind("rpc")
	if err != nil {
		return 0, fmt.Errorf("get rpcs: %w", err)
	}

	var linked int
	for _, rpc := range rpcs {
		methods, err := s.GetByNameAndKind(rpc.Name, "method")
		if err != nil || len(methods) == 0 {
			continue
		}
		impl := methods[0]
		for _, m := range methods {
			if strings.Contains(m.Package, "svc") {
				impl = m
				break
			}
		}
		edge := store.Edge{
			FromID: impl.ID,
			ToID:   rpc.ID,
			Kind:   "implements",
		}
		if err := s.UpsertEdge(edge); err != nil {
			log.Printf("link proto-go edge from %s to %s failed: %v", impl.ID, rpc.ID, err)
			continue
		}
		linked++
	}

	return linked, nil
}
