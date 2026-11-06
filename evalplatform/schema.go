package evalplatform

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Dataset is a versioned case list. Cases use input, expected, evaluation,
// and wait. The old prompt field is rejected.
type Dataset struct {
	Name  string `json:"name"`
	Cases []Case `json:"cases"`
}

// Case is one frozen conversation. Turns, when set, replace the top-level fields.
type Case struct {
	ID       string `json:"id"`
	Input    Input  `json:"input"`
	Expected any    `json:"expected"`
	Eval     Eval   `json:"evaluation"`
	Wait     any    `json:"wait"`
	Turns    []Turn `json:"turns,omitempty"`
}

// Turn is one user step inside a multi-turn case.
type Turn struct {
	Input    Input `json:"input"`
	Expected any   `json:"expected"`
	Eval     Eval  `json:"evaluation"`
	Wait     any   `json:"wait"`
}

// Input is the user text for one turn.
type Input struct {
	User string `json:"user"`
}

// Eval lists deterministic checks. An empty eval is skip, never pass.
type Eval struct {
	Keyword     *KeywordEval     `json:"keyword,omitempty"`
	Regex       *RegexEval       `json:"regex,omitempty"`
	Trace       *TraceEval       `json:"trace,omitempty"`
	MemoryWrite *MemoryWriteEval `json:"memory_write,omitempty"`
}

// KeywordEval requires every string in the assistant text.
type KeywordEval struct {
	Contains []string `json:"contains"`
}

// RegexEval requires every pattern in the assistant text.
type RegexEval struct {
	Patterns []string `json:"patterns"`
}

// TraceEval requires tool names and rejects forbidden ones.
type TraceEval struct {
	Required  []string `json:"required"`
	Forbidden []string `json:"forbidden"`
}

// MemoryWriteEval requires a recorded memory write.
type MemoryWriteEval struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

func (e Eval) configured() bool {
	return e.Keyword != nil || e.Regex != nil || e.Trace != nil || e.MemoryWrite != nil
}

// LoadDataset reads a case file and rejects the old prompt-only shape.
func LoadDataset(path string) (Dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, err
	}
	return ParseDataset(data)
}

// ParseDataset decodes a dataset and checks the case schema.
func ParseDataset(data []byte) (Dataset, error) {
	var raw struct {
		Name  string            `json:"name"`
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Dataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	out := Dataset{Name: raw.Name}
	for i, item := range raw.Cases {
		parsed, err := parseCase(item)
		if err != nil {
			return Dataset{}, fmt.Errorf("case %d: %w", i, err)
		}
		out.Cases = append(out.Cases, parsed)
	}
	return out, nil
}

func parseCase(data []byte) (Case, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return Case{}, err
	}
	if _, ok := probe["prompt"]; ok {
		return Case{}, fmt.Errorf("old field prompt is rejected; use input, expected, evaluation, and wait")
	}
	var parsed Case
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Case{}, err
	}
	if strings.TrimSpace(parsed.ID) == "" {
		return Case{}, fmt.Errorf("id is required")
	}
	if len(parsed.Turns) > 0 {
		for i, turn := range parsed.Turns {
			if err := requireTurn(turn.Input, probeTurn(data, i)); err != nil {
				return Case{}, fmt.Errorf("turn %d: %w", i, err)
			}
		}
		return parsed, nil
	}
	if err := requireFields(probe, "input", "expected", "evaluation"); err != nil {
		return Case{}, err
	}
	if strings.TrimSpace(parsed.Input.User) == "" {
		return Case{}, fmt.Errorf("input.user is required")
	}
	return parsed, nil
}

func probeTurn(data []byte, index int) map[string]json.RawMessage {
	var probe struct {
		Turns []map[string]json.RawMessage `json:"turns"`
	}
	_ = json.Unmarshal(data, &probe)
	if index >= len(probe.Turns) {
		return nil
	}
	return probe.Turns[index]
}

func requireTurn(input Input, raw map[string]json.RawMessage) error {
	if raw == nil {
		return fmt.Errorf("turn is missing")
	}
	if _, ok := raw["prompt"]; ok {
		return fmt.Errorf("old field prompt is rejected")
	}
	if err := requireFields(raw, "input", "expected", "evaluation"); err != nil {
		return err
	}
	if strings.TrimSpace(input.User) == "" {
		return fmt.Errorf("input.user is required")
	}
	return nil
}

func requireFields(raw map[string]json.RawMessage, names ...string) error {
	var missing []string
	for _, name := range names {
		if _, ok := raw[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// Steps returns the turns that a runner evaluates. A single-shot case is one step.
func (c Case) Steps() []Turn {
	if len(c.Turns) > 0 {
		return c.Turns
	}
	return []Turn{{Input: c.Input, Expected: c.Expected, Eval: c.Eval, Wait: c.Wait}}
}
