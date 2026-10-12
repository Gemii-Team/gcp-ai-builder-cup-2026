package agents

import (
	"bytes"
	_ "embed" // registry.yaml
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"kiwi/internal/platform/llm"
)

//go:embed registry.yaml
var registryYAML []byte

// Spec is one agent of the registry: the same runtime with a different instruction,
// model tier and tool allowlist. Name is the handle a person @mentions.
type Spec struct {
	Name        string
	Instruction string
	Tier        llm.Tier
	// Tools are the tools the agent may call, by name; each is one of toolNames.
	Tools []string
	// KnowledgeFilter names the document collection the agent is limited to. The
	// knowledge index has no collections yet, so it is empty for @assistant and
	// nothing reads it until the first agent that needs one (@hr).
	KnowledgeFilter string
}

// entry is a Spec as registry.yaml writes it.
type entry struct {
	Instruction     string   `yaml:"instruction"`
	Tier            string   `yaml:"tier"`
	Tools           []string `yaml:"tools"`
	KnowledgeFilter string   `yaml:"knowledge_filter"`
}

// LoadRegistry reads the agents of the embedded registry.yaml, by name. It fails on an
// entry that names a tool that does not exist, so an allowlist can never promise
// more than the code offers. Every call returns a registry of its own.
func LoadRegistry() (map[string]Spec, error) { return parseRegistry(registryYAML) }

// parseRegistry reads and checks the registry document doc.
func parseRegistry(doc []byte) (map[string]Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	var entries map[string]entry
	if err := dec.Decode(&entries); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("agents: read the registry: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("agents: the registry is one YAML document")
	}
	if len(entries) == 0 {
		return nil, errors.New("agents: the registry lists no agent")
	}

	reg := make(map[string]Spec, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		spec, err := entries[name].spec(name)
		if err != nil {
			return nil, fmt.Errorf("agents: registry entry %q: %w", name, err)
		}
		reg[name] = spec
	}
	return reg, nil
}

// spec checks e and turns it into the Spec called name.
func (e entry) spec(name string) (Spec, error) {
	instruction := strings.TrimSpace(e.Instruction)
	if name == "" || instruction == "" {
		return Spec{}, errors.New("an agent needs a name and an instruction")
	}
	tier := llm.Tier(e.Tier)
	if tier != llm.TierFlash && tier != llm.TierPro {
		return Spec{}, fmt.Errorf("tier %q is not %q or %q", e.Tier, llm.TierFlash, llm.TierPro)
	}
	if len(e.Tools) == 0 {
		return Spec{}, errors.New("an agent needs at least one tool")
	}
	for i, tool := range e.Tools {
		if !slices.Contains(toolNames, tool) {
			return Spec{}, fmt.Errorf("unknown tool %q: the tools are %s", tool, strings.Join(toolNames, ", "))
		}
		if slices.Contains(e.Tools[:i], tool) {
			return Spec{}, fmt.Errorf("tool %q is listed twice", tool)
		}
	}
	return Spec{
		Name: name, Instruction: instruction, Tier: tier,
		Tools: slices.Clone(e.Tools), KnowledgeFilter: strings.TrimSpace(e.KnowledgeFilter),
	}, nil
}
