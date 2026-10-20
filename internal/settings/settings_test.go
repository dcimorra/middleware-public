package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAFileWithoutTheAgentsSectionLoadsWithItsDefaults(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	os.WriteFile(Path(root), []byte(`{"panel_domain":"panel.test"}`), 0o644)
	s, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.PanelDomain != "panel.test" || s.Agents.MaxConcurrent != 8 || s.Agents.ReservedForHouse != 1 || s.Agents.DefaultProvider != "claude" || s.Agents.PerProvider["claude"] != 0 {
		t.Fatalf("settings = %+v", s)
	}
	// A partial section keeps what it says and fills the rest.
	os.WriteFile(Path(root), []byte(`{"agents":{"max_concurrent":2,"reserved_for_house":0}}`), 0o644)
	s, _ = Load(root)
	if s.Agents.MaxConcurrent != 2 || s.Agents.ReservedForHouse != 0 || s.Agents.DefaultProvider != "claude" || s.Agents.PerProvider == nil {
		t.Fatalf("partial section = %+v", s.Agents)
	}
}

func TestTheAgentsSectionIsValidatedAsAWhole(t *testing.T) {
	ok := DefaultAgents()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Agents{
		"zero":           {MaxConcurrent: 0, DefaultProvider: "claude"},
		"too many":       {MaxConcurrent: 65, DefaultProvider: "claude"},
		"reserved = max": {MaxConcurrent: 2, ReservedForHouse: 2, DefaultProvider: "claude"},
		"negative":       {MaxConcurrent: 2, ReservedForHouse: -1, DefaultProvider: "claude"},
		"provider < 0":   {MaxConcurrent: 2, PerProvider: map[string]int{"claude": -1}, DefaultProvider: "claude"},
		"no provider":    {MaxConcurrent: 2},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
