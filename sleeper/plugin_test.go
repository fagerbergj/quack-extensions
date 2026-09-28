package sleeper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// quackBuiltinTools are the quack-native tools the plugin's agents may name,
// each a key of quack's internal/tools/registry.go builtin map; add one only after checking it is there.
var quackBuiltinTools = map[string]bool{
	"current_date": true, "grep_artifacts": true, "summarize": true,
	"weather": true, "web_fetch": true, "web_search": true,
}

type pluginAgent struct {
	name   string
	dir    string
	Tools  []string `yaml:"tools"`
	Skills []string `yaml:"skills"`
}

func loadPluginAgents(t *testing.T) []pluginAgent {
	t.Helper()
	dirs, err := filepath.Glob("plugin/agents/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no plugin agents found: %v", err)
	}
	var out []pluginAgent
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		a := pluginAgent{name: filepath.Base(dir), dir: dir}
		if err := yaml.Unmarshal(raw, &a); err != nil {
			t.Fatalf("%s: agent.yaml: %v", a.name, err)
		}
		out = append(out, a)
	}
	return out
}

// TestPluginAgentToolsExist: quack drops a plugin agent naming a tool it cannot
// build, so every tool must be this extension's own or a quack builtin.
func TestPluginAgentToolsExist(t *testing.T) {
	registered := map[string]bool{}
	for _, tl := range testExtension(t).Tools() {
		registered[tl.Name()] = true
	}
	for _, a := range loadPluginAgents(t) {
		if len(a.Tools) == 0 {
			t.Errorf("%s: agent.yaml lists no tools", a.name)
		}
		for _, name := range a.Tools {
			if !registered[name] && !quackBuiltinTools[name] {
				t.Errorf("%s: tool %q is neither a sleeper extension tool nor an allowlisted quack builtin", a.name, name)
			}
		}
	}
}

// TestPluginAgentsShipRubricSkillAndSchema: each judged agent has a rubric, a
// plugin skill on disk, and a card artifact kind this extension declares a schema for.
func TestPluginAgentsShipRubricSkillAndSchema(t *testing.T) {
	schemas := testExtension(t).ArtifactSchemas()
	for _, a := range loadPluginAgents(t) {
		if _, err := os.Stat(filepath.Join(a.dir, "rubric.yaml")); err != nil {
			t.Errorf("%s: no rubric.yaml", a.name)
		}
		if len(a.Skills) == 0 {
			t.Errorf("%s: agent.yaml lists no skill", a.name)
		}
		for _, s := range a.Skills {
			name, ok := strings.CutPrefix(s, "sleeper:")
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join("plugin/skills", name, "SKILL.md")); err != nil {
				t.Errorf("%s: skill %q has no SKILL.md in the plugin", a.name, s)
			}
		}
		raw, err := os.ReadFile(filepath.Join(a.dir, "agent-card.json"))
		if err != nil {
			t.Fatal(err)
		}
		var card struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatalf("%s: agent-card.json: %v", a.name, err)
		}
		if _, ok := schemas[card.Artifact]; !ok {
			t.Errorf("%s: card artifact %q has no schema in ArtifactSchemas", a.name, card.Artifact)
		}
	}
}
