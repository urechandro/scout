package eval

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexTaskManifest(t *testing.T) {
	data, err := os.ReadFile("codex_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tasks []struct {
		ID       string `yaml:"id"`
		Category string `yaml:"category"`
		Prompt   string `yaml:"prompt"`
	}
	if err := yaml.Unmarshal(data, &tasks); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"explanation":            5,
		"bug-localization":       5,
		"implementation-pattern": 4,
		"refactor":               3,
		"cross-layer":            3,
	}
	got := make(map[string]int)
	seen := make(map[string]bool)
	for _, task := range tasks {
		if task.ID == "" || task.Prompt == "" {
			t.Errorf("task must have id and prompt: %+v", task)
		}
		if seen[task.ID] {
			t.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
		got[task.Category]++
	}
	if len(tasks) != 20 {
		t.Fatalf("task count = %d, want 20", len(tasks))
	}
	for category, count := range want {
		if got[category] != count {
			t.Errorf("%s task count = %d, want %d", category, got[category], count)
		}
	}
}
