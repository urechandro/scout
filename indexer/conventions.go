package indexer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/urechandro/scout/store"
)

// conventionYAML is the on-disk format of a convention entry.
type conventionYAML struct {
	Name        string   `yaml:"name"`
	Terms       []string `yaml:"terms"`
	Description string   `yaml:"description"`
	Structure   string   `yaml:"structure"`
	Examples    []string `yaml:"examples"`
}

// IndexConventions reads conventions.yaml from dir and upserts entries into the store.
// If the file does not exist, it silently returns (conventions are optional).
func IndexConventions(dir string, s *store.Store) error {
	path := findConventionsFile(dir)
	if path == "" {
		log.Printf("no conventions.yaml found in %s (optional, skipping)", dir)
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read conventions file %s: %w", path, err)
	}

	var entries []conventionYAML
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse conventions file %s: %w", path, err)
	}

	for _, entry := range entries {
		c := store.Convention{
			Name:        entry.Name,
			Terms:       entry.Terms,
			Description: entry.Description,
			Structure:   entry.Structure,
			Examples:    entry.Examples,
		}
		if err := s.UpsertConvention(c); err != nil {
			return fmt.Errorf("upsert convention %s: %w", c.Name, err)
		}
	}

	log.Printf("indexed %d conventions from %s", len(entries), path)
	return nil
}

// findConventionsFile looks for conventions.yaml or conventions.yml in dir
// and its parent directories (up to 3 levels), returning the first match.
func findConventionsFile(dir string) string {
	for i := 0; i < 4; i++ {
		for _, name := range []string{"conventions.yaml", "conventions.yml"} {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
